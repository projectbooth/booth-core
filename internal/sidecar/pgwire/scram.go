package pgwire

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/pbkdf2"
)

// SCRAM-SHA-256 client (RFC 5802/7677) — PostgreSQL's default password authentication method
// since v10, and what core's own bundled Postgres actually requires (internal/dbprov.scramVerifier
// derives the exact same stored-verifier shape this implements the client side of, confirming this
// isn't a hypothetical auth method to support). Channel binding is not implemented (this proxy
// speaks plaintext to upstream in v0 — see docs/decisions/0015 — so `-PLUS` variants don't apply).

// SASLMechanism is the exact string PostgreSQL advertises for this method.
const SASLMechanism = "SCRAM-SHA-256"

// scramClient holds state across the two round trips a SCRAM exchange needs.
type scramClient struct {
	password        string
	clientNonce     string
	clientFirstBare string // the "n=...,r=..." part, needed again to build the auth message
}

func newScramClient(password string) (*scramClient, error) {
	nonce, err := randomNonce()
	if err != nil {
		return nil, err
	}
	return &scramClient{password: password, clientNonce: nonce}, nil
}

func randomNonce() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating SCRAM nonce: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// saslEscape applies RFC 5802's required escaping of ',' and '=' in a SASL name.
func saslEscape(s string) string {
	s = strings.ReplaceAll(s, "=", "=3D")
	s = strings.ReplaceAll(s, ",", "=2C")
	return s
}

// clientFirstWire builds the bytes sent as the SASLInitialResponse body: the gs2 header ("n,," —
// no channel binding, no authzid) immediately followed by the bare client-first-message
// ("n=<user>,r=<nonce>"), with no separator between them. clientFirstBare (without the gs2
// header) is recorded for reuse when computing the auth message in ClientFinalMessage.
func (c *scramClient) clientFirstWire(user string) []byte {
	c.clientFirstBare = "n=" + saslEscape(user) + ",r=" + c.clientNonce
	return []byte("n,," + c.clientFirstBare)
}

// serverFirst is the parsed "r=<nonce>,s=<salt>,i=<iterations>" server response.
type serverFirst struct {
	nonce      string
	salt       []byte
	iterations int
}

func parseServerFirst(msg []byte) (serverFirst, error) {
	var out serverFirst
	for _, part := range strings.Split(string(msg), ",") {
		if len(part) < 2 || part[1] != '=' {
			continue
		}
		switch part[0] {
		case 'r':
			out.nonce = part[2:]
		case 's':
			salt, err := base64.StdEncoding.DecodeString(part[2:])
			if err != nil {
				return serverFirst{}, fmt.Errorf("scram: bad salt encoding: %w", err)
			}
			out.salt = salt
		case 'i':
			n, err := strconv.Atoi(part[2:])
			if err != nil {
				return serverFirst{}, fmt.Errorf("scram: bad iteration count: %w", err)
			}
			out.iterations = n
		}
	}
	if out.nonce == "" || out.salt == nil || out.iterations <= 0 {
		return serverFirst{}, errors.New("scram: server-first-message missing r/s/i")
	}
	return out, nil
}

// ClientFinalMessage computes the client's proof and returns the final message bytes to send, per
// RFC 5802 §3. serverNonce must start with the client's own nonce (checked by the caller via
// VerifyServerNonce) before calling this.
func (c *scramClient) ClientFinalMessage(sf serverFirst, serverFirstRaw []byte) ([]byte, []byte, error) {
	saltedPassword := pbkdf2.Key([]byte(c.password), sf.salt, sf.iterations, sha256.Size, sha256.New)
	clientKey := hmacSHA256(saltedPassword, "Client Key")
	storedKey := sha256.Sum256(clientKey)

	channelBinding := base64.StdEncoding.EncodeToString([]byte("n,,")) // no channel binding
	clientFinalWithoutProof := "c=" + channelBinding + ",r=" + sf.nonce

	authMessage := c.clientFirstBare + "," + string(serverFirstRaw) + "," + clientFinalWithoutProof
	clientSignature := hmacSHA256(storedKey[:], authMessage)
	clientProof := xorBytes(clientKey, clientSignature)

	serverKey := hmacSHA256(saltedPassword, "Server Key")
	expectedServerSignature := hmacSHA256(serverKey, authMessage)

	final := clientFinalWithoutProof + ",p=" + base64.StdEncoding.EncodeToString(clientProof)
	return []byte(final), expectedServerSignature, nil
}

// VerifyServerFinal checks the server's final "v=<signature>" against what the client computed,
// per RFC 5802 §3's mutual-authentication guarantee — without this, a man-in-the-middle that knows
// nothing about the password could still complete the exchange from the client's point of view.
func VerifyServerFinal(serverFinal []byte, expectedSignature []byte) error {
	s := string(serverFinal)
	if !strings.HasPrefix(s, "v=") {
		return fmt.Errorf("scram: malformed server-final-message: %q", s)
	}
	got, err := base64.StdEncoding.DecodeString(s[2:])
	if err != nil {
		return fmt.Errorf("scram: bad server signature encoding: %w", err)
	}
	if !hmac.Equal(got, expectedSignature) {
		return errors.New("scram: server signature mismatch — possible man-in-the-middle")
	}
	return nil
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

func xorBytes(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// saslInitialResponse builds the full 'p' message body for the first SASL round trip: mechanism
// name, NUL, Int32 length, response bytes.
func saslInitialResponse(mechanism string, response []byte) []byte {
	var out []byte
	out = append(out, mechanism...)
	out = append(out, 0)
	out = binary.BigEndian.AppendUint32(out, uint32(len(response)))
	out = append(out, response...)
	return out
}

// PerformSCRAM runs the full SCRAM-SHA-256 exchange as the client (ADR 0095's proxy authenticating
// to the real upstream Postgres), assuming the caller has already read the AuthenticationSASL
// message naming SCRAM-SHA-256 as an available mechanism. Returns nil only once the server's final
// signature has verified and AuthenticationOK has been received — anything else is a hard failure,
// never a partial/best-effort success, since SCRAM's whole point is mutual authentication.
func PerformSCRAM(r *bufio.Reader, w io.Writer, user, password string) error {
	client, err := newScramClient(password)
	if err != nil {
		return err
	}

	if err := WriteMessage(w, typePasswordMessage, saslInitialResponse(SASLMechanism, client.clientFirstWire(user))); err != nil {
		return fmt.Errorf("scram: sending client-first-message: %w", err)
	}

	msg, err := ReadMessage(r)
	if err != nil {
		return fmt.Errorf("scram: reading server-first-message: %w", err)
	}
	subCode, serverFirstRaw, err := ParseAuthenticationMessage(msg)
	if err != nil {
		return err
	}
	if subCode != authSASLContinue {
		return fmt.Errorf("scram: expected AuthenticationSASLContinue (11), got sub-code %d", subCode)
	}
	sf, err := parseServerFirst(serverFirstRaw)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(sf.nonce, client.clientNonce) {
		return errors.New("scram: server nonce does not extend the client nonce — possible attack")
	}

	finalMsg, expectedSig, err := client.ClientFinalMessage(sf, serverFirstRaw)
	if err != nil {
		return err
	}
	if err := WriteMessage(w, typePasswordMessage, finalMsg); err != nil {
		return fmt.Errorf("scram: sending client-final-message: %w", err)
	}

	msg, err = ReadMessage(r)
	if err != nil {
		return fmt.Errorf("scram: reading server-final-message: %w", err)
	}
	subCode, serverFinalRaw, err := ParseAuthenticationMessage(msg)
	if err != nil {
		return err
	}
	if subCode != authSASLFinal {
		return fmt.Errorf("scram: expected AuthenticationSASLFinal (12), got sub-code %d", subCode)
	}
	if err := VerifyServerFinal(serverFinalRaw, expectedSig); err != nil {
		return err
	}

	msg, err = ReadMessage(r)
	if err != nil {
		return fmt.Errorf("scram: reading final AuthenticationOK: %w", err)
	}
	subCode, _, err = ParseAuthenticationMessage(msg)
	if err != nil {
		return err
	}
	if subCode != authOK {
		return fmt.Errorf("scram: expected AuthenticationOK (0) after a verified exchange, got sub-code %d", subCode)
	}
	return nil
}
