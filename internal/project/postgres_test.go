package project

import (
	"context"
	"errors"
	"testing"
	"time"

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
	if _, err := s.pool.Exec(context.Background(), "TRUNCATE booth_projects, booth_project_bindings"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func finance(workspace string) Project {
	return Project{
		Workspace: workspace, Slug: "finance", Name: "Finance",
		CreatedBy: "sub-alice", CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
}

// --- Create / Get / List ---------------------------------------------------------------------

func TestPostgresStore_CreateThenGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	want := finance("acme")
	if err := s.Create(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(ctx, "acme", "finance")
	if err != nil || !ok {
		t.Fatalf("Get = %+v, %v, %v", got, ok, err)
	}
	if got.Name != want.Name || got.CreatedBy != want.CreatedBy {
		t.Errorf("got %+v, want fields of %+v", got, want)
	}
	if got.Archived {
		t.Error("a newly created project must not be archived")
	}
	if got.Parent != "" {
		t.Errorf("Parent = %q, want empty (top-level)", got.Parent)
	}
}

func TestPostgresStore_GetMissingReturnsNotFoundNotError(t *testing.T) {
	s := newTestStore(t)
	_, ok, err := s.Get(context.Background(), "acme", "nope")
	if err != nil || ok {
		t.Fatalf("Get = _, %v, %v, want false, nil", ok, err)
	}
}

func TestPostgresStore_CreateRefusesADuplicateSlugInTheSameWorkspace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, finance("acme")); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Create = %v, want ErrAlreadyExists", err)
	}
}

func TestPostgresStore_SameSlugInDifferentWorkspacesIsFine(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, finance("labs")); err != nil {
		t.Fatalf("Create in a different workspace = %v, want success (workspace-scoped)", err)
	}
	if _, ok, _ := s.Get(ctx, "acme", "finance"); !ok {
		t.Error("acme's project vanished")
	}
	if _, ok, _ := s.Get(ctx, "labs", "finance"); !ok {
		t.Error("labs's project was never created")
	}
}

func TestPostgresStore_List(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	f := finance("acme")
	n := f
	n.Slug, n.Name = "nfl", "NFL"
	if err := s.Create(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, finance("labs")); err != nil {
		t.Fatal(err)
	}

	got, err := s.List(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List(acme) returned %d projects, want 2 (labs's must not leak in)", len(got))
	}
	if got[0].Slug != "finance" || got[1].Slug != "nfl" {
		t.Errorf("List is not ordered by slug: %v", []string{got[0].Slug, got[1].Slug})
	}
}

// --- Parent / depth (ADR 0124 point 5: two levels to start) ----------------------------------

func TestPostgresStore_CreateWithAValidParentSucceeds(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	dev := finance("acme")
	dev.Slug, dev.Name, dev.Parent = "finance-dev", "Finance (dev)", "finance"
	if err := s.Create(ctx, dev); err != nil {
		t.Fatalf("Create with a valid top-level parent = %v, want success", err)
	}
	got, _, _ := s.Get(ctx, "acme", "finance-dev")
	if got.Parent != "finance" {
		t.Errorf("Parent = %q, want finance", got.Parent)
	}
}

func TestPostgresStore_CreateRefusesAnUnknownParent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	p := finance("acme")
	p.Parent = "nope"
	if err := s.Create(ctx, p); !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("Create with an unknown parent = %v, want ErrParentNotFound", err)
	}
}

func TestPostgresStore_CreateRefusesAnArchivedParent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	archived := true
	if _, err := s.Update(ctx, "acme", "finance", Patch{Archived: &archived}); err != nil {
		t.Fatal(err)
	}
	child := finance("acme")
	child.Slug, child.Parent = "finance-dev", "finance"
	if err := s.Create(ctx, child); !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("Create under an archived parent = %v, want ErrParentNotFound", err)
	}
}

func TestPostgresStore_CreateRefusesThreeLevels(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	dev := finance("acme")
	dev.Slug, dev.Parent = "finance-dev", "finance"
	if err := s.Create(ctx, dev); err != nil {
		t.Fatal(err)
	}
	grandchild := finance("acme")
	grandchild.Slug, grandchild.Parent = "finance-dev-sub", "finance-dev"
	if err := s.Create(ctx, grandchild); !errors.Is(err, ErrParentHasAParent) {
		t.Fatalf("Create under a non-top-level parent = %v, want ErrParentHasAParent", err)
	}
}

func TestPostgresStore_UpdateCanReparentToAValidTopLevelProject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	nfl := finance("acme")
	nfl.Slug, nfl.Name = "nfl", "NFL"
	if err := s.Create(ctx, nfl); err != nil {
		t.Fatal(err)
	}
	parent := "finance"
	got, err := s.Update(ctx, "acme", "nfl", Patch{Parent: &parent})
	if err != nil {
		t.Fatalf("re-parent = %v, want success", err)
	}
	if got.Parent != "finance" {
		t.Errorf("Parent = %q, want finance", got.Parent)
	}
}

func TestPostgresStore_UpdateRefusesReparentingAProjectThatHasChildren(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	dev := finance("acme")
	dev.Slug, dev.Parent = "finance-dev", "finance"
	if err := s.Create(ctx, dev); err != nil {
		t.Fatal(err)
	}
	nfl := finance("acme")
	nfl.Slug, nfl.Name = "nfl", "NFL"
	if err := s.Create(ctx, nfl); err != nil {
		t.Fatal(err)
	}

	parent := "nfl"
	if _, err := s.Update(ctx, "acme", "finance", Patch{Parent: &parent}); !errors.Is(err, ErrHasChildren) {
		t.Fatalf("re-parenting a project with children = %v, want ErrHasChildren", err)
	}
}

func TestPostgresStore_UpdateCanClearParentBackToTopLevel(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	dev := finance("acme")
	dev.Slug, dev.Parent = "finance-dev", "finance"
	if err := s.Create(ctx, dev); err != nil {
		t.Fatal(err)
	}

	empty := ""
	got, err := s.Update(ctx, "acme", "finance-dev", Patch{Parent: &empty})
	if err != nil {
		t.Fatalf("clearing parent = %v, want success", err)
	}
	if got.Parent != "" {
		t.Errorf("Parent = %q, want empty after clearing", got.Parent)
	}
}

// --- Update, general -------------------------------------------------------------------------

func TestPostgresStore_UpdatePartialFieldsLeaveOthersAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}

	newDesc := "quarterly numbers"
	got, err := s.Update(ctx, "acme", "finance", Patch{Description: &newDesc})
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "quarterly numbers" {
		t.Errorf("Description = %q, want %q", got.Description, "quarterly numbers")
	}
	if got.Name != "Finance" {
		t.Errorf("Name = %q, want unchanged %q", got.Name, "Finance")
	}
}

func TestPostgresStore_UpdateCanArchiveAndUnarchive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	got, err := s.Update(ctx, "acme", "finance", Patch{Archived: &yes})
	if err != nil || !got.Archived {
		t.Fatalf("archive: %+v, %v", got, err)
	}
	got, err = s.Update(ctx, "acme", "finance", Patch{Archived: &no})
	if err != nil || got.Archived {
		t.Fatalf("unarchive: %+v, %v", got, err)
	}
}

func TestPostgresStore_ArchivingAParentDoesNotCascadeToChildren(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	dev := finance("acme")
	dev.Slug, dev.Parent = "finance-dev", "finance"
	if err := s.Create(ctx, dev); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s.Update(ctx, "acme", "finance", Patch{Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get(ctx, "acme", "finance-dev")
	if got.Archived {
		t.Error("archiving the parent must not archive the child (independent flags)")
	}
}

func TestPostgresStore_UpdateMissingReturnsNotFound(t *testing.T) {
	s := newTestStore(t)
	name := "x"
	_, err := s.Update(context.Background(), "acme", "nope", Patch{Name: &name})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update on a missing project = %v, want ErrNotFound", err)
	}
}

func TestPostgresStore_ValidateNew(t *testing.T) {
	cases := []struct {
		slug, name string
		ok         bool
	}{
		{"finance", "Finance", true},
		{"finance-dev", "Finance (dev)", true},
		{"", "Finance", false},
		{"Finance", "Finance", false}, // lowercase only
		{"finance!", "Finance", false},
		{"finance", "", false}, // name required
	}
	for _, c := range cases {
		err := ValidateNew(c.slug, c.name)
		if (err == nil) != c.ok {
			t.Errorf("ValidateNew(%q, %q) error = %v, want ok=%v", c.slug, c.name, err, c.ok)
		}
	}
}

// --- Bindings ---------------------------------------------------------------------------------

func newBinding(workspace, slug string, kind BindingKind, backendID, value string) Binding {
	return Binding{
		Workspace: workspace, ProjectSlug: slug, Kind: kind, BackendID: backendID, Value: value,
		CreatedBy: "sub-alice", CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
}

func TestPostgresStore_CreateBindingThenList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateBinding(ctx, newBinding("acme", "finance", BindingPostgresSchema, "", "finance_dev"))
	if err != nil {
		t.Fatal(err)
	}
	if b.ID == "" {
		t.Error("CreateBinding did not assign an ID")
	}

	got, err := s.ListBindings(ctx, "acme", BindingFilter{ProjectSlug: "finance"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Value != "finance_dev" {
		t.Fatalf("ListBindings(project=finance) = %+v, want one binding on finance_dev", got)
	}
}

func TestPostgresStore_CreateBindingRefusesAnUnknownProject(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateBinding(context.Background(), newBinding("acme", "nope", BindingPostgresSchema, "", "x"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateBinding for an unknown project = %v, want ErrNotFound", err)
	}
}

func TestPostgresStore_CreateBindingRefusesADuplicateExternalName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	nfl := finance("acme")
	nfl.Slug, nfl.Name = "nfl", "NFL"
	if err := s.Create(ctx, nfl); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBinding(ctx, newBinding("acme", "finance", BindingPostgresSchema, "", "shared_schema")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBinding(ctx, newBinding("acme", "nfl", BindingPostgresSchema, "", "shared_schema")); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("second binding on the same schema = %v, want ErrBindingConflict", err)
	}
}

// Same path string, different Storage backends: must not collide — the backendId is part of the
// uniqueness key precisely so this is allowed (coordinator ruling on the design note).
func TestPostgresStore_StoragePrefixBindingsAreScopedPerBackend(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	nfl := finance("acme")
	nfl.Slug, nfl.Name = "nfl", "NFL"
	if err := s.Create(ctx, nfl); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBinding(ctx, newBinding("acme", "finance", BindingStoragePrefix, "local", "inbox/")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBinding(ctx, newBinding("acme", "nfl", BindingStoragePrefix, "s3", "inbox/")); err != nil {
		t.Fatalf("same prefix on a different backend = %v, want success", err)
	}
}

func TestPostgresStore_ListBindingsByKindAcrossTheWholeWorkspace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	nfl := finance("acme")
	nfl.Slug, nfl.Name = "nfl", "NFL"
	if err := s.Create(ctx, nfl); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBinding(ctx, newBinding("acme", "finance", BindingPostgresSchema, "", "finance_dev")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBinding(ctx, newBinding("acme", "nfl", BindingPostgresSchema, "", "nfl_data")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBinding(ctx, newBinding("acme", "nfl", BindingStoragePrefix, "local", "nfl/")); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListBindings(ctx, "acme", BindingFilter{Kind: BindingPostgresSchema})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListBindings(kind=postgres_schema) = %d bindings, want 2 (storage_prefix must not leak in)", len(got))
	}
}

func TestPostgresStore_DeleteBinding(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, finance("acme")); err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateBinding(ctx, newBinding("acme", "finance", BindingPostgresSchema, "", "finance_dev"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBinding(ctx, "acme", b.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListBindings(ctx, "acme", BindingFilter{ProjectSlug: "finance"})
	if len(got) != 0 {
		t.Errorf("bindings after delete = %+v, want none", got)
	}
}

func TestPostgresStore_DeleteBindingMissingReturnsNotFound(t *testing.T) {
	s := newTestStore(t)
	if err := s.DeleteBinding(context.Background(), "acme", "nope"); !errors.Is(err, ErrBindingNotFound) {
		t.Fatalf("DeleteBinding on a missing id = %v, want ErrBindingNotFound", err)
	}
}

func TestValidateNewBinding(t *testing.T) {
	cases := []struct {
		kind      BindingKind
		backendID string
		value     string
		ok        bool
	}{
		{BindingPostgresSchema, "", "finance_dev", true},
		{BindingIcebergNamespace, "", "lakehouse.nfl", true},
		{BindingStoragePrefix, "local", "finance/dev/", true},
		{BindingStoragePrefix, "", "finance/dev/", false},      // backendId required
		{BindingPostgresSchema, "local", "finance_dev", false}, // backendId must be empty
		{BindingKind("bogus"), "", "x", false},
		{BindingPostgresSchema, "", "", false},
	}
	for _, c := range cases {
		err := ValidateNewBinding(c.kind, c.backendID, c.value)
		if (err == nil) != c.ok {
			t.Errorf("ValidateNewBinding(%q, %q, %q) error = %v, want ok=%v", c.kind, c.backendID, c.value, err, c.ok)
		}
	}
}
