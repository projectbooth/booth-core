package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/registry"
)

type fakeLookup struct {
	modules map[string]registry.Module
}

func (f fakeLookup) Get(id string) (registry.Module, bool) {
	m, ok := f.modules[id]
	return m, ok
}

func TestGatewayHandler_ProxiesToModuleWithResolvedHeaders(t *testing.T) {
	var gotWorkspace, gotRole, gotPath, gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotWorkspace = r.Header.Get(auth.HeaderBoothWorkspace)
		gotRole = r.Header.Get(auth.HeaderBoothRole)
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	lookup := fakeLookup{modules: map[string]registry.Module{
		"storage": {Spec: boothv1alpha1.BoothModuleSpec{ID: "storage", ServiceRef: boothv1alpha1.ServiceReference{Name: backend.Listener.Addr().String()}}},
	}}

	gw := New(lookup)
	handler := gw.Handler(
		func(r *http.Request) string { return "storage" },
		func(r *http.Request) string { return "/health" },
	)

	req := httptest.NewRequest(http.MethodGet, "/modules/storage/health", nil)
	req.Header.Set("Authorization", "Bearer original-token")
	identity := auth.Identity{
		Claims: &auth.Claims{Subject: "user-1"},
		Active: auth.Membership{Workspace: "acme-analytics", Role: auth.RoleEditor},
	}
	req = req.WithContext(auth.WithIdentityForTesting(req.Context(), identity))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotWorkspace != "acme-analytics" {
		t.Errorf("X-Booth-Workspace = %q, want acme-analytics", gotWorkspace)
	}
	if gotRole != "editor" {
		t.Errorf("X-Booth-Role = %q, want editor", gotRole)
	}
	if gotPath != "/health" {
		t.Errorf("forwarded path = %q, want /health", gotPath)
	}
	if gotAuth != "Bearer original-token" {
		t.Errorf("Authorization = %q, want Bearer original-token", gotAuth)
	}
}

func TestGatewayHandler_UnknownModuleReturns404(t *testing.T) {
	gw := New(fakeLookup{modules: map[string]registry.Module{}})
	handler := gw.Handler(
		func(r *http.Request) string { return "nonexistent" },
		func(r *http.Request) string { return "/" },
	)

	req := httptest.NewRequest(http.MethodGet, "/modules/nonexistent/", nil)
	req = req.WithContext(auth.WithIdentityForTesting(req.Context(), auth.Identity{
		Claims: &auth.Claims{Subject: "user-1"},
		Active: auth.Membership{Workspace: "acme-analytics", Role: auth.RoleViewer},
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestGatewayHandler_MissingIdentityReturns401(t *testing.T) {
	gw := New(fakeLookup{})
	handler := gw.Handler(
		func(r *http.Request) string { return "storage" },
		func(r *http.Request) string { return "/" },
	)

	req := httptest.NewRequest(http.MethodGet, "/modules/storage/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
