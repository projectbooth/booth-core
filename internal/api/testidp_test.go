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
)

// testIDP is a minimal real OIDC provider (discovery + JWKS) with a token issuer, so
// router-level tests exercise the actual verification path.
type testIDP struct {
	holder *auth.VerifierHolder
	url    string
	signer josejwt.Signer
}

func newTestIDP(t *testing.T) *testIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": srv.URL, "jwks_uri": srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(josejwt.JSONWebKeySet{Keys: []josejwt.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k", Algorithm: "RS256", Use: "sig"},
		}})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	verifier, err := auth.NewVerifier(context.Background(), config.OIDCConfig{
		IssuerURL: srv.URL, ClientID: "c", GroupsClaim: "groups",
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
	return &testIDP{holder: holder, url: srv.URL, signer: signer}
}

// token issues a valid token for sub, with the given extra claims (groups, email, ...).
func (i *testIDP) token(t *testing.T, sub string, extra map[string]any) string {
	t.Helper()
	claims := map[string]any{
		"iss": i.url, "sub": sub, "aud": "c",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}
	for k, v := range extra {
		claims[k] = v
	}
	raw, err := jwt.Signed(i.signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
