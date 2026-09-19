// Package testpg starts a real PostgreSQL server for tests — no Docker needed. It's imported
// only from _test files; keeping it a normal package lets several test packages share it.
//
// If BOOTH_TEST_POSTGRES_DSN is set it's used as-is (a server whose superuser you don't mind
// tests creating databases and roles on); otherwise an embedded PostgreSQL build is
// downloaded once and started on a free port. Set BOOTH_SKIP_POSTGRES_TESTS=1 to skip.
package testpg

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// Server describes a running test PostgreSQL.
type Server struct {
	Host, Port string
	User, Pass string
	Database   string
}

// DSN returns a connection string for the given database as the server's superuser.
func (s Server) DSN(database string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(s.User, s.Pass),
		Host:     net.JoinHostPort(s.Host, s.Port),
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// Start returns a running server, skipping the test if BOOTH_SKIP_POSTGRES_TESTS is set.
// An embedded server is stopped when the test ends.
func Start(t *testing.T) Server {
	t.Helper()
	if os.Getenv("BOOTH_SKIP_POSTGRES_TESTS") != "" {
		t.Skip("BOOTH_SKIP_POSTGRES_TESTS set")
	}

	if raw := os.Getenv("BOOTH_TEST_POSTGRES_DSN"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("BOOTH_TEST_POSTGRES_DSN: %v", err)
		}
		pass, _ := u.User.Password()
		return Server{Host: u.Hostname(), Port: u.Port(), User: u.User.Username(), Pass: pass, Database: "postgres"}
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()

	dir := t.TempDir()
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).
		Port(port).
		DataPath(filepath.Join(dir, "data")).
		RuntimePath(filepath.Join(dir, "runtime")).
		Logger(nil).
		StartTimeout(90 * time.Second))
	if err := pg.Start(); err != nil {
		t.Fatalf("starting embedded postgres (set BOOTH_SKIP_POSTGRES_TESTS=1 to skip): %v", err)
	}
	t.Cleanup(func() { _ = pg.Stop() })

	return Server{Host: "localhost", Port: fmt.Sprint(port), User: "postgres", Pass: "postgres", Database: "postgres"}
}
