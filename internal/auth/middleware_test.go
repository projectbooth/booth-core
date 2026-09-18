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
