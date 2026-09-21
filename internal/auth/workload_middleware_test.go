package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
)

// stubWorkload is a WorkloadVerifier that accepts exactly one token string and counts every
// token it is asked about, so a test can tell whether dispatch consulted it at all.
type stubWorkload struct {
	issuer string
	accept string
	groups []string
	calls  int
}

func (s *stubWorkload) Issuer() string { return s.issuer }

func (s *stubWorkload) Verify(_ context.Context, raw string) (*Claims, error) {
	s.calls++
	if raw != s.accept {
		return nil, errors.New("not mine")
	}
	return &Claims{Subject: "job:1", Groups: s.groups}, nil
}

// jwtLike builds a three-part string whose payload claims iss — enough to be dispatched on.
func jwtLike(iss string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"RS256"}`) + "." + enc(`{"iss":"`+iss+`"}`) + ".c2ln"
}

const coreIssuer = "http://core.svc:8080"

// Dispatch is by `iss` and exclusive: a token is judged by one verifier only, never retried
// under the other's rules.
func TestMiddleware_WorkloadDispatchIsByIssuerAndExclusive(t *testing.T) {
	provider, holder := newReadyMiddlewareFixture(t)
	human := provider.issueToken(t, map[string]any{"groups": []string{"/workspaces/acme/owner"}})

	good := jwtLike(coreIssuer)
	wl := &stubWorkload{issuer: coreIssuer, accept: good, groups: []string{"/workspaces/acme/viewer"}}
	opt := WithWorkloadVerifier(wl)

	t.Run("a token issued by core goes to the workload verifier and gets its role", func(t *testing.T) {
		code, id := serve(t, holder, good, "acme", opt)
		if code != http.StatusOK || id == nil || id.Active.Role != RoleViewer || id.Claims.Subject != "job:1" {
			t.Fatalf("status %d identity %+v; want 200 as job:1 viewer", code, id)
		}
	})

	t.Run("a human token never reaches the workload verifier", func(t *testing.T) {
		wl.calls = 0
		code, id := serve(t, holder, human, "acme", opt)
		if code != http.StatusOK || id == nil || id.Active.Role != RoleOwner {
			t.Fatalf("status %d identity %+v; want 200 as owner", code, id)
		}
		if wl.calls != 0 {
			t.Errorf("workload verifier was consulted for an OIDC token (%d calls)", wl.calls)
		}
	})

	t.Run("a token claiming core's issuer that the workload verifier rejects is refused, not retried as OIDC", func(t *testing.T) {
		if code, _ := serve(t, holder, jwtLike(coreIssuer)+"x", "acme", opt); code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", code)
		}
	})

	t.Run("a valid IdP token relabelled as core's issuer is refused", func(t *testing.T) {
		// Same signature and everything; only the payload's iss differs, which is enough to steer
		// it to the workload verifier, which rejects it. It is never re-tried as an OIDC token.
		forged := provider.issueToken(t, map[string]any{"iss": coreIssuer, "groups": []string{"/workspaces/acme/owner"}})
		if code, _ := serve(t, holder, forged, "acme", opt); code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", code)
		}
	})

	t.Run("garbage and unknown issuers fall to OIDC and are refused", func(t *testing.T) {
		wl.calls = 0
		for name, tok := range map[string]string{
			"not a jwt":         "abc",
			"another issuer":    jwtLike("https://evil.example"),
			"no issuer":         jwtLike(""),
			"two segments only": "a.b",
		} {
			if code, _ := serve(t, holder, tok, "acme", opt); code != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401", name, code)
			}
		}
		if wl.calls != 0 {
			t.Errorf("workload verifier consulted for non-core issuers (%d calls)", wl.calls)
		}
	})
}

// Without the option — every route except the gateway — a workload token is just an unknown
// token, and the workload verifier is never consulted.
func TestMiddleware_WithoutTheOptionWorkloadTokensAreRefused(t *testing.T) {
	_, holder := newReadyMiddlewareFixture(t)
	good := jwtLike(coreIssuer)
	if code, _ := serve(t, holder, good, "acme"); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
}

// A run's token verifies against core's own key, so an unreachable IdP must not take it down;
// a human's token still gets the 503 it always did.
func TestMiddleware_WorkloadTokensDoNotDependOnTheOIDCProviderBeingReady(t *testing.T) {
	good := jwtLike(coreIssuer)
	wl := &stubWorkload{issuer: coreIssuer, accept: good, groups: []string{"/workspaces/acme/editor"}}
	notReady := &VerifierHolder{}

	if code, id := serve(t, notReady, good, "acme", WithWorkloadVerifier(wl)); code != http.StatusOK || id.Active.Role != RoleEditor {
		t.Fatalf("workload token with OIDC down: status %d, identity %+v; want 200 editor", code, id)
	}
	if code, _ := serve(t, notReady, jwtLike("https://idp.example"), "acme", WithWorkloadVerifier(wl)); code != http.StatusServiceUnavailable {
		t.Fatalf("human token with OIDC down: status = %d, want 503", code)
	}
}

// Workload tokens name runs, not people, so they must never be recorded in the user directory.
func TestMiddleware_WorkloadTokensAreNotShownToTheClaimsObserver(t *testing.T) {
	provider, holder := newReadyMiddlewareFixture(t)
	good := jwtLike(coreIssuer)
	wl := &stubWorkload{issuer: coreIssuer, accept: good, groups: []string{"/workspaces/acme/viewer"}}

	var observed []string
	obs := WithClaimsObserver(func(_ context.Context, c *Claims, _ []Membership) { observed = append(observed, c.Subject) })

	serve(t, holder, good, "acme", WithWorkloadVerifier(wl), obs)
	if len(observed) != 0 {
		t.Fatalf("observer saw a workload token: %v", observed)
	}
	serve(t, holder, provider.issueToken(t, map[string]any{"sub": "alice", "groups": []string{"/workspaces/acme/owner"}}), "acme", WithWorkloadVerifier(wl), obs)
	if len(observed) != 1 || observed[0] != "alice" {
		t.Fatalf("observer should still see human tokens: %v", observed)
	}
}

func TestUnverifiedIssuer(t *testing.T) {
	for tok, want := range map[string]string{
		jwtLike("http://x"): "http://x",
		"":                  "",
		"a.b.c":             "",
		"a.!!!.c":           "",
		"a." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":42}`)) + ".c": "",
	} {
		if got := unverifiedIssuer(tok); got != want {
			t.Errorf("unverifiedIssuer(%q) = %q, want %q", tok, got, want)
		}
	}
}
