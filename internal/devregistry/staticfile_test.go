package devregistry

import (
	"os"
	"path/filepath"
	"testing"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.yaml")
	content := `
modules:
  - id: storage
    displayName: Storage
    version: 0.1.0
    contractVersion: 0.1.0
    hasOwnUi: true
    uiIntegrationMode: native
    healthCheckPath: /health
    navPath: /storage
    navGroup: manage
    host: localhost:9001
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	modules, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(modules) != 1 {
		t.Fatalf("len(modules) = %d, want 1", len(modules))
	}

	m := modules[0]
	if m.Spec.ID != "storage" {
		t.Errorf("ID = %q, want storage", m.Spec.ID)
	}
	if m.Spec.UIIntegrationMode != boothv1alpha1.UIIntegrationModeNative {
		t.Errorf("UIIntegrationMode = %q, want native", m.Spec.UIIntegrationMode)
	}
	if m.Status.Phase != boothv1alpha1.ModulePhaseHealthy {
		t.Errorf("Phase = %q, want Healthy", m.Status.Phase)
	}
	if got, want := m.BaseURL(), "http://localhost:9001"; got != want {
		t.Errorf("BaseURL() = %q, want %q", got, want)
	}
}

func TestLoad_RejectsEntryMissingID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.yaml")
	if err := os.WriteFile(path, []byte("modules:\n  - displayName: Nameless\n"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("expected error for entry missing id, got nil")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
