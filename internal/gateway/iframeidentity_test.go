package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/registry"
)

// stubIdentityMinter records every Mint call and returns a token derived from its inputs, so a
// test can tell exactly what reached it without decoding a real JWT.
type stubIdentityMinter struct {
	calls []struct {
		moduleID, workspace, role, subject, preferredUsername string
		isOperator                                            bool
	}
	err error
}

func (s *stubIdentityMinter) Mint(moduleID, workspace, role, subject string, isOperator bool, preferredUsername string) (string, error) {
	s.calls = append(s.calls, struct {
		moduleID, workspace, role, subject, preferredUsername string
		isOperator                                            bool
	}{moduleID, workspace, role, subject, preferredUsername, isOperator})
	if s.err != nil {
		return "", s.err
	}
	suffix := ""
	if isOperator {
		suffix = ":operator"
	}
	if preferredUsername != "" {
		suffix += ":" + preferredUsername
	}
	return fmt.Sprintf("assertion:%s:%s:%s:%s%s", moduleID, workspace, role, subject, suffix), nil
}

func newIdentityFixture(t *testing.T, minter IframeIdentityMinter) (gw *Gateway, tokens *IframeTokenIssuer, gotHeader *http.Header) {
	t.Helper()
	gotHeader = &http.Header{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotHeader = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	lookup := fakeLookup{modules: map[string]registry.Module{
		"superset": {Spec: boothv1alpha1.BoothModuleSpec{ID: "superset", ServiceRef: boothv1alpha1.ServiceReference{Name: backend.Listener.Addr().String()}}},
	}}
	gw = New(lookup)
	gw.IframeIdentity = minter
	tokens = NewIframeTokenIssuer([]byte("test-secret"))
	return gw, tokens, gotHeader
}

func sessionRequest(t *testing.T, tokens *IframeTokenIssuer, claims IframeClaims, extraHeaders map[string]string) *http.Request {
	t.Helper()
	sessionToken, err := tokens.Issue(claims, CookieTTL)
	if err != nil {
		t.Fatalf("issuing session token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/iframe/"+claims.ModuleID+"/x", nil)
	req.AddCookie(&http.Cookie{Name: IframeCookieName, Value: sessionToken})
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	return req
}

// ADR 0069: the module receives a signed identity assertion, minted with the caller's real
// workspace/role/subject, through both entry points that reach proxyIframeRequest.
func TestIframeProxy_AttachesTheSignedIdentityAssertion(t *testing.T) {
	claims := IframeClaims{ModuleID: "superset", Workspace: "acme-analytics", Role: "editor", Subject: "u-alice"}
	want := "assertion:superset:acme-analytics:editor:u-alice"

	t.Run("via IframeEntryHandler (the /iframe/{id}/* path)", func(t *testing.T) {
		minter := &stubIdentityMinter{}
		gw, tokens, gotHeader := newIdentityFixture(t, minter)
		entry := gw.IframeEntryHandler(tokens, func(r *http.Request) string { return "superset" }, func(r *http.Request) string { return "/x" })

		rec := httptest.NewRecorder()
		entry.ServeHTTP(rec, sessionRequest(t, tokens, claims, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := gotHeader.Get(auth.HeaderBoothIdentity); got != want {
			t.Errorf("%s = %q, want %q", auth.HeaderBoothIdentity, got, want)
		}
		if len(minter.calls) != 1 || minter.calls[0].moduleID != "superset" || minter.calls[0].workspace != "acme-analytics" ||
			minter.calls[0].role != "editor" || minter.calls[0].subject != "u-alice" {
			t.Errorf("Mint called with %+v", minter.calls)
		}
	})

	t.Run("via IframeFallbackHandler (a third-party UI's root-relative follow-up call)", func(t *testing.T) {
		minter := &stubIdentityMinter{}
		gw, tokens, gotHeader := newIdentityFixture(t, minter)
		fallback := gw.IframeFallbackHandler(tokens)

		rec := httptest.NewRecorder()
		fallback.ServeHTTP(rec, sessionRequest(t, tokens, claims, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := gotHeader.Get(auth.HeaderBoothIdentity); got != want {
			t.Errorf("%s = %q, want %q", auth.HeaderBoothIdentity, got, want)
		}
	})
}

// Optional small addition: a caller's preferred_username reaches Mint through the iframe-proxy
// path the same way IsOperator does, so a module (e.g. booth-streamlit) can show something more
// readable than the raw subject. Empty (the common case) passes through unchanged.
func TestIframeProxy_CarriesPreferredUsernameThroughToMint(t *testing.T) {
	t.Run("non-empty", func(t *testing.T) {
		claims := IframeClaims{ModuleID: "superset", Workspace: "acme-analytics", Role: "editor", Subject: "u-alice", PreferredUsername: "alice"}
		minter := &stubIdentityMinter{}
		gw, tokens, _ := newIdentityFixture(t, minter)
		entry := gw.IframeEntryHandler(tokens, func(r *http.Request) string { return "superset" }, func(r *http.Request) string { return "/x" })

		rec := httptest.NewRecorder()
		entry.ServeHTTP(rec, sessionRequest(t, tokens, claims, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if len(minter.calls) != 1 || minter.calls[0].preferredUsername != "alice" {
			t.Fatalf("Mint called with %+v, want preferredUsername=alice", minter.calls)
		}
	})

	t.Run("empty", func(t *testing.T) {
		claims := IframeClaims{ModuleID: "superset", Workspace: "acme-analytics", Role: "viewer", Subject: "u-bob"}
		minter := &stubIdentityMinter{}
		gw, tokens, _ := newIdentityFixture(t, minter)
		entry := gw.IframeEntryHandler(tokens, func(r *http.Request) string { return "superset" }, func(r *http.Request) string { return "/x" })

		rec := httptest.NewRecorder()
		entry.ServeHTTP(rec, sessionRequest(t, tokens, claims, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if len(minter.calls) != 1 || minter.calls[0].preferredUsername != "" {
			t.Fatalf("Mint called with %+v, want preferredUsername=\"\"", minter.calls)
		}
	})
}

// ADR 0094's correction (2026-09-30/2026-10-07): a caller carrying the platform-operator claim in
// their real token must still carry it through the iframe-proxy path, since an iframe-proxied
// module never sees that real token at all — only the minted X-Booth-Identity assertion.
func TestIframeProxy_CarriesThePlatformOperatorClaimThroughToTheMintedIdentity(t *testing.T) {
	t.Run("IsOperator true reaches Mint and the minted assertion", func(t *testing.T) {
		claims := IframeClaims{ModuleID: "superset", Workspace: "acme-analytics", Role: "owner", Subject: "u-alice", IsOperator: true}
		minter := &stubIdentityMinter{}
		gw, tokens, gotHeader := newIdentityFixture(t, minter)
		entry := gw.IframeEntryHandler(tokens, func(r *http.Request) string { return "superset" }, func(r *http.Request) string { return "/x" })

		rec := httptest.NewRecorder()
		entry.ServeHTTP(rec, sessionRequest(t, tokens, claims, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if len(minter.calls) != 1 || !minter.calls[0].isOperator {
			t.Fatalf("Mint called with %+v, want isOperator=true", minter.calls)
		}
		want := "assertion:superset:acme-analytics:owner:u-alice:operator"
		if got := gotHeader.Get(auth.HeaderBoothIdentity); got != want {
			t.Errorf("%s = %q, want %q (carrying the operator claim)", auth.HeaderBoothIdentity, got, want)
		}
	})

	t.Run("IsOperator false (the default) does not", func(t *testing.T) {
		claims := IframeClaims{ModuleID: "superset", Workspace: "acme-analytics", Role: "viewer", Subject: "u-bob"}
		minter := &stubIdentityMinter{}
		gw, tokens, gotHeader := newIdentityFixture(t, minter)
		entry := gw.IframeEntryHandler(tokens, func(r *http.Request) string { return "superset" }, func(r *http.Request) string { return "/x" })

		rec := httptest.NewRecorder()
		entry.ServeHTTP(rec, sessionRequest(t, tokens, claims, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if len(minter.calls) != 1 || minter.calls[0].isOperator {
			t.Fatalf("Mint called with %+v, want isOperator=false", minter.calls)
		}
		want := "assertion:superset:acme-analytics:viewer:u-bob"
		if got := gotHeader.Get(auth.HeaderBoothIdentity); got != want {
			t.Errorf("%s = %q, want %q (no operator claim)", auth.HeaderBoothIdentity, got, want)
		}
	})
}

// A caller-supplied X-Booth-Identity (the embedded page's own JS trying to smuggle one in) never
// survives — core's own, freshly minted assertion always replaces it.
func TestIframeProxy_StripsAnyClientSuppliedIdentityHeader(t *testing.T) {
	claims := IframeClaims{ModuleID: "superset", Workspace: "acme-analytics", Role: "viewer", Subject: "u-alice"}
	minter := &stubIdentityMinter{}
	gw, tokens, gotHeader := newIdentityFixture(t, minter)
	entry := gw.IframeEntryHandler(tokens, func(r *http.Request) string { return "superset" }, func(r *http.Request) string { return "/x" })

	req := sessionRequest(t, tokens, claims, map[string]string{auth.HeaderBoothIdentity: "forged-assertion-claiming-owner"})
	rec := httptest.NewRecorder()
	entry.ServeHTTP(rec, req)

	want := "assertion:superset:acme-analytics:viewer:u-alice"
	if got := gotHeader.Get(auth.HeaderBoothIdentity); got != want {
		t.Errorf("%s = %q, want the minted assertion %q (forged header must not survive)", auth.HeaderBoothIdentity, got, want)
	}
	if len(gotHeader.Values(auth.HeaderBoothIdentity)) != 1 {
		t.Errorf("%s appears %d times, want exactly one value", auth.HeaderBoothIdentity, len(gotHeader.Values(auth.HeaderBoothIdentity)))
	}
}

// Pre-ADR-0069 behavior is preserved verbatim when no issuer is configured: no header at all,
// not an empty one — a module gates on the header's presence, and an empty string is not "absent".
func TestIframeProxy_NoIssuerConfiguredMeansNoIdentityHeaderAtAll(t *testing.T) {
	claims := IframeClaims{ModuleID: "superset", Workspace: "acme-analytics", Role: "owner", Subject: "u-alice"}
	gw, tokens, gotHeader := newIdentityFixture(t, nil) // Gateway.IframeIdentity left nil
	entry := gw.IframeEntryHandler(tokens, func(r *http.Request) string { return "superset" }, func(r *http.Request) string { return "/x" })

	rec := httptest.NewRecorder()
	entry.ServeHTTP(rec, sessionRequest(t, tokens, claims, map[string]string{auth.HeaderBoothIdentity: "should-be-stripped-anyway"}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if _, present := (*gotHeader)[http.CanonicalHeaderKey(auth.HeaderBoothIdentity)]; present {
		t.Errorf("%s present although no iframe-identity issuer is configured: %v", auth.HeaderBoothIdentity, gotHeader.Values(auth.HeaderBoothIdentity))
	}
}

// A minting failure degrades to no header rather than blocking the request or forwarding
// anything stale/forged — the module's own re-verification requirement (ADR 0041) is the backstop.
func TestIframeProxy_MintingFailureOmitsHeaderButStillProxies(t *testing.T) {
	claims := IframeClaims{ModuleID: "superset", Workspace: "acme-analytics", Role: "owner", Subject: "u-alice"}
	minter := &stubIdentityMinter{err: errors.New("signing key unavailable")}
	gw, tokens, gotHeader := newIdentityFixture(t, minter)
	entry := gw.IframeEntryHandler(tokens, func(r *http.Request) string { return "superset" }, func(r *http.Request) string { return "/x" })

	rec := httptest.NewRecorder()
	entry.ServeHTTP(rec, sessionRequest(t, tokens, claims, map[string]string{auth.HeaderBoothIdentity: "should-be-stripped"}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the request to still succeed", rec.Code)
	}
	if _, present := (*gotHeader)[http.CanonicalHeaderKey(auth.HeaderBoothIdentity)]; present {
		t.Errorf("%s present despite a minting failure: %v", auth.HeaderBoothIdentity, gotHeader.Values(auth.HeaderBoothIdentity))
	}
}
