package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/registry"
)

func TestIframeFlow_NavigationThenCookieProxiesToModule(t *testing.T) {
	var gotPath, gotWorkspace, gotRole string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotWorkspace = r.Header.Get(auth.HeaderBoothWorkspace)
		gotRole = r.Header.Get(auth.HeaderBoothRole)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	lookup := fakeLookup{modules: map[string]registry.Module{
		"superset": {Spec: boothv1alpha1.BoothModuleSpec{ID: "superset", ServiceRef: boothv1alpha1.ServiceReference{Name: backend.Listener.Addr().String()}}},
	}}
	gw := New(lookup)
	tokens := NewIframeTokenIssuer([]byte("test-secret"))

	issuer := NewIframeURLIssuer(tokens, "https://booth.example.com")
	iframeURL, err := issuer.URLFor("superset", auth.Identity{
		Claims: &auth.Claims{Subject: "user-1"},
		Active: auth.Membership{Workspace: "acme-analytics", Role: auth.RoleOwner},
	})
	if err != nil {
		t.Fatalf("URLFor: %v", err)
	}

	entry := gw.IframeEntryHandler(tokens,
		func(r *http.Request) string { return "superset" },
		func(r *http.Request) string { return "/dashboard/1" },
	)

	// First hit: navigation token in the URL. Expect a redirect + cookie set.
	parsedURL, err := url.Parse(iframeURL)
	if err != nil {
		t.Fatalf("parsing iframe URL: %v", err)
	}
	req1 := httptest.NewRequest(http.MethodGet, parsedURL.RequestURI(), nil)
	rec1 := httptest.NewRecorder()
	entry.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusFound {
		t.Fatalf("first hit status = %d, want 302", rec1.Code)
	}
	cookies := rec1.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != IframeCookieName {
		t.Fatalf("expected %s cookie to be set, got %v", IframeCookieName, cookies)
	}

	// Second hit: cookie present, no query param. Expect a real proxy to the module.
	req2 := httptest.NewRequest(http.MethodGet, "/iframe/superset/dashboard/1", nil)
	req2.AddCookie(cookies[0])
	rec2 := httptest.NewRecorder()
	entry.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("second hit status = %d, want 200", rec2.Code)
	}
	if gotPath != "/dashboard/1" {
		t.Errorf("forwarded path = %q, want /dashboard/1", gotPath)
	}
	if gotWorkspace != "acme-analytics" || gotRole != "owner" {
		t.Errorf("workspace/role = %q/%q, want acme-analytics/owner", gotWorkspace, gotRole)
	}
}

// TestIframeFallback_CatchesRootRelativeFollowUpCalls exercises the exact failure mode
// ui-integration.md calls out: a third-party UI's own follow-up API call issued as a
// root-relative path (e.g. Superset calling fetch("/api/v1/chart")) resolves against
// core's origin root, not the /iframe/{id}/ mount path, and would 404 against a
// prefix-only proxy. The session cookie (Path=/) is what lets the fallback handler
// still route it correctly.
func TestIframeFallback_CatchesRootRelativeFollowUpCalls(t *testing.T) {
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	lookup := fakeLookup{modules: map[string]registry.Module{
		"superset": {Spec: boothv1alpha1.BoothModuleSpec{ID: "superset", ServiceRef: boothv1alpha1.ServiceReference{Name: backend.Listener.Addr().String()}}},
	}}
	gw := New(lookup)
	tokens := NewIframeTokenIssuer([]byte("test-secret"))

	sessionToken, err := tokens.Issue(IframeClaims{
		ModuleID:  "superset",
		Workspace: "acme-analytics",
		Role:      "owner",
		Subject:   "user-1",
	}, CookieTTL)
	if err != nil {
		t.Fatalf("issuing session token: %v", err)
	}

	fallback := gw.IframeFallbackHandler(tokens)

	// A root-relative follow-up call a real third-party UI would issue, hitting
	// core's root rather than /iframe/superset/... .
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chart", nil)
	req.AddCookie(&http.Cookie{Name: IframeCookieName, Value: sessionToken})

	rec := httptest.NewRecorder()
	fallback.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotPath != "/api/v1/chart" {
		t.Errorf("forwarded path = %q, want /api/v1/chart (unmodified)", gotPath)
	}
}

func TestIframeFallback_NoCookieReturns404(t *testing.T) {
	gw := New(fakeLookup{})
	tokens := NewIframeTokenIssuer([]byte("test-secret"))
	fallback := gw.IframeFallbackHandler(tokens)

	req := httptest.NewRequest(http.MethodGet, "/some/random/path", nil)
	rec := httptest.NewRecorder()
	fallback.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
