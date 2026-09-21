package directory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS booth_users (
    sub                TEXT PRIMARY KEY,
    preferred_username TEXT        NOT NULL DEFAULT '',
    name               TEXT        NOT NULL DEFAULT '',
    email              TEXT        NOT NULL DEFAULT '',
    workspaces         TEXT[]      NOT NULL DEFAULT '{}',
    first_seen_at      TIMESTAMPTZ NOT NULL,
    last_seen_at       TIMESTAMPTZ NOT NULL
);
ALTER TABLE booth_users ADD COLUMN IF NOT EXISTS roles JSONB NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS booth_users_workspaces_idx ON booth_users USING GIN (workspaces);
`

// PostgresStore persists the directory in core's own database on the shared PostgreSQL
// cluster (ADR 0014).
//
// Construction never contacts the database (pgxpool connects lazily) and the schema is
// created on first successful use, retried on failure — so a database that isn't up yet at
// boot doesn't crash core, it just makes directory calls fail until it is. The schema is a
// single idempotent CREATE ... IF NOT EXISTS: fine for one small table, and the point at
// which a real migration tool is worth introducing is when core has a second one.
type PostgresStore struct {
	pool *pgxpool.Pool

	mu       sync.Mutex
	migrated bool
}

// NewPostgresStore builds a store for dsn. It only parses the DSN; see PostgresStore.
func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("configuring postgres pool: %w", err)
	}
	return &PostgresStore{pool: pool}, nil
}

// Close releases the connection pool.
func (s *PostgresStore) Close() { s.pool.Close() }

func (s *PostgresStore) ready(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.migrated {
		return nil
	}
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("ensuring booth_users schema: %w", err)
	}
	s.migrated = true
	return nil
}

func (s *PostgresStore) Upsert(ctx context.Context, u User) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	workspaces := u.Workspaces
	if workspaces == nil {
		workspaces = []string{}
	}
	roles := u.Roles
	if roles == nil {
		roles = map[string]string{}
	}
	now := time.Now().UTC()
	// An empty incoming display claim never erases a stored one (see Store.Upsert).
	_, err := s.pool.Exec(ctx, `
		INSERT INTO booth_users (sub, preferred_username, name, email, workspaces, roles, first_seen_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $7, $6, $6)
		ON CONFLICT (sub) DO UPDATE SET
			preferred_username = COALESCE(NULLIF(EXCLUDED.preferred_username, ''), booth_users.preferred_username),
			name               = COALESCE(NULLIF(EXCLUDED.name, ''),               booth_users.name),
			email              = COALESCE(NULLIF(EXCLUDED.email, ''),              booth_users.email),
			workspaces         = EXCLUDED.workspaces,
			roles              = EXCLUDED.roles,
			last_seen_at       = EXCLUDED.last_seen_at`,
		u.Sub, u.PreferredUsername, u.Name, u.Email, workspaces, now, roles)
	if err != nil {
		return fmt.Errorf("upserting user: %w", err)
	}
	return nil
}

const userColumns = `sub, preferred_username, name, email, workspaces, roles, first_seen_at, last_seen_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.Sub, &u.PreferredUsername, &u.Name, &u.Email, &u.Workspaces, &u.Roles, &u.FirstSeenAt, &u.LastSeenAt)
	return u, err
}

func (s *PostgresStore) Get(ctx context.Context, sub, workspace string) (User, bool, error) {
	if err := s.ready(ctx); err != nil {
		return User{}, false, err
	}
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM booth_users WHERE sub = $1 AND $2 = ANY(workspaces)`, sub, workspace))
	if err == pgx.ErrNoRows {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, fmt.Errorf("looking up user: %w", err)
	}
	return u, true, nil
}

// likeEscaper makes user input match literally inside a LIKE pattern.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (s *PostgresStore) Search(ctx context.Context, workspace, q string, limit int) ([]User, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	pattern := "%" + likeEscaper.Replace(q) + "%"
	rows, err := s.pool.Query(ctx, `
		SELECT `+userColumns+` FROM booth_users
		WHERE $1 = ANY(workspaces)
		  AND ($2 = '' OR name ILIKE $3 OR preferred_username ILIKE $3 OR email ILIKE $3)
		ORDER BY lower(COALESCE(NULLIF(name, ''), NULLIF(preferred_username, ''), NULLIF(email, ''), sub)), sub
		LIMIT $4`, workspace, q, pattern, limit)
	if err != nil {
		return nil, fmt.Errorf("searching users: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("reading user row: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
