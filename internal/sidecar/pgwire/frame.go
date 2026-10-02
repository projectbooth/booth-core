// Package pgwire implements just enough of the PostgreSQL frontend/backend protocol (v3,
// https://www.postgresql.org/docs/current/protocol.html) for the credential sidecar's `postgres`
// mode (ADR 0095) to terminate a client's connection and separately authenticate to the real
// server with a broker-issued credential: StartupMessage, the authentication message family
// (trust/cleartext/MD5/SCRAM-SHA-256), and the handful of messages needed to complete a handshake.
// Ordinary query traffic after a handshake is never parsed — once both sides are authenticated,
// the proxy relays raw bytes (see sidecar.PostgresProxy), which is why this package stops here
// rather than modeling the rest of the protocol.
package pgwire

import (
	"bufio"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Backend/frontend message type bytes this package knows about (protocol v3).
const (
	typeAuthentication   = 'R'
	typeParameterStatus  = 'S'
	typeBackendKeyData   = 'K'
	typeReadyForQuery    = 'Z'
	typeErrorResponse    = 'E'
	typePasswordMessage  = 'p' // also used for SASLInitialResponse/SASLResponse (same type byte)
	typeNegotiateVersion = 'v'
)

// Authentication request sub-codes (the Int32 following the 'R' type+length).
const (
	authOK                = 0
	authCleartextPassword = 3
	authMD5Password       = 5
	authSASL              = 10
	authSASLContinue      = 11
	authSASLFinal         = 12
)

// protocolVersion3 is the only startup protocol version this package speaks.
const protocolVersion3 = 196608 // 3 << 16 | 0

// sslRequestCode is sent instead of a real StartupMessage when a client probes for TLS.
const sslRequestCode = 80877103

// Message is one backend-to-frontend (or frontend-to-backend) regular message: a type byte
// followed by a length-prefixed payload. Startup-phase messages (StartupMessage, SSLRequest, and
// password/SASL responses before the type byte was introduced historically) are handled by their
// own functions below rather than through this generic type, matching the protocol's own
// asymmetry (the very first frontend message has no type byte).
type Message struct {
	Type    byte
	Payload []byte
}

// ReadMessage reads one regular (typed) message.
func ReadMessage(r *bufio.Reader) (Message, error) {
	head := make([]byte, 5)
	if _, err := io.ReadFull(r, head); err != nil {
		return Message{}, err
	}
	length := int(binary.BigEndian.Uint32(head[1:5]))
	if length < 4 {
		return Message{}, fmt.Errorf("pgwire: invalid message length %d", length)
	}
	payload := make([]byte, length-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Message{}, err
	}
	return Message{Type: head[0], Payload: payload}, nil
}

// WriteMessage writes one regular (typed) message.
func WriteMessage(w io.Writer, typ byte, payload []byte) error {
	buf := make([]byte, 0, 5+len(payload))
	buf = append(buf, typ)
	buf = binary.BigEndian.AppendUint32(buf, uint32(4+len(payload)))
	buf = append(buf, payload...)
	_, err := w.Write(buf)
	return err
}

// StartupParams is the key/value pairs a client sends in its StartupMessage (ADR 0095's proxy
// reads `user`/`database` from this; everything else is passed through unused).
type StartupParams map[string]string

// ReadStartupOrSSLRequest reads the client's very first message, which has no type byte: either a
// real StartupMessage (returns params, sslRequest=false) or an SSLRequest probe (returns
// sslRequest=true, params=nil — the proxy answers 'N' and expects a real StartupMessage next,
// since TLS termination is out of scope for v0; see docs/decisions/0015's residual limits).
func ReadStartupOrSSLRequest(r *bufio.Reader) (params StartupParams, sslRequest bool, err error) {
	head := make([]byte, 8)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, false, err
	}
	length := int(binary.BigEndian.Uint32(head[0:4]))
	code := int32(binary.BigEndian.Uint32(head[4:8]))

	if code == sslRequestCode {
		if length != 8 {
			return nil, false, fmt.Errorf("pgwire: malformed SSLRequest length %d", length)
		}
		return nil, true, nil
	}
	if int(code) != protocolVersion3 {
		return nil, false, fmt.Errorf("pgwire: unsupported startup protocol version %d", code)
	}
	if length < 9 {
		return nil, false, fmt.Errorf("pgwire: invalid StartupMessage length %d", length)
	}
	rest := make([]byte, length-8)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, false, err
	}
	params, err = parseCStringPairs(rest)
	return params, false, err
}

func parseCStringPairs(b []byte) (StartupParams, error) {
	out := StartupParams{}
	strs, err := splitCStrings(b)
	if err != nil {
		return nil, err
	}
	// The list ends with one trailing empty string (the final \0 after the last pair); drop it.
	if len(strs) > 0 && strs[len(strs)-1] == "" {
		strs = strs[:len(strs)-1]
	}
	if len(strs)%2 != 0 {
		return nil, errors.New("pgwire: odd number of StartupMessage key/value strings")
	}
	for i := 0; i < len(strs); i += 2 {
		out[strs[i]] = strs[i+1]
	}
	return out, nil
}

func splitCStrings(b []byte) ([]string, error) {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	if start != len(b) {
		return nil, errors.New("pgwire: trailing bytes after last NUL-terminated string")
	}
	return out, nil
}

// WriteSSLDenied answers an SSLRequest with 'N' (TLS not supported by this proxy).
func WriteSSLDenied(w io.Writer) error {
	_, err := w.Write([]byte{'N'})
	return err
}

// WriteStartupMessage sends the proxy's own StartupMessage to the real upstream server.
func WriteStartupMessage(w io.Writer, params StartupParams) error {
	var body []byte
	body = binary.BigEndian.AppendUint32(body, protocolVersion3)
	for k, v := range params {
		body = append(body, k...)
		body = append(body, 0)
		body = append(body, v...)
		body = append(body, 0)
	}
	body = append(body, 0)
	out := make([]byte, 0, 4+len(body))
	out = binary.BigEndian.AppendUint32(out, uint32(4+len(body)))
	out = append(out, body...)
	_, err := w.Write(out)
	return err
}

// WriteAuthenticationOK writes the backend's "authentication succeeded" message.
func WriteAuthenticationOK(w io.Writer) error {
	return WriteMessage(w, typeAuthentication, binary.BigEndian.AppendUint32(nil, authOK))
}

// WriteParameterStatus writes one ParameterStatus message.
func WriteParameterStatus(w io.Writer, key, value string) error {
	payload := append(append([]byte(key), 0), append([]byte(value), 0)...)
	return WriteMessage(w, typeParameterStatus, payload)
}

// WriteBackendKeyData writes a (fabricated, in the proxy's case — see its own doc comment on why
// real cancel-request routing isn't supported in v0) BackendKeyData message.
func WriteBackendKeyData(w io.Writer, pid, secret int32) error {
	payload := binary.BigEndian.AppendUint32(nil, uint32(pid))
	payload = binary.BigEndian.AppendUint32(payload, uint32(secret))
	return WriteMessage(w, typeBackendKeyData, payload)
}

// WriteReadyForQuery writes ReadyForQuery with transaction status 'I' (idle).
func WriteReadyForQuery(w io.Writer) error {
	return WriteMessage(w, typeReadyForQuery, []byte{'I'})
}

// WriteErrorResponse writes a minimal ErrorResponse (severity + message + terminator), enough for
// a real client library to surface something readable without implementing every SQLSTATE field.
func WriteErrorResponse(w io.Writer, severity, message string) error {
	var payload []byte
	payload = append(payload, 'S')
	payload = append(payload, severity...)
	payload = append(payload, 0)
	payload = append(payload, 'M')
	payload = append(payload, message...)
	payload = append(payload, 0)
	payload = append(payload, 0)
	return WriteMessage(w, typeErrorResponse, payload)
}

// ParseAuthenticationMessage splits an 'R' message into its sub-code and remaining payload.
func ParseAuthenticationMessage(m Message) (subCode int32, payload []byte, err error) {
	if m.Type != typeAuthentication {
		return 0, nil, fmt.Errorf("pgwire: expected an Authentication ('R') message, got %q", m.Type)
	}
	if len(m.Payload) < 4 {
		return 0, nil, errors.New("pgwire: truncated Authentication message")
	}
	return int32(binary.BigEndian.Uint32(m.Payload[:4])), m.Payload[4:], nil
}

// WritePasswordMessage sends a cleartext or pre-hashed password response.
func WritePasswordMessage(w io.Writer, password string) error {
	payload := append([]byte(password), 0)
	return WriteMessage(w, typePasswordMessage, payload)
}

// MD5Password computes the `md5` + salt digest PostgreSQL's AuthenticationMD5Password expects:
// "md5" + md5(md5(password+user)+salt), hex-encoded.
func MD5Password(user, password string, salt [4]byte) string {
	inner := md5.Sum([]byte(password + user))
	outer := md5.Sum(append([]byte(hex.EncodeToString(inner[:])), salt[:]...))
	return "md5" + hex.EncodeToString(outer[:])
}
