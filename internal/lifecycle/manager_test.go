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

// TestLocateChartName_OCI pins the actual bug ADR 0113's Integration check first caught: an
// OCI RepoURL ("oci://ghcr.io/projectbooth/charts") with ChartName "booth-storage" must
// produce the single combined "oci://.../booth-storage" string Helm's own LocateChart
// recognizes as OCI -- passing ChartName alone there silently falls through to the classic
// HTTP chart-repo-index path and fails against a real OCI registry.
func TestLocateChartName_OCI(t *testing.T) {
	name, isOCI := locateChartName(ChartRef{RepoURL: "oci://ghcr.io/projectbooth/charts", ChartName: "booth-storage"})
	if !isOCI {
		t.Error("isOCI = false, want true for an oci:// RepoURL")
	}
	if want := "oci://ghcr.io/projectbooth/charts/booth-storage"; name != want {
		t.Errorf("name = %q, want %q", name, want)
	}
}

func TestLocateChartName_OCI_TrailingSlashOnRepoURL(t *testing.T) {
	name, isOCI := locateChartName(ChartRef{RepoURL: "oci://ghcr.io/projectbooth/charts/", ChartName: "booth-storage"})
	if !isOCI {
		t.Error("isOCI = false, want true for an oci:// RepoURL")
	}
	if want := "oci://ghcr.io/projectbooth/charts/booth-storage"; name != want {
		t.Errorf("name = %q, want %q (no double slash)", name, want)
	}
}

func TestLocateChartName_ClassicHTTPRepoIsUnchanged(t *testing.T) {
	name, isOCI := locateChartName(ChartRef{RepoURL: "https://charts.example.com", ChartName: "booth-storage"})
	if isOCI {
		t.Error("isOCI = true, want false for an https:// RepoURL")
	}
	if name != "booth-storage" {
		t.Errorf("name = %q, want the bare chart name unchanged", name)
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
