package api

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/lifecycle"
)

// captureLog redirects the standard logger for the duration of fn and returns what it wrote, the
// same helper internal/lifecycle/identity_test.go and internal/dbwait/retry_test.go already use
// for this exact purpose.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)
	fn()
	return buf.String()
}

// writeMinimalChart writes a trivial but valid Helm chart to dir, just enough that loadChart
// succeeds and mgr.Install actually reaches Helm's own install action -- internal/lifecycle's
// own writeFixtureChart is unexported and this package can't call it.
func writeMinimalChart(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: probe\nversion: 0.1.0\n"), 0o644); err != nil {
		t.Fatalf("writing Chart.yaml: %v", err)
	}
	templatesDir := filepath.Join(dir, "templates")
	if err := os.MkdirAll(templatesDir, 0o755); err != nil {
		t.Fatalf("creating templates dir: %v", err)
	}
	cm := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: probe\ndata:\n  key: value\n"
	if err := os.WriteFile(filepath.Join(templatesDir, "configmap.yaml"), []byte(cm), 0o644); err != nil {
		t.Fatalf("writing template: %v", err)
	}
}

func TestRequireAdmin_RejectsNonOwner(t *testing.T) {
	called := false
	handler := requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/modules/storage/install", nil), auth.RoleEditor)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if called {
		t.Fatal("expected wrapped handler not to be called for a non-owner")
	}
}

func TestRequireAdmin_AllowsOwner(t *testing.T) {
	called := false
	handler := requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/modules/storage/install", nil), auth.RoleOwner)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !called {
		t.Fatal("expected wrapped handler to be called for an owner")
	}
}

func TestHandleInstallModule_RequiresNamespace(t *testing.T) {
	body := strings.NewReader(`{"chart":{"path":"/tmp/whatever"}}`)
	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/modules/storage/install", body), auth.RoleOwner)
	rec := httptest.NewRecorder()
	handleInstallModule(lifecycle.IdentitySettings{})(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleUninstallModule_RequiresNamespace(t *testing.T) {
	req := withTestIdentity(httptest.NewRequest(http.MethodDelete, "/api/modules/storage", nil), auth.RoleOwner)
	rec := httptest.NewRecorder()
	handleUninstallModule(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestHandleInstallModule_RejectsWhenNeitherChartNorChartRefGiven exercises ADR 0028's
// either/or requirement once namespace is present, so resolveChartRef's own validation
// (not the earlier namespace check) is what's actually under test.
func TestHandleInstallModule_RejectsWhenNeitherChartNorChartRefGiven(t *testing.T) {
	body := strings.NewReader(`{"namespace":"booth-storage"}`)
	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/modules/storage/install", body), auth.RoleOwner)
	rec := httptest.NewRecorder()
	handleInstallModule(lifecycle.IdentitySettings{})(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestHandleInstallModule_RejectsMalformedChartRef confirms a bad chartRef string fails
// loudly (400) rather than falling back to the (empty) structured chart object.
func TestHandleInstallModule_RejectsMalformedChartRef(t *testing.T) {
	body := strings.NewReader(`{"namespace":"booth-storage","chartRef":"https://not-oci.example.com/chart"}`)
	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/modules/storage/install", body), auth.RoleOwner)
	rec := httptest.NewRecorder()
	handleInstallModule(lifecycle.IdentitySettings{})(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestHandleInstallModule_500LogsTheErrorWithModuleID is the gap found on the homelab: chi's
// middleware.Logger only ever logs the status code and the response size, never the body, so a
// 500 from this route previously left no trace of *why* anywhere in core's own logs -- only in
// the HTTP response, which nothing captures after the fact. A missing kubeconfig/cluster makes
// mgr.Install fail fast and deterministically (Helm's own "Kubernetes cluster unreachable"), with
// no real cluster needed -- exactly the Helm/chart error text this fix must get into the log.
func TestHandleInstallModule_500LogsTheErrorWithModuleID(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))
	dir := t.TempDir()
	writeMinimalChart(t, dir)

	body := strings.NewReader(`{"namespace":"probe-ns","chart":{"path":"` + filepath.ToSlash(dir) + `"}}`)
	req := withTestIdentity(httptest.NewRequest(http.MethodPost, "/api/modules/probe-mod/install", body), auth.RoleOwner)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "probe-mod")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()

	logged := captureLog(t, func() {
		handleInstallModule(lifecycle.IdentitySettings{})(rec, req)
	})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logged, "probe-mod") {
		t.Errorf("log output = %q, want it to mention the module id %q", logged, "probe-mod")
	}
	if !strings.Contains(logged, rec.Body.String()) {
		t.Errorf("log output = %q, want it to contain the same Helm/chart error text sent in the response (%q)", logged, rec.Body.String())
	}
}

func TestResolveChartRef_PrefersChartRefStringOverStructuredChart(t *testing.T) {
	req := installRequest{
		ChartRef:     "oci://registry.example.com/charts/acme-forecast",
		ChartVersion: "1.4.2",
		Chart:        chartRefBody{Path: "/should/be/ignored"},
	}

	got, err := resolveChartRef(req)
	if err != nil {
		t.Fatalf("resolveChartRef: %v", err)
	}
	if got.RepoURL != "oci://registry.example.com/charts" || got.ChartName != "acme-forecast" || got.Version != "1.4.2" {
		t.Errorf("got %+v, want parsed oci ref", got)
	}
}

func TestResolveChartRef_FallsBackToStructuredChart(t *testing.T) {
	req := installRequest{
		Chart: chartRefBody{RepoURL: "https://charts.example.com", ChartName: "storage", Version: "0.1.0"},
	}

	got, err := resolveChartRef(req)
	if err != nil {
		t.Fatalf("resolveChartRef: %v", err)
	}
	if got.RepoURL != "https://charts.example.com" || got.ChartName != "storage" {
		t.Errorf("got %+v, want the structured chart passed through", got)
	}
}

func TestResolveChartRef_ErrorsWhenBothEmpty(t *testing.T) {
	if _, err := resolveChartRef(installRequest{}); err == nil {
		t.Fatal("expected error when neither chartRef nor chart is given")
	}
}
