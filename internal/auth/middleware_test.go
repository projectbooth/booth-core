package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/projectbooth/booth-core/internal/config"
)

// TestMiddleware_ReturnsServiceUnavailableWhenVerifierNotReady guards the fix for
// booth-core crash-looping when its configured OIDC provider isn't reachable yet at
// boot: auth-gated routes should degrade to 503 rather than the whole process going
// down, since nothing guarantees IdP-before-core startup ordering in a real cluster.
func TestMiddleware_ReturnsServiceUnavailableWhenVerifierNotReady(t *testing.T) {
	holder := &VerifierHolder{} // never Store()'d — simulates "not ready yet"

	handler := Middleware(holder)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("wrapped handler should not run when verifier isn't ready")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	req.Header.Set(HeaderWorkspace, "acme-analytics")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestMiddleware_WorksOnceVerifierIsStored confirms the holder indirection doesn't
// change normal-path behavior once a verifier becomes ready.
func TestMiddleware_WorksOnceVerifierIsStored(t *testing.T) {
	provider := newTestOIDCProvider(t)
	verifier, err := NewVerifier(context.Background(), config.OIDCConfig{
		IssuerURL:   provider.server.URL,
		ClientID:    "test-client",
		GroupsClaim: "groups",
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	holder := &VerifierHolder{}
	holder.Store(verifier)

	called := false
	handler := Middleware(holder)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	token := provider.issueToken(t, map[string]any{
		"groups": []string{"/workspaces/acme-analytics/owner"},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(HeaderWorkspace, "acme-analytics")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !called {
		t.Fatal("expected wrapped handler to run once verifier is ready")
	}
}

func TestVerifierHolder_GetBeforeStore(t *testing.T) {
	holder := &VerifierHolder{}
	if _, ok := holder.Get(); ok {
		t.Fatal("expected ok=false before Store is called")
	}
}

// newReadyMiddlewareFixture returns a token issuer and a holder with a live verifier.
func newReadyMiddlewareFixture(t *testing.T) (*testOIDCProvider, *VerifierHolder) {
	t.Helper()
	provider := newTestOIDCProvider(t)
	verifier, err := NewVerifier(context.Background(), config.OIDCConfig{
		IssuerURL: provider.server.URL, ClientID: "test-client", GroupsClaim: "groups",
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	holder := &VerifierHolder{}
	holder.Store(verifier)
	return provider, holder
}

// serve runs one GET through Middleware(holder, opts...) and reports the status plus the
// Identity the wrapped handler saw (nil if it never ran).
func serve(t *testing.T, holder *VerifierHolder, token, workspace string, opts ...Option) (int, *Identity) {
	t.Helper()
	var seen *Identity
	h := Middleware(holder, opts...)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := FromContext(r.Context())
		seen = &id
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if workspace != "" {
		req.Header.Set(HeaderWorkspace, workspace)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, seen
}

func TestMiddleware_DefaultRequiresWorkspaceHeader(t *testing.T) {
	provider, holder := newReadyMiddlewareFixture(t)
	token := provider.issueToken(t, map[string]any{"groups": []string{"/workspaces/acme-analytics/owner"}})

	if code, _ := serve(t, holder, token, ""); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (ADR 0025: header mandatory by default)", code)
	}
}

// ADR 0034: with WorkspaceOptional and no header, the request proceeds with Active unresolved.
func TestMiddleware_WorkspaceOptional_AbsentHeaderLeavesActiveUnresolved(t *testing.T) {
	provider, holder := newReadyMiddlewareFixture(t)
	token := provider.issueToken(t, map[string]any{"groups": []string{"/workspaces/acme-analytics/owner"}})

	code, id := serve(t, holder, token, "", WorkspaceOptional)
	if code != http.StatusOK || id == nil {
		t.Fatalf("status = %d, want 200 with handler run", code)
	}
	if id.HasActive() {
		t.Errorf("Active = %+v, want unresolved", id.Active)
	}
	if len(id.Memberships) != 1 {
		t.Errorf("Memberships = %v, want the token's 1 membership", id.Memberships)
	}
}

// When the header is present on a WorkspaceOptional route, behavior is unchanged.
func TestMiddleware_WorkspaceOptional_PresentHeaderStillValidated(t *testing.T) {
	provider, holder := newReadyMiddlewareFixture(t)
	token := provider.issueToken(t, map[string]any{"groups": []string{"/workspaces/acme-analytics/owner"}})

	code, id := serve(t, holder, token, "acme-analytics", WorkspaceOptional)
	if code != http.StatusOK || !id.HasActive() || id.Active.Role != RoleOwner {
		t.Fatalf("status = %d, identity = %+v; want 200 with owner active", code, id)
	}

	if code, _ := serve(t, holder, token, "not-mine", WorkspaceOptional); code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a workspace the caller isn't in", code)
	}
}
