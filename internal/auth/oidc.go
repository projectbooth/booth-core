// Package auth implements booth-core's OIDC relying-party contract (ADR 0004): JWT
// verification against a configurable identity provider, and derivation of
// workspace/role membership from the verified token's groups claim (ADR 0008, and
// docs/decisions/0001-workspace-role-claim-shape.md).
package auth

import (
	"context"
	"fmt"
	"log"

	oidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/projectbooth/booth-core/internal/config"
)

// Claims is the subset of a verified ID/access token booth-core cares about. Extra
// claims a provider includes are ignored, not rejected.
type Claims struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	// PreferredUsername and Name are the display claims the user directory records
	// (ADR 0047). Either may be empty — not every provider/client scope includes them.
	PreferredUsername string   `json:"preferred_username"`
	Name              string   `json:"name"`
	Groups            []string `json:"-"` // populated from config.OIDCConfig.GroupsClaim, name varies by provider
}

// Verifier verifies bearer tokens against a configured OIDC provider's published JWKS,
// issuer, and expiry — the generic relying-party contract every module also implements
// independently (core-platform-api.md: "a module must independently verify the identity
// core forwards it").
type Verifier struct {
	idTokenVerifier *oidc.IDTokenVerifier
	groupsClaim     string
}

// NewVerifier prepares JWKS-based signature verification against cfg.IssuerURL.
// cfg.RequireAudience controls whether the token's `aud` must match cfg.ClientID — a
// per-deployment policy choice per core-platform-api.md, not hardcoded.
//
// Ordinarily this fetches the provider's discovery document and uses its own
// self-reported jwks_uri. If cfg.JWKSURL is set (ADR 0108), discovery is skipped
// entirely and keys are fetched directly from that URL instead; `iss` is still validated
// exactly against cfg.IssuerURL either way — only where keys are physically fetched from
// changes. config.Load already rejects cfg.JWKSURL set without cfg.IssuerURL, but that
// check is repeated here since this is also a usable library entry point on its own.
func NewVerifier(ctx context.Context, cfg config.OIDCConfig) (*Verifier, error) {
	if cfg.JWKSURL != "" && cfg.IssuerURL == "" {
		return nil, fmt.Errorf("oidc.jwksUrl is set but oidc.issuerUrl is empty: the issuer is still required to validate `iss`")
	}

	verifierCfg := &oidc.Config{
		SkipClientIDCheck: !cfg.RequireAudience,
		ClientID:          cfg.ClientID,
	}

	var idTokenVerifier *oidc.IDTokenVerifier
	keysFrom := "discovery (" + cfg.IssuerURL + "/.well-known/openid-configuration)"
	if cfg.JWKSURL != "" {
		keySet := oidc.NewRemoteKeySet(ctx, cfg.JWKSURL)
		idTokenVerifier = oidc.NewVerifier(cfg.IssuerURL, keySet, verifierCfg)
		keysFrom = cfg.JWKSURL
	} else {
		provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
		if err != nil {
			return nil, fmt.Errorf("oidc discovery against %s: %w", cfg.IssuerURL, err)
		}
		idTokenVerifier = provider.Verifier(verifierCfg)
	}

	log.Printf("oidc: verifying tokens with issuer=%s keys-from=%s", cfg.IssuerURL, keysFrom)

	return &Verifier{
		idTokenVerifier: idTokenVerifier,
		groupsClaim:     cfg.GroupsClaim,
	}, nil
}

// Verify checks signature (via JWKS), issuer, expiry, and (per config) audience, then
// extracts Claims including the configured groups claim. Subject presence is enforced by
// go-oidc's token parsing; an empty subject is rejected.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (*Claims, error) {
	idToken, err := v.idTokenVerifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("token verification failed: %w", err)
	}

	var raw map[string]any
	if err := idToken.Claims(&raw); err != nil {
		return nil, fmt.Errorf("decoding claims: %w", err)
	}

	if idToken.Subject == "" {
		return nil, fmt.Errorf("token missing subject")
	}

	claims := &Claims{
		Subject: idToken.Subject,
	}
	if email, ok := raw["email"].(string); ok {
		claims.Email = email
	}
	if v, ok := raw["preferred_username"].(string); ok {
		claims.PreferredUsername = v
	}
	if v, ok := raw["name"].(string); ok {
		claims.Name = v
	}
	claims.Groups = extractStringSlice(raw, v.groupsClaim)

	return claims, nil
}

func extractStringSlice(raw map[string]any, key string) []string {
	val, ok := raw[key]
	if !ok {
		return nil
	}
	list, ok := val.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
