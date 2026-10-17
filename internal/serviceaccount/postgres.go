package serviceaccount

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-core/internal/auth"
)

const schema = `
CREATE TABLE IF NOT EXISTS booth_service_accounts (
    workspace   TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    role        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    created_by  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    disabled    BOOLEAN     NOT NULL DEFAULT FALSE,
    PRIMARY KEY (workspace, name)
);
`

// PostgresStore persists service accounts in core's own database on the shared PostgreSQL
// cluster (ADR 0014), the same server internal/directory.PostgresStore and
// internal/credentialbroker's audit store use. See the package doc comment for why this is the
// only Store implementation this package offers — there is deliberately no in-memory fallback.
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

// Ping verifies the server is actually reachable. Callers wrap this in internal/dbwait.Retry
// at startup rather than treating one refused connection as fatal (ARCHITECTURE.md item 66).
func (s *PostgresStore) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *PostgresStore) ready(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.migrated {
		return nil
	}
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("ensuring booth_service_accounts schema: %w", err)
	}
	s.migrated = true
	return nil
}

func (s *PostgresStore) Create(ctx context.Context, a Account) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO booth_service_accounts (workspace, name, role, description, created_by, created_at, disabled)
		VALUES ($1, $2, $3, $4, $5, $6, FALSE)`,
		a.Workspace, a.Name, string(a.Role), a.Description, a.CreatedBy, a.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("creating service account: %w", err)
	}
	return nil
}

const accountColumns = `workspace, name, role, description, created_by, created_at, disabled`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	var role string
	err := row.Scan(&a.Workspace, &a.Name, &role, &a.Description, &a.CreatedBy, &a.CreatedAt, &a.Disabled)
	a.Role = auth.Role(role)
	return a, err
}

func (s *PostgresStore) Get(ctx context.Context, workspace, name string) (Account, bool, error) {
	if err := s.ready(ctx); err != nil {
		return Account{}, false, err
	}
	a, err := scanAccount(s.pool.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM booth_service_accounts WHERE workspace = $1 AND name = $2`,
		workspace, name))
	if err == pgx.ErrNoRows {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, fmt.Errorf("looking up service account: %w", err)
	}
	return a, true, nil
}

func (s *PostgresStore) List(ctx context.Context, workspace string) ([]Account, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+accountColumns+` FROM booth_service_accounts WHERE workspace = $1 ORDER BY name`,
		workspace)
	if err != nil {
		return nil, fmt.Errorf("listing service accounts: %w", err)
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("reading service account row: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *PostgresStore) Update(ctx context.Context, workspace, name string, patch Patch) (Account, error) {
	if err := s.ready(ctx); err != nil {
		return Account{}, err
	}
	a, err := scanAccount(s.pool.QueryRow(ctx, `
		UPDATE booth_service_accounts SET
			role        = COALESCE($3, role),
			description = COALESCE($4, description),
			disabled    = COALESCE($5, disabled)
		WHERE workspace = $1 AND name = $2
		RETURNING `+accountColumns,
		workspace, name, patchRole(patch), patch.Description, patch.Disabled))
	if err == pgx.ErrNoRows {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("updating service account: %w", err)
	}
	return a, nil
}

// patchRole converts patch.Role to the *string Update's SQL needs (nil stays nil, so COALESCE
// leaves the column alone when the caller didn't ask to change it).
func patchRole(patch Patch) *string {
	if patch.Role == nil {
		return nil
	}
	s := string(*patch.Role)
	return &s
}

func (s *PostgresStore) Delete(ctx context.Context, workspace, name string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting delete: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	var disabled bool
	err = tx.QueryRow(ctx,
		`SELECT disabled FROM booth_service_accounts WHERE workspace = $1 AND name = $2 FOR UPDATE`,
		workspace, name).Scan(&disabled)
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("locking service account for delete: %w", err)
	}
	if !disabled {
		return ErrMustDisableBeforeDelete
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM booth_service_accounts WHERE workspace = $1 AND name = $2`, workspace, name); err != nil {
		return fmt.Errorf("deleting service account: %w", err)
	}
	return tx.Commit(ctx)
}

// uniqueViolationCode is Postgres's SQLSTATE for a unique/primary-key constraint violation —
// Create's expected failure mode when (workspace, name) is already taken.
const uniqueViolationCode = "23505"

// sqlState is the interface pgconn.PgError satisfies; matched this way so this package doesn't
// need to import pgconn just for one error code.
type sqlState interface{ SQLState() string }

func isUniqueViolation(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if s, ok := e.(sqlState); ok {
			return s.SQLState() == uniqueViolationCode
		}
	}
	return false
}
