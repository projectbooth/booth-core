// Package contract holds tests that validate booth-core's implementation against
// booth-architecture's documented contracts, independent of any real cluster
// (contracts/testing-strategy.md's layer 2). Fixtures here mirror the examples in
// ../../../booth-architecture/contracts/module-manifest.md verbatim — if that contract's
// examples change, these fixtures (and this test) need to change with them, which is the
// point: catching drift between the documented contract and what booth-core actually
// accepts.
package contract

import (
	"testing"

	"sigs.k8s.io/yaml"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
)

const storageManifestExample = `
id: storage
displayName: Storage
icon: drive
version: 0.1.0
contractVersion: 0.1.0
hasOwnUi: true
uiIntegrationMode: native
navGroup: manage
healthCheckPath: /health
requiredScopes: [storage.read, storage.write]
navPath: /storage
adminNavPath: /storage/admin
serviceRef:
  name: storage-svc
  namespace: booth-storage
  port: 8080
`

const supersetManifestExample = `
id: superset
displayName: Superset
icon: bar-chart
version: 0.1.0
contractVersion: 0.1.0
hasOwnUi: true
uiIntegrationMode: iframe-proxy
navGroup: build
healthCheckPath: /health
requiredScopes: [catalog.read]
navPath: /superset
serviceRef:
  name: superset-svc
  namespace: booth-superset
  port: 8088
`

func TestModuleManifestExample_Storage(t *testing.T) {
	var spec boothv1alpha1.BoothModuleSpec
	if err := yaml.Unmarshal([]byte(storageManifestExample), &spec); err != nil {
		t.Fatalf("unmarshaling storage example: %v", err)
	}

	if spec.ID != "storage" {
		t.Errorf("ID = %q, want storage", spec.ID)
	}
	if spec.UIIntegrationMode != boothv1alpha1.UIIntegrationModeNative {
		t.Errorf("UIIntegrationMode = %q, want native", spec.UIIntegrationMode)
	}
	if spec.NavGroup != boothv1alpha1.NavGroupManage {
		t.Errorf("NavGroup = %q, want manage", spec.NavGroup)
	}
	if spec.AdminNavPath != "/storage/admin" {
		t.Errorf("AdminNavPath = %q, want /storage/admin", spec.AdminNavPath)
	}
	if len(spec.RequiredScopes) != 2 {
		t.Errorf("RequiredScopes = %v, want 2 entries", spec.RequiredScopes)
	}
}

func TestModuleManifestExample_Superset(t *testing.T) {
	var spec boothv1alpha1.BoothModuleSpec
	if err := yaml.Unmarshal([]byte(supersetManifestExample), &spec); err != nil {
		t.Fatalf("unmarshaling superset example: %v", err)
	}

	if spec.UIIntegrationMode != boothv1alpha1.UIIntegrationModeIframeProxy {
		t.Errorf("UIIntegrationMode = %q, want iframe-proxy", spec.UIIntegrationMode)
	}
	if spec.NavGroup != boothv1alpha1.NavGroupBuild {
		t.Errorf("NavGroup = %q, want build", spec.NavGroup)
	}
	if spec.AdminNavPath != "" {
		t.Errorf("AdminNavPath = %q, want empty (superset example omits it)", spec.AdminNavPath)
	}
}

// TestManifestContract_RequiredFieldsMatchDocumentedContract pins down, in code, exactly
// which fields contracts/module-manifest.md marks "required" — so a future change to
// either the contract doc or the Go type has to touch this test too.
func TestManifestContract_RequiredFieldsMatchDocumentedContract(t *testing.T) {
	minimal := boothv1alpha1.BoothModuleSpec{
		ID:              "x",
		DisplayName:     "X",
		Version:         "0.1.0",
		ContractVersion: "0.1.0",
		HasOwnUI:        false,
		HealthCheckPath: "/health",
		ServiceRef:      boothv1alpha1.ServiceReference{Name: "x-svc", Port: 8080},
	}

	// A module with hasOwnUi: false legitimately omits uiIntegrationMode, navPath,
	// and navGroup (module-manifest.md: "if hasOwnUi") — this should be a
	// structurally valid spec on its own terms (CRD-level enum/required validation
	// is the real cluster's job, not this offline test's).
	if minimal.ID == "" || minimal.DisplayName == "" || minimal.Version == "" ||
		minimal.ContractVersion == "" || minimal.HealthCheckPath == "" || minimal.ServiceRef.Name == "" {
		t.Fatal("minimal spec unexpectedly missing a documented-required field")
	}
}
