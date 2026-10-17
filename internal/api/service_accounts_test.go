package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/serviceaccount"
)

// fakeServiceAccountStore is a minimal map-backed serviceaccount.Store for handler tests — this
// package has no real Postgres to exercise, and internal/serviceaccount's own package tests
// already cover the real store's behaviour against one.
type fakeServiceAccountStore struct {
	accounts map[string]serviceaccount.Account
	err      error // when set, every call fails with this (simulates ErrUnavailable)
}

func newFakeServiceAccountStore() *fakeServiceAccountStore {
	return &fakeServiceAccountStore{accounts: map[string]serviceaccount.Account{}}
}

func (f *fakeServiceAccountStore) key(ws, name string) string { return ws + "\x00" + name }

func (f *fakeServiceAccountStore) Create(_ context.Context, a serviceaccount.Account) error {
	if f.err != nil {
		return f.err
	}
	k := f.key(a.Workspace, a.Name)
	if _, exists := f.accounts[k]; exists {
		return serviceaccount.ErrAlreadyExists
	}
	f.accounts[k] = a
	return nil
}

func (f *fakeServiceAccountStore) Get(_ context.Context, ws, name string) (serviceaccount.Account, bool, error) {
	if f.err != nil {
		return serviceaccount.Account{}, false, f.err
	}
	a, ok := f.accounts[f.key(ws, name)]
	return a, ok, nil
}

func (f *fakeServiceAccountStore) List(_ context.Context, ws string) ([]serviceaccount.Account, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []serviceaccount.Account
	for _, a := range f.accounts {
		if a.Workspace == ws {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeServiceAccountStore) Update(_ context.Context, ws, name string, patch serviceaccount.Patch) (serviceaccount.Account, error) {
	if f.err != nil {
		return serviceaccount.Account{}, f.err
	}
	k := f.key(ws, name)
	a, ok := f.accounts[k]
	if !ok {
		return serviceaccount.Account{}, serviceaccount.ErrNotFound
	}
	if patch.Role != nil {
		a.Role = *patch.Role
	}
	if patch.Description != nil {
		a.Description = *patch.Description
	}
	if patch.Disabled != nil {
		a.Disabled = *patch.Disabled
	}
	f.accounts[k] = a
	return a, nil
}

func (f *fakeServiceAccountStore) Delete(_ context.Context, ws, name string) error {
	if f.err != nil {
		return f.err
	}
	k := f.key(ws, name)
	a, ok := f.accounts[k]
	if !ok {
		return serviceaccount.ErrNotFound
	}
	if !a.Disabled {
		return serviceaccount.ErrMustDisableBeforeDelete
	}
	delete(f.accounts, k)
	return nil
}

func withNameParam(req *http.Request, name string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", name)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// --- handler-level tests. -----------------------------------------------------------------------

func TestListServiceAccounts_ScopedToCallersWorkspace(t *testing.T) {
	store := newFakeServiceAccountStore()
	_ = store.Create(context.Background(), serviceaccount.Account{Workspace: "acme-analytics", Name: "nfl-bot", Role: auth.RoleEditor})
	_ = store.Create(context.Background(), serviceaccount.Account{Workspace: "other-ws", Name: "nfl-bot", Role: auth.RoleEditor})

	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/service-accounts", nil), auth.RoleViewer)
	rec := httptest.NewRecorder()
	handleListServiceAccounts(store)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out []serviceAccountView
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "nfl-bot" {
		t.Errorf("out = %+v, want exactly the caller's workspace's account", out)
	}
}

func TestGetServiceAccount_MissingIs404(t *testing.T) {
	store := newFakeServiceAccountStore()
	req := withNameParam(withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/service-accounts/nope", nil), auth.RoleViewer), "nope")
	rec := httptest.NewRecorder()
	handleGetServiceAccount(store)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// ADR 0119 amendment 3 point 5: any member gets the trimmed badge {name, role, disabled}, not
// the full detail (description, createdBy, createdAt) the list route reserves for owners.
func TestGetServiceAccount_ReturnsTheTrimmedBadgeNotTheFullView(t *testing.T) {
	store := newFakeServiceAccountStore()
	_ = store.Create(context.Background(), serviceaccount.Account{
		Workspace: "acme-analytics", Name: "nfl-bot", Role: auth.RoleEditor,
		Description: "secret rationale", CreatedBy: "sub-someone",
	})
	req := withNameParam(withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/service-accounts/nfl-bot", nil), auth.RoleViewer), "nfl-bot")
	rec := httptest.NewRecorder()
	handleGetServiceAccount(store)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["name"] != "nfl-bot" || body["role"] != "editor" || body["disabled"] != false {
		t.Errorf("badge fields = %v", body)
	}
	for _, leaked := range []string{"description", "createdBy", "createdAt", "id"} {
		if _, present := body[leaked]; present {
			t.Errorf("badge view leaks %q: %v", leaked, body)
		}
	}
}

func TestCreateServiceAccount_RecordsCreatedByAndRejectsOwnerRole(t *testing.T) {
	store := newFakeServiceAccountStore()
	now := func() time.Time { return time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC) }

	body := strings.NewReader(`{"name":"nfl-bot","role":"editor","description":"weekly pipeline"}`)
	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/service-accounts", body), auth.RoleOwner)
	rec := httptest.NewRecorder()
	handleCreateServiceAccount(store, now)(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var view serviceAccountView
	if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.CreatedBy != "user-1" { // withTestIdentity's Subject
		t.Errorf("createdBy = %q, want %q", view.CreatedBy, "user-1")
	}
	if view.ID != "svc:acme-analytics:nfl-bot" {
		t.Errorf("id = %q, want svc:acme-analytics:nfl-bot", view.ID)
	}
	if view.Disabled {
		t.Error("a newly created account must not be disabled")
	}

	// A role of "owner" (or anything else unreal) is refused — ADR 0120 #1/#6: never owner.
	for _, role := range []string{"owner", "admin", ""} {
		body := strings.NewReader(`{"name":"another-bot","role":"` + role + `"}`)
		req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/service-accounts", body), auth.RoleOwner)
		rec := httptest.NewRecorder()
		handleCreateServiceAccount(store, now)(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("role %q: status = %d, want 400", role, rec.Code)
		}
	}
}

func TestCreateServiceAccount_DuplicateNameIs409(t *testing.T) {
	store := newFakeServiceAccountStore()
	now := time.Now
	mk := func() *http.Request {
		body := strings.NewReader(`{"name":"nfl-bot","role":"editor"}`)
		return withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/service-accounts", body), auth.RoleOwner)
	}
	rec := httptest.NewRecorder()
	handleCreateServiceAccount(store, now)(rec, mk())
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: status = %d, want 201", rec.Code)
	}
	rec = httptest.NewRecorder()
	handleCreateServiceAccount(store, now)(rec, mk())
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create: status = %d, want 409", rec.Code)
	}
}

func TestPatchServiceAccount_PartialUpdateAndDisable(t *testing.T) {
	store := newFakeServiceAccountStore()
	_ = store.Create(context.Background(), serviceaccount.Account{Workspace: "acme-analytics", Name: "nfl-bot", Role: auth.RoleEditor, Description: "orig"})

	body := strings.NewReader(`{"disabled":true}`)
	req := withNameParam(withTestIdentity(httptest.NewRequest(http.MethodPatch, "/api/service-accounts/nfl-bot", body), auth.RoleOwner), "nfl-bot")
	rec := httptest.NewRecorder()
	handlePatchServiceAccount(store)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var view serviceAccountView
	if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if !view.Disabled {
		t.Error("Disabled = false, want true")
	}
	if view.Description != "orig" {
		t.Errorf("Description = %q, want unchanged %q", view.Description, "orig")
	}
}

func TestPatchServiceAccount_InvalidRoleIs400(t *testing.T) {
	store := newFakeServiceAccountStore()
	_ = store.Create(context.Background(), serviceaccount.Account{Workspace: "acme-analytics", Name: "nfl-bot", Role: auth.RoleEditor})

	body := strings.NewReader(`{"role":"owner"}`)
	req := withNameParam(withTestIdentity(httptest.NewRequest(http.MethodPatch, "/api/service-accounts/nfl-bot", body), auth.RoleOwner), "nfl-bot")
	rec := httptest.NewRecorder()
	handlePatchServiceAccount(store)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDeleteServiceAccount_RefusesUntilDisabled(t *testing.T) {
	store := newFakeServiceAccountStore()
	_ = store.Create(context.Background(), serviceaccount.Account{Workspace: "acme-analytics", Name: "nfl-bot", Role: auth.RoleEditor})

	req := withNameParam(withTestIdentity(httptest.NewRequest(http.MethodDelete, "/api/service-accounts/nfl-bot", nil), auth.RoleOwner), "nfl-bot")
	rec := httptest.NewRecorder()
	handleDeleteServiceAccount(store)(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete while enabled: status = %d, want 409", rec.Code)
	}

	patchBody := strings.NewReader(`{"disabled":true}`)
	patchReq := withNameParam(withTestIdentity(httptest.NewRequest(http.MethodPatch, "/api/service-accounts/nfl-bot", patchBody), auth.RoleOwner), "nfl-bot")
	handlePatchServiceAccount(store)(httptest.NewRecorder(), patchReq)

	req2 := withNameParam(withTestIdentity(httptest.NewRequest(http.MethodDelete, "/api/service-accounts/nfl-bot", nil), auth.RoleOwner), "nfl-bot")
	rec2 := httptest.NewRecorder()
	handleDeleteServiceAccount(store)(rec2, req2)
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("delete once disabled: status = %d, want 204", rec2.Code)
	}
}

func TestServiceAccountError_UnavailableIs503(t *testing.T) {
	store := newFakeServiceAccountStore()
	store.err = serviceaccount.ErrUnavailable

	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/service-accounts", nil), auth.RoleViewer)
	rec := httptest.NewRecorder()
	handleListServiceAccounts(store)(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// --- router-level: requireAdmin actually gates the mutating routes. -----------------------------

func TestRouter_ServiceAccountMutationsAreOwnerOnly(t *testing.T) {
	idp := newTestIDP(t)
	store := newFakeServiceAccountStore()
	router := routerWith(t, idp, func(d *Deps) { d.ServiceAccounts = store })

	editorToken := idp.token(t, "u-editor", map[string]any{"groups": []string{"/workspaces/acme/editor"}})
	ownerToken := idp.token(t, "u-owner", map[string]any{"groups": []string{"/workspaces/acme/owner"}})

	post := func(token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/service-accounts", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(auth.HeaderWorkspace, "acme")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	if rec := post(editorToken, `{"name":"nfl-bot","role":"editor"}`); rec.Code != http.StatusForbidden {
		t.Errorf("editor create: status = %d, want 403", rec.Code)
	}
	if rec := post(ownerToken, `{"name":"nfl-bot","role":"editor"}`); rec.Code != http.StatusCreated {
		t.Errorf("owner create: status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}

	// Listing the full detail is owner-only too (ADR 0119 amendment 3 point 5: it's the picker a
	// workspace owner uses to assign an account).
	list := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/service-accounts", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(auth.HeaderWorkspace, "acme")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := list(editorToken); rec.Code != http.StatusForbidden {
		t.Errorf("editor list: status = %d, want 403", rec.Code)
	}
	if rec := list(ownerToken); rec.Code != http.StatusOK {
		t.Errorf("owner list: status = %d, want 200", rec.Code)
	}

	// A single account's trimmed badge, by contrast, is open to any member.
	getReq := httptest.NewRequest(http.MethodGet, "/api/service-accounts/nfl-bot", nil)
	getReq.Header.Set("Authorization", "Bearer "+editorToken)
	getReq.Header.Set(auth.HeaderWorkspace, "acme")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, getReq)
	if rec.Code != http.StatusOK {
		t.Errorf("editor get: status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

// Deps.ServiceAccounts == nil (not yet wired, or a build that never sets it) leaves the routes
// unregistered entirely — a plain 404, the same "optional: nil disables the routes" convention
// every other optional Deps field already uses.
func TestRouter_ServiceAccountRoutesAreUnregisteredWhenNil(t *testing.T) {
	idp := newTestIDP(t)
	router := routerWith(t, idp, nil) // ServiceAccounts left nil
	token := idp.token(t, "u1", map[string]any{"groups": []string{"/workspaces/acme/owner"}})

	req := httptest.NewRequest(http.MethodGet, "/api/service-accounts", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(auth.HeaderWorkspace, "acme")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
