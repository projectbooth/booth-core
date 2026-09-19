package dbprov

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/testpg"
)

// Everything here runs against a real PostgreSQL server (see internal/testpg): the
// properties that matter — a module can't open another module's database, its role is
// unprivileged, data survives credential loss — are properties of the server, not of our
// string building.

func TestAgainstRealPostgres(t *testing.T) {
	srv := testpg.Start(t)
	ctx := context.Background()

	newAdmin := func(t *testing.T, mut func(*Config)) *Admin {
		t.Helper()
		port := 0
		if p, err := parsePort(srv.Port); err == nil {
			port = p
		}
		cfg := Config{Host: srv.Host, Port: port, AdminUser: srv.User, AdminPassword: srv.Pass, SSLMode: "disable"}
		if mut != nil {
			mut(&cfg)
		}
		a, err := NewAdmin(ctx, cfg)
		if err != nil {
			t.Fatalf("NewAdmin: %v", err)
		}
		t.Cleanup(a.Close)
		return a
	}
	connect := func(t *testing.T, dsn string) (*pgx.Conn, error) {
		t.Helper()
		c, err := pgx.Connect(ctx, dsn)
		if err == nil {
			t.Cleanup(func() { _ = c.Close(ctx) })
		}
		return c, err
	}

	t.Run("Admin", func(t *testing.T) {
		// Documents the default's residual exposure. Must run before any restricting admin
		// touches this server, since a revoke is permanent for the server's lifetime.
		t.Run("by default a module role can still log in to the maintenance database", func(t *testing.T) {
			open := newAdmin(t, nil)
			if err := open.Ensure(ctx, "booth_mod_unrestricted", "pw"); err != nil {
				t.Fatal(err)
			}
			c, err := connect(t, open.cfg.dsn("booth_mod_unrestricted", "pw", open.cfg.Host, "postgres"))
			if err != nil {
				t.Fatalf("expected PostgreSQL's default (PUBLIC can connect) without RestrictMaintenanceAccess: %v", err)
			}
			// It can list database names — the metadata leak RestrictMaintenanceAccess closes —
			// but not read another database's data.
			var n int
			if err := c.QueryRow(ctx, "SELECT count(*) FROM pg_database").Scan(&n); err != nil || n == 0 {
				t.Errorf("catalog read = %d, %v", n, err)
			}
		})

		a := newAdmin(t, func(c *Config) { c.RestrictMaintenanceAccess = true })

		t.Run("creates a usable role and database the role owns", func(t *testing.T) {
			if err := a.Ensure(ctx, "booth_mod_alpha", "pw-alpha"); err != nil {
				t.Fatal(err)
			}
			c, err := connect(t, a.DSN("booth_mod_alpha", "pw-alpha"))
			if err != nil {
				t.Fatalf("module can't log in: %v", err)
			}
			for _, q := range []string{
				"CREATE TABLE things (id int primary key, note text)",
				"INSERT INTO things VALUES (1, 'hello')",
			} {
				if _, err := c.Exec(ctx, q); err != nil {
					t.Fatalf("owner can't use its own database (%s): %v", q, err)
				}
			}
			if ok, _ := a.Exists(ctx, "booth_mod_alpha"); !ok {
				t.Error("Exists = false for a provisioned database")
			}
			if ok, _ := a.Exists(ctx, "booth_mod_never_made"); ok {
				t.Error("Exists = true for a database that was never provisioned")
			}
		})

		t.Run("the role is unprivileged", func(t *testing.T) {
			if err := a.Ensure(ctx, "booth_mod_priv", "pw"); err != nil {
				t.Fatal(err)
			}
			var super, createdb, createrole, repl bool
			if err := a.pool.QueryRow(ctx,
				"SELECT rolsuper, rolcreatedb, rolcreaterole, rolreplication FROM pg_roles WHERE rolname = 'booth_mod_priv'").
				Scan(&super, &createdb, &createrole, &repl); err != nil {
				t.Fatal(err)
			}
			if super || createdb || createrole || repl {
				t.Errorf("role has privileges it shouldn't: super=%v createdb=%v createrole=%v replication=%v", super, createdb, createrole, repl)
			}
			c, err := connect(t, a.DSN("booth_mod_priv", "pw"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Exec(ctx, "CREATE DATABASE booth_mod_smuggled"); err == nil {
				t.Error("a module role was able to create a database")
			}
			if _, err := c.Exec(ctx, "CREATE ROLE smuggled LOGIN"); err == nil {
				t.Error("a module role was able to create a role")
			}
		})

		// The isolation property ADR 0014 promised: "every module gets its own database/role".
		t.Run("one module's credentials cannot open another module's database", func(t *testing.T) {
			if err := a.Ensure(ctx, "booth_mod_left", "pw-left"); err != nil {
				t.Fatal(err)
			}
			if err := a.Ensure(ctx, "booth_mod_right", "pw-right"); err != nil {
				t.Fatal(err)
			}
			// left's role, pointed at right's database.
			crossed := a.cfg.dsn("booth_mod_left", "pw-left", a.cfg.Host, "booth_mod_right")
			if _, err := connect(t, crossed); err == nil {
				t.Fatal("booth_mod_left was able to connect to booth_mod_right's database")
			}
			// And the other way round, plus the maintenance databases (restricted, as in the
			// bundled server).
			for _, target := range []string{"booth_mod_left", "postgres", "template1"} {
				dsn := a.cfg.dsn("booth_mod_right", "pw-right", a.cfg.Host, target)
				if _, err := connect(t, dsn); err == nil {
					t.Errorf("booth_mod_right was able to connect to %q", target)
				}
			}
		})

		t.Run("re-running is idempotent and resets the password, keeping data", func(t *testing.T) {
			if err := a.Ensure(ctx, "booth_mod_rot", "old-pw"); err != nil {
				t.Fatal(err)
			}
			c, _ := connect(t, a.DSN("booth_mod_rot", "old-pw"))
			if _, err := c.Exec(ctx, "CREATE TABLE keep (v text); INSERT INTO keep VALUES ('still here')"); err != nil {
				t.Fatal(err)
			}

			if err := a.Ensure(ctx, "booth_mod_rot", "new-pw"); err != nil {
				t.Fatalf("second Ensure: %v", err)
			}
			if _, err := connect(t, a.DSN("booth_mod_rot", "old-pw")); err == nil {
				t.Error("the old password still works after rotation")
			}
			c2, err := connect(t, a.DSN("booth_mod_rot", "new-pw"))
			if err != nil {
				t.Fatalf("new password rejected: %v", err)
			}
			var v string
			if err := c2.QueryRow(ctx, "SELECT v FROM keep").Scan(&v); err != nil || v != "still here" {
				t.Errorf("data after rotation = %q, %v", v, err)
			}
		})

		t.Run("replicas provisioning at once don't collide", func(t *testing.T) {
			var wg sync.WaitGroup
			errs := make(chan error, 16)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs <- a.Ensure(ctx, "booth_mod_racy", "same-pw")
				}()
			}
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					errs <- a.Ensure(ctx, "booth_mod_racy_"+string(rune('a'+i)), "pw")
				}(i)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Errorf("concurrent Ensure failed: %v", err)
				}
			}
			if _, err := connect(t, a.DSN("booth_mod_racy", "same-pw")); err != nil {
				t.Errorf("database unusable after concurrent provisioning: %v", err)
			}
		})

		t.Run("awkward passwords and names are quoted by the server, not by us", func(t *testing.T) {
			pw := `it's "a" \ p%ss;--`
			if err := a.Ensure(ctx, "booth_mod_quotes", pw); err != nil {
				t.Fatal(err)
			}
			if _, err := connect(t, a.DSN("booth_mod_quotes", pw)); err != nil {
				t.Fatalf("password with quotes/backslashes rejected: %v", err)
			}

			hostile := `x"; DROP DATABASE postgres; --`
			if err := a.Ensure(ctx, hostile, "pw"); err != nil {
				t.Fatalf("Ensure(hostile name): %v", err)
			}
			var n int
			if err := a.pool.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname IN ('postgres', $1)", hostile).Scan(&n); err != nil || n != 2 {
				t.Errorf("expected the maintenance database intact plus a literally-named one, got %d (%v)", n, err)
			}
		})
	})

	t.Run("DatabaseName", func(t *testing.T) {
		for id, want := range map[string]string{
			"storage":      "booth_mod_storage",
			"module-store": "booth_mod_module_store",
			"core":         "booth_mod_core", // never collides with booth_core
		} {
			got, err := DatabaseName(id)
			if err != nil || got != want {
				t.Errorf("DatabaseName(%q) = %q, %v; want %q", id, got, err, want)
			}
		}
		if got, _ := DatabaseName("core"); got == CoreDatabaseName {
			t.Error("a module named core would collide with core's own database")
		}
		for _, bad := range []string{"", "Storage", "a_b", `a"b`, "1abc", "a b", "a;b", strings.Repeat("x", 60)} {
			if _, err := DatabaseName(bad); err == nil {
				t.Errorf("DatabaseName(%q) accepted an invalid id", bad)
			}
		}
		// Distinct ids never share a database.
		a, _ := DatabaseName("a-b")
		b, _ := DatabaseName("a-c")
		if a == b {
			t.Error("distinct ids mapped to one database")
		}
	})

	scheme := func(t *testing.T) *runtime.Scheme {
		t.Helper()
		s := runtime.NewScheme()
		_ = corev1.AddToScheme(s)
		_ = boothv1alpha1.AddToScheme(s)
		return s
	}
	ns := func(name string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	mod := func(id, namespace string, enabled *bool) *boothv1alpha1.BoothModule {
		m := &boothv1alpha1.BoothModule{
			ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: namespace, UID: types.UID("uid-" + id)},
			Spec: boothv1alpha1.BoothModuleSpec{
				ID: id, ServiceRef: boothv1alpha1.ServiceReference{Name: id, Port: 80},
			},
		}
		if enabled != nil {
			m.Spec.Database = &boothv1alpha1.DatabaseRequirement{Enabled: *enabled}
		}
		return m
	}
	yes, no := true, false
	getSecret := func(t *testing.T, c client.Client, namespace, name string) (*corev1.Secret, bool) {
		t.Helper()
		var s corev1.Secret
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &s)
		if apierrors.IsNotFound(err) {
			return nil, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return &s, true
	}

	t.Run("Provisioner", func(t *testing.T) {
		admin := newAdmin(t, func(c *Config) { c.ModuleHost = c.Host })
		newP := func(t *testing.T, objs ...client.Object) (*Provisioner, client.Client) {
			t.Helper()
			c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
			return NewProvisioner(c, admin), c
		}

		t.Run("delivers a working DSN in a Secret owned by the module", func(t *testing.T) {
			p, c := newP(t, ns("p1"))
			m := mod("prov-one", "p1", &yes)
			if err := p.Ensure(ctx, m); err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			sec, ok := getSecret(t, c, "p1", CredentialsSecretName)
			if !ok {
				t.Fatal("no credential Secret")
			}
			for _, k := range []string{KeyDSN, KeyHost, KeyPort, KeyDatabase, KeyUsername, KeyPassword} {
				if sec.StringData[k] == "" {
					t.Errorf("Secret is missing key %q", k)
				}
			}
			if sec.StringData[KeyDatabase] != "booth_mod_prov_one" {
				t.Errorf("database = %q", sec.StringData[KeyDatabase])
			}
			if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Name != "prov-one" {
				t.Errorf("ownerReferences = %v, want the BoothModule", sec.OwnerReferences)
			}

			conn, err := pgx.Connect(ctx, sec.StringData[KeyDSN])
			if err != nil {
				t.Fatalf("the DSN we handed out doesn't work: %v", err)
			}
			defer conn.Close(ctx)
			if _, err := conn.Exec(ctx, "CREATE TABLE ok (x int)"); err != nil {
				t.Errorf("module can't create tables in its database: %v", err)
			}

			var n corev1.Namespace
			_ = c.Get(ctx, types.NamespacedName{Name: "p1"}, &n)
			if n.Labels[ClientNamespaceLabel] != "true" {
				t.Error("namespace not labelled for the PostgreSQL NetworkPolicy")
			}
		})

		t.Run("a second reconcile keeps the same password", func(t *testing.T) {
			p, c := newP(t, ns("p2"))
			m := mod("prov-two", "p2", &yes)
			_ = p.Ensure(ctx, m)
			first, _ := getSecret(t, c, "p2", CredentialsSecretName)
			if err := p.Ensure(ctx, m); err != nil {
				t.Fatal(err)
			}
			second, _ := getSecret(t, c, "p2", CredentialsSecretName)
			if first.StringData[KeyPassword] != second.StringData[KeyPassword] {
				t.Fatal("password changed on a no-op reconcile: a running module's connection would break")
			}
		})

		t.Run("no database signal, no database", func(t *testing.T) {
			p, c := newP(t, ns("p3"))
			for _, m := range []*boothv1alpha1.BoothModule{mod("prov-three", "p3", nil), mod("prov-three", "p3", &no)} {
				if err := p.Ensure(ctx, m); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok := getSecret(t, c, "p3", CredentialsSecretName); ok {
				t.Fatal("a module that didn't ask for a database got credentials")
			}
			if ok, _ := admin.Exists(ctx, "booth_mod_prov_three"); ok {
				t.Fatal("a database was created for a module that didn't ask for one")
			}
		})

		t.Run("disabling removes the Secret but never the data", func(t *testing.T) {
			p, c := newP(t, ns("p4"))
			m := mod("prov-four", "p4", &yes)
			_ = p.Ensure(ctx, m)
			sec, _ := getSecret(t, c, "p4", CredentialsSecretName)
			conn, _ := pgx.Connect(ctx, sec.StringData[KeyDSN])
			_, _ = conn.Exec(ctx, "CREATE TABLE precious (v text); INSERT INTO precious VALUES ('irreplaceable')")
			conn.Close(ctx)

			m.Spec.Database = &boothv1alpha1.DatabaseRequirement{Enabled: false}
			if err := p.Ensure(ctx, m); err != nil {
				t.Fatal(err)
			}
			if _, ok := getSecret(t, c, "p4", CredentialsSecretName); ok {
				t.Error("credential Secret outlived the database signal")
			}
			if ok, _ := admin.Exists(ctx, "booth_mod_prov_four"); !ok {
				t.Fatal("the database was dropped: data must never be deleted automatically")
			}

			// Re-enabling reconnects to the same data.
			m.Spec.Database.Enabled = true
			if err := p.Ensure(ctx, m); err != nil {
				t.Fatal(err)
			}
			sec, _ = getSecret(t, c, "p4", CredentialsSecretName)
			conn, err := pgx.Connect(ctx, sec.StringData[KeyDSN])
			if err != nil {
				t.Fatalf("reconnect after re-enable: %v", err)
			}
			defer conn.Close(ctx)
			var v string
			if err := conn.QueryRow(ctx, "SELECT v FROM precious").Scan(&v); err != nil || v != "irreplaceable" {
				t.Errorf("data after re-enable = %q, %v", v, err)
			}
		})

		t.Run("losing the Secret (an uninstall/reinstall) keeps the data and issues a new password", func(t *testing.T) {
			p, c := newP(t, ns("p5"))
			m := mod("prov-five", "p5", &yes)
			_ = p.Ensure(ctx, m)
			old, _ := getSecret(t, c, "p5", CredentialsSecretName)
			conn, _ := pgx.Connect(ctx, old.StringData[KeyDSN])
			_, _ = conn.Exec(ctx, "CREATE TABLE survivor (v text); INSERT INTO survivor VALUES ('kept')")
			conn.Close(ctx)

			if err := c.Delete(ctx, old); err != nil { // garbage-collected with the BoothModule
				t.Fatal(err)
			}
			if err := p.Ensure(ctx, m); err != nil {
				t.Fatal(err)
			}
			fresh, _ := getSecret(t, c, "p5", CredentialsSecretName)
			if fresh.StringData[KeyPassword] == old.StringData[KeyPassword] {
				t.Error("expected a new password after the Secret was lost")
			}
			if _, err := pgx.Connect(ctx, old.StringData[KeyDSN]); err == nil {
				t.Error("the previous credential still works")
			}
			nc, err := pgx.Connect(ctx, fresh.StringData[KeyDSN])
			if err != nil {
				t.Fatalf("new credential rejected: %v", err)
			}
			defer nc.Close(ctx)
			var v string
			if err := nc.QueryRow(ctx, "SELECT v FROM survivor").Scan(&v); err != nil || v != "kept" {
				t.Errorf("data after reinstall = %q, %v", v, err)
			}
		})

		t.Run("a server that lost the role is rebuilt with the existing password", func(t *testing.T) {
			p, c := newP(t, ns("p6"))
			m := mod("prov-six", "p6", &yes)
			_ = p.Ensure(ctx, m)
			sec, _ := getSecret(t, c, "p6", CredentialsSecretName)

			// Simulate a restored/recreated server: drop the database and role behind our back.
			for _, q := range []string{"DROP DATABASE booth_mod_prov_six", "DROP ROLE booth_mod_prov_six"} {
				if _, err := admin.pool.Exec(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			p.Now = func() time.Time { return time.Now().Add(recheckInterval + time.Minute) } // past the recheck window

			if err := p.Ensure(ctx, m); err != nil {
				t.Fatal(err)
			}
			after, _ := getSecret(t, c, "p6", CredentialsSecretName)
			if after.StringData[KeyPassword] != sec.StringData[KeyPassword] {
				t.Error("password changed; the running module's credential would have been invalidated")
			}
			conn, err := pgx.Connect(ctx, sec.StringData[KeyDSN])
			if err != nil {
				t.Fatalf("original credential doesn't work against the rebuilt database: %v", err)
			}
			conn.Close(ctx)
		})

		t.Run("moving the server rewrites the DSN but not the password", func(t *testing.T) {
			p, c := newP(t, ns("p7"))
			m := mod("prov-seven", "p7", &yes)
			_ = p.Ensure(ctx, m)
			before, _ := getSecret(t, c, "p7", CredentialsSecretName)

			moved := newAdmin(t, func(cfg *Config) { cfg.ModuleHost = "127.0.0.1" }) // a different address for the same server
			p.Admin = moved
			if err := p.Ensure(ctx, m); err != nil {
				t.Fatal(err)
			}
			after, _ := getSecret(t, c, "p7", CredentialsSecretName)
			if after.StringData[KeyDSN] == before.StringData[KeyDSN] {
				t.Error("DSN not updated after the endpoint changed")
			}
			if !strings.Contains(after.StringData[KeyDSN], "127.0.0.1") {
				t.Errorf("DSN = %q, want the new host", after.StringData[KeyDSN])
			}
			if after.StringData[KeyPassword] != before.StringData[KeyPassword] {
				t.Error("password changed just because the endpoint did")
			}
		})

		t.Run("a Secret in another namespace isn't owner-referenced", func(t *testing.T) {
			p, c := newP(t, ns("crd-ns"), ns("pod-ns"))
			m := mod("prov-eight", "crd-ns", &yes)
			m.Spec.ServiceRef.Namespace = "pod-ns"
			if err := p.Ensure(ctx, m); err != nil {
				t.Fatal(err)
			}
			sec, ok := getSecret(t, c, "pod-ns", CredentialsSecretName)
			if !ok {
				t.Fatal("Secret should land in the module's own namespace")
			}
			if len(sec.OwnerReferences) != 0 {
				t.Errorf("cross-namespace ownerReference is invalid in Kubernetes: %v", sec.OwnerReferences)
			}
		})

		t.Run("an invalid module id is rejected, not provisioned", func(t *testing.T) {
			p, _ := newP(t, ns("p9"))
			if err := p.Ensure(ctx, mod("Bad_ID", "p9", &yes)); err == nil {
				t.Fatal("expected an error for an invalid module id")
			}
		})

		t.Run("core's own database uses the same mechanism", func(t *testing.T) {
			p, c := newP(t, ns("booth-system"))
			dsn, err := p.EnsureCore(ctx, "booth-system")
			if err != nil {
				t.Fatal(err)
			}
			conn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatalf("core DSN doesn't work: %v", err)
			}
			conn.Close(ctx)

			again, err := p.EnsureCore(ctx, "booth-system")
			if err != nil || again != dsn {
				t.Errorf("EnsureCore not stable: %q vs %q (%v)", again, dsn, err)
			}
			sec, _ := getSecret(t, c, "booth-system", CoreCredentialsSecretName)
			if sec == nil || sec.StringData[KeyDatabase] != CoreDatabaseName {
				t.Errorf("core credentials Secret = %v", sec)
			}
		})

		// Two modules through the provisioner, end to end: the isolation guarantee holds for
		// credentials actually handed out, not only for hand-built ones.
		t.Run("credentials it hands out can't cross module boundaries", func(t *testing.T) {
			p, c := newP(t, ns("iso-a"), ns("iso-b"))
			_ = p.Ensure(ctx, mod("iso-a", "iso-a", &yes))
			_ = p.Ensure(ctx, mod("iso-b", "iso-b", &yes))
			a, _ := getSecret(t, c, "iso-a", CredentialsSecretName)
			b, _ := getSecret(t, c, "iso-b", CredentialsSecretName)

			crossed := strings.Replace(a.StringData[KeyDSN], "/booth_mod_iso_a", "/booth_mod_iso_b", 1)
			if crossed == a.StringData[KeyDSN] {
				t.Fatal("test bug: DSN didn't contain the expected database name")
			}
			if conn, err := pgx.Connect(ctx, crossed); err == nil {
				conn.Close(ctx)
				t.Fatal("iso-a's credential opened iso-b's database")
			}
			if conn, err := pgx.Connect(ctx, b.StringData[KeyDSN]); err != nil {
				t.Fatalf("iso-b can't open its own database: %v", err)
			} else {
				conn.Close(ctx)
			}
		})
	})
}

func TestEnsureAdminPassword(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()

	first, err := EnsureAdminPassword(ctx, c, "booth-system")
	if err != nil || len(first) < 32 {
		t.Fatalf("first = %q, %v", first, err)
	}
	again, err := EnsureAdminPassword(ctx, c, "booth-system")
	if err != nil || again != first {
		t.Fatalf("not stable across calls: %q vs %q (%v)", again, first, err)
	}
}

func parsePort(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, context.Canceled
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
