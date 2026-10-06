package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/projectbooth/booth-core/internal/credentialbroker"
	"github.com/projectbooth/booth-core/internal/testpg"
)

// freePort binds a throwaway listener to find an unused loopback port, then releases it — the
// same pattern internal/testpg itself uses to hand an embedded Postgres a real port.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// newProxyFixture starts PostgresProxy.Listen in the background on a free port and returns a
// function to feed it a credential (simulating what Renewer.OnRenew would do on a real lease),
// plus the listen address a test client connects to.
func newProxyFixture(t *testing.T) (setCredential func(credentialbroker.Response), addr string) {
	t.Helper()
	renewer := &Renewer{}
	proxy := NewPostgresProxy(renewer)
	addr = freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- proxy.Listen(ctx, "tcp", addr) }()
	t.Cleanup(func() {
		select {
		case err := <-errCh:
			if err != nil {
				t.Logf("proxy.Listen exited with: %v", err)
			}
		case <-time.After(time.Second):
		}
	})

	// Give the listener a moment to actually bind before the first connection attempt.
	for i := 0; i < 50; i++ {
		if conn, err := net.DialTimeout("tcp", addr, 20*time.Millisecond); err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return renewer.OnRenew, addr
}

func realPostgresCredential(t *testing.T, srv testpg.Server, user, password, database string) credentialbroker.Response {
	t.Helper()
	port, err := strconv.Atoi(srv.Port)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(PostgresCredential{Host: srv.Host, Port: port, Database: database, User: user, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	return credentialbroker.Response{LeaseID: "lease-" + user, ExpiresAt: time.Now().Add(time.Hour), Credential: raw}
}

// createRole creates a SCRAM-authenticated role and a database it owns, against the real embedded
// server — exercising exactly the auth method core's own bundled Postgres actually requires
// (internal/dbprov.scramVerifier), so PerformSCRAM is proven against real PostgreSQL here, not
// only against this package's own fakeScramServer in pgwire's unit tests.
func createRole(t *testing.T, srv testpg.Server, user, password, database string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, srv.DSN("postgres"))
	if err != nil {
		t.Fatalf("connecting as admin: %v", err)
	}
	defer admin.Close(ctx)

	for _, stmt := range []string{
		fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, database),
		fmt.Sprintf(`DROP ROLE IF EXISTS %s`, user),
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, user, password),
		fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, database, user),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("admin setup %q: %v", stmt, err)
		}
	}
}

// --- The real, end-to-end thing: a genuine Postgres client, through the proxy, to a genuine
// --- PostgreSQL server, authenticated with SCRAM-SHA-256 (not a mock in either direction). --------

func TestPostgresProxy_RealClientThroughProxyToRealPostgres(t *testing.T) {
	srv := testpg.Start(t)
	createRole(t, srv, "sidecar_test_user", "sidecar-test-password", "sidecar_test_db")

	setCredential, addr := newProxyFixture(t)
	setCredential(realPostgresCredential(t, srv, "sidecar_test_user", "sidecar-test-password", "sidecar_test_db"))

	ctx := context.Background()
	// The client's own user/password are irrelevant — the proxy authenticates client connections
	// by trust (the pod's own network namespace is the boundary) and uses the broker's credential
	// upstream, exactly as ADR 0095 specifies.
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://whatever:unused@%s/ignored?sslmode=disable", addr))
	if err != nil {
		t.Fatalf("connecting through the proxy: %v", err)
	}
	defer conn.Close(ctx)

	var got int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&got); err != nil {
		t.Fatalf("querying through the proxy: %v", err)
	}
	if got != 1 {
		t.Errorf("got %d, want 1", got)
	}

	var currentUser, currentDB string
	if err := conn.QueryRow(ctx, "SELECT current_user, current_database()").Scan(&currentUser, &currentDB); err != nil {
		t.Fatal(err)
	}
	if currentUser != "sidecar_test_user" || currentDB != "sidecar_test_db" {
		t.Errorf("authenticated as %s/%s upstream, want sidecar_test_user/sidecar_test_db — the broker's credential, not the client's own", currentUser, currentDB)
	}
}

// --- The hard requirement the task calls out by name: a renewal failure must not drop an
// --- already-open client connection — tested against the real server, mid-connection. -------------

func TestPostgresProxy_RenewalFailureDoesNotDropAnOpenConnection(t *testing.T) {
	srv := testpg.Start(t)
	createRole(t, srv, "sidecar_test_user2", "sidecar-test-password2", "sidecar_test_db2")

	setCredential, addr := newProxyFixture(t)
	setCredential(realPostgresCredential(t, srv, "sidecar_test_user2", "sidecar-test-password2", "sidecar_test_db2"))

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://x:y@%s/z?sslmode=disable", addr))
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("query before the failed renewal: %v", err)
	}

	// Simulate a renewal that produced garbage (the provider's response didn't parse) — the
	// proxy's own setCredential logs and keeps the previous credential; this already-open
	// connection, which doesn't even consult the stored credential again, must be completely
	// unaffected either way.
	setCredential(credentialbroker.Response{LeaseID: "bad", ExpiresAt: time.Now().Add(time.Hour), Credential: json.RawMessage(`"not an object"`)})

	var got int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&got); err != nil {
		t.Fatalf("query after the failed renewal: %v (the existing connection must survive untouched)", err)
	}
	if got != 1 {
		t.Errorf("got %d, want 1", got)
	}

	// A *new* connection attempted while the bad renewal is the most recent event must still use
	// the last genuinely good credential — not the unparseable one, and not a zeroed-out one
	// either (which would fail to connect to host="" port=0). This is the part a parse failure
	// that silently overwrote the stored credential with a zero value would break even though the
	// already-open connection above stays fine either way.
	connDuringBadState, err := pgx.Connect(ctx, fmt.Sprintf("postgres://x:y@%s/z?sslmode=disable", addr))
	if err != nil {
		t.Fatalf("connecting while the last renewal was bad: %v (must fall back to the last good credential)", err)
	}
	var userDuringBadState string
	if err := connDuringBadState.QueryRow(ctx, "SELECT current_user").Scan(&userDuringBadState); err != nil {
		t.Fatal(err)
	}
	if userDuringBadState != "sidecar_test_user2" {
		t.Errorf("connection during the bad-renewal window authenticated as %q, want sidecar_test_user2 (the last good credential)", userDuringBadState)
	}
	connDuringBadState.Close(ctx)

	// A genuinely new connection after a renewal to a *different*, valid credential uses the new
	// one — the swap does take effect for new connections, it just never touches old ones.
	createRole(t, srv, "sidecar_test_user3", "sidecar-test-password3", "sidecar_test_db3")
	setCredential(realPostgresCredential(t, srv, "sidecar_test_user3", "sidecar-test-password3", "sidecar_test_db3"))

	conn2, err := pgx.Connect(ctx, fmt.Sprintf("postgres://x:y@%s/z?sslmode=disable", addr))
	if err != nil {
		t.Fatalf("connecting after a valid renewal: %v", err)
	}
	defer conn2.Close(ctx)
	var currentUser string
	if err := conn2.QueryRow(ctx, "SELECT current_user").Scan(&currentUser); err != nil {
		t.Fatal(err)
	}
	if currentUser != "sidecar_test_user3" {
		t.Errorf("new connection authenticated as %q, want sidecar_test_user3 (the renewed credential)", currentUser)
	}

	// And the original connection is STILL fine, even now that the credential has moved on twice.
	if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
		t.Errorf("original connection broke after a later valid renewal: %v", err)
	}
}

// --- Regression: the fifth amendment's actual guarantee — a connection opened just before a
// --- renewal swap survives at least half the lease's own lifetime, driven by a fake clock and a
// --- fake (short) lease TTL, never a real hour. --------------------------------------------------

func TestPostgresProxy_HalfLifetimeRenewalGuaranteesHalfALeaseForAnExistingConnection(t *testing.T) {
	srv := testpg.Start(t)
	createRole(t, srv, "sidecar_half_user_a", "pw-a", "sidecar_half_db_a")
	createRole(t, srv, "sidecar_half_user_b", "pw-b", "sidecar_half_db_b")

	const fakeLeaseLifetime = 10 * time.Minute // a fake, short lease TTL — never a real hour
	clock := &fakeClock{now: time.Now()}
	port, err := strconv.Atoi(srv.Port)
	if err != nil {
		t.Fatal(err)
	}

	broker := newFakeBroker(t)
	broker.respond = func(call int, req credentialbroker.Request) (int, any) {
		user, pass, db := "sidecar_half_user_a", "pw-a", "sidecar_half_db_a"
		if call > 1 {
			user, pass, db = "sidecar_half_user_b", "pw-b", "sidecar_half_db_b"
		}
		raw, err := json.Marshal(PostgresCredential{Host: srv.Host, Port: port, Database: db, User: user, Password: pass})
		if err != nil {
			t.Fatal(err)
		}
		return http.StatusCreated, credentialbroker.Response{
			LeaseID: fmt.Sprintf("lease-%d", call), ExpiresAt: clock.Now().Add(fakeLeaseLifetime), Credential: raw,
		}
	}

	renewer := &Renewer{
		Client: newClient(broker), Request: testRequest(),
		HalfLifetime: true, Interval: time.Millisecond, Now: clock.Now,
	}
	proxy := NewPostgresProxy(renewer)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = proxy.Listen(ctx, "tcp", addr) }()
	for i := 0; i < 50; i++ {
		if conn, err := net.DialTimeout("tcp", addr, 20*time.Millisecond); err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	go func() { _ = renewer.Run(ctx) }()
	waitForCalls(t, broker, 1)
	waitForReady(t, renewer) // not just received by the broker — actually stored in p.current

	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://x:y@%s/z?sslmode=disable", addr))
	if err != nil {
		t.Fatalf("connecting under the first lease: %v", err)
	}
	defer conn.Close(ctx)
	var currentUser string
	if err := conn.QueryRow(ctx, "SELECT current_user").Scan(&currentUser); err != nil {
		t.Fatal(err)
	}
	if currentUser != "sidecar_half_user_a" {
		t.Fatalf("connected as %q, want sidecar_half_user_a", currentUser)
	}

	// Just under half the lease's lifetime: the half-lifetime default must not have renewed yet —
	// pins that this is HALF the lease, not the old fixed ~60s margin (which would have renewed
	// almost immediately).
	clock.Advance(fakeLeaseLifetime/2 - 5*time.Second)
	time.Sleep(30 * time.Millisecond)
	if got := broker.callCount(); got != 1 {
		t.Fatalf("calls = %d, want 1 (renewed before half the lease's lifetime elapsed)", got)
	}

	// Cross the half-lifetime threshold: the renewal swap fires now.
	clock.Advance(10 * time.Second)
	waitForCalls(t, broker, 2)
	waitForLeaseID(t, renewer, "lease-2") // the second renewal's credential has actually landed

	// The connection opened under the first lease — immediately before this swap — must still be
	// alive and must still be authenticated as the FIRST lease's role: the swap only ever affects
	// new connections, never an already-open one.
	if err := conn.QueryRow(ctx, "SELECT current_user").Scan(&currentUser); err != nil {
		t.Fatalf("connection opened before the half-lifetime swap is no longer alive: %v", err)
	}
	if currentUser != "sidecar_half_user_a" {
		t.Errorf("connection now authenticated as %q, want it to remain sidecar_half_user_a (unaffected by the swap)", currentUser)
	}

	// A genuinely new connection, made after the swap, does use the renewed (second) credential —
	// confirming the swap really happened, not just that nothing was checked.
	conn2, err := pgx.Connect(ctx, fmt.Sprintf("postgres://x:y@%s/z?sslmode=disable", addr))
	if err != nil {
		t.Fatalf("connecting after the swap: %v", err)
	}
	defer conn2.Close(ctx)
	if err := conn2.QueryRow(ctx, "SELECT current_user").Scan(&currentUser); err != nil {
		t.Fatal(err)
	}
	if currentUser != "sidecar_half_user_b" {
		t.Errorf("new connection authenticated as %q, want sidecar_half_user_b (the renewed lease)", currentUser)
	}
}

// --- Health: 503 before any lease, 200 once one has been obtained, served on the same listener. ----

func TestPostgresProxy_HealthzOnTheSameListener(t *testing.T) {
	renewer := &Renewer{}
	proxy := NewPostgresProxy(renewer)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = proxy.Listen(ctx, "tcp", addr) }()
	for i := 0; i < 50; i++ {
		if conn, err := net.DialTimeout("tcp", addr, 20*time.Millisecond); err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	get := func() int {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		line := string(buf[:n])
		if len(line) < 12 {
			t.Fatalf("short response: %q", line)
		}
		status, err := strconv.Atoi(line[9:12])
		if err != nil {
			t.Fatalf("unparseable status in %q: %v", line, err)
		}
		return status
	}

	if got := get(); got != 503 {
		t.Errorf("before any lease: status = %d, want 503", got)
	}
	renewer.ready.Store(true)
	if got := get(); got != 200 {
		t.Errorf("after a lease: status = %d, want 200", got)
	}
}

// --- Regression: decodes booth-database's real provider response, not this package's own guess. --
//
// booth-database shipped a real postgres-kind provider before ADR 0095 existed
// (booth-database@b84fc3b, internal/credentialbroker/provider.go's pgCredential) — it's that
// module's only access path (credentialBroker.enabled: true by default), not a hypothetical.
// This test's JSON is hand-copied from that struct's own field list and tags
// (host/port/database/username/password/sslMode) rather than re-derived from this package's own
// PostgresCredential, specifically so a future rename on either side shows up here instead of
// passing because both sides happened to agree with the same wrong assumption — which is exactly
// how `user` vs. `username` got through undetected the first time: every test's fixture was shaped
// to match this package's own (wrong) guess, never booth-database's actual response.
func TestPostgresCredential_DecodesBoothDatabasesRealResponseShape(t *testing.T) {
	// Field-for-field from booth-database/internal/credentialbroker/provider.go's pgCredential,
	// with the example host from that repo's own provider_test.go.
	raw := []byte(`{
		"host": "booth-database-postgres.booth-database.svc",
		"port": 5432,
		"database": "bdb_ws_0123456789abcdef01234567",
		"username": "bdb_role_0123456789abcdef01234567",
		"password": "s3cr3t-lease-password",
		"sslMode": "disable"
	}`)

	var cred PostgresCredential
	if err := decodeStrict(raw, &cred); err != nil {
		t.Fatalf("decoding booth-database's real response shape: %v", err)
	}
	if cred.Host != "booth-database-postgres.booth-database.svc" {
		t.Errorf("Host = %q", cred.Host)
	}
	if cred.Port != 5432 {
		t.Errorf("Port = %d", cred.Port)
	}
	if cred.Database != "bdb_ws_0123456789abcdef01234567" {
		t.Errorf("Database = %q", cred.Database)
	}
	if cred.User != "bdb_role_0123456789abcdef01234567" {
		t.Errorf("User = %q, want the decoded \"username\" field (this is exactly the field that silently came back empty before)", cred.User)
	}
	if cred.Password != "s3cr3t-lease-password" {
		t.Errorf("Password = %q", cred.Password)
	}
	if cred.SSLMode != "disable" {
		t.Errorf("SSLMode = %q", cred.SSLMode)
	}
}

// A response carrying a field this package's PostgresCredential doesn't recognize (e.g. a renamed
// or added field on booth-database's side) must fail loudly, not silently decode with a zeroed
// field — the whole reason setCredential now uses decodeStrict instead of json.Unmarshal.
func TestPostgresCredential_UnknownFieldErrorsRatherThanSilentlyZeroing(t *testing.T) {
	raw := []byte(`{"host":"h","port":1,"database":"d","user":"u-not-username","password":"p","sslMode":"disable"}`)
	var cred PostgresCredential
	if err := decodeStrict(raw, &cred); err == nil {
		t.Fatalf("decoded %+v from a field name (\"user\") this struct doesn't have — want a decode error, not a silently empty User", cred)
	}
}
