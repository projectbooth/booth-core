package serviceaccount

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/testpg"
)

func newTestStore(t *testing.T) *PostgresStore {
	t.Helper()
	dsn := testpg.Start(t).DSN("postgres")
	s, err := NewPostgresStore(context.Background(), dsn)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	t.Cleanup(s.Close)
	if _, err := s.pool.Exec(context.Background(), schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := s.pool.Exec(context.Background(), "TRUNCATE booth_service_accounts"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func bot(workspace string) Account {
	return Account{
		Workspace: workspace, Name: "nfl-bot", Role: auth.RoleEditor,
		Description: "owns the weekly NFL pipeline", CreatedBy: "sub-alice", CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
}

func TestPostgresStore_CreateThenGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	want := bot("acme")
	if err := s.Create(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(ctx, "acme", "nfl-bot")
	if err != nil || !ok {
		t.Fatalf("Get = %+v, %v, %v", got, ok, err)
	}
	if got.Role != auth.RoleEditor || got.Description != want.Description || got.CreatedBy != want.CreatedBy {
		t.Errorf("got %+v, want fields of %+v", got, want)
	}
	if got.Disabled {
		t.Error("a newly created account must not be disabled")
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	if got.ID() != "svc:acme:nfl-bot" {
		t.Errorf("ID() = %q, want svc:acme:nfl-bot", got.ID())
	}
}

func TestPostgresStore_GetMissingReturnsNotFoundNotError(t *testing.T) {
	s := newTestStore(t)
	_, ok, err := s.Get(context.Background(), "acme", "nope")
	if err != nil || ok {
		t.Fatalf("Get = _, %v, %v, want false, nil", ok, err)
	}
}

func TestPostgresStore_CreateRefusesADuplicateNameInTheSameWorkspace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, bot("acme")); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, bot("acme")); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Create = %v, want ErrAlreadyExists", err)
	}
}

func TestPostgresStore_SameNameInDifferentWorkspacesIsFine(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, bot("acme")); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, bot("labs")); err != nil {
		t.Fatalf("Create in a different workspace = %v, want success (ADR 0120 #6: workspace-scoped)", err)
	}
	if _, ok, _ := s.Get(ctx, "acme", "nfl-bot"); !ok {
		t.Error("acme's account vanished")
	}
	if _, ok, _ := s.Get(ctx, "labs", "nfl-bot"); !ok {
		t.Error("labs's account was never created")
	}
}

func TestPostgresStore_List(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	b := bot("acme")
	a := b
	a.Name = "aardvark-bot"
	if err := s.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, bot("labs")); err != nil {
		t.Fatal(err)
	}

	got, err := s.List(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List(acme) returned %d accounts, want 2 (labs's must not leak in)", len(got))
	}
	if got[0].Name != "aardvark-bot" || got[1].Name != "nfl-bot" {
		t.Errorf("List is not ordered by name: %v", []string{got[0].Name, got[1].Name})
	}
}

func TestPostgresStore_UpdatePartialFieldsLeaveOthersAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, bot("acme")); err != nil {
		t.Fatal(err)
	}

	newDesc := "renamed"
	got, err := s.Update(ctx, "acme", "nfl-bot", Patch{Description: &newDesc})
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "renamed" {
		t.Errorf("Description = %q, want %q", got.Description, "renamed")
	}
	if got.Role != auth.RoleEditor {
		t.Errorf("Role = %q, want unchanged %q", got.Role, auth.RoleEditor)
	}

	viewer := auth.RoleViewer
	got, err = s.Update(ctx, "acme", "nfl-bot", Patch{Role: &viewer})
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != auth.RoleViewer {
		t.Errorf("Role = %q, want %q", got.Role, auth.RoleViewer)
	}
	if got.Description != "renamed" {
		t.Errorf("Description = %q, want unchanged %q", got.Description, "renamed")
	}
}

func TestPostgresStore_UpdateCanDisableAndReenable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, bot("acme")); err != nil {
		t.Fatal(err)
	}

	yes, no := true, false
	got, err := s.Update(ctx, "acme", "nfl-bot", Patch{Disabled: &yes})
	if err != nil || !got.Disabled {
		t.Fatalf("disable: %+v, %v", got, err)
	}
	got, err = s.Update(ctx, "acme", "nfl-bot", Patch{Disabled: &no})
	if err != nil || got.Disabled {
		t.Fatalf("re-enable: %+v, %v", got, err)
	}
}

func TestPostgresStore_UpdateMissingReturnsNotFound(t *testing.T) {
	s := newTestStore(t)
	viewer := auth.RoleViewer
	_, err := s.Update(context.Background(), "acme", "nope", Patch{Role: &viewer})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update on a missing account = %v, want ErrNotFound", err)
	}
}

// --- Delete: disable-before-delete (coordinator ruling, ADR 0120 addendum point 5) -------------

func TestPostgresStore_DeleteRefusesAnEnabledAccount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, bot("acme")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "acme", "nfl-bot"); !errors.Is(err, ErrMustDisableBeforeDelete) {
		t.Fatalf("Delete on an enabled account = %v, want ErrMustDisableBeforeDelete", err)
	}
	if _, ok, _ := s.Get(ctx, "acme", "nfl-bot"); !ok {
		t.Error("the refused delete must not have removed the account")
	}
}

func TestPostgresStore_DeleteSucceedsOnceDisabled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, bot("acme")); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s.Update(ctx, "acme", "nfl-bot", Patch{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "acme", "nfl-bot"); err != nil {
		t.Fatalf("Delete after disabling = %v, want success", err)
	}
	if _, ok, _ := s.Get(ctx, "acme", "nfl-bot"); ok {
		t.Error("account still present after Delete")
	}
}

func TestPostgresStore_DeleteMissingReturnsNotFound(t *testing.T) {
	s := newTestStore(t)
	if err := s.Delete(context.Background(), "acme", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete on a missing account = %v, want ErrNotFound", err)
	}
}

// A deleted name is not available for reuse at all in this v0 (no code enforces it beyond "the
// row is just gone, so a new Create is free to reuse the name") — documenting that reuse is
// allowed once disabled-and-deleted, which is the natural consequence of Delete being a real
// DELETE, not a soft-delete flag.
func TestPostgresStore_NameIsReusableAfterARealDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, bot("acme")); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s.Update(ctx, "acme", "nfl-bot", Patch{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "acme", "nfl-bot"); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, bot("acme")); err != nil {
		t.Fatalf("recreating after delete = %v, want success", err)
	}
}

func TestPostgresStore_ValidateNew(t *testing.T) {
	cases := []struct {
		name string
		role auth.Role
		ok   bool
	}{
		{"nfl-bot", auth.RoleEditor, true},
		{"nfl-bot", auth.RoleViewer, true},
		{"nfl-bot", auth.RoleOwner, false},     // ADR 0120 #1/#6: never owner
		{"nfl-bot", auth.Role("admin"), false}, // not a real role at all
		{"", auth.RoleEditor, false},
		{"NFL-Bot", auth.RoleEditor, false}, // same grammar as a workspace slug: lowercase only
		{"nfl bot", auth.RoleEditor, false}, // no spaces
	}
	for _, c := range cases {
		err := ValidateNew(c.name, c.role)
		if (err == nil) != c.ok {
			t.Errorf("ValidateNew(%q, %q) error = %v, want ok=%v", c.name, c.role, err, c.ok)
		}
	}
}
