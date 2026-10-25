package project

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS booth_projects (
    workspace   TEXT        NOT NULL,
    slug        TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    parent_slug TEXT        NOT NULL DEFAULT '',
    description TEXT        NOT NULL DEFAULT '',
    color       TEXT        NOT NULL DEFAULT '',
    icon        TEXT        NOT NULL DEFAULT '',
    archived    BOOLEAN     NOT NULL DEFAULT FALSE,
    created_by  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workspace, slug)
);

CREATE TABLE IF NOT EXISTS booth_project_bindings (
    id           TEXT        NOT NULL,
    workspace    TEXT        NOT NULL,
    project_slug TEXT        NOT NULL,
    kind         TEXT        NOT NULL,
    backend_id   TEXT        NOT NULL DEFAULT '',
    value        TEXT        NOT NULL,
    created_by   TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (workspace, id),
    UNIQUE (workspace, kind, backend_id, value)
);
`

// PostgresStore persists projects and bindings in core's own database on the shared PostgreSQL
// cluster (ADR 0014), the same server internal/serviceaccount.PostgresStore uses. See the
// package doc comment for why this is the only Store implementation this package offers.
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

// Ping verifies the server is actually reachable. Callers wrap this in internal/dbwait.Retry at
// startup rather than treating one refused connection as fatal (ARCHITECTURE.md item 66).
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
		return fmt.Errorf("ensuring booth_projects/booth_project_bindings schema: %w", err)
	}
	s.migrated = true
	return nil
}

func (s *PostgresStore) Create(ctx context.Context, p Project) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if p.Parent != "" {
		if err := s.checkParent(ctx, p.Workspace, p.Parent); err != nil {
			return err
		}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO booth_projects (workspace, slug, name, parent_slug, description, color, icon, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		p.Workspace, p.Slug, p.Name, p.Parent, p.Description, p.Color, p.Icon, p.CreatedBy, p.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("creating project: %w", err)
	}
	return nil
}

// checkParent enforces ADR 0124 point 5's two-level limit: parent must exist, be in the same
// workspace, be non-archived, and itself be top-level.
func (s *PostgresStore) checkParent(ctx context.Context, workspace, parent string) error {
	var parentOfParent string
	var archived bool
	err := s.pool.QueryRow(ctx,
		`SELECT parent_slug, archived FROM booth_projects WHERE workspace = $1 AND slug = $2`,
		workspace, parent).Scan(&parentOfParent, &archived)
	if err == pgx.ErrNoRows {
		return ErrParentNotFound
	}
	if err != nil {
		return fmt.Errorf("checking parent project: %w", err)
	}
	if archived {
		return ErrParentNotFound
	}
	if parentOfParent != "" {
		return ErrParentHasAParent
	}
	return nil
}

// hasChildren reports whether any project in workspace names slug as its parent.
func (s *PostgresStore) hasChildren(ctx context.Context, workspace, slug string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM booth_projects WHERE workspace = $1 AND parent_slug = $2)`,
		workspace, slug).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking for children: %w", err)
	}
	return exists, nil
}

const projectColumns = `workspace, slug, name, parent_slug, description, color, icon, archived, created_by, created_at`

func scanProject(row pgx.Row) (Project, error) {
	var p Project
	err := row.Scan(&p.Workspace, &p.Slug, &p.Name, &p.Parent, &p.Description, &p.Color, &p.Icon,
		&p.Archived, &p.CreatedBy, &p.CreatedAt)
	return p, err
}

func (s *PostgresStore) Get(ctx context.Context, workspace, slug string) (Project, bool, error) {
	if err := s.ready(ctx); err != nil {
		return Project{}, false, err
	}
	p, err := scanProject(s.pool.QueryRow(ctx,
		`SELECT `+projectColumns+` FROM booth_projects WHERE workspace = $1 AND slug = $2`,
		workspace, slug))
	if err == pgx.ErrNoRows {
		return Project{}, false, nil
	}
	if err != nil {
		return Project{}, false, fmt.Errorf("looking up project: %w", err)
	}
	return p, true, nil
}

func (s *PostgresStore) List(ctx context.Context, workspace string) ([]Project, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+projectColumns+` FROM booth_projects WHERE workspace = $1 ORDER BY slug`,
		workspace)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("reading project row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (s *PostgresStore) Update(ctx context.Context, workspace, slug string, patch Patch) (Project, error) {
	if err := s.ready(ctx); err != nil {
		return Project{}, err
	}

	if patch.Parent != nil && *patch.Parent != "" {
		if err := s.checkParent(ctx, workspace, *patch.Parent); err != nil {
			return Project{}, err
		}
		if hasKids, err := s.hasChildren(ctx, workspace, slug); err != nil {
			return Project{}, err
		} else if hasKids {
			return Project{}, ErrHasChildren
		}
	}

	p, err := scanProject(s.pool.QueryRow(ctx, `
		UPDATE booth_projects SET
			name        = COALESCE($3, name),
			parent_slug = COALESCE($4, parent_slug),
			description = COALESCE($5, description),
			color       = COALESCE($6, color),
			icon        = COALESCE($7, icon),
			archived    = COALESCE($8, archived)
		WHERE workspace = $1 AND slug = $2
		RETURNING `+projectColumns,
		workspace, slug, patch.Name, patch.Parent, patch.Description, patch.Color, patch.Icon, patch.Archived))
	if err == pgx.ErrNoRows {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("updating project: %w", err)
	}
	return p, nil
}

func (s *PostgresStore) CreateBinding(ctx context.Context, b Binding) (Binding, error) {
	if err := s.ready(ctx); err != nil {
		return Binding{}, err
	}
	if _, found, err := s.Get(ctx, b.Workspace, b.ProjectSlug); err != nil {
		return Binding{}, err
	} else if !found {
		return Binding{}, ErrNotFound
	}

	id := b.ID
	if id == "" {
		id = newBindingID()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO booth_project_bindings (id, workspace, project_slug, kind, backend_id, value, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, b.Workspace, b.ProjectSlug, string(b.Kind), b.BackendID, b.Value, b.CreatedBy, b.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Binding{}, ErrBindingConflict
		}
		return Binding{}, fmt.Errorf("creating binding: %w", err)
	}
	b.ID = id
	return b, nil
}

const bindingColumns = `id, workspace, project_slug, kind, backend_id, value, created_by, created_at`

func scanBinding(row pgx.Row) (Binding, error) {
	var b Binding
	var kind string
	err := row.Scan(&b.ID, &b.Workspace, &b.ProjectSlug, &kind, &b.BackendID, &b.Value, &b.CreatedBy, &b.CreatedAt)
	b.Kind = BindingKind(kind)
	return b, err
}

func (s *PostgresStore) ListBindings(ctx context.Context, workspace string, filter BindingFilter) ([]Binding, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	query := `SELECT ` + bindingColumns + ` FROM booth_project_bindings WHERE workspace = $1`
	args := []any{workspace}
	if filter.ProjectSlug != "" {
		args = append(args, filter.ProjectSlug)
		query += fmt.Sprintf(" AND project_slug = $%d", len(args))
	}
	if filter.Kind != "" {
		args = append(args, string(filter.Kind))
		query += fmt.Sprintf(" AND kind = $%d", len(args))
	}
	query += " ORDER BY created_at"

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing bindings: %w", err)
	}
	defer rows.Close()

	var out []Binding
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("reading binding row: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *PostgresStore) DeleteBinding(ctx context.Context, workspace, id string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM booth_project_bindings WHERE workspace = $1 AND id = $2`, workspace, id)
	if err != nil {
		return fmt.Errorf("deleting binding: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrBindingNotFound
	}
	return nil
}

// newBindingID is a unique binding id, same shape as internal/workload.randomID /
// internal/iframeidentity.randomID.
func newBindingID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err)) // the process can't safely continue
	}
	return hex.EncodeToString(b)
}

// uniqueViolationCode is Postgres's SQLSTATE for a unique/primary-key constraint violation.
const uniqueViolationCode = "23505"

// sqlState is the interface pgconn.PgError satisfies; matched this way so this package doesn't
// need to import pgconn just for one error code (same trick as internal/serviceaccount).
type sqlState interface{ SQLState() string }

func isUniqueViolation(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if s, ok := e.(sqlState); ok {
			return s.SQLState() == uniqueViolationCode
		}
	}
	return false
}
