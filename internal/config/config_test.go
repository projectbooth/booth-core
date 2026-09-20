package config

import (
	"strings"
	"testing"
)

func TestQualifyNATSURL(t *testing.T) {
	cases := []struct{ in, ns, want string }{
		{"nats://booth-core-nats:4222", "booth-system", "nats://booth-core-nats.booth-system.svc.cluster.local:4222"},
		{"nats://booth-core-nats", "booth-system", "nats://booth-core-nats.booth-system.svc.cluster.local"},
		{"nats://nats.example.com:4222", "booth-system", "nats://nats.example.com:4222"},
		{"nats://booth-core-nats.other.svc:4222", "booth-system", "nats://booth-core-nats.other.svc:4222"},
		{"::not a url", "booth-system", "::not a url"},
	}
	for _, c := range cases {
		if got := qualifyNATSURL(c.in, c.ns); got != c.want {
			t.Errorf("qualifyNATSURL(%q, %q) = %q, want %q", c.in, c.ns, got, c.want)
		}
	}
}

func TestQualifyHost(t *testing.T) {
	for in, want := range map[string]string{
		"booth-core-postgresql":      "booth-core-postgresql.booth-system.svc.cluster.local",
		"db.example.com":             "db.example.com",
		"10.0.0.5":                   "10.0.0.5",
		"":                           "",
		"pg.other.svc.cluster.local": "pg.other.svc.cluster.local",
	} {
		if got := qualifyHost(in, "booth-system"); got != want {
			t.Errorf("qualifyHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoad_PostgresBundledImpliesMaintenanceLockdown(t *testing.T) {
	t.Setenv("BOOTH_OIDC_ISSUER_URL", "https://idp")
	t.Setenv("BOOTH_OIDC_CLIENT_ID", "c")
	t.Setenv("BOOTH_POSTGRES_HOST", "booth-core-postgresql")
	t.Setenv("BOOTH_POSTGRES_BUNDLED", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	pg := cfg.Postgres
	if !pg.Bundled || !pg.RestrictMaintenanceAccess {
		t.Errorf("bundled = %v restrict = %v; a bundled server must be locked down", pg.Bundled, pg.RestrictMaintenanceAccess)
	}
	if pg.ModuleHost != "booth-core-postgresql.booth-system.svc.cluster.local" || pg.Port != 5432 || pg.AdminUser != "postgres" {
		t.Errorf("defaults wrong: %+v", pg)
	}
}

func TestLoad_PostgresExternalIsOptIn(t *testing.T) {
	t.Setenv("BOOTH_OIDC_ISSUER_URL", "https://idp")
	t.Setenv("BOOTH_OIDC_CLIENT_ID", "c")
	t.Setenv("BOOTH_POSTGRES_HOST", "pg.example.com")
	t.Setenv("BOOTH_POSTGRES_PORT", "6432")
	t.Setenv("BOOTH_POSTGRES_ADMIN_USER", "core_admin")
	t.Setenv("BOOTH_POSTGRES_SSLMODE", "verify-full")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	pg := cfg.Postgres
	if pg.Bundled || pg.RestrictMaintenanceAccess {
		t.Errorf("an external server must not have its maintenance databases altered by default: %+v", pg)
	}
	if pg.Host != "pg.example.com" || pg.Port != 6432 || pg.AdminUser != "core_admin" || pg.SSLMode != "verify-full" {
		t.Errorf("got %+v", pg)
	}
}

func TestLoad_PostgresUnsetMeansProvisioningOff(t *testing.T) {
	t.Setenv("BOOTH_OIDC_ISSUER_URL", "https://idp")
	t.Setenv("BOOTH_OIDC_CLIENT_ID", "c")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Postgres.Host != "" {
		t.Errorf("host = %q, want provisioning off", cfg.Postgres.Host)
	}
}

func TestLoad_RejectsBadPostgresPort(t *testing.T) {
	t.Setenv("BOOTH_OIDC_ISSUER_URL", "https://idp")
	t.Setenv("BOOTH_OIDC_CLIENT_ID", "c")
	t.Setenv("BOOTH_POSTGRES_PORT", "99999")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for an out-of-range port")
	}
}

// ADR 0054: operators must not be able to assume a protection that silently isn't there.
func TestStartupWarnings(t *testing.T) {
	warned := func(p PostgresConfig) bool { return len(p.StartupWarnings()) > 0 }

	if !warned(PostgresConfig{Host: "pg.example.com"}) {
		t.Error("expected a warning for an external server with the restriction off")
	}
	msg := PostgresConfig{Host: "pg.example.com"}.StartupWarnings()[0]
	for _, want := range []string{"pg.example.com", "restrictMaintenanceAccess", "postgres", "template1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning %q doesn't mention %q", msg, want)
		}
	}

	for name, p := range map[string]PostgresConfig{
		"external with the restriction on": {Host: "pg.example.com", RestrictMaintenanceAccess: true},
		"bundled (always restricted)":      {Host: "booth-postgresql", Bundled: true, RestrictMaintenanceAccess: true},
		"provisioning off":                 {},
	} {
		if warned(p) {
			t.Errorf("%s: unexpected warning: %v", name, p.StartupWarnings())
		}
	}
}

// End to end through Load: the chart's external mode really does yield the warning, and the
// bundled mode really doesn't.
func TestLoad_ExternalDefaultsWarnBundledDoesNot(t *testing.T) {
	t.Setenv("BOOTH_OIDC_ISSUER_URL", "https://idp")
	t.Setenv("BOOTH_OIDC_CLIENT_ID", "c")
	t.Setenv("BOOTH_POSTGRES_HOST", "pg.example.com")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Postgres.StartupWarnings()) == 0 {
		t.Error("an external server left at defaults should warn")
	}

	t.Setenv("BOOTH_POSTGRES_BUNDLED", "true")
	cfg, _ = Load()
	if len(cfg.Postgres.StartupWarnings()) != 0 {
		t.Errorf("bundled server warned: %v", cfg.Postgres.StartupWarnings())
	}
}
