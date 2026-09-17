package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
)

func reqFor(name, namespace string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
}

func TestReconcile_UpsertsHealthyModuleIntoRegistry(t *testing.T) {
	healthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthServer.Close()

	mod := &boothv1alpha1.BoothModule{
		ObjectMeta: metav1.ObjectMeta{Name: "storage", Namespace: "booth-system"},
		Spec: boothv1alpha1.BoothModuleSpec{
			ID:              "storage",
			DisplayName:     "Storage",
			Version:         "0.1.0",
			ContractVersion: "0.1.0",
			HasOwnUI:        true,
			HealthCheckPath: "/health",
			ServiceRef: boothv1alpha1.ServiceReference{
				Name:      "storage-svc",
				Namespace: "booth-system",
				Port:      8080,
			},
		},
	}

	scheme := runtime.NewScheme()
	_ = boothv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&boothv1alpha1.BoothModule{}).
		WithObjects(mod).
		Build()

	reg := New()
	rc := NewController(c, reg)
	rc.HTTPClient = healthServer.Client()
	rc.HealthCheckURL = func(spec boothv1alpha1.BoothModuleSpec) string {
		return healthServer.URL + spec.HealthCheckPath
	}

	if _, err := rc.Reconcile(context.Background(), reqFor("storage", "booth-system")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got, ok := reg.Get("storage")
	if !ok {
		t.Fatal("expected module to be registered")
	}
	if got.Status.Phase != boothv1alpha1.ModulePhaseHealthy {
		t.Fatalf("Phase = %v, want Healthy", got.Status.Phase)
	}
}

func TestReconcile_MarksUnreachableModuleWhenHealthCheckFails(t *testing.T) {
	mod := &boothv1alpha1.BoothModule{
		ObjectMeta: metav1.ObjectMeta{Name: "pipeline", Namespace: "booth-system"},
		Spec: boothv1alpha1.BoothModuleSpec{
			ID:              "pipeline",
			DisplayName:     "Pipeline",
			Version:         "0.1.0",
			ContractVersion: "0.1.0",
			HealthCheckPath: "/health",
			ServiceRef:      boothv1alpha1.ServiceReference{Name: "nope", Namespace: "booth-system", Port: 1},
		},
	}

	scheme := runtime.NewScheme()
	_ = boothv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&boothv1alpha1.BoothModule{}).
		WithObjects(mod).
		Build()

	reg := New()
	rc := NewController(c, reg)
	rc.HealthCheckURL = func(spec boothv1alpha1.BoothModuleSpec) string {
		return "http://127.0.0.1:1" + spec.HealthCheckPath // nothing listens here
	}

	if _, err := rc.Reconcile(context.Background(), reqFor("pipeline", "booth-system")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got, ok := reg.Get("pipeline")
	if !ok {
		t.Fatal("expected module to be registered even when unhealthy")
	}
	if got.Status.Phase != boothv1alpha1.ModulePhaseUnreachable {
		t.Fatalf("Phase = %v, want Unreachable", got.Status.Phase)
	}
}

func TestReconcile_RemovesModuleOnDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = boothv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&boothv1alpha1.BoothModule{}).Build()

	reg := New()
	reg.Put(Module{Spec: boothv1alpha1.BoothModuleSpec{ID: "storage"}})

	rc := NewController(c, reg)
	if _, err := rc.Reconcile(context.Background(), reqFor("storage", "booth-system")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, ok := reg.Get("storage"); ok {
		t.Fatal("expected module to be removed from registry after deletion")
	}
}
