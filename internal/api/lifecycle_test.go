package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/projectbooth/booth-core/internal/auth"
)

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
	handleInstallModule(rec, req)

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
	handleInstallModule(rec, req)

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
	handleInstallModule(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
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
