package registry

import (
	"testing"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
)

func TestRegistryPutGetListDelete(t *testing.T) {
	r := New()

	r.Put(Module{Spec: boothv1alpha1.BoothModuleSpec{ID: "storage", DisplayName: "Storage"}})
	r.Put(Module{Spec: boothv1alpha1.BoothModuleSpec{ID: "catalog", DisplayName: "Catalog"}})

	if _, ok := r.Get("storage"); !ok {
		t.Fatal("expected storage to be present")
	}

	if len(r.List()) != 2 {
		t.Fatalf("List() len = %d, want 2", len(r.List()))
	}

	r.Delete("storage")
	if _, ok := r.Get("storage"); ok {
		t.Fatal("expected storage to be removed")
	}
	if len(r.List()) != 1 {
		t.Fatalf("List() len after delete = %d, want 1", len(r.List()))
	}
}

func TestModuleBaseURL(t *testing.T) {
	clusterModule := Module{Spec: boothv1alpha1.BoothModuleSpec{
		ServiceRef: boothv1alpha1.ServiceReference{Name: "storage-svc", Namespace: "booth-system", Port: 8080},
	}}
	if got, want := clusterModule.BaseURL(), "http://storage-svc.booth-system.svc.cluster.local:8080"; got != want {
		t.Errorf("cluster BaseURL() = %q, want %q", got, want)
	}

	devModule := Module{Spec: boothv1alpha1.BoothModuleSpec{
		ServiceRef: boothv1alpha1.ServiceReference{Name: "localhost:9001"},
	}}
	if got, want := devModule.BaseURL(), "http://localhost:9001"; got != want {
		t.Errorf("dev BaseURL() = %q, want %q", got, want)
	}
}
