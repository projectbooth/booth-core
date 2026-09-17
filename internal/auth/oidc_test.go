package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	josejwt "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/projectbooth/booth-core/internal/config"
)

// testOIDCProvider stands up a minimal, real HTTP OIDC discovery + JWKS endpoint so
// Verifier is exercised against the actual go-oidc discovery/JWKS-fetch path, not a
// mocked-out verifier.
type testOIDCProvider struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	keyID  string
}

func newTestOIDCProvider(t *testing.T) *testOIDCProvider {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	p := &testOIDCProvider{key: key, keyID: "test-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.server.URL,
			"jwks_uri":                              p.server.URL + "/jwks",
			"authorization_endpoint":                p.server.URL + "/authorize",
			"token_endpoint":                        p.server.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		jwks := josejwt.JSONWebKeySet{
			Keys: []josejwt.JSONWebKey{
				{
					Key:       &p.key.PublicKey,
					KeyID:     p.keyID,
					Algorithm: "RS256",
					Use:       "sig",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	})

	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

func (p *testOIDCProvider) issueToken(t *testing.T, claims map[string]any) string {
	t.Helper()

	signer, err := josejwt.NewSigner(josejwt.SigningKey{
		Algorithm: josejwt.RS256,
		Key:       p.key,
	}, (&josejwt.SignerOptions{}).WithHeader("kid", p.keyID).WithType("JWT"))
	if err != nil {
		t.Fatalf("creating signer: %v", err)
	}

	builder := jwt.Signed(signer)
	base := map[string]any{
		"iss": p.server.URL,
		"sub": "user-123",
		"aud": "test-client",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	for k, v := range claims {
		base[k] = v
	}

	raw, err := builder.Claims(base).Serialize()
	if err != nil {
		t.Fatalf("serializing token: %v", err)
	}
	return raw
}

func TestVerifierVerifiesRealTokenAgainstDiscoveryAndJWKS(t *testing.T) {
	provider := newTestOIDCProvider(t)

	token := provider.issueToken(t, map[string]any{
		"email":  "alice@example.com",
		"groups": []string{"/workspaces/acme-analytics/owner"},
	})

	verifier, err := NewVerifier(context.Background(), config.OIDCConfig{
		IssuerURL:       provider.server.URL,
		ClientID:        "test-client",
		RequireAudience: true,
		GroupsClaim:     "groups",
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	claims, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if claims.Subject != "user-123" {
		t.Errorf("Subject = %q, want user-123", claims.Subject)
	}
	if claims.Email != "alice@example.com" {
		t.Errorf("Email = %q, want alice@example.com", claims.Email)
	}
	if len(claims.Groups) != 1 || claims.Groups[0] != "/workspaces/acme-analytics/owner" {
		t.Errorf("Groups = %v, want [/workspaces/acme-analytics/owner]", claims.Groups)
	}
}

func TestVerifierRejectsExpiredToken(t *testing.T) {
	provider := newTestOIDCProvider(t)

	token := provider.issueToken(t, map[string]any{
		"exp": time.Now().Add(-time.Hour).Unix(),
	})

	verifier, err := NewVerifier(context.Background(), config.OIDCConfig{
		IssuerURL:   provider.server.URL,
		ClientID:    "test-client",
		GroupsClaim: "groups",
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	if _, err := verifier.Verify(context.Background(), token); err == nil {
		t.Fatal("expected error verifying expired token, got nil")
	}
}

func TestVerifierRejectsWrongAudienceWhenRequired(t *testing.T) {
	provider := newTestOIDCProvider(t)

	token := provider.issueToken(t, map[string]any{
		"aud": "some-other-client",
	})

	verifier, err := NewVerifier(context.Background(), config.OIDCConfig{
		IssuerURL:       provider.server.URL,
		ClientID:        "test-client",
		RequireAudience: true,
		GroupsClaim:     "groups",
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	if _, err := verifier.Verify(context.Background(), token); err == nil {
		t.Fatal("expected audience mismatch error, got nil")
	}
}
