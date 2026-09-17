package lifecycle

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadChart_FromLocalPath exercises the Path branch of loadChart, which needs no
// cluster and no chart repo — a minimal on-disk chart is enough. The RepoURL branch
// (LocateChart) and the Install/Uninstall Helm actions themselves need a real
// Kubernetes API server and are exercised by the real-cluster integration test layer
// (contracts/testing-strategy.md layer 3), not here.
func TestLoadChart_FromLocalPath(t *testing.T) {
	dir := t.TempDir()
	writeFixtureChart(t, dir)

	c, err := loadChart(&Manager{}, ChartRef{Path: dir})
	if err != nil {
		t.Fatalf("loadChart: %v", err)
	}
	if c.Name() != "fixture" {
		t.Errorf("chart name = %q, want fixture", c.Name())
	}
}

func writeFixtureChart(t *testing.T, dir string) {
	t.Helper()

	chartYAML := "apiVersion: v2\nname: fixture\nversion: 0.1.0\n"
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(chartYAML), 0o644); err != nil {
		t.Fatalf("writing Chart.yaml: %v", err)
	}

	templatesDir := filepath.Join(dir, "templates")
	if err := os.MkdirAll(templatesDir, 0o755); err != nil {
		t.Fatalf("creating templates dir: %v", err)
	}

	cm := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: fixture\ndata:\n  key: value\n"
	if err := os.WriteFile(filepath.Join(templatesDir, "configmap.yaml"), []byte(cm), 0o644); err != nil {
		t.Fatalf("writing template: %v", err)
	}
}
