package directory

import (
	"context"
	"testing"
	"time"

	"github.com/projectbooth/booth-core/internal/testpg"
)

// The same behavioural suite runs against every Store implementation, so the in-memory
// fallback and the PostgreSQL store can't drift apart.

func TestMemoryStore(t *testing.T) {
	runStoreContract(t, func(t *testing.T) Store { return NewMemoryStore() })
}

// PostgreSQL is exercised against a real server (see internal/testpg for how one is
// obtained and how to skip).
func TestPostgresStore(t *testing.T) {
	dsn := testpg.Start(t).DSN("postgres")

	runStoreContract(t, func(t *testing.T) Store {
		s, err := NewPostgresStore(context.Background(), dsn)
		if err != nil {
			t.Fatalf("NewPostgresStore: %v", err)
		}
		t.Cleanup(s.Close)
		// Each subtest starts from an empty table.
		if _, err := s.pool.Exec(context.Background(), schema); err != nil {
			t.Fatalf("schema: %v", err)
		}
		if _, err := s.pool.Exec(context.Background(), "TRUNCATE booth_users"); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		return s
	})
}

func runStoreContract(t *testing.T, newStore func(*testing.T) Store) {
	ctx := context.Background()

	alice := func() User {
		return User{Sub: "sub-alice", PreferredUsername: "alice", Name: "Alice Anderson", Email: "alice@example.com",
			Workspaces: []string{"acme", "labs"}}
	}

	t.Run("upsert then get within a shared workspace", func(t *testing.T) {
		s := newStore(t)
		if err := s.Upsert(ctx, alice()); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.Get(ctx, "sub-alice", "acme")
		if err != nil || !ok {
			t.Fatalf("Get = %v, %v, %v", got, ok, err)
		}
		if got.PreferredUsername != "alice" || got.Name != "Alice Anderson" || got.Email != "alice@example.com" {
			t.Errorf("got %+v", got)
		}
		if got.FirstSeenAt.IsZero() || got.LastSeenAt.IsZero() {
			t.Error("timestamps were not recorded")
		}
	})

	t.Run("a user in another workspace is invisible", func(t *testing.T) {
		s := newStore(t)
		_ = s.Upsert(ctx, alice())
		if _, ok, err := s.Get(ctx, "sub-alice", "someone-elses-workspace"); err != nil || ok {
			t.Fatalf("Get across workspaces = ok:%v err:%v, want not found", ok, err)
		}
		res, err := s.Search(ctx, "someone-elses-workspace", "", 10)
		if err != nil || len(res) != 0 {
			t.Fatalf("Search across workspaces = %v, %v, want nothing", res, err)
		}
	})

	t.Run("an unknown sub is not found", func(t *testing.T) {
		s := newStore(t)
		if _, ok, err := s.Get(ctx, "nobody", "acme"); err != nil || ok {
			t.Fatalf("Get = ok:%v err:%v", ok, err)
		}
	})

	t.Run("later claims update, but empty claims never erase", func(t *testing.T) {
		s := newStore(t)
		_ = s.Upsert(ctx, alice())

		// A token from a client with a narrower scope: no email, no name.
		narrow := User{Sub: "sub-alice", PreferredUsername: "alice2", Workspaces: []string{"acme", "labs"}}
		if err := s.Upsert(ctx, narrow); err != nil {
			t.Fatal(err)
		}
		got, _, _ := s.Get(ctx, "sub-alice", "acme")
		if got.PreferredUsername != "alice2" {
			t.Errorf("username = %q, want the most recent value alice2", got.PreferredUsername)
		}
		if got.Email != "alice@example.com" || got.Name != "Alice Anderson" {
			t.Errorf("empty claims erased stored values: %+v", got)
		}
	})

	t.Run("workspaces are replaced, so leaving a workspace removes visibility", func(t *testing.T) {
		s := newStore(t)
		_ = s.Upsert(ctx, alice())
		left := alice()
		left.Workspaces = []string{"labs"}
		if err := s.Upsert(ctx, left); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.Get(ctx, "sub-alice", "acme"); ok {
			t.Error("still visible in a workspace the user no longer belongs to")
		}
		if _, ok, _ := s.Get(ctx, "sub-alice", "labs"); !ok {
			t.Error("no longer visible in a workspace the user still belongs to")
		}
	})

	t.Run("first-seen is preserved and last-seen advances", func(t *testing.T) {
		s := newStore(t)
		_ = s.Upsert(ctx, alice())
		first, _, _ := s.Get(ctx, "sub-alice", "acme")
		time.Sleep(20 * time.Millisecond)
		_ = s.Upsert(ctx, alice())
		second, _, _ := s.Get(ctx, "sub-alice", "acme")
		if !second.FirstSeenAt.Equal(first.FirstSeenAt) {
			t.Errorf("first-seen changed: %v -> %v", first.FirstSeenAt, second.FirstSeenAt)
		}
		if !second.LastSeenAt.After(first.LastSeenAt) {
			t.Errorf("last-seen did not advance: %v -> %v", first.LastSeenAt, second.LastSeenAt)
		}
	})

	seed := func(t *testing.T, s Store) {
		t.Helper()
		for _, u := range []User{
			{Sub: "s1", PreferredUsername: "carol", Name: "Carol Zhang", Email: "carol@example.com", Workspaces: []string{"acme"}},
			{Sub: "s2", PreferredUsername: "bob", Name: "Bob Baker", Email: "bob@corp.test", Workspaces: []string{"acme"}},
			{Sub: "s3", PreferredUsername: "alice", Name: "Alice Anderson", Email: "alice@example.com", Workspaces: []string{"acme", "labs"}},
			{Sub: "s4", PreferredUsername: "dave", Name: "Dave", Email: "dave@labs.test", Workspaces: []string{"labs"}},
			{Sub: "s5", PreferredUsername: "pct", Name: "100%_Sure", Email: "pct@example.com", Workspaces: []string{"acme"}},
		} {
			if err := s.Upsert(ctx, u); err != nil {
				t.Fatal(err)
			}
		}
	}
	subs := func(us []User) []string {
		out := make([]string, len(us))
		for i, u := range us {
			out[i] = u.Sub
		}
		return out
	}
	equal := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	t.Run("search: empty query lists the workspace, ordered by display name", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		got, err := s.Search(ctx, "acme", "", 50)
		if err != nil {
			t.Fatal(err)
		}
		// "100%_Sure" < "Alice Anderson" < "Bob Baker" < "Carol Zhang".
		if want := []string{"s5", "s3", "s2", "s1"}; !equal(subs(got), want) {
			t.Errorf("got %v, want %v", subs(got), want)
		}
	})

	t.Run("search: matches name, username, and email, case-insensitively", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		for q, want := range map[string][]string{
			"CAROL":       {"s1"}, // name / username, upper-cased query
			"bob@corp":    {"s2"}, // email
			"anderson":    {"s3"}, // part of name
			"example.com": {"s5", "s3", "s1"},
			"nobody-here": {},
		} {
			got, err := s.Search(ctx, "acme", q, 50)
			if err != nil {
				t.Fatal(err)
			}
			if !equal(subs(got), want) {
				t.Errorf("Search(%q) = %v, want %v", q, subs(got), want)
			}
		}
	})

	t.Run("search: scoped to the workspace", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		got, _ := s.Search(ctx, "labs", "", 50)
		if want := []string{"s3", "s4"}; !equal(subs(got), want) {
			t.Errorf("labs = %v, want %v (dave and alice; not acme-only users)", subs(got), want)
		}
	})

	t.Run("search: respects the limit", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		got, _ := s.Search(ctx, "acme", "", 2)
		if len(got) != 2 {
			t.Errorf("got %d results, want 2", len(got))
		}
	})

	t.Run("search: LIKE wildcards in the query match literally", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		for q, want := range map[string][]string{
			"%":   {"s5"}, // only the user whose name actually contains a percent sign
			"_":   {"s5"},
			"100": {"s5"},
			`\`:   {},
		} {
			got, err := s.Search(ctx, "acme", q, 50)
			if err != nil {
				t.Fatal(err)
			}
			if !equal(subs(got), want) {
				t.Errorf("Search(%q) = %v, want %v", q, subs(got), want)
			}
		}
	})

	t.Run("hostile input is inert", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		evil := `'; DROP TABLE booth_users; --`
		if err := s.Upsert(ctx, User{Sub: evil, Name: evil, Workspaces: []string{"acme"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Search(ctx, "acme", evil, 10); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.Get(ctx, evil, "acme"); err != nil || !ok {
			t.Fatalf("Get(evil) = ok:%v err:%v", ok, err)
		}
		if got, _ := s.Search(ctx, "acme", "", 50); len(got) < 5 {
			t.Errorf("table lost rows after hostile input: %d", len(got))
		}
	})
}
