package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/registry"
)

// publicHandlerFixture builds a Gateway with one module, "api", declaring publicRoutes'
// pathPrefixes (nil means the module doesn't declare the field at all), served by a real HTTP
// backend that records what actually reached it.
func publicHandlerFixture(t *testing.T, prefixes []string) (handler http.Handler, gotHeader *http.Header, gotPath *string) {
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

	spec := boothv1alpha1.BoothModuleSpec{ID: "api", ServiceRef: boothv1alpha1.ServiceReference{Name: backend.Listener.Addr().String()}}
	if prefixes != nil {
		spec.PublicRoutes = &boothv1alpha1.PublicRoutesSpec{PathPrefixes: prefixes}
	}
	lookup := fakeLookup{modules: map[string]registry.Module{"api": {Spec: spec}}}
	gw := New(lookup)

	handler = gw.PublicHandler(
		func(r *http.Request) string { return "api" },
		func(r *http.Request) string { return "/" + chiStarParam(r) },
	)
	return handler, gotHeader, gotPath
}

// chiStarParam stands in for chi.URLParam(r, "*") in these unit-level tests, which don't run
// through chi's router at all. Mirrors chi's own choice in mux.go's routeHTTP: RawPath when
// non-empty (set by net/url whenever the request's original encoding wouldn't round-trip through
// Path's own canonical re-escaping — e.g. any percent-encoded ".", "/", or "\", none of which
// strictly need encoding), falling back to the already-decoded-once Path otherwise. Getting this
// wrong here would let these tests pass for the wrong reason: PublicHandler must cope with
// whichever of the two chi actually hands it, not just with Path.
func chiStarParam(r *http.Request) string {
	raw := r.URL.Path
	if r.URL.RawPath != "" {
		raw = r.URL.RawPath
	}
	const prefix = "/modules/api/public/"
	if len(raw) > len(prefix) && raw[:len(prefix)] == prefix {
		return raw[len(prefix):]
	}
	return ""
}

// ADR 0101 item 1: a module without the field gets no public route at all — not merely an empty
// prefix list, but PublicRoutes nil entirely, which is the shape 99% of modules have today.
func TestPublicHandler_ModuleWithoutTheFieldGetsNoPublicRoute(t *testing.T) {
	handler, _, _ := publicHandlerFixture(t, nil) // PublicRoutes left nil

	req := httptest.NewRequest(http.MethodGet, "/modules/api/public/v1/anything", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (module declared no publicRoutes at all)", rec.Code)
	}
}

// ADR 0101 item 2: only a path matching a declared prefix is proxied; anything else under
// /public/ is a 404 from core, never reaching the module.
func TestPublicHandler_UndeclaredPathUnderPublicIs404(t *testing.T) {
	handler, _, gotPath := publicHandlerFixture(t, []string{"/v1/"})

	req := httptest.NewRequest(http.MethodGet, "/modules/api/public/v2/anything", nil) // /v2/, not /v1/
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (path doesn't match any declared prefix)", rec.Code)
	}
	if *gotPath != "" {
		t.Errorf("request reached the module backend at %q; an undeclared path must never be proxied", *gotPath)
	}
}

// The declared-prefix case actually works: a matching path is proxied through, unmodified.
func TestPublicHandler_DeclaredPrefixIsProxied(t *testing.T) {
	handler, _, gotPath := publicHandlerFixture(t, []string{"/v1/"})

	req := httptest.NewRequest(http.MethodGet, "/modules/api/public/v1/datasets/orders", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if *gotPath != "/v1/datasets/orders" {
		t.Errorf("forwarded path = %q, want /v1/datasets/orders (unmodified, prefix included)", *gotPath)
	}
}

// ADR 0101 item 4: X-Booth-Workspace/X-Booth-Role/X-Booth-Identity are stripped from the inbound
// request (even if a caller tried to forge them) and never set by core — nothing downstream can
// mistake a caller-supplied value for one core vouched for. Authorization and every other header
// pass through completely unchanged, since this path authenticates nothing itself.
func TestPublicHandler_StripsTrustHeadersAndLeavesOthersUntouched(t *testing.T) {
	handler, gotHeader, _ := publicHandlerFixture(t, []string{"/v1/"})

	req := httptest.NewRequest(http.MethodGet, "/modules/api/public/v1/datasets/orders", nil)
	req.Header.Set("Authorization", "Bearer booth_ak_live_abc123")
	req.Header.Set(auth.HeaderBoothWorkspace, "forged-workspace")
	req.Header.Set(auth.HeaderBoothRole, "owner")
	req.Header.Set(auth.HeaderBoothIdentity, "forged-assertion")
	req.Header.Set("X-Custom-Caller-Header", "untouched")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, h := range []string{auth.HeaderBoothWorkspace, auth.HeaderBoothRole, auth.HeaderBoothIdentity} {
		if _, present := (*gotHeader)[http.CanonicalHeaderKey(h)]; present {
			t.Errorf("%s reached the module (value %v); a public route must strip it and never set it", h, gotHeader.Values(h))
		}
	}
	if got := gotHeader.Get("Authorization"); got != "Bearer booth_ak_live_abc123" {
		t.Errorf("Authorization = %q, want the caller's own value passed through unchanged", got)
	}
	if got := gotHeader.Get("X-Custom-Caller-Header"); got != "untouched" {
		t.Errorf("X-Custom-Caller-Header = %q, want it passed through unchanged", got)
	}
}

// A traversal-shaped path must never reach the module, no matter how it's spelled — a prefix
// check against the raw, uncleaned path lets a caller escape the declared prefix entirely (the
// original bug: /v1/../admin passes a naive strings.HasPrefix(forwardPath, "/v1/") check and
// would have been forwarded as-is). Every variant here is rejected before ever reaching
// matchesPublicRoute.
func TestPublicHandler_RejectsTraversalShapedPaths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
	}{
		{"literal dot-dot segment", "/modules/api/public/v1/../admin"},
		{"lowercase percent-encoded dot-dot (decodes to a literal .. before this handler sees it)", "/modules/api/public/v1/%2e%2e/admin"},
		{"uppercase percent-encoded dot-dot", "/modules/api/public/v1/%2E%2E/admin"},
		{"percent-encoded slash splitting a dot-dot out of the final segment", "/modules/api/public/v1/..%2fadmin"},
		{"literal single-dot segment", "/modules/api/public/v1/./x"},
		{"double-encoded dot-dot (the literal %2e the net/http stack couldn't have produced itself)", "/modules/api/public/v1/%252e%252e/admin"},
		{"double-encoded slash", "/modules/api/public/v1/..%252fadmin"},
		{"backslash", "/modules/api/public/v1/..%5c..%5cadmin"},
		{"embedded NUL byte", "/modules/api/public/v1/%00/admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, _, gotPath := publicHandlerFixture(t, []string{"/v1/"})
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
			if *gotPath != "" {
				t.Errorf("request reached the module backend at %q; a traversal-shaped path must never be proxied", *gotPath)
			}
		})
	}
}

// A doubled slash is benign, not traversal-shaped — it must still match the declared prefix and
// forward in its canonical, cleaned form (not the raw doubled-slash form).
func TestPublicHandler_DoubledSlashIsCleanedAndStillMatches(t *testing.T) {
	handler, _, gotPath := publicHandlerFixture(t, []string{"/v1/"})

	req := httptest.NewRequest(http.MethodGet, "/modules/api/public/v1//x", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a doubled slash is not a traversal attempt)", rec.Code)
	}
	if *gotPath != "/v1/x" {
		t.Errorf("forwarded path = %q, want /v1/x (the cleaned form)", *gotPath)
	}
}

// A legitimate, ordinary multi-segment path still works end to end after the fix.
func TestPublicHandler_OrdinaryMultiSegmentPathStillWorks(t *testing.T) {
	handler, _, gotPath := publicHandlerFixture(t, []string{"/v1/"})

	req := httptest.NewRequest(http.MethodGet, "/modules/api/public/v1/x/y", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if *gotPath != "/v1/x/y" {
		t.Errorf("forwarded path = %q, want /v1/x/y", *gotPath)
	}
}

// A request for exactly the declared prefix itself (with its trailing slash) must still match
// after cleaning — path.Clean alone would drop that trailing slash and break this case.
func TestPublicHandler_ExactPrefixWithTrailingSlashStillMatches(t *testing.T) {
	handler, _, gotPath := publicHandlerFixture(t, []string{"/v1/"})

	req := httptest.NewRequest(http.MethodGet, "/modules/api/public/v1/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if *gotPath != "/v1/" {
		t.Errorf("forwarded path = %q, want /v1/ (trailing slash preserved)", *gotPath)
	}
}

// An unknown module ID under /public/ is also a plain 404, same as the ordinary route.
func TestPublicHandler_UnknownModuleReturns404(t *testing.T) {
	gw := New(fakeLookup{modules: map[string]registry.Module{}})
	handler := gw.PublicHandler(
		func(r *http.Request) string { return "nonexistent" },
		func(r *http.Request) string { return "/v1/x" },
	)

	req := httptest.NewRequest(http.MethodGet, "/modules/nonexistent/public/v1/x", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
