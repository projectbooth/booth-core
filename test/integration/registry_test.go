// Package integration runs booth-core's registry controller against a real (test)
// Kubernetes API server via envtest — contracts/testing-strategy.md's layer 3, "Verifies
// your BoothModule CRD (ADR 0019) reconciles correctly." This is lighter than the full
// kind/k3d cluster CI describes (no container runtime needed, no actual pod scheduling),
// but exercises the real CRD registration, watch, and status-subresource update path
// against a real kube-apiserver + etcd, which the fake client in
// internal/registry/controller_test.go deliberately does not.
package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/registry"
)

func reqFor(name, namespace string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
}

func TestRegistryController_ReconcilesAgainstRealAPIServer(t *testing.T) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest environment: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("stopping envtest environment: %v", err)
		}
	})

	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := boothv1alpha1.AddToScheme(sch); err != nil {
		t.Fatalf("adding booth v1alpha1 scheme: %v", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mod := &boothv1alpha1.BoothModule{
		ObjectMeta: metav1.ObjectMeta{Name: "storage", Namespace: "default"},
		Spec: boothv1alpha1.BoothModuleSpec{
			ID:              "storage",
			DisplayName:     "Storage",
			Version:         "0.1.0",
			ContractVersion: "0.1.0",
			HasOwnUI:        true,
			HealthCheckPath: "/health",
			ServiceRef: boothv1alpha1.ServiceReference{
				Name:      "storage-svc",
				Namespace: "default",
				Port:      8080,
			},
		},
	}
	if err := k8sClient.Create(ctx, mod); err != nil {
		t.Fatalf("creating BoothModule against real API server: %v", err)
	}

	// Confirm the CRD's OpenAPI schema (generated from api/v1alpha1's kubebuilder
	// markers) actually accepted the object as-is — this is the real, cluster-side
	// half of the contract test in test/contract, which only checks Go-level
	// (de)serialization.
	var fetched boothv1alpha1.BoothModule
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "storage", Namespace: "default"}, &fetched); err != nil {
		t.Fatalf("fetching created BoothModule: %v", err)
	}
	if fetched.Spec.ID != "storage" {
		t.Fatalf("fetched.Spec.ID = %q, want storage", fetched.Spec.ID)
	}

	reg := registry.New()
	rc := registry.NewController(k8sClient, reg)
	rc.HealthCheckURL = func(spec boothv1alpha1.BoothModuleSpec) string {
		return backend.URL + spec.HealthCheckPath
	}

	if _, err := rc.Reconcile(ctx, reqFor("storage", "default")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got, ok := reg.Get("storage")
	if !ok {
		t.Fatal("expected module in registry after reconcile")
	}
	if got.Status.Phase != boothv1alpha1.ModulePhaseHealthy {
		t.Fatalf("Phase = %v, want Healthy", got.Status.Phase)
	}

	// Confirm the status subresource write actually landed against the real API
	// server (RBAC/subresource wiring bugs are exactly the class of thing the fake
	// client can't catch).
	var updated boothv1alpha1.BoothModule
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "storage", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("re-fetching BoothModule: %v", err)
	}
	if updated.Status.Phase != boothv1alpha1.ModulePhaseHealthy {
		t.Fatalf("persisted Status.Phase = %v, want Healthy", updated.Status.Phase)
	}

	// Delete and reconcile once more: registry should drop the module.
	if err := k8sClient.Delete(ctx, &fetched); err != nil {
		t.Fatalf("deleting BoothModule: %v", err)
	}
	if _, err := rc.Reconcile(ctx, reqFor("storage", "default")); err != nil {
		t.Fatalf("Reconcile after delete: %v", err)
	}
	if _, ok := reg.Get("storage"); ok {
		t.Fatal("expected module to be removed from registry after deletion")
	}
}
