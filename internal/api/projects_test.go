package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/project"
)

// fakeProjectStore is a minimal map-backed project.Store for handler tests — this package has no
// real Postgres to exercise, and internal/project's own package tests already cover the real
// store's behaviour against one.
type fakeProjectStore struct {
	projects map[string]project.Project
	bindings map[string]project.Binding
	nextID   int
	err      error // when set, every call fails with this (simulates ErrUnavailable)
}

func newFakeProjectStore() *fakeProjectStore {
	return &fakeProjectStore{projects: map[string]project.Project{}, bindings: map[string]project.Binding{}}
}

func (f *fakeProjectStore) key(ws, slug string) string { return ws + "\x00" + slug }

func (f *fakeProjectStore) Create(_ context.Context, p project.Project) error {
	if f.err != nil {
		return f.err
	}
	k := f.key(p.Workspace, p.Slug)
	if _, exists := f.projects[k]; exists {
		return project.ErrAlreadyExists
	}
	if p.Parent != "" {
		parent, ok := f.projects[f.key(p.Workspace, p.Parent)]
		if !ok {
			return project.ErrParentNotFound
		}
		if parent.Parent != "" {
			return project.ErrParentHasAParent
		}
	}
	f.projects[k] = p
	return nil
}

func (f *fakeProjectStore) Get(_ context.Context, ws, slug string) (project.Project, bool, error) {
	if f.err != nil {
		return project.Project{}, false, f.err
	}
	p, ok := f.projects[f.key(ws, slug)]
	return p, ok, nil
}

func (f *fakeProjectStore) List(_ context.Context, ws string) ([]project.Project, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []project.Project
	for _, p := range f.projects {
		if p.Workspace == ws {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeProjectStore) Update(_ context.Context, ws, slug string, patch project.Patch) (project.Project, error) {
	if f.err != nil {
		return project.Project{}, f.err
	}
	k := f.key(ws, slug)
	p, ok := f.projects[k]
	if !ok {
		return project.Project{}, project.ErrNotFound
	}
	if patch.Name != nil {
		p.Name = *patch.Name
	}
	if patch.Parent != nil {
		p.Parent = *patch.Parent
	}
	if patch.Description != nil {
		p.Description = *patch.Description
	}
	if patch.Archived != nil {
		p.Archived = *patch.Archived
	}
	f.projects[k] = p
	return p, nil
}

func (f *fakeProjectStore) CreateBinding(_ context.Context, b project.Binding) (project.Binding, error) {
	if f.err != nil {
		return project.Binding{}, f.err
	}
	if _, ok := f.projects[f.key(b.Workspace, b.ProjectSlug)]; !ok {
		return project.Binding{}, project.ErrNotFound
	}
	f.nextID++
	b.ID = fmt.Sprintf("bind-%d", f.nextID)
	f.bindings[b.Workspace+"\x00"+b.ID] = b
	return b, nil
}

func (f *fakeProjectStore) ListBindings(_ context.Context, ws string, filter project.BindingFilter) ([]project.Binding, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []project.Binding
	for _, b := range f.bindings {
		if b.Workspace != ws {
			continue
		}
		if filter.ProjectSlug != "" && b.ProjectSlug != filter.ProjectSlug {
			continue
		}
		if filter.Kind != "" && b.Kind != filter.Kind {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

func (f *fakeProjectStore) DeleteBinding(_ context.Context, ws, id string) error {
	if f.err != nil {
		return f.err
	}
	k := ws + "\x00" + id
	if _, ok := f.bindings[k]; !ok {
		return project.ErrBindingNotFound
	}
	delete(f.bindings, k)
	return nil
}

func withSlugParam(req *http.Request, slug string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slug", slug)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// --- handler-level tests ------------------------------------------------------------------------

func TestListProjects_ScopedToCallersWorkspace(t *testing.T) {
	store := newFakeProjectStore()
	_ = store.Create(context.Background(), project.Project{Workspace: "acme-analytics", Slug: "finance", Name: "Finance"})
	_ = store.Create(context.Background(), project.Project{Workspace: "other-ws", Slug: "finance", Name: "Finance"})

	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/projects", nil), auth.RoleViewer)
	rec := httptest.NewRecorder()
	handleListProjects(store)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out []projectView
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Slug != "finance" {
		t.Errorf("out = %+v, want exactly the caller's workspace's project", out)
	}
}

func TestGetProject_MissingIs404(t *testing.T) {
	store := newFakeProjectStore()
	req := withSlugParam(withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/projects/nope", nil), auth.RoleViewer), "nope")
	rec := httptest.NewRecorder()
	handleGetProject(store)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestCreateProject_RecordsCreatedByAndValidatesSlug(t *testing.T) {
	store := newFakeProjectStore()
	now := func() time.Time { return time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC) }

	body := strings.NewReader(`{"slug":"finance","name":"Finance"}`)
	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/projects", body), auth.RoleOwner)
	rec := httptest.NewRecorder()
	handleCreateProject(store, now)(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var view projectView
	if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.CreatedBy != "user-1" { // withTestIdentity's Subject
		t.Errorf("createdBy = %q, want %q", view.CreatedBy, "user-1")
	}
	if view.Archived {
		t.Error("a newly created project must not be archived")
	}

	for _, slug := range []string{"Finance", "finance!", ""} {
		body := strings.NewReader(`{"slug":"` + slug + `","name":"x"}`)
		req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/projects", body), auth.RoleOwner)
		rec := httptest.NewRecorder()
		handleCreateProject(store, now)(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("slug %q: status = %d, want 400", slug, rec.Code)
		}
	}
}

func TestCreateProject_DuplicateSlugIs409(t *testing.T) {
	store := newFakeProjectStore()
	now := time.Now
	mk := func() *http.Request {
		body := strings.NewReader(`{"slug":"finance","name":"Finance"}`)
		return withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/projects", body), auth.RoleOwner)
	}
	rec := httptest.NewRecorder()
	handleCreateProject(store, now)(rec, mk())
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: status = %d, want 201", rec.Code)
	}
	rec = httptest.NewRecorder()
	handleCreateProject(store, now)(rec, mk())
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create: status = %d, want 409", rec.Code)
	}
}

func TestCreateProject_UnknownParentIs404(t *testing.T) {
	store := newFakeProjectStore()
	now := time.Now
	body := strings.NewReader(`{"slug":"finance-dev","name":"Finance (dev)","parent":"nope"}`)
	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/projects", body), auth.RoleOwner)
	rec := httptest.NewRecorder()
	handleCreateProject(store, now)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestPatchProject_PartialUpdateAndArchive(t *testing.T) {
	store := newFakeProjectStore()
	_ = store.Create(context.Background(), project.Project{Workspace: "acme-analytics", Slug: "finance", Name: "Finance", Description: "orig"})

	body := strings.NewReader(`{"archived":true}`)
	req := withSlugParam(withTestIdentity(httptest.NewRequest(http.MethodPatch, "/api/projects/finance", body), auth.RoleOwner), "finance")
	rec := httptest.NewRecorder()
	handlePatchProject(store)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var view projectView
	if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if !view.Archived {
		t.Error("Archived = false, want true")
	}
	if view.Description != "orig" {
		t.Errorf("Description = %q, want unchanged %q", view.Description, "orig")
	}
}

func TestPatchProject_InvalidNameIs400(t *testing.T) {
	store := newFakeProjectStore()
	_ = store.Create(context.Background(), project.Project{Workspace: "acme-analytics", Slug: "finance", Name: "Finance"})

	body := strings.NewReader(`{"name":""}`)
	req := withSlugParam(withTestIdentity(httptest.NewRequest(http.MethodPatch, "/api/projects/finance", body), auth.RoleOwner), "finance")
	rec := httptest.NewRecorder()
	handlePatchProject(store)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestProjectError_UnavailableIs503(t *testing.T) {
	store := newFakeProjectStore()
	store.err = project.ErrUnavailable

	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/projects", nil), auth.RoleViewer)
	rec := httptest.NewRecorder()
	handleListProjects(store)(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// --- bindings -------------------------------------------------------------------------------

func TestCreateProjectBinding_ValidatesAndRecordsIt(t *testing.T) {
	store := newFakeProjectStore()
	_ = store.Create(context.Background(), project.Project{Workspace: "acme-analytics", Slug: "finance", Name: "Finance"})
	now := time.Now

	body := strings.NewReader(`{"kind":"postgres_schema","value":"finance_dev"}`)
	req := withSlugParam(withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/projects/finance/bindings", body), auth.RoleOwner), "finance")
	rec := httptest.NewRecorder()
	handleCreateProjectBinding(store, now)(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var view bindingView
	if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.Value != "finance_dev" || view.Kind != "postgres_schema" {
		t.Errorf("view = %+v", view)
	}

	// storage_prefix without a backendId is refused (project.ValidateNewBinding).
	body2 := strings.NewReader(`{"kind":"storage_prefix","value":"inbox/"}`)
	req2 := withSlugParam(withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/projects/finance/bindings", body2), auth.RoleOwner), "finance")
	rec2 := httptest.NewRecorder()
	handleCreateProjectBinding(store, now)(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("storage_prefix without backendId: status = %d, want 400", rec2.Code)
	}
}

func TestCreateProjectBinding_UnknownProjectIs404(t *testing.T) {
	store := newFakeProjectStore()
	body := strings.NewReader(`{"kind":"postgres_schema","value":"finance_dev"}`)
	req := withSlugParam(withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/projects/nope/bindings", body), auth.RoleOwner), "nope")
	rec := httptest.NewRecorder()
	handleCreateProjectBinding(store, time.Now)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestListWorkspaceBindings_FiltersByKind(t *testing.T) {
	store := newFakeProjectStore()
	ctx := context.Background()
	_ = store.Create(ctx, project.Project{Workspace: "acme-analytics", Slug: "finance", Name: "Finance"})
	_, _ = store.CreateBinding(ctx, project.Binding{Workspace: "acme-analytics", ProjectSlug: "finance", Kind: project.BindingPostgresSchema, Value: "finance_dev"})
	_, _ = store.CreateBinding(ctx, project.Binding{Workspace: "acme-analytics", ProjectSlug: "finance", Kind: project.BindingStoragePrefix, BackendID: "local", Value: "finance/dev/"})

	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/projects/bindings?kind=postgres_schema", nil), auth.RoleViewer)
	rec := httptest.NewRecorder()
	handleListWorkspaceBindings(store)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out []bindingView
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Kind != "postgres_schema" {
		t.Errorf("out = %+v, want exactly the one postgres_schema binding", out)
	}
}

// withSlugAndIDParams sets both chi route params the merged contract's DELETE path carries:
// /api/projects/{slug}/bindings/{id}.
func withSlugAndIDParams(req *http.Request, slug, id string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slug", slug)
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestDeleteProjectBinding(t *testing.T) {
	store := newFakeProjectStore()
	ctx := context.Background()
	_ = store.Create(ctx, project.Project{Workspace: "acme-analytics", Slug: "finance", Name: "Finance"})
	b, err := store.CreateBinding(ctx, project.Binding{Workspace: "acme-analytics", ProjectSlug: "finance", Kind: project.BindingPostgresSchema, Value: "finance_dev"})
	if err != nil {
		t.Fatal(err)
	}

	req := withSlugAndIDParams(
		withTestIdentity(httptest.NewRequest(http.MethodDelete, "/api/projects/finance/bindings/"+b.ID, nil), auth.RoleOwner),
		"finance", b.ID)
	rec := httptest.NewRecorder()
	handleDeleteProjectBinding(store)(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}
}

// The path's {slug} must actually own the binding being deleted — a correct id under the wrong
// project in the URL is refused as a 404, not silently honoured.
func TestDeleteProjectBinding_RefusesWhenTheSlugDoesNotOwnTheBinding(t *testing.T) {
	store := newFakeProjectStore()
	ctx := context.Background()
	_ = store.Create(ctx, project.Project{Workspace: "acme-analytics", Slug: "finance", Name: "Finance"})
	_ = store.Create(ctx, project.Project{Workspace: "acme-analytics", Slug: "nfl", Name: "NFL"})
	b, err := store.CreateBinding(ctx, project.Binding{Workspace: "acme-analytics", ProjectSlug: "finance", Kind: project.BindingPostgresSchema, Value: "finance_dev"})
	if err != nil {
		t.Fatal(err)
	}

	req := withSlugAndIDParams(
		withTestIdentity(httptest.NewRequest(http.MethodDelete, "/api/projects/nfl/bindings/"+b.ID, nil), auth.RoleOwner),
		"nfl", b.ID)
	rec := httptest.NewRecorder()
	handleDeleteProjectBinding(store)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	// The binding must still exist under its real project.
	got, err := store.ListBindings(ctx, "acme-analytics", project.BindingFilter{ProjectSlug: "finance"})
	if err != nil || len(got) != 1 {
		t.Fatalf("binding was deleted despite the mismatched slug: %+v, %v", got, err)
	}
}

// --- router-level: requireAdmin gates writes; reads stay open to any member. --------------------

func TestRouter_ProjectWritesAreOwnerOnlyReadsAreOpen(t *testing.T) {
	idp := newTestIDP(t)
	store := newFakeProjectStore()
	router := routerWith(t, idp, func(d *Deps) { d.Projects = store })

	editorToken := idp.token(t, "u-editor", map[string]any{"groups": []string{"/workspaces/acme/editor"}})
	ownerToken := idp.token(t, "u-owner", map[string]any{"groups": []string{"/workspaces/acme/owner"}})

	post := func(token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(auth.HeaderWorkspace, "acme")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(editorToken, `{"slug":"finance","name":"Finance"}`); rec.Code != http.StatusForbidden {
		t.Errorf("editor create: status = %d, want 403", rec.Code)
	}
	if rec := post(ownerToken, `{"slug":"finance","name":"Finance"}`); rec.Code != http.StatusCreated {
		t.Errorf("owner create: status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}

	// Listing is open to any member — unlike service accounts, a project carries no sensitive
	// detail and the picker/grouping UI needs it for every role.
	getList := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	getList.Header.Set("Authorization", "Bearer "+editorToken)
	getList.Header.Set(auth.HeaderWorkspace, "acme")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, getList)
	if rec.Code != http.StatusOK {
		t.Errorf("editor list: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	patch := httptest.NewRequest(http.MethodPatch, "/api/projects/finance", strings.NewReader(`{"archived":true}`))
	patch.Header.Set("Authorization", "Bearer "+editorToken)
	patch.Header.Set(auth.HeaderWorkspace, "acme")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, patch)
	if rec.Code != http.StatusForbidden {
		t.Errorf("editor patch: status = %d, want 403", rec.Code)
	}
}

// The static "/api/projects/bindings" route must win over the "/api/projects/{slug}" wildcard —
// otherwise a request for the workspace-wide bindings list would be mistaken for a lookup of a
// project literally named "bindings".
func TestRouter_ProjectsBindingsRouteDoesNotCollideWithTheSlugWildcard(t *testing.T) {
	idp := newTestIDP(t)
	store := newFakeProjectStore()
	router := routerWith(t, idp, func(d *Deps) { d.Projects = store })
	token := idp.token(t, "u1", map[string]any{"groups": []string{"/workspaces/acme/owner"}})

	req := httptest.NewRequest(http.MethodGet, "/api/projects/bindings", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(auth.HeaderWorkspace, "acme")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the workspace-wide bindings route, not a 404 project lookup)", rec.Code)
	}
	var out []bindingView
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("response is not a binding list: %v (body=%s)", err, rec.Body.String())
	}
}

// Deps.Projects == nil leaves the routes unregistered entirely — a plain 404, the same
// "optional: nil disables the routes" convention every other optional Deps field uses.
func TestRouter_ProjectRoutesAreUnregisteredWhenNil(t *testing.T) {
	idp := newTestIDP(t)
	router := routerWith(t, idp, nil) // Projects left nil
	token := idp.token(t, "u1", map[string]any{"groups": []string{"/workspaces/acme/owner"}})

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(auth.HeaderWorkspace, "acme")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
