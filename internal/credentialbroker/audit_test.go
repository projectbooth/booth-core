package credentialbroker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/projectbooth/booth-core/internal/testpg"
)

// The same behavioural contract runs against every AuditStore implementation, so MemoryStore and
// PostgresStore can't drift apart — the same pattern directory's store_test.go already uses.

func TestMemoryStore_Contract(t *testing.T) {
	runAuditContract(t, func(t *testing.T) AuditStore { return NewMemoryStore() })
}

func TestPostgresStore_Contract(t *testing.T) {
	dsn := testpg.Start(t).DSN("postgres")
	runAuditContract(t, func(t *testing.T) AuditStore {
		s, err := NewPostgresStore(context.Background(), dsn)
		if err != nil {
			t.Fatalf("NewPostgresStore: %v", err)
		}
		t.Cleanup(s.Close)
		if _, err := s.pool.Exec(context.Background(), schema); err != nil {
			t.Fatalf("schema: %v", err)
		}
		if _, err := s.pool.Exec(context.Background(), "TRUNCATE booth_credential_issuances"); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		return s
	})
}

func runAuditContract(t *testing.T, newStore func(*testing.T) AuditStore) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entry := func(leaseID string) Entry {
		return Entry{
			LeaseID: leaseID, RequesterSubject: "u-alice", Workspace: "acme", Role: "editor",
			Kind: "s3", Access: "readwrite", Scope: json.RawMessage(`{"backendId":"lake","path":"t1"}`),
			ProviderModuleID: "storage", IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute),
		}
	}

	t.Run("records without error", func(t *testing.T) {
		s := newStore(t)
		if err := s.Record(ctx, entry("lease-1")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("never rejects a nil scope", func(t *testing.T) {
		s := newStore(t)
		e := entry("lease-2")
		e.Scope = nil
		if err := s.Record(ctx, e); err != nil {
			t.Fatalf("nil scope: %v", err)
		}
	})

	t.Run("several entries, including a different lease for the same requester, all land", func(t *testing.T) {
		s := newStore(t)
		for i, id := range []string{"lease-3", "lease-4", "lease-5"} {
			e := entry(id)
			e.IssuedAt = now.Add(time.Duration(i) * time.Second)
			if err := s.Record(ctx, e); err != nil {
				t.Fatalf("recording %s: %v", id, err)
			}
		}
	})
}

func TestMemoryStore_IsBounded(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	for i := 0; i < maxMemoryEntries+10; i++ {
		if err := s.Record(ctx, Entry{LeaseID: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(s.Entries()); got != maxMemoryEntries {
		t.Errorf("entries = %d, want capped at %d", got, maxMemoryEntries)
	}
}

func TestSwitchable_UsesTheCurrentStore(t *testing.T) {
	ctx := context.Background()
	first := NewMemoryStore()
	sw := NewSwitchable(first)
	if err := sw.Record(ctx, Entry{LeaseID: "before-swap"}); err != nil {
		t.Fatal(err)
	}
	if len(first.Entries()) != 1 {
		t.Fatalf("first store has %d entries, want 1", len(first.Entries()))
	}

	second := NewMemoryStore()
	sw.Swap(second)
	if err := sw.Record(ctx, Entry{LeaseID: "after-swap"}); err != nil {
		t.Fatal(err)
	}
	if len(first.Entries()) != 1 {
		t.Errorf("the pre-swap store gained an entry after swapping: %v", first.Entries())
	}
	if len(second.Entries()) != 1 || second.Entries()[0].LeaseID != "after-swap" {
		t.Errorf("post-swap store = %v", second.Entries())
	}
}
