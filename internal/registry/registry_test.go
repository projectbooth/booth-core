package registry

import (
	"strings"
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

// The documented default (contracts/module-manifest.md): an unset serviceRef.namespace means
// the BoothModule's own namespace. Not applying it produced `name..svc.cluster.local`, marking
// every such module Unreachable and making every gateway call 502.
func TestModuleBaseURL_DefaultsServiceNamespaceToTheResourcesOwn(t *testing.T) {
	m := Module{
		Namespace: "booth-system",
		Spec: boothv1alpha1.BoothModuleSpec{
			ServiceRef: boothv1alpha1.ServiceReference{Name: "booth-storage", Port: 8080}, // no namespace
		},
	}
	got := m.BaseURL()
	if got != "http://booth-storage.booth-system.svc.cluster.local:8080" {
		t.Fatalf("BaseURL() = %q, want the resource's namespace applied", got)
	}
	if strings.Contains(got, "..") {
		t.Fatalf("BaseURL() = %q contains an empty namespace segment", got)
	}
}

func TestModuleBaseURL_ExplicitServiceNamespaceWinsOverTheDefault(t *testing.T) {
	m := Module{
		Namespace: "crds-live-here",
		Spec: boothv1alpha1.BoothModuleSpec{
			ServiceRef: boothv1alpha1.ServiceReference{Name: "svc", Namespace: "pods-live-here", Port: 80},
		},
	}
	if got, want := m.BaseURL(), "http://svc.pods-live-here.svc.cluster.local:80"; got != want {
		t.Fatalf("BaseURL() = %q, want %q", got, want)
	}
}

// If no namespace can be determined at all, never emit a malformed `name..svc` address.
func TestModuleBaseURL_NeverEmitsAnEmptyNamespaceSegment(t *testing.T) {
	m := Module{Spec: boothv1alpha1.BoothModuleSpec{
		ServiceRef: boothv1alpha1.ServiceReference{Name: "svc", Port: 80},
	}}
	got := m.BaseURL()
	if strings.Contains(got, "..") {
		t.Fatalf("BaseURL() = %q contains an empty namespace segment", got)
	}
	if got != "http://svc:80" {
		t.Errorf("BaseURL() = %q, want the bare service name", got)
	}
}
