// Package dbprov provisions PostgreSQL databases for the platform (ADR 0053): one database
// and one login role per module that signals it needs one, plus one for booth-core itself,
// on a single shared server. It stands up the database and hands over a DSN — what a module
// puts in its database (schema, migrations) is entirely the module's own.
//
// Isolation model: every database is owned by its own role, which is a plain login role
// (no superuser, createdb, createrole, or replication), and CONNECT is revoked from PUBLIC
// on every database, so one module's credentials cannot open another module's database.
// (The maintenance databases are covered too when Config.RestrictMaintenanceAccess is set.)
//
// Nothing is ever dropped automatically. Uninstalling a module removes its credential Secret
// but leaves its database and role, so reinstalling gets its data back; deleting data is a
// deliberate operator action.
package dbprov

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/pbkdf2"
)

const (
	// ModulePrefix namespaces module databases/roles. Core's own database is
	// CoreDatabaseName; because module names always carry this prefix, a module can never
	// collide with it (even a module whose id is "core").
	ModulePrefix     = "booth_mod_"
	CoreDatabaseName = "booth_core"

	maxIdentifier = 63 // PostgreSQL's identifier length limit

	// provisionLockKey serialises provisioning across booth-core replicas.
	provisionLockKey int64 = 0x626f6f7468 // "booth"
)

var moduleIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// DatabaseName maps a module id (contracts/module-manifest.md's [a-z][a-z0-9-]*) to its
// database and role name. Ids can't contain '_', so the '-' -> '_' mapping is injective:
// two distinct modules never share a database.
func DatabaseName(moduleID string) (string, error) {
	if !moduleIDRe.MatchString(moduleID) {
		return "", fmt.Errorf("module id %q is not a valid identifier", moduleID)
	}
	name := ModulePrefix + strings.ReplaceAll(moduleID, "-", "_")
	if len(name) > maxIdentifier {
		return "", fmt.Errorf("module id %q is too long for a database name (%d > %d)", moduleID, len(name), maxIdentifier)
	}
	return name, nil
}

// Config locates the PostgreSQL server and the privileged account used to provision on it.
type Config struct {
	Host string
	Port int

	// ModuleHost is the address written into module DSNs. Modules live in other namespaces,
	// so it must resolve from anywhere in the cluster (Host, used by core itself, may be a
	// short in-namespace name). Defaults to Host.
	ModuleHost string

	// AdminUser needs to be able to create roles and databases and to hand ownership of a
	// database to the role it created: in practice a superuser (or your provider's
	// equivalent, e.g. rds_superuser). The bundled server uses "postgres".
	AdminUser     string
	AdminPassword string
	AdminDatabase string

	// SSLMode is the libpq sslmode for every connection (disable for the bundled
	// in-cluster server; use require or stronger for an external one).
	SSLMode string

	// RestrictMaintenanceAccess also revokes CONNECT from PUBLIC on the maintenance
	// databases (AdminDatabase and template1). Without it, PostgreSQL's defaults let any
	// module's role log in to them and read the catalogs — it can't read other modules'
	// data, but it can list their database and role names. On for the bundled server, which
	// core owns; off by default for an external cluster, where revoking PUBLIC access to a
	// maintenance database could disrupt other tenants core doesn't know about (turn it on
	// there once you've checked).
	RestrictMaintenanceAccess bool
}

func (c Config) port() int {
	if c.Port == 0 {
		return 5432
	}
	return c.Port
}

func (c Config) sslmode() string {
	if c.SSLMode == "" {
		return "prefer"
	}
	return c.SSLMode
}

func (c Config) dsn(user, password, host, database string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(host, strconv.Itoa(c.port())),
		Path:     "/" + database,
		RawQuery: "sslmode=" + c.sslmode(),
	}
	return u.String()
}

// Admin performs provisioning operations against the server.
type Admin struct {
	cfg  Config
	pool *pgxpool.Pool
}

// NewAdmin prepares an Admin. It does not contact the server (connections are made lazily),
// so a database that isn't up yet at boot doesn't fail construction.
func NewAdmin(ctx context.Context, cfg Config) (*Admin, error) {
	if cfg.Host == "" {
		return nil, errors.New("postgres host is required")
	}
	if cfg.AdminUser == "" {
		cfg.AdminUser = "postgres"
	}
	if cfg.AdminDatabase == "" {
		cfg.AdminDatabase = "postgres"
	}
	pcfg, err := pgxpool.ParseConfig(cfg.dsn(cfg.AdminUser, cfg.AdminPassword, cfg.Host, cfg.AdminDatabase))
	if err != nil {
		return nil, fmt.Errorf("parsing postgres admin connection: %w", err)
	}
	// Provisioning is rare and serialised; a small pool is plenty.
	pcfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("configuring postgres admin pool: %w", err)
	}
	return &Admin{cfg: cfg, pool: pool}, nil
}

// Close releases the admin connection pool.
func (a *Admin) Close() { a.pool.Close() }

// DSN builds the connection string a module (or core) uses for its own database, as seen
// from another namespace.
func (a *Admin) DSN(name, password string) string {
	host := a.cfg.ModuleHost
	if host == "" {
		host = a.cfg.Host
	}
	return a.cfg.dsn(name, password, host, name)
}

// Endpoint returns the host and port written into a module's credential Secret.
func (a *Admin) Endpoint() (host string, port int) {
	host = a.cfg.ModuleHost
	if host == "" {
		host = a.cfg.Host
	}
	return host, a.cfg.port()
}

// Exists reports whether both the role and the database named name exist.
func (a *Admin) Exists(ctx context.Context, name string) (bool, error) {
	var ok bool
	err := a.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)
		   AND EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("checking database %q: %w", name, err)
	}
	return ok, nil
}

// Ensure makes a login role and an owned database named name exist, with the role's
// password set to password. It's idempotent, safe to call concurrently from several
// booth-core replicas, and always (re)sets the password — callers that have an existing
// credential pass that credential.
func (a *Admin) Ensure(ctx context.Context, name, password string) (err error) {
	if len(name) == 0 || len(name) > maxIdentifier {
		return fmt.Errorf("invalid database name %q", name)
	}

	conn, err := a.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("connecting to postgres: %w", err)
	}
	defer conn.Release()

	// Session-level advisory lock, so replicas provisioning at once take turns.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", provisionLockKey); err != nil {
		return fmt.Errorf("taking provisioning lock: %w", err)
	}
	defer func() {
		// If we can't release the lock, drop the whole session (which releases it) rather
		// than hand a lock-holding connection back to the pool.
		if _, uerr := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", provisionLockKey); uerr != nil {
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
		}
	}()

	// Role. The statement is composed server-side with format(%I, %L), so identifiers and
	// the password verifier are quoted by Postgres itself rather than by string
	// concatenation here.
	var roleExists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", name).Scan(&roleExists); err != nil {
		return fmt.Errorf("checking role %q: %w", name, err)
	}
	verb := "CREATE ROLE"
	if roleExists {
		verb = "ALTER ROLE"
	}
	// A pre-hashed SCRAM verifier, not the password itself: PostgreSQL logs the text of a
	// statement that errors, and a plaintext password must not end up in the server log.
	verifier, err := scramVerifier(password)
	if err != nil {
		return err
	}
	if err := execFormat(ctx, conn,
		verb+" %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD %L", name, verifier); err != nil {
		return fmt.Errorf("%s %q: %w", strings.ToLower(verb), name, err)
	}

	// Database, owned by that role. CREATE DATABASE can't run in a transaction block, so it
	// executes on its own; a concurrent creator (another process not using our lock) losing
	// the race is success.
	var dbExists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&dbExists); err != nil {
		return fmt.Errorf("checking database %q: %w", name, err)
	}
	if !dbExists {
		if err := execFormat(ctx, conn, "CREATE DATABASE %I OWNER %I", name, name); err != nil {
			var pgErr *pgconn.PgError
			if !(errors.As(err, &pgErr) && pgErr.Code == "42P04") { // duplicate_database
				return fmt.Errorf("creating database %q: %w", name, err)
			}
		}
	}

	// New databases are connectable by everyone by default; that would let any module's
	// credentials open any other module's database.
	if err := execFormat(ctx, conn, "REVOKE ALL ON DATABASE %I FROM PUBLIC", name); err != nil {
		return fmt.Errorf("revoking public access to %q: %w", name, err)
	}

	if a.cfg.RestrictMaintenanceAccess {
		for _, maint := range []string{a.cfg.AdminDatabase, "template1"} {
			if err := execFormat(ctx, conn, "REVOKE CONNECT ON DATABASE %I FROM PUBLIC", maint); err != nil {
				return fmt.Errorf("restricting access to maintenance database %q: %w", maint, err)
			}
		}
	}
	return nil
}

// execFormat runs the statement produced by PostgreSQL's own format(): %I quotes an
// identifier, %L a literal.
func execFormat(ctx context.Context, conn *pgxpool.Conn, template string, args ...any) error {
	params := make([]any, 0, len(args)+1)
	params = append(params, template)
	placeholders := make([]string, len(args))
	for i, a := range args {
		params = append(params, a)
		placeholders[i] = fmt.Sprintf("$%d::text", i+2)
	}
	var stmt string
	q := "SELECT format($1"
	if len(args) > 0 {
		q += ", " + strings.Join(placeholders, ", ")
	}
	q += ")"
	if err := conn.QueryRow(ctx, q, params...).Scan(&stmt); err != nil {
		return fmt.Errorf("composing statement: %w", err)
	}
	_, err := conn.Exec(ctx, stmt)
	return err
}

// GeneratePassword returns a random URL-safe password (hex, so it's safe to embed in a DSN
// without escaping).
func GeneratePassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// scramVerifier derives the PostgreSQL SCRAM-SHA-256 stored verifier for password
// (RFC 5802 / RFC 7677, the format `CREATE ROLE ... PASSWORD` accepts pre-hashed).
func scramVerifier(password string) (string, error) {
	const iterations = 4096
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}
	salted := pbkdf2.Key([]byte(password), salt, iterations, sha256.Size, sha256.New)

	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	clientKey := mac(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := mac(salted, "Server Key")

	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}
