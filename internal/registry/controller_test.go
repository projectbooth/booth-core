package registry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

type recordingProvisioner struct {
	calls []string
	err   error
}

func (r *recordingProvisioner) Ensure(_ context.Context, mod *boothv1alpha1.BoothModule) error {
	r.calls = append(r.calls, mod.Spec.ID)
	return r.err
}

func newControllerWithModule(t *testing.T) (*Controller, *Registry) {
	t.Helper()
	mod := &boothv1alpha1.BoothModule{
		ObjectMeta: metav1.ObjectMeta{Name: "superset", Namespace: "booth-system"},
		Spec: boothv1alpha1.BoothModuleSpec{
			ID: "superset", HealthCheckPath: "/health",
			ServiceRef: boothv1alpha1.ServiceReference{Name: "s", Namespace: "booth-system", Port: 1},
		},
	}
	scheme := runtime.NewScheme()
	_ = boothv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&boothv1alpha1.BoothModule{}).WithObjects(mod).Build()
	reg := New()
	rc := NewController(c, reg)
	rc.HealthCheckURL = func(boothv1alpha1.BoothModuleSpec) string { return "http://127.0.0.1:1/health" }
	return rc, reg
}

// ADR 0049: every reconcile asks the event-bus provisioner to make the module's
// credentials match its manifest.
func TestReconcile_ProvisionsEventBusCredentials(t *testing.T) {
	rc, _ := newControllerWithModule(t)
	rec := &recordingProvisioner{}
	rc.EventBus = rec

	if _, err := rc.Reconcile(context.Background(), reqFor("superset", "booth-system")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rec.calls) != 1 || rec.calls[0] != "superset" {
		t.Fatalf("provisioner calls = %v, want one call for superset", rec.calls)
	}
}

// A credential failure must surface (so it's retried with backoff) but must not stop the
// module's health from being recorded in the registry.
func TestReconcile_ProvisioningErrorIsReturnedButHealthStillRecorded(t *testing.T) {
	rc, reg := newControllerWithModule(t)
	rc.EventBus = &recordingProvisioner{err: errors.New("boom")}

	if _, err := rc.Reconcile(context.Background(), reqFor("superset", "booth-system")); err == nil {
		t.Fatal("expected the provisioning error to be returned so the reconcile is retried")
	}
	if _, ok := reg.Get("superset"); !ok {
		t.Fatal("module should still be in the registry when provisioning fails")
	}
}

func TestReconcile_NoProvisionerIsFine(t *testing.T) {
	rc, _ := newControllerWithModule(t)
	if _, err := rc.Reconcile(context.Background(), reqFor("superset", "booth-system")); err != nil {
		t.Fatalf("Reconcile without an event-bus provisioner: %v", err)
	}
}

// ADR 0053: the database provisioner is called on every reconcile, independently of the
// event-bus one — a failure in one must not skip the other.
func TestReconcile_ProvisionsDatabaseAndKeepsGoingWhenEventBusFails(t *testing.T) {
	rc, _ := newControllerWithModule(t)
	bus := &recordingProvisioner{err: errors.New("bus down")}
	db := &recordingProvisioner{}
	rc.EventBus = bus
	rc.Database = db

	_, err := rc.Reconcile(context.Background(), reqFor("superset", "booth-system"))
	if err == nil {
		t.Fatal("expected the event-bus failure to be returned")
	}
	if len(db.calls) != 1 {
		t.Fatalf("database provisioner calls = %v, want 1: an event-bus failure must not skip it", db.calls)
	}
	if !strings.Contains(err.Error(), "event-bus") {
		t.Errorf("error %q doesn't say which provisioner failed", err)
	}
}

func TestReconcile_DatabaseProvisioningErrorIsReturned(t *testing.T) {
	rc, reg := newControllerWithModule(t)
	rc.Database = &recordingProvisioner{err: errors.New("postgres not up yet")}

	if _, err := rc.Reconcile(context.Background(), reqFor("superset", "booth-system")); err == nil {
		t.Fatal("expected the error so the reconcile is retried with backoff")
	}
	if _, ok := reg.Get("superset"); !ok {
		t.Fatal("module health must still be recorded while its database can't be provisioned")
	}
}

// roundTripFunc lets a test observe exactly what the controller dials without DNS or a server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}
}

// Reproduces booth-e2e's finding using the controller's REAL default health-check builder
// (no HealthCheckURL override): a module with no serviceRef.namespace must be checked at, and
// routed to, its resource's namespace — not at `name..svc.cluster.local`.
func TestReconcile_AppliesDefaultServiceNamespace(t *testing.T) {
	mod := &boothv1alpha1.BoothModule{
		ObjectMeta: metav1.ObjectMeta{Name: "storage", Namespace: "booth-system"},
		Spec: boothv1alpha1.BoothModuleSpec{
			ID: "storage", HealthCheckPath: "/health",
			ServiceRef: boothv1alpha1.ServiceReference{Name: "booth-storage", Port: 8080}, // no namespace
		},
	}
	scheme := runtime.NewScheme()
	_ = boothv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&boothv1alpha1.BoothModule{}).WithObjects(mod).Build()

	reg := New()
	rc := NewController(c, reg) // the real HealthCheckURL builder
	var dialed string
	rc.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		dialed = r.URL.String()
		return okResponse(), nil
	})}

	if _, err := rc.Reconcile(context.Background(), reqFor("storage", "booth-system")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if want := "http://booth-storage.booth-system.svc.cluster.local:8080/health"; dialed != want {
		t.Fatalf("health check dialed %q, want %q", dialed, want)
	}
	got, ok := reg.Get("storage")
	if !ok {
		t.Fatal("module missing from the registry")
	}
	if got.Status.Phase != boothv1alpha1.ModulePhaseHealthy {
		t.Errorf("Phase = %s, want Healthy (it was Unreachable before the fix)", got.Status.Phase)
	}
	// The gateway routes with the same registry entry, so it must resolve identically.
	if want := "http://booth-storage.booth-system.svc.cluster.local:8080"; got.BaseURL() != want {
		t.Errorf("gateway BaseURL() = %q, want %q", got.BaseURL(), want)
	}
}

func TestReconcile_ExplicitServiceNamespaceIsRespected(t *testing.T) {
	mod := &boothv1alpha1.BoothModule{
		ObjectMeta: metav1.ObjectMeta{Name: "storage", Namespace: "crds-live-here"},
		Spec: boothv1alpha1.BoothModuleSpec{
			ID: "storage", HealthCheckPath: "/health",
			ServiceRef: boothv1alpha1.ServiceReference{Name: "svc", Namespace: "pods-live-here", Port: 80},
		},
	}
	scheme := runtime.NewScheme()
	_ = boothv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&boothv1alpha1.BoothModule{}).WithObjects(mod).Build()

	rc := NewController(c, New())
	var dialed string
	rc.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		dialed = r.URL.Host
		return okResponse(), nil
	})}
	if _, err := rc.Reconcile(context.Background(), reqFor("storage", "crds-live-here")); err != nil {
		t.Fatal(err)
	}
	if want := "svc.pods-live-here.svc.cluster.local:80"; dialed != want {
		t.Fatalf("dialed %q, want %q", dialed, want)
	}
}
