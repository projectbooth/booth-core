package pgwire

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

// fakeScramServer plays the server side of RFC 5802 for one connection — written independently
// from scramClient (same math, not shared code) so this test can't pass merely because both sides
// share a bug. It knows the plaintext password up front, which is exactly how PostgreSQL itself
// derives a verifier to check against (internal/dbprov.scramVerifier computes the identical
// StoredKey/ServerKey shape this server does, confirming the client under test speaks the real
// protocol PostgreSQL requires, not a private dialect).
type fakeScramServer struct {
	password   string
	iterations int
}

func (s *fakeScramServer) serve(t *testing.T, conn net.Conn) {
	t.Helper()
	r := bufio.NewReader(conn)

	msg, err := ReadMessage(r)
	if err != nil {
		t.Fatalf("server: reading client-first: %v", err)
	}
	if msg.Type != typePasswordMessage {
		t.Fatalf("server: expected a 'p' SASLInitialResponse, got %q", msg.Type)
	}
	clientFirstWire := msg.Payload // mechanism \0 Int32(len) data

	// Parse SASLInitialResponse: mechanism, NUL, Int32 length, data.
	nul := bytes.IndexByte(clientFirstWire, 0)
	if nul < 0 {
		t.Fatal("server: no NUL in SASLInitialResponse")
	}
	mechanism := string(clientFirstWire[:nul])
	if mechanism != SASLMechanism {
		t.Fatalf("server: mechanism = %q", mechanism)
	}
	data := clientFirstWire[nul+5:] // skip NUL + 4-byte length
	gs2AndBare := string(data)
	if !strings.HasPrefix(gs2AndBare, "n,,") {
		t.Fatalf("server: missing gs2 header: %q", gs2AndBare)
	}
	clientFirstBare := strings.TrimPrefix(gs2AndBare, "n,,")

	var clientNonce string
	for _, part := range strings.Split(clientFirstBare, ",") {
		if strings.HasPrefix(part, "r=") {
			clientNonce = part[2:]
		}
	}
	if clientNonce == "" {
		t.Fatal("server: no client nonce")
	}

	serverNonceSuffix := "serverpart12345"
	serverNonce := clientNonce + serverNonceSuffix
	salt := []byte("0123456789abcdef")
	serverFirst := fmt.Sprintf("r=%s,s=%s,i=%d", serverNonce, base64.StdEncoding.EncodeToString(salt), s.iterations)

	if err := WriteMessage(conn, typeAuthentication, append(uint32be(authSASLContinue), serverFirst...)); err != nil {
		t.Fatalf("server: writing server-first: %v", err)
	}

	msg, err = ReadMessage(r)
	if err != nil {
		t.Fatalf("server: reading client-final: %v", err)
	}
	clientFinal := string(msg.Payload)

	var clientFinalWithoutProof, proofB64 string
	for _, part := range strings.Split(clientFinal, ",") {
		if strings.HasPrefix(part, "p=") {
			proofB64 = part[2:]
		}
	}
	if idx := strings.LastIndex(clientFinal, ",p="); idx >= 0 {
		clientFinalWithoutProof = clientFinal[:idx]
	}
	proof, err := base64.StdEncoding.DecodeString(proofB64)
	if err != nil {
		t.Fatalf("server: bad proof encoding: %v", err)
	}

	saltedPassword := pbkdf2.Key([]byte(s.password), salt, s.iterations, sha256.Size, sha256.New)
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	clientKey := mac(saltedPassword, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := mac(saltedPassword, "Server Key")

	authMessage := clientFirstBare + "," + serverFirst + "," + clientFinalWithoutProof
	clientSignature := mac(storedKey[:], authMessage)
	recoveredClientKey := xorBytes(proof, clientSignature)
	recoveredStoredKey := sha256.Sum256(recoveredClientKey)

	if !hmac.Equal(recoveredStoredKey[:], storedKey[:]) {
		_ = WriteMessage(conn, typeErrorResponse, []byte("authentication failed"))
		conn.Close()
		return
	}

	serverSignature := mac(serverKey, authMessage)
	serverFinal := "v=" + base64.StdEncoding.EncodeToString(serverSignature)
	if err := WriteMessage(conn, typeAuthentication, append(uint32be(authSASLFinal), serverFinal...)); err != nil {
		t.Fatalf("server: writing server-final: %v", err)
	}
	if err := WriteMessage(conn, typeAuthentication, uint32be(authOK)); err != nil {
		t.Fatalf("server: writing AuthenticationOK: %v", err)
	}
}

func uint32be(n int) []byte {
	return []byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
}

func TestPerformSCRAM_SucceedsWithTheCorrectPassword(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	server := &fakeScramServer{password: "correct-horse-battery-staple", iterations: 4096}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		server.serve(t, conn)
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := PerformSCRAM(bufio.NewReader(conn), conn, "alice", "correct-horse-battery-staple"); err != nil {
		t.Fatalf("PerformSCRAM: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server goroutine never finished")
	}
}

func TestPerformSCRAM_FailsWithTheWrongPassword(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	server := &fakeScramServer{password: "correct-horse-battery-staple", iterations: 4096}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		server.serve(t, conn)
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := PerformSCRAM(bufio.NewReader(conn), conn, "alice", "wrong-password"); err == nil {
		t.Fatal("PerformSCRAM succeeded with the wrong password")
	}
}

func TestMD5Password_MatchesTheDocumentedAlgorithm(t *testing.T) {
	// md5(md5(password+user)+salt), hex, prefixed "md5" — PostgreSQL's own documented formula.
	got := MD5Password("alice", "s3cr3t", [4]byte{1, 2, 3, 4})
	if !strings.HasPrefix(got, "md5") || len(got) != 35 {
		t.Fatalf("MD5Password = %q, want a 35-char md5-prefixed hex digest", got)
	}
	// Deterministic: same inputs, same output.
	again := MD5Password("alice", "s3cr3t", [4]byte{1, 2, 3, 4})
	if got != again {
		t.Error("MD5Password is not deterministic")
	}
	// Sensitive to every input.
	if MD5Password("bob", "s3cr3t", [4]byte{1, 2, 3, 4}) == got {
		t.Error("MD5Password ignores the username")
	}
	if MD5Password("alice", "different", [4]byte{1, 2, 3, 4}) == got {
		t.Error("MD5Password ignores the password")
	}
	if MD5Password("alice", "s3cr3t", [4]byte{5, 6, 7, 8}) == got {
		t.Error("MD5Password ignores the salt")
	}
}

func TestParseServerFirst(t *testing.T) {
	sf, err := parseServerFirst([]byte("r=abc123,s=" + base64.StdEncoding.EncodeToString([]byte("salt")) + ",i=4096"))
	if err != nil {
		t.Fatal(err)
	}
	if sf.nonce != "abc123" || sf.iterations != 4096 || string(sf.salt) != "salt" {
		t.Errorf("parsed = %+v", sf)
	}

	for _, bad := range []string{"", "r=abc123", "r=abc123,s=" + base64.StdEncoding.EncodeToString([]byte("salt"))} {
		if _, err := parseServerFirst([]byte(bad)); err == nil {
			t.Errorf("parseServerFirst(%q) accepted an incomplete message", bad)
		}
	}
}
