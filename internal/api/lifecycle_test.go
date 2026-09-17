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
