package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/directory"
)

func seededDirectory(t *testing.T) *directory.MemoryStore {
	t.Helper()
	s := directory.NewMemoryStore()
	for _, u := range []directory.User{
		{Sub: "sub-alice", PreferredUsername: "alice", Name: "Alice Anderson", Email: "alice@example.com", Workspaces: []string{"acme", "labs"}},
		{Sub: "sub-bob", PreferredUsername: "bob", Name: "Bob Baker", Email: "bob@example.com", Workspaces: []string{"acme"}},
		{Sub: "sub-dave", PreferredUsername: "dave", Name: "Dave", Email: "dave@labs.test", Workspaces: []string{"labs"}},
		{Sub: "auth0|x@y", PreferredUsername: "odd", Workspaces: []string{"acme"}},
	} {
		if err := s.Upsert(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// asWorkspace builds a request as a member of the given (single) workspace.
func asWorkspace(method, target, workspace string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	id := auth.Identity{
		Claims:      &auth.Claims{Subject: "caller"},
		Memberships: []auth.Membership{{Workspace: workspace, Role: auth.RoleViewer}},
		Active:      auth.Membership{Workspace: workspace, Role: auth.RoleViewer},
	}
	return req.WithContext(auth.WithIdentityForTesting(req.Context(), id))
}

func withSubParam(req *http.Request, rawSub string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("sub", rawSub)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestGetUser_ResolvesUserInCallersWorkspace(t *testing.T) {
	h := handleGetUser(seededDirectory(t))
	req := withSubParam(asWorkspace(http.MethodGet, "/api/users/sub-alice", "acme"), "sub-alice")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var v map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v["sub"] != "sub-alice" || v["preferredUsername"] != "alice" || v["name"] != "Alice Anderson" ||
		v["email"] != "alice@example.com" || v["displayName"] != "Alice Anderson" {
		t.Errorf("unexpected body: %v", v)
	}
	if _, leaked := v["workspaces"]; leaked {
		t.Error("response leaks the user's workspace memberships")
	}
}

// A user in a different workspace must look exactly like a user that doesn't exist, so the
// endpoint can't be used to probe which identities the deployment knows.
func TestGetUser_OtherWorkspaceIsIndistinguishableFromMissing(t *testing.T) {
	h := handleGetUser(seededDirectory(t))

	other := httptest.NewRecorder()
	h(other, withSubParam(asWorkspace(http.MethodGet, "/", "acme"), "sub-dave")) // dave is labs-only
	missing := httptest.NewRecorder()
	h(missing, withSubParam(asWorkspace(http.MethodGet, "/", "acme"), "sub-nobody"))

	if other.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
		t.Fatalf("codes = %d and %d, want 404 for both", other.Code, missing.Code)
	}
	if other.Body.String() != missing.Body.String() {
		t.Errorf("bodies differ (%q vs %q): distinguishable", other.Body.String(), missing.Body.String())
	}
}

// Some IdPs issue subs with reserved characters; chi hands them over still percent-encoded.
func TestGetUser_DecodesPercentEncodedSub(t *testing.T) {
	h := handleGetUser(seededDirectory(t))
	rec := httptest.NewRecorder()
	h(rec, withSubParam(asWorkspace(http.MethodGet, "/", "acme"), url.PathEscape("auth0|x@y")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an encoded sub", rec.Code)
	}
}

func TestGetUser_RejectsMalformedEncoding(t *testing.T) {
	h := handleGetUser(seededDirectory(t))
	rec := httptest.NewRecorder()
	h(rec, withSubParam(asWorkspace(http.MethodGet, "/", "acme"), "%zz"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func searchSubs(t *testing.T, target, workspace string) ([]string, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	handleSearchUsers(seededDirectory(t))(rec, asWorkspace(http.MethodGet, target, workspace))
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	var out []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	subs := make([]string, len(out))
	for i, u := range out {
		subs[i] = u["sub"].(string)
	}
	return subs, rec.Code
}

func TestSearchUsers_ScopedToCallersWorkspaceAndFiltered(t *testing.T) {
	got, code := searchSubs(t, "/api/users?q=bob", "acme")
	if code != 200 || len(got) != 1 || got[0] != "sub-bob" {
		t.Errorf("q=bob in acme = %v (%d), want [sub-bob]", got, code)
	}

	// dave exists, but only in labs.
	got, _ = searchSubs(t, "/api/users?q=dave", "acme")
	if len(got) != 0 {
		t.Errorf("q=dave in acme = %v, want none: dave isn't in that workspace", got)
	}

	// An empty q lists the whole workspace (a picker's initial state).
	got, _ = searchSubs(t, "/api/users", "acme")
	if len(got) != 3 {
		t.Errorf("empty q in acme = %v, want 3 users", got)
	}
}

func TestSearchUsers_ReturnsAnEmptyArrayNotNull(t *testing.T) {
	rec := httptest.NewRecorder()
	handleSearchUsers(seededDirectory(t))(rec, asWorkspace(http.MethodGet, "/api/users?q=zzzz", "acme"))
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("body = %q, want []", rec.Body.String())
	}
}

func TestSearchUsers_ValidatesParameters(t *testing.T) {
	for target, want := range map[string]int{
		"/api/users?limit=0":                             http.StatusBadRequest,
		"/api/users?limit=-3":                            http.StatusBadRequest,
		"/api/users?limit=abc":                           http.StatusBadRequest,
		"/api/users?q=" + strings.Repeat("x", 101):       http.StatusBadRequest,
		"/api/users?limit=1":                             http.StatusOK,
		"/api/users?limit=100000":                        http.StatusOK, // clamped, not rejected
		"/api/users?q=" + url.QueryEscape("' OR '1'='1"): http.StatusOK,
	} {
		if _, code := searchSubs(t, target, "acme"); code != want {
			t.Errorf("%s: status = %d, want %d", target, code, want)
		}
	}
	if got, _ := searchSubs(t, "/api/users?limit=1", "acme"); len(got) != 1 {
		t.Errorf("limit=1 returned %d results", len(got))
	}
}

// The directory is populated as a side effect of authentication itself (ADR 0047): after a
// user makes any authenticated call through the real router, they can be resolved by sub,
// and routes require X-Workspace like every other authenticated route.
func TestRouter_RecordsCallersFromVerifiedTokensAndServesLookups(t *testing.T) {
	idp := newTestIDP(t)
	store := directory.NewMemoryStore()
	router := routerWith(t, idp, func(d *Deps) {
		d.Directory = store
		d.DirectoryRecorder = directory.NewRecorder(store)
	})

	alice := idp.token(t, "sub-alice", map[string]any{
		"groups": []string{"/workspaces/acme/owner"}, "preferred_username": "alice",
		"name": "Alice Anderson", "email": "alice@example.com",
	})
	bob := idp.token(t, "sub-bob", map[string]any{
		"groups": []string{"/workspaces/acme/viewer"}, "preferred_username": "bob",
	})
	mallory := idp.token(t, "sub-mallory", map[string]any{
		"groups": []string{"/workspaces/elsewhere/owner"}, "preferred_username": "mallory",
	})

	// Nobody is in the directory until they authenticate. /api/me needs no workspace header.
	for _, tok := range []string{alice, bob, mallory} {
		if rec := get(router, tok, "/api/me", ""); rec.Code != http.StatusOK {
			t.Fatalf("/api/me: %d", rec.Code)
		}
	}

	// Bob (acme) can resolve Alice from her token's claims.
	rec := get(router, bob, "/api/users/sub-alice", "acme")
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var v map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&v)
	if v["preferredUsername"] != "alice" || v["email"] != "alice@example.com" {
		t.Errorf("recorded claims = %v", v)
	}

	// Searching from acme finds alice and bob but never mallory (a different tenant).
	rec = get(router, bob, "/api/users", "acme")
	var list []map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&list)
	if len(list) != 2 {
		t.Errorf("acme directory = %v, want alice and bob only", list)
	}

	// Mallory, in another workspace, can't resolve Alice — and can't claim acme either.
	if rec := get(router, mallory, "/api/users/sub-alice", "elsewhere"); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant lookup = %d, want 404", rec.Code)
	}
	if rec := get(router, mallory, "/api/users/sub-alice", "acme"); rec.Code != http.StatusForbidden {
		t.Errorf("claiming a workspace she isn't in = %d, want 403", rec.Code)
	}

	// Like every authenticated route except /api/me, the header is mandatory.
	if rec := get(router, bob, "/api/users", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("/api/users without X-Workspace = %d, want 400", rec.Code)
	}
}

func TestRouter_UserRoutesAbsentWithoutADirectory(t *testing.T) {
	idp := newTestIDP(t)
	router := routerWith(t, idp, nil)
	tok := idp.token(t, "u", map[string]any{"groups": []string{"/workspaces/acme/owner"}})
	if rec := get(router, tok, "/api/users", "acme"); rec.Code == http.StatusOK {
		t.Errorf("/api/users served (%d) with no directory configured", rec.Code)
	}
}
