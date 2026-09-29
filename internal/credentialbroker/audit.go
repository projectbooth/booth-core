package credentialbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Entry is one credential issuance, exactly the fields ADR 0080 requires an audit record to
// carry — "lease id, requester identity, kind, scope, and expiry" — and never the credential
// value itself, which never even reaches this type.
type Entry struct {
	LeaseID          string
	RequesterSubject string
	Workspace        string
	// Role is the caller's role at issuance time (workspace-role grammar, ADR 0025) — part of
	// "requester identity" for audit purposes, the same way it's part of what authorized the
	// request in the first place.
	Role             string
	Kind             string
	Access           string // "read" or "readwrite" — see Request.Access
	Scope            json.RawMessage
	ProviderModuleID string
	IssuedAt         time.Time
	ExpiresAt        time.Time
}

// AuditStore records credential issuances, append-only — this is ADR 0080's audit trail, kept
// deliberately distinct from ordinary request/access logging (core-platform-api.md; ADR 0022's
// stdout-only convention is for ordinary logs, not this). There is no Update/Delete: an issuance,
// once recorded, is a fact about the past.
type AuditStore interface {
	Record(ctx context.Context, e Entry) error
}

// MemoryStore keeps the audit trail in process memory. It's what core uses before (or absent) a
// configured Postgres — real durability requires the Postgres-backed store; this exists so the
// broker still functions, and still has *some* record queryable within the process's own life,
// rather than blocking on database provisioning. Bounded so a long-lived dev process without
// Postgres doesn't grow this without limit; the bound is not a real retention policy.
type MemoryStore struct {
	mu      sync.Mutex
	entries []Entry
}

// maxMemoryEntries bounds MemoryStore; oldest entries are dropped once exceeded. Dev/fallback
// use only — real retention is Postgres's job (an operator's own backup/retention policy, ADR
// 0054), not this package's.
const maxMemoryEntries = 10000

func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (m *MemoryStore) Record(_ context.Context, e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	if len(m.entries) > maxMemoryEntries {
		m.entries = m.entries[len(m.entries)-maxMemoryEntries:]
	}
	return nil
}

// Entries returns a copy of everything recorded so far. Test-only in practice today (there's no
// production read path yet — see docs/decisions/0014's residual limits — an operator reads the
// Postgres-backed table directly).
func (m *MemoryStore) Entries() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, len(m.entries))
	copy(out, m.entries)
	return out
}

const schema = `
CREATE TABLE IF NOT EXISTS booth_credential_issuances (
    id                  BIGSERIAL PRIMARY KEY,
    lease_id            TEXT        NOT NULL,
    requester_subject   TEXT        NOT NULL,
    workspace           TEXT        NOT NULL,
    role                TEXT        NOT NULL,
    kind                TEXT        NOT NULL,
    access              TEXT        NOT NULL,
    scope               JSONB       NOT NULL,
    provider_module_id  TEXT        NOT NULL,
    issued_at           TIMESTAMPTZ NOT NULL,
    expires_at          TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS booth_credential_issuances_issued_at_idx ON booth_credential_issuances (issued_at);
`

// PostgresStore persists the audit trail in core's own database (ADR 0053's shared cluster),
// genuinely append-only: no UPDATE/DELETE statement exists anywhere in this type. Mirrors
// directory.PostgresStore's lazy-schema, never-blocks-construction shape.
type PostgresStore struct {
	pool *pgxpool.Pool

	mu       sync.Mutex
	migrated bool
}

// NewPostgresStore builds a store for dsn. It only parses the DSN; the connection and schema are
// established lazily on first use, so a database that isn't up yet doesn't fail construction.
func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("configuring postgres pool: %w", err)
	}
	return &PostgresStore{pool: pool}, nil
}

func (s *PostgresStore) Close() { s.pool.Close() }

func (s *PostgresStore) ready(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.migrated {
		return nil
	}
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("ensuring booth_credential_issuances schema: %w", err)
	}
	s.migrated = true
	return nil
}

func (s *PostgresStore) Record(ctx context.Context, e Entry) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	scope := e.Scope
	if scope == nil {
		scope = json.RawMessage("null")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO booth_credential_issuances
			(lease_id, requester_subject, workspace, role, kind, access, scope, provider_module_id, issued_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.LeaseID, e.RequesterSubject, e.Workspace, e.Role, e.Kind, e.Access, scope, e.ProviderModuleID, e.IssuedAt, e.ExpiresAt)
	if err != nil {
		return fmt.Errorf("recording credential issuance: %w", err)
	}
	return nil
}

// Switchable is an AuditStore whose backing store can be replaced while the process runs — core
// starts on MemoryStore and switches to PostgresStore once its own database is available (ADR
// 0053), the same pattern directory.Switchable already uses, and with the same honest limit:
// swapping discards whatever the previous store held. An issuance recorded before the switch is
// not retroactively persisted; see docs/decisions/0014's residual limits.
type Switchable struct {
	cur atomic.Pointer[storeBox]
}

type storeBox struct{ AuditStore }

func NewSwitchable(initial AuditStore) *Switchable {
	s := &Switchable{}
	s.cur.Store(&storeBox{initial})
	return s
}

func (s *Switchable) Swap(next AuditStore) { s.cur.Store(&storeBox{next}) }

func (s *Switchable) Record(ctx context.Context, e Entry) error {
	return s.cur.Load().Record(ctx, e)
}
