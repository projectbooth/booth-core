package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

	issuer := NewIframeURLIssuer(tokens)
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

// --- Bug 2 (ADR 0069's "Implementation notes"): the iframe URL is relative, not built from a
// --- deployment-specific base URL that no chart value ever set. ------------------------------

func TestIframeURLIssuer_URLForIsRelative(t *testing.T) {
	issuer := NewIframeURLIssuer(NewIframeTokenIssuer([]byte("test-secret")))
	got, err := issuer.URLFor("superset", auth.Identity{
		Claims: &auth.Claims{Subject: "user-1"},
		Active: auth.Membership{Workspace: "acme-analytics", Role: auth.RoleOwner},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "/iframe/superset/?") {
		t.Fatalf("URLFor = %q, want a path starting with /iframe/superset/? (relative — no scheme/host, so it resolves against whatever origin the browser is already on)", got)
	}
	if strings.Contains(got, "localhost") || strings.Contains(got, "://") {
		t.Errorf("URLFor = %q, want no scheme/host at all", got)
	}
}

// ADR 0094's correction (2026-09-30/2026-10-07): URLFor derives IsOperator from the caller's real
// token's groups, and it must survive the Issue -> Verify round trip the navigation and cookie
// tokens both go through, since that's the only way it reaches proxyIframeRequest at all.
func TestIframeURLIssuer_URLForDerivesIsOperatorFromGroups(t *testing.T) {
	tokens := NewIframeTokenIssuer([]byte("test-secret"))
	issuer := NewIframeURLIssuer(tokens)

	for _, tc := range []struct {
		name   string
		groups []string
		want   bool
	}{
		{"carries the operator claim", []string{"/workspaces/acme-analytics/owner", "/platform/operator"}, true},
		{"does not carry it", []string{"/workspaces/acme-analytics/owner"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iframeURL, err := issuer.URLFor("superset", auth.Identity{
				Claims: &auth.Claims{Subject: "user-1", Groups: tc.groups},
				Active: auth.Membership{Workspace: "acme-analytics", Role: auth.RoleOwner},
			})
			if err != nil {
				t.Fatalf("URLFor: %v", err)
			}
			parsedURL, err := url.Parse(iframeURL)
			if err != nil {
				t.Fatal(err)
			}
			claims, err := tokens.Verify(parsedURL.Query().Get(iframeQueryParam))
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if claims.IsOperator != tc.want {
				t.Errorf("IsOperator = %v, want %v", claims.IsOperator, tc.want)
			}
		})
	}
}

// Optional small addition: URLFor derives PreferredUsername from the caller's real token, and it
// must survive the Issue -> Verify round trip — specifically including a value containing "|",
// the one character this token's pipe-delimited format (see encodeClaims' doc comment) can't
// otherwise tolerate. This is the regression test for the percent-encoding fix: mutation-tested
// by temporarily removing it, which confirmed the real failure mode is Verify returning "wrong
// number of claim fields" (the raw pipe splits the username into an extra field) — a legitimate
// user whose IdP happens to set a username containing "|" would be denied the iframe-proxy
// session entirely, not silently misrouted.
func TestIframeURLIssuer_URLForPreferredUsernameSurvivesRoundTripIncludingPipeCharacter(t *testing.T) {
	tokens := NewIframeTokenIssuer([]byte("test-secret"))
	issuer := NewIframeURLIssuer(tokens)

	for _, tc := range []struct {
		name     string
		username string
	}{
		{"ordinary username", "alice"},
		{"empty (the common case)", ""},
		{"contains a literal pipe", "alice|injected-field"},
		{"contains other special characters", "alice bob/éx&=?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iframeURL, err := issuer.URLFor("superset", auth.Identity{
				Claims: &auth.Claims{Subject: "user-1", PreferredUsername: tc.username},
				Active: auth.Membership{Workspace: "acme-analytics", Role: auth.RoleOwner},
			})
			if err != nil {
				t.Fatalf("URLFor: %v", err)
			}
			parsedURL, err := url.Parse(iframeURL)
			if err != nil {
				t.Fatal(err)
			}
			claims, err := tokens.Verify(parsedURL.Query().Get(iframeQueryParam))
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if claims.PreferredUsername != tc.username {
				t.Errorf("PreferredUsername = %q, want %q", claims.PreferredUsername, tc.username)
			}
			// Confirms the whole token still decoded successfully (an unescaped pipe would have
			// made Verify fail outright — see this test's doc comment), not just that
			// PreferredUsername itself came back right.
			if claims.Role != "owner" {
				t.Errorf("Role = %q, want owner", claims.Role)
			}
		})
	}
}

// --- Bug 1 (ADR 0069's "Implementation notes"): the fallback must not proxy a top-level
// --- document navigation into whatever module happens to have a live session cookie. ----------

func TestIframeFallback_RefusesATopLevelDocumentNavigation(t *testing.T) {
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
	sessionToken, err := tokens.Issue(IframeClaims{ModuleID: "superset", Workspace: "acme-analytics", Role: "owner", Subject: "user-1"}, CookieTTL)
	if err != nil {
		t.Fatal(err)
	}
	fallback := gw.IframeFallbackHandler(tokens)

	newReq := func(secFetchDest string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/storage", nil)
		req.AddCookie(&http.Cookie{Name: IframeCookieName, Value: sessionToken})
		if secFetchDest != "" {
			req.Header.Set("Sec-Fetch-Dest", secFetchDest)
		}
		return req
	}

	// The exact hijack the ADR describes: a person leaves the embedded module (whose session
	// cookie is still live, kept alive by the shell's renewal) and asks for an unrelated shell
	// route by a top-level navigation. Must not silently proxy into the module.
	t.Run("document navigation is refused, not proxied", func(t *testing.T) {
		gotPath = ""
		rec := httptest.NewRecorder()
		fallback.ServeHTTP(rec, newReq("document"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (same as no session cookie at all)", rec.Code)
		}
		if gotPath != "" {
			t.Errorf("request reached the module backend at %q; a document navigation must never be proxied", gotPath)
		}
	})

	// The module's own follow-up calls (Sec-Fetch-Dest: empty for fetch/XHR, iframe for a nested
	// navigation within the embed itself) are the entire reason this handler exists — must keep
	// working exactly as before.
	for _, dest := range []string{"empty", "iframe", ""} {
		t.Run("dest="+dest+" still proxies", func(t *testing.T) {
			gotPath = ""
			rec := httptest.NewRecorder()
			fallback.ServeHTTP(rec, newReq(dest))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if gotPath != "/storage" {
				t.Errorf("forwarded path = %q, want /storage (unmodified)", gotPath)
			}
		})
	}
}
