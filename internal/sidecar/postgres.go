package sidecar

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/projectbooth/booth-core/internal/credentialbroker"
	"github.com/projectbooth/booth-core/internal/sidecar/pgwire"
)

// PostgresCredential is the shape of a postgres-kind broker response's Credential field — not
// fixed by contracts/credential-broker.md (kind-specific, opaque to the broker itself), but
// matching booth-database's actual provider exactly, field for field
// (booth-database/internal/credentialbroker/provider.go's pgCredential — see docs/decisions/0015).
type PostgresCredential struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"username"`
	Password string `json:"password"`
	// SSLMode is advisory only in v0 — the proxy always speaks plaintext to upstream (matching
	// core's own bundled-Postgres connections, which run sslmode=disable); see docs/decisions/
	// 0015's residual limits. Accepted and logged if set to anything requiring TLS, not acted on.
	SSLMode string `json:"sslMode,omitempty"`
}

// PostgresProxy is the `--kind=postgres` mode (ADR 0095, contracts/credential-sidecar.md): a
// stateful TCP proxy that terminates a client's Postgres wire-protocol handshake locally (trust —
// the pod's own network namespace is the trust boundary, since this never binds beyond loopback)
// and separately authenticates to the real upstream with whatever credential the Renewer most
// recently obtained. GET /healthz is served on the same listener by sniffing the first bytes of
// each new connection (contracts/credential-sidecar.md: "the same loopback-only listener's port
// space... a second path on the proxy's own address").
type PostgresProxy struct {
	renewer *Renewer

	current atomic.Pointer[PostgresCredential]

	// Dial is overridden in tests to connect to a fixture upstream without a real network.
	Dial func(network, address string) (net.Conn, error)
}

// NewPostgresProxy builds a proxy whose upstream credential is kept current by renewer — call
// Listen to accept connections once renewer.Run has obtained at least one lease (Listen itself
// doesn't block on that; a client connecting before then just gets ErrorResponse, matching the
// contract's "a notebook kernel or pipeline task that starts before its sidecar has a lease should
// wait, not fail with an opaque connection-refused" — healthz is what that readiness probe checks).
func NewPostgresProxy(renewer *Renewer) *PostgresProxy {
	p := &PostgresProxy{renewer: renewer, Dial: net.Dial}
	renewer.OnRenew = p.setCredential
	return p
}

func (p *PostgresProxy) setCredential(resp credentialbroker.Response) {
	var cred PostgresCredential
	if err := decodeStrict(resp.Credential, &cred); err != nil {
		log.Printf("sidecar: postgres credential from lease %s is unparseable, keeping the previous one: %v", resp.LeaseID, err)
		return
	}
	p.current.Store(&cred)
}

// Listen accepts connections on addr until ctx is canceled. addr must be a loopback address or a
// Unix socket path (the caller is responsible for choosing one — contracts/credential-sidecar.md's
// "--listen ... never a non-loopback interface" is enforced by the chart/caller, not re-validated
// here, the same trust-the-deployment-not-the-binary posture core's own chart RBAC comments use).
func (p *PostgresProxy) Listen(ctx context.Context, network, addr string) error {
	ln, err := net.Listen(network, addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go p.handleConn(conn)
	}
}

// httpSniffPrefixes are the request-line starts ReadStartupOrSSLRequest's first 8 bytes could
// never produce from a real Postgres client (a StartupMessage's first 4 bytes are a length, and
// its next 4 are the protocol version 0x00030000 or the SSLRequest code 0x04D2162F — neither looks
// like ASCII text), so an 8-byte prefix matching one of these is unambiguously HTTP.
var httpSniffPrefixes = [][]byte{[]byte("GET "), []byte("HEAD ")}

func (p *PostgresProxy) handleConn(client net.Conn) {
	defer client.Close()

	br := bufio.NewReader(client)
	peek, err := br.Peek(8)
	if err == nil {
		for _, prefix := range httpSniffPrefixes {
			if bytes.HasPrefix(peek, prefix) {
				p.serveHealthz(client, br)
				return
			}
		}
	}

	params, sslRequested, err := pgwire.ReadStartupOrSSLRequest(br)
	if err != nil {
		log.Printf("sidecar: reading client startup: %v", err)
		return
	}
	if sslRequested {
		if err := pgwire.WriteSSLDenied(client); err != nil {
			return
		}
		params, _, err = pgwire.ReadStartupOrSSLRequest(br)
		if err != nil {
			log.Printf("sidecar: reading client startup after SSL denial: %v", err)
			return
		}
	}
	_ = params // user/database the client asked for; the credential's own values are authoritative

	cred := p.current.Load()
	if cred == nil {
		_ = pgwire.WriteErrorResponse(client, "FATAL", "no credential lease obtained yet")
		return
	}

	upstream, err := p.dialAndAuthenticate(*cred)
	if err != nil {
		log.Printf("sidecar: upstream authentication failed: %v", err)
		_ = pgwire.WriteErrorResponse(client, "FATAL", "upstream authentication failed")
		return
	}
	defer upstream.Close()

	if err := pgwire.WriteAuthenticationOK(client); err != nil {
		return
	}
	for _, kv := range [][2]string{{"server_version", "16.0"}, {"client_encoding", "UTF8"}} {
		if err := pgwire.WriteParameterStatus(client, kv[0], kv[1]); err != nil {
			return
		}
	}
	// A fabricated BackendKeyData: real cancel-request routing (a client reconnecting to send a
	// CancelRequest keyed on this pid/secret) isn't supported in v0 — a documented residual limit,
	// not silent data loss, since a cancel request is an optimization (killing a running query)
	// that simply won't do anything here, not a correctness issue for ordinary query traffic.
	if err := pgwire.WriteBackendKeyData(client, int32(rand.Int31()), int32(rand.Int31())); err != nil {
		return
	}
	if err := pgwire.WriteReadyForQuery(client); err != nil {
		return
	}

	relay(client, upstream)
}

// dialAndAuthenticate opens a fresh upstream connection and completes its handshake using cred.
// Supports trust (AuthenticationOK with no further exchange), cleartext, MD5, and SCRAM-SHA-256 —
// the methods PostgreSQL itself ships; core's own bundled server uses SCRAM (internal/dbprov.
// scramVerifier), so that path is the one this sidecar is actually exercised against in practice.
func (p *PostgresProxy) dialAndAuthenticate(cred PostgresCredential) (net.Conn, error) {
	conn, err := p.Dial("tcp", net.JoinHostPort(cred.Host, strconv.Itoa(cred.Port)))
	if err != nil {
		return nil, fmt.Errorf("dialing upstream: %w", err)
	}

	if err := pgwire.WriteStartupMessage(conn, pgwire.StartupParams{
		"user": cred.User, "database": cred.Database, "client_encoding": "UTF8",
	}); err != nil {
		conn.Close()
		return nil, err
	}

	r := bufio.NewReader(conn)
	for {
		msg, err := pgwire.ReadMessage(r)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("reading upstream response: %w", err)
		}
		switch msg.Type {
		case 'E':
			conn.Close()
			return nil, fmt.Errorf("upstream refused: %s", errorResponseMessage(msg.Payload))
		case 'R':
			subCode, payload, err := pgwire.ParseAuthenticationMessage(msg)
			if err != nil {
				conn.Close()
				return nil, err
			}
			switch subCode {
			case 0: // AuthenticationOK
				if err := drainToReadyForQuery(r); err != nil {
					conn.Close()
					return nil, err
				}
				return conn, nil
			case 3: // AuthenticationCleartextPassword
				if err := pgwire.WritePasswordMessage(conn, cred.Password); err != nil {
					conn.Close()
					return nil, err
				}
			case 5: // AuthenticationMD5Password
				var salt [4]byte
				copy(salt[:], payload)
				if err := pgwire.WritePasswordMessage(conn, pgwire.MD5Password(cred.User, cred.Password, salt)); err != nil {
					conn.Close()
					return nil, err
				}
			case 10: // AuthenticationSASL: payload is a NUL-separated, NUL-terminated mechanism list
				if !bytes.Contains(payload, []byte(pgwire.SASLMechanism)) {
					conn.Close()
					return nil, fmt.Errorf("upstream requires an unsupported SASL mechanism: %q", payload)
				}
				if err := pgwire.PerformSCRAM(r, conn, cred.User, cred.Password); err != nil {
					conn.Close()
					return nil, fmt.Errorf("SCRAM authentication: %w", err)
				}
				if err := drainToReadyForQuery(r); err != nil {
					conn.Close()
					return nil, err
				}
				return conn, nil
			default:
				conn.Close()
				return nil, fmt.Errorf("unsupported authentication method (sub-code %d)", subCode)
			}
		}
	}
}

// drainToReadyForQuery absorbs the ParameterStatus/BackendKeyData messages a server sends right
// after authentication succeeds, stopping at ReadyForQuery — none of it needs forwarding to the
// client, which already got its own fabricated handshake completion before this function's caller
// even dials upstream in the common case, and whose own ReadyForQuery was already sent.
func drainToReadyForQuery(r *bufio.Reader) error {
	for {
		msg, err := pgwire.ReadMessage(r)
		if err != nil {
			return fmt.Errorf("draining post-auth handshake: %w", err)
		}
		if msg.Type == 'Z' {
			return nil
		}
		if msg.Type == 'E' {
			return fmt.Errorf("upstream error during handshake: %s", errorResponseMessage(msg.Payload))
		}
	}
}

func errorResponseMessage(payload []byte) string {
	for _, field := range bytes.Split(payload, []byte{0}) {
		if len(field) > 1 && field[0] == 'M' {
			return string(field[1:])
		}
	}
	return "(no message)"
}

// relay splices client and upstream together once both handshakes are complete — from here on
// every byte is opaque query/result traffic that never needs a credential re-injected into it, so
// no further protocol parsing happens (see the package doc comment on why).
func relay(client, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

func (p *PostgresProxy) serveHealthz(client net.Conn, br *bufio.Reader) {
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	defer req.Body.Close()
	status := http.StatusServiceUnavailable
	if p.renewer.Ready() {
		status = http.StatusOK
	}
	resp := http.Response{
		StatusCode: status, ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Length": {"0"}, "Connection": {"close"}},
		Body:   http.NoBody,
	}
	_ = resp.Write(client)
}
