package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
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

// testJWKSOnlyServer stands up a real HTTP server that serves only a JWKS endpoint, no
// discovery document — exercising the ADR 0108 key-fetch-override path, which must never
// touch discovery at all.
type testJWKSOnlyServer struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	keyID  string
}

func newTestJWKSOnlyServer(t *testing.T) *testJWKSOnlyServer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	s := &testJWKSOnlyServer{key: key, keyID: "jwks-only-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		jwks := josejwt.JSONWebKeySet{
			Keys: []josejwt.JSONWebKey{
				{Key: &s.key.PublicKey, KeyID: s.keyID, Algorithm: "RS256", Use: "sig"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	})
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		t.Error("discovery was fetched even though JWKSURL was set — the key-fetch override must bypass it entirely")
		w.WriteHeader(http.StatusNotFound)
	})

	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *testJWKSOnlyServer) issueToken(t *testing.T, issuer string, claims map[string]any) string {
	t.Helper()

	signer, err := josejwt.NewSigner(josejwt.SigningKey{
		Algorithm: josejwt.RS256,
		Key:       s.key,
	}, (&josejwt.SignerOptions{}).WithHeader("kid", s.keyID).WithType("JWT"))
	if err != nil {
		t.Fatalf("creating signer: %v", err)
	}

	base := map[string]any{
		"iss": issuer,
		"sub": "user-123",
		"aud": "test-client",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	for k, v := range claims {
		base[k] = v
	}
	raw, err := jwt.Signed(signer).Claims(base).Serialize()
	if err != nil {
		t.Fatalf("serializing token: %v", err)
	}
	return raw
}

// This is ADR 0108's central claim: a verifying service can fetch signing keys from a
// plain-http, purely-in-cluster URL while the token's `iss` is an https URL the test
// process cannot even reach — proving the key fetch and the issuer check are genuinely
// decoupled, not just configured with different-looking strings that happen to agree.
func TestVerifierFetchesKeysFromPlainHTTPURLWhileIssuerIsHTTPS(t *testing.T) {
	jwks := newTestJWKSOnlyServer(t)
	const unreachableHTTPSIssuer = "https://booth.home.arpa.invalid/realms/booth"

	token := jwks.issueToken(t, unreachableHTTPSIssuer, map[string]any{
		"groups": []string{"/workspaces/acme-analytics/owner"},
	})

	verifier, err := NewVerifier(context.Background(), config.OIDCConfig{
		IssuerURL:       unreachableHTTPSIssuer,
		JWKSURL:         jwks.server.URL + "/jwks",
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
	if len(claims.Groups) != 1 || claims.Groups[0] != "/workspaces/acme-analytics/owner" {
		t.Errorf("Groups = %v, want [/workspaces/acme-analytics/owner]", claims.Groups)
	}
}

// `iss` is still validated exactly even when keys come from a separate URL: a token
// signed by the same key but claiming a different issuer must still be rejected.
func TestVerifierWithJWKSURLStillValidatesIssuerExactly(t *testing.T) {
	jwks := newTestJWKSOnlyServer(t)

	token := jwks.issueToken(t, "https://wrong-issuer.invalid/realms/booth", nil)

	verifier, err := NewVerifier(context.Background(), config.OIDCConfig{
		IssuerURL: "https://booth.home.arpa.invalid/realms/booth",
		JWKSURL:   jwks.server.URL + "/jwks",
		ClientID:  "test-client",
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	if _, err := verifier.Verify(context.Background(), token); err == nil {
		t.Fatal("expected an issuer-mismatch error, got nil")
	}
}

// ADR 0108 condition 2: the effective key URL and issuer are logged once at startup, so
// an operator can see which key source is actually in effect without reading config.
func TestNewVerifierLogsIssuerAndKeySourceOnce(t *testing.T) {
	jwks := newTestJWKSOnlyServer(t)
	const issuer = "https://booth.home.arpa.invalid/realms/booth"

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	if _, err := NewVerifier(context.Background(), config.OIDCConfig{
		IssuerURL: issuer,
		JWKSURL:   jwks.server.URL + "/jwks",
		ClientID:  "test-client",
	}); err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	out := buf.String()
	if strings.Count(out, "oidc: verifying tokens") != 1 {
		t.Fatalf("expected exactly one startup log line, got: %q", out)
	}
	if !strings.Contains(out, issuer) {
		t.Errorf("log line does not mention the issuer: %q", out)
	}
	if !strings.Contains(out, jwks.server.URL+"/jwks") {
		t.Errorf("log line does not mention the effective key URL: %q", out)
	}
}

func TestNewVerifierRejectsJWKSURLWithoutIssuerURL(t *testing.T) {
	jwks := newTestJWKSOnlyServer(t)

	_, err := NewVerifier(context.Background(), config.OIDCConfig{
		JWKSURL:  jwks.server.URL + "/jwks",
		ClientID: "test-client",
	})
	if err == nil {
		t.Fatal("expected an error: JWKSURL set without IssuerURL")
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
