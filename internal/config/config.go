// Package config loads booth-core's runtime configuration from environment variables.
// Every value maps 1:1 to a Helm chart value/env var — there is no config file format
// of our own to version alongside the chart.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config is booth-core's full runtime configuration.
type Config struct {
	// HTTPAddr is the address the gateway/API HTTP server listens on.
	HTTPAddr string

	// OIDC is the identity-provider configuration (ADR 0004).
	OIDC OIDCConfig

	// NATSURL is the connection string for the NATS server backing the event bus
	// (ADR 0021). In-cluster, this points at the NATS service the Helm chart bundles.
	NATSURL string

	// DevRegistryPath, if set, points at a static YAML file listing BoothModule-shaped
	// entries for local development without a real Kubernetes cluster (ADR 0019's
	// "worth building as a convenience" fallback). Empty means "use the real CRD
	// watch" — the production path.
	DevRegistryPath string

	// PostgresDSN is the connection string for core's own database on the shared
	// PostgreSQL cluster (ADR 0014) — workspaces, module registry cache, users.
	PostgresDSN string

	// KubeNamespace is the namespace booth-core itself runs in, used as the default
	// namespace for module BoothModule watches and Secret/ConfigMap provisioning.
	KubeNamespace string
}

// OIDCConfig is the pluggable-OIDC relying-party configuration (ADR 0004). Any
// OIDC-compliant provider works here — Keycloak is just the default value shape.
type OIDCConfig struct {
	// IssuerURL is the provider's issuer; discovery document + JWKS are fetched from
	// here per the standard OIDC discovery flow.
	IssuerURL string

	// ClientID is used both for the browser PKCE flow and, when RequireAudience is
	// true, as the expected `aud` claim.
	ClientID string

	// RequireAudience makes audience-check policy an explicit per-deployment choice
	// rather than hardcoded (core-platform-api.md's "audience-check policy is a
	// per-deployment config choice").
	RequireAudience bool

	// GroupsClaim is the token claim carrying workspace/role membership, following
	// OpenDataPlatform's `groups` claim -> `/workspaces/<name>/<role>` shape
	// (ARCHITECTURE.md §6, ADR 0004, ADR 0008). Configurable because not every OIDC
	// provider names this claim "groups".
	GroupsClaim string
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:        getEnv("BOOTH_HTTP_ADDR", ":8080"),
		NATSURL:         getEnv("BOOTH_NATS_URL", "nats://booth-core-nats:4222"),
		DevRegistryPath: os.Getenv("BOOTH_DEV_REGISTRY_PATH"),
		PostgresDSN:     os.Getenv("BOOTH_POSTGRES_DSN"),
		KubeNamespace:   getEnv("BOOTH_NAMESPACE", "booth-system"),
		OIDC: OIDCConfig{
			IssuerURL:   os.Getenv("BOOTH_OIDC_ISSUER_URL"),
			ClientID:    os.Getenv("BOOTH_OIDC_CLIENT_ID"),
			GroupsClaim: getEnv("BOOTH_OIDC_GROUPS_CLAIM", "groups"),
		},
	}

	if v := os.Getenv("BOOTH_OIDC_REQUIRE_AUDIENCE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("BOOTH_OIDC_REQUIRE_AUDIENCE: %w", err)
		}
		cfg.OIDC.RequireAudience = b
	}

	if cfg.DevRegistryPath == "" {
		if cfg.OIDC.IssuerURL == "" {
			return Config{}, fmt.Errorf("BOOTH_OIDC_ISSUER_URL is required")
		}
		if cfg.OIDC.ClientID == "" {
			return Config{}, fmt.Errorf("BOOTH_OIDC_CLIENT_ID is required")
		}
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
