// Package config loads booth-core's runtime configuration from environment variables.
// Every value maps 1:1 to a Helm chart value/env var — there is no config file format
// of our own to version alongside the chart.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
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

	// EventBusAuth turns on event-bus authentication (ADR 0049): core bootstraps the
	// NATS trust chain, connects with its own credential, and provisions per-module
	// credentials from each BoothModule's declared events. Every real deployment should
	// run with it on; the bundled Helm chart does. Off means the bus is unauthenticated.
	EventBusAuth bool

	// NATSModuleURL is the address written into each module's credential Secret. Modules
	// live in other namespaces, so it must be resolvable from anywhere in the cluster
	// (unlike NATSURL, which core itself uses from its own namespace).
	NATSModuleURL string

	// DevRegistryPath, if set, points at a static YAML file listing BoothModule-shaped
	// entries for local development without a real Kubernetes cluster (ADR 0019's
	// "worth building as a convenience" fallback). Empty means "use the real CRD
	// watch" — the production path.
	DevRegistryPath string

	// PostgresDSN, if set, is used verbatim as the connection string for core's own
	// database (workspace metadata, the user directory) instead of core provisioning one
	// itself. Leave it empty to use the provisioned database (Postgres below).
	PostgresDSN string

	// Postgres locates the shared PostgreSQL server core provisions module databases on
	// (ADR 0053) and its own. Provisioning is on only when Postgres.Host is set.
	Postgres PostgresConfig

	// Workload configures workload identity (ADR 0056): core as a second token issuer.
	Workload WorkloadConfig

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

// WorkloadConfig is the workload-identity configuration (ADR 0056).
type WorkloadConfig struct {
	// IssuerURL is core's own issuer URL for workload tokens: the `iss` it mints, the base of
	// its JWKS and minting endpoint, and what other modules are configured to trust. It must be
	// resolvable from every module's namespace (an in-cluster Service URL), and distinct from
	// the OIDC provider's. Setting it turns workload identity on; empty leaves it off.
	IssuerURL string

	// OwnerMaxAge bounds how long since a run owner's role was last seen (in a token core
	// verified) before that role stops counting. Zero means the default (7 days).
	OwnerMaxAge time.Duration
}

// PostgresConfig is the shared-PostgreSQL configuration (ADR 0053). The bundled Helm chart
// fills it in for its bundled server; an operator running at scale sets it to point at an
// external cluster.
type PostgresConfig struct {
	// Host, if set, turns database provisioning on.
	Host string
	Port int

	// ModuleHost is the address written into module DSNs (must resolve from any namespace).
	// Defaults to Host qualified with core's namespace, like the NATS module URL.
	ModuleHost string

	AdminUser     string
	AdminPassword string
	AdminDatabase string
	SSLMode       string

	// Bundled means the server is the chart's own StatefulSet: core generates its admin
	// password (the StatefulSet reads the same Secret) instead of taking one from config,
	// and locks down the maintenance databases.
	Bundled bool

	// RestrictMaintenanceAccess revokes PUBLIC connect on the maintenance databases; always
	// on for a bundled server, opt-in for an external one.
	RestrictMaintenanceAccess bool

	// BackupClaim, if its Name is set, is a PersistentVolumeClaim core creates (if absent) for
	// the bundled server's backup CronJob to write to (ADR 0054). See dbprov.EnsureBackupClaim
	// for why it isn't a Helm-managed resource.
	BackupClaim BackupClaimConfig
}

// BackupClaimConfig describes the backup PersistentVolumeClaim.
type BackupClaimConfig struct {
	Name         string
	Size         string
	StorageClass string
}

// StartupWarnings returns conditions an operator should know about at boot, as one-line
// messages ready to log. Currently: an external PostgreSQL with the maintenance-database
// restriction left off (ADR 0054).
func (p PostgresConfig) StartupWarnings() []string {
	var w []string
	if p.Host != "" && !p.Bundled && !p.RestrictMaintenanceAccess {
		w = append(w, "connected to an EXTERNAL PostgreSQL ("+p.Host+") with postgres.external.restrictMaintenanceAccess "+
			"off: PostgreSQL lets any role connect to the maintenance databases (postgres, template1) by default, so a "+
			"module's database credentials can list the names of every other database and role on that server (not their data). "+
			"Core does not change privileges on a cluster it doesn't own; set postgres.external.restrictMaintenanceAccess=true "+
			"once you've checked nothing else relies on that access (ADR 0054)")
	}
	return w
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

	if v := os.Getenv("BOOTH_EVENTBUS_AUTH"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("BOOTH_EVENTBUS_AUTH: %w", err)
		}
		cfg.EventBusAuth = b
	}
	cfg.Workload.IssuerURL = strings.TrimRight(os.Getenv("BOOTH_WORKLOAD_ISSUER_URL"), "/")
	if v := os.Getenv("BOOTH_WORKLOAD_OWNER_MAX_AGE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("BOOTH_WORKLOAD_OWNER_MAX_AGE: %q is not a positive duration (e.g. 168h)", v)
		}
		cfg.Workload.OwnerMaxAge = d
	}
	cfg.NATSModuleURL = getEnv("BOOTH_NATS_MODULE_URL", qualifyNATSURL(cfg.NATSURL, cfg.KubeNamespace))

	pg := PostgresConfig{
		Host:          os.Getenv("BOOTH_POSTGRES_HOST"),
		AdminUser:     getEnv("BOOTH_POSTGRES_ADMIN_USER", "postgres"),
		AdminPassword: os.Getenv("BOOTH_POSTGRES_ADMIN_PASSWORD"),
		AdminDatabase: getEnv("BOOTH_POSTGRES_ADMIN_DATABASE", "postgres"),
		SSLMode:       os.Getenv("BOOTH_POSTGRES_SSLMODE"),
		Port:          5432,
	}
	if v := os.Getenv("BOOTH_POSTGRES_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return Config{}, fmt.Errorf("BOOTH_POSTGRES_PORT: %q is not a valid port", v)
		}
		pg.Port = n
	}
	for env, dst := range map[string]*bool{
		"BOOTH_POSTGRES_BUNDLED":                     &pg.Bundled,
		"BOOTH_POSTGRES_RESTRICT_MAINTENANCE_ACCESS": &pg.RestrictMaintenanceAccess,
	} {
		if v := os.Getenv(env); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return Config{}, fmt.Errorf("%s: %w", env, err)
			}
			*dst = b
		}
	}
	if pg.Bundled {
		pg.RestrictMaintenanceAccess = true
	}
	pg.BackupClaim = BackupClaimConfig{
		Name:         os.Getenv("BOOTH_POSTGRES_BACKUP_PVC_NAME"),
		Size:         getEnv("BOOTH_POSTGRES_BACKUP_PVC_SIZE", "8Gi"),
		StorageClass: os.Getenv("BOOTH_POSTGRES_BACKUP_PVC_STORAGE_CLASS"),
	}
	pg.ModuleHost = getEnv("BOOTH_POSTGRES_MODULE_HOST", qualifyHost(pg.Host, cfg.KubeNamespace))
	cfg.Postgres = pg

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

// qualifyNATSURL turns an in-namespace NATS URL like nats://booth-core-nats:4222 into one
// resolvable from any namespace (nats://booth-core-nats.<ns>.svc.cluster.local:4222).
// URLs whose host is already qualified, or that don't parse, are returned unchanged.
func qualifyNATSURL(raw, namespace string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || strings.Contains(u.Hostname(), ".") {
		return raw
	}
	host := u.Hostname() + "." + namespace + ".svc.cluster.local"
	if port := u.Port(); port != "" {
		host += ":" + port
	}
	u.Host = host
	return u.String()
}

// qualifyHost turns an in-namespace service name into one resolvable from any namespace.
// A host that's empty or already contains a dot (a FQDN or an IP) is returned unchanged.
func qualifyHost(host, namespace string) string {
	if host == "" || strings.Contains(host, ".") || strings.Contains(host, ":") {
		return host
	}
	return host + "." + namespace + ".svc.cluster.local"
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
