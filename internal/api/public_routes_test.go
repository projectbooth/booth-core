package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/registry"
)

// addPublicBackend registers module id in deps' registry, backed by a real HTTP server that
// records the last request's headers and path, and declares publicRoutes.pathPrefixes when
// prefixes is non-nil (nil means the module doesn't declare the field at all).
func addPublicBackend(t *testing.T, deps *Deps, id string, prefixes []string) (gotHeader *http.Header, gotPath *string) {
	t.Helper()
	gotHeader = &http.Header{}
	gotPath = new(string)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotHeader = r.Header.Clone()
		*gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	}))
	t.Cleanup(backend.Close)

	host, port, _ := net.SplitHostPort(backend.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	spec := boothv1alpha1.BoothModuleSpec{ID: id, ServiceRef: boothv1alpha1.ServiceReference{Name: host, Port: int32(n)}}
	if prefixes != nil {
		spec.PublicRoutes = &boothv1alpha1.PublicRoutesSpec{PathPrefixes: prefixes}
	}
	deps.Registry.Put(registry.Module{Spec: spec})
	return gotHeader, gotPath
}

// ADR 0101: the whole point — a caller with no platform login at all (no Authorization header,
// no X-Workspace) reaches a declared public route through the REAL router, proving no
// auth.Middleware runs on this path, not just that PublicHandler itself doesn't check identity.
func TestRouter_PublicRouteRequiresNoPlatformLogin(t *testing.T) {
	idp := newTestIDP(t)
	router := routerWith(t, idp, func(d *Deps) { addPublicBackend(t, d, "api", []string{"/v1/"}) })

	req := httptest.NewRequest(http.MethodGet, "/modules/api/public/v1/datasets/orders", nil)
	// Deliberately no Authorization header, no X-Booth-Workspace — the entire point of the test.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a public route must not require a platform login)", rec.Code)
	}
}

// ADR 0101 item 1: a module that never declared publicRoutes gets no public route — the request
// 404s at core, never reaching the module backend.
func TestRouter_ModuleWithoutPublicRoutesFieldGetsNoPublicRoute(t *testing.T) {
	idp := newTestIDP(t)
	var gotPath *string
	router := routerWith(t, idp, func(d *Deps) { _, gotPath = addPublicBackend(t, d, "api", nil) })

	req := httptest.NewRequest(http.MethodGet, "/modules/api/public/v1/anything", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if *gotPath != "" {
		t.Errorf("request reached the module backend at %q; a module with no publicRoutes field must never be routed to", *gotPath)
	}
}

// An undeclared path under /public/ (the module exists and declared OTHER prefixes) is also 404.
func TestRouter_UndeclaredPathUnderPublicIs404(t *testing.T) {
	idp := newTestIDP(t)
	var gotPath *string
	router := routerWith(t, idp, func(d *Deps) { _, gotPath = addPublicBackend(t, d, "api", []string{"/v1/"}) })

	req := httptest.NewRequest(http.MethodGet, "/modules/api/public/v2/anything", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if *gotPath != "" {
		t.Errorf("request reached the module backend at %q; an undeclared path must never be proxied", *gotPath)
	}
}

// ADR 0101 item 4, through the real router: trust headers stripped, Authorization passed through.
func TestRouter_PublicRouteStripsTrustHeaders(t *testing.T) {
	idp := newTestIDP(t)
	var gotHeader *http.Header
	router := routerWith(t, idp, func(d *Deps) { gotHeader, _ = addPublicBackend(t, d, "api", []string{"/v1/"}) })

	req := httptest.NewRequest(http.MethodGet, "/modules/api/public/v1/x", nil)
	req.Header.Set("Authorization", "Bearer booth_ak_live_abc123")
	req.Header.Set(auth.HeaderBoothWorkspace, "forged-workspace")
	req.Header.Set(auth.HeaderBoothRole, "owner")
	req.Header.Set(auth.HeaderBoothIdentity, "forged-assertion")

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, h := range []string{auth.HeaderBoothWorkspace, auth.HeaderBoothRole, auth.HeaderBoothIdentity} {
		if _, present := (*gotHeader)[http.CanonicalHeaderKey(h)]; present {
			t.Errorf("%s reached the module (value %v); must be stripped", h, gotHeader.Values(h))
		}
	}
	if got := gotHeader.Get("Authorization"); got != "Bearer booth_ak_live_abc123" {
		t.Errorf("Authorization = %q, want the caller's own value passed through unchanged", got)
	}
}

// ADR 0101 item 7: the ordinary /modules/{id}/* route is unchanged by this new route's presence
// — it still requires a real platform login, same as before this change.
func TestRouter_OrdinaryGatewayRouteStillRequiresAuth(t *testing.T) {
	idp := newTestIDP(t)
	router := routerWith(t, idp, func(d *Deps) { addPublicBackend(t, d, "api", []string{"/v1/"}) })

	// No Authorization header at all: the ordinary route must still 401.
	req := httptest.NewRequest(http.MethodGet, "/modules/api/v1/x", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (the ordinary route must still require a real platform login)", rec.Code)
	}

	// A valid token but no X-Workspace: still the existing 400 behaviour, unchanged.
	token := idp.token(t, "u1", map[string]any{"groups": []string{"/workspaces/acme/owner"}})
	rec = get(router, token, "/modules/api/v1/x", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (X-Workspace still required on the ordinary route)", rec.Code)
	}

	// A valid token with X-Workspace: still proxies, exactly as before.
	rec = get(router, token, "/modules/api/v1/x", "acme")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (the ordinary route still works end to end)", rec.Code)
	}
}
