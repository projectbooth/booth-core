package gateway

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
	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/registry"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func ok200() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}
}

// reconciledRegistry runs the real registry controller (with its real default health-check
// builder) over a BoothModule that leaves serviceRef.namespace unset, exactly as
// booth-storage, booth-catalog and booth-module-store's charts do, and returns the registry
// it populated.
func reconciledRegistry(t *testing.T, healthDialed *string) *registry.Registry {
	t.Helper()
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

	reg := registry.New()
	rc := registry.NewController(c, reg)
	rc.HTTPClient = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		*healthDialed = r.URL.Host
		return ok200(), nil
	})}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "storage", Namespace: "booth-system"}}
	if _, err := rc.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return reg
}

func viewerRequest(path string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer tok")
	return req.WithContext(auth.WithIdentityForTesting(req.Context(), auth.Identity{
		Claims: &auth.Claims{Subject: "u1"},
		Active: auth.Membership{Workspace: "acme", Role: auth.RoleViewer},
	}))
}

// End to end from BoothModule to the wire: a module with no serviceRef.namespace is health
// checked AND routed to at its resource's namespace. Before the fix both used
// `booth-storage..svc.cluster.local`, so the module showed Unreachable and every gateway call
// to it returned 502 — the fleet-wide failure booth-e2e found.
func TestGateway_RoutesAModuleWithNoServiceNamespaceToItsResourcesNamespace(t *testing.T) {
	var healthHost string
	reg := reconciledRegistry(t, &healthHost)

	const want = "booth-storage.booth-system.svc.cluster.local:8080"
	if healthHost != want {
		t.Fatalf("health check went to %q, want %q", healthHost, want)
	}

	var routedHost string
	gw := New(reg)
	gw.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		routedHost = r.URL.Host
		return ok200(), nil
	})
	h := gw.Handler(func(*http.Request) string { return "storage" }, func(*http.Request) string { return "/things" })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, viewerRequest("/modules/storage/things"))

	if rec.Code != http.StatusOK {
		t.Fatalf("gateway status = %d, want 200 (it was 502 before the fix)", rec.Code)
	}
	if routedHost != want {
		t.Fatalf("gateway routed to %q, want %q", routedHost, want)
	}
}

// ADR 0041's forged-role check, on core's side: the gateway must reach the module (not 502)
// and must overwrite any role/workspace the client tried to assert with the values it derived
// from the verified token. booth-e2e's version of this check was masked by the 502 above.
func TestGateway_ForgedRoleAndWorkspaceHeadersNeverReachTheModule(t *testing.T) {
	var healthHost string
	reg := reconciledRegistry(t, &healthHost)

	var gotRole, gotWorkspace string
	gw := New(reg)
	gw.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		gotRole = r.Header.Get(auth.HeaderBoothRole)
		gotWorkspace = r.Header.Get(auth.HeaderBoothWorkspace)
		return ok200(), nil
	})
	h := gw.Handler(func(*http.Request) string { return "storage" }, func(*http.Request) string { return "/admin" })

	req := viewerRequest("/modules/storage/admin")
	req.Header.Set(auth.HeaderBoothRole, "owner")          // a viewer claiming to be an owner
	req.Header.Set(auth.HeaderBoothWorkspace, "victim-ws") // and to be in someone else's workspace

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: the request never reached the module, so this check would prove nothing", rec.Code)
	}
	if gotRole != "viewer" || gotWorkspace != "acme" {
		t.Fatalf("module saw role=%q workspace=%q, want the token-derived viewer/acme (a forged header got through)", gotRole, gotWorkspace)
	}
}
