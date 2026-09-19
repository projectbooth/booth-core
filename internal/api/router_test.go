package api

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

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/config"
	"github.com/projectbooth/booth-core/internal/gateway"
	"github.com/projectbooth/booth-core/internal/registry"
)

// TestRouter_OnlyAPIMeIsWorkspaceHeaderExempt pins ADR 0034's wiring through the real
// router and a real verifier: GET /api/me works without X-Workspace (and omits "active"),
// while every other authenticated route still 400s without it.
func TestRouter_OnlyAPIMeIsWorkspaceHeaderExempt(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var idp *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": idp.URL, "jwks_uri": idp.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(josejwt.JSONWebKeySet{Keys: []josejwt.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k", Algorithm: "RS256", Use: "sig"},
		}})
	})
	idp = httptest.NewServer(mux)
	defer idp.Close()

	verifier, err := auth.NewVerifier(context.Background(), config.OIDCConfig{
		IssuerURL: idp.URL, ClientID: "c", GroupsClaim: "groups",
	})
	if err != nil {
		t.Fatal(err)
	}
	holder := &auth.VerifierHolder{}
	holder.Store(verifier)

	signer, err := josejwt.NewSigner(josejwt.SigningKey{Algorithm: josejwt.RS256, Key: key},
		(&josejwt.SignerOptions{}).WithHeader("kid", "k").WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.Signed(signer).Claims(map[string]any{
		"iss": idp.URL, "sub": "u1", "aud": "c",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"groups": []string{"/workspaces/acme-analytics/owner"},
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}

	tokens := gateway.NewIframeTokenIssuer([]byte("s"))
	reg := registry.New()
	router := NewRouter(Deps{
		Verifier: holder, Registry: reg, Gateway: gateway.New(reg),
		IframeTokens: tokens, IframeURLs: gateway.NewIframeURLIssuer(tokens, "http://x"),
	})

	get := func(path, workspace string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if workspace != "" {
			req.Header.Set(auth.HeaderWorkspace, workspace)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// /api/me without the header: 200, memberships present, active omitted.
	rec := get("/api/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/me without header: status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, present := body["active"]; present {
		t.Errorf("active present without header: %v", body["active"])
	}
	if ms, _ := body["memberships"].([]any); len(ms) != 1 {
		t.Errorf("memberships = %v, want 1 entry", body["memberships"])
	}

	// /api/me with the header: active populated as before.
	rec = get("/api/me", "acme-analytics")
	body = nil
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if rec.Code != http.StatusOK || body["active"] == nil {
		t.Errorf("/api/me with header: status = %d, active = %v; want 200 with active", rec.Code, body["active"])
	}

	// Every other authenticated route still requires the header.
	if rec := get("/api/modules", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("/api/modules without header: status = %d, want 400", rec.Code)
	}
	if rec := get("/api/modules", "acme-analytics"); rec.Code != http.StatusOK {
		t.Errorf("/api/modules with header: status = %d, want 200", rec.Code)
	}
}
