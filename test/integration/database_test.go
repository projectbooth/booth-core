package integration

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/dbprov"
	"github.com/projectbooth/booth-core/internal/testpg"
)

// TestDatabaseProvisioning_RealAPIServerAndRealPostgres runs ADR 0053 against both real
// halves: a kube-apiserver (the CRD schema must admit the `database` field, Secrets round-trip
// StringData to Data, and the ownerReference must be one Kubernetes accepts) and a PostgreSQL
// server (the DSN we deliver must actually work).
func TestDatabaseProvisioning_RealAPIServerAndRealPostgres(t *testing.T) {
	pg := testpg.Start(t)

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := boothv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	for _, name := range []string{"booth-system", "booth-storage"} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}

	port, _ := strconv.Atoi(pg.Port)
	admin, err := dbprov.NewAdmin(ctx, dbprov.Config{
		Host: pg.Host, Port: port, AdminUser: pg.User, AdminPassword: pg.Pass, SSLMode: "disable",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	p := dbprov.NewProvisioner(c, admin)

	// The bundled server's admin password: generated once, stable afterwards.
	pw1, err := dbprov.EnsureAdminPassword(ctx, c, "booth-system")
	if err != nil {
		t.Fatal(err)
	}
	pw2, err := dbprov.EnsureAdminPassword(ctx, c, "booth-system")
	if err != nil || pw1 != pw2 {
		t.Fatalf("admin password not stable against a real API server: %q vs %q (%v)", pw1, pw2, err)
	}

	mod := &boothv1alpha1.BoothModule{
		ObjectMeta: metav1.ObjectMeta{Name: "storage", Namespace: "booth-storage"},
		Spec: boothv1alpha1.BoothModuleSpec{
			ID: "storage", DisplayName: "Storage", Version: "0.1.0", ContractVersion: "0.1.0",
			HealthCheckPath: "/health",
			ServiceRef:      boothv1alpha1.ServiceReference{Name: "storage", Namespace: "booth-storage", Port: 8080},
			Database:        &boothv1alpha1.DatabaseRequirement{Enabled: true},
		},
	}
	if err := c.Create(ctx, mod); err != nil {
		t.Fatalf("the API server rejected a BoothModule with database.enabled: %v", err)
	}

	if err := p.Ensure(ctx, mod); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-storage", Name: dbprov.CredentialsSecretName}, &sec); err != nil {
		t.Fatalf("credential Secret: %v", err)
	}
	if string(sec.Data[dbprov.KeyDatabase]) != "booth_mod_storage" {
		t.Errorf("database = %q", sec.Data[dbprov.KeyDatabase])
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].UID != mod.UID {
		t.Errorf("ownerReferences = %v, want the BoothModule (uid %s)", sec.OwnerReferences, mod.UID)
	}

	// The DSN read back out of a real Secret works against the real server.
	conn, err := pgx.Connect(ctx, string(sec.Data[dbprov.KeyDSN]))
	if err != nil {
		t.Fatalf("delivered DSN doesn't work: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE TABLE ok (x int)"); err != nil {
		t.Errorf("module can't use its database: %v", err)
	}

	// A second reconcile keeps the same credential.
	before := string(sec.Data[dbprov.KeyPassword])
	if err := p.Ensure(ctx, mod); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-storage", Name: dbprov.CredentialsSecretName}, &sec); err != nil {
		t.Fatal(err)
	}
	if string(sec.Data[dbprov.KeyPassword]) != before {
		t.Error("credential changed on a no-op reconcile against a real API server")
	}
}
