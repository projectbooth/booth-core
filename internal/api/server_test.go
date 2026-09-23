package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/gateway"
	"github.com/projectbooth/booth-core/internal/registry"
)

func withTestIdentity(req *http.Request, role auth.Role) *http.Request {
	identity := auth.Identity{
		Claims:      &auth.Claims{Subject: "user-1", Email: "user@example.com"},
		Memberships: []auth.Membership{{Workspace: "acme-analytics", Role: role}},
		Active:      auth.Membership{Workspace: "acme-analytics", Role: role},
	}
	return req.WithContext(auth.WithIdentityForTesting(req.Context(), identity))
}

func TestHandleMe(t *testing.T) {
	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/me", nil), auth.RoleOwner)
	rec := httptest.NewRecorder()

	handleMe(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	if body["subject"] != "user-1" {
		t.Errorf("subject = %v, want user-1", body["subject"])
	}
}

// ADR 0034: with no active workspace resolved, "active" must be absent from the JSON
// entirely, not a zero-value {workspace:"",role:""}.
func TestHandleMe_OmitsActiveWhenUnresolved(t *testing.T) {
	identity := auth.Identity{
		Claims:      &auth.Claims{Subject: "user-1"},
		Memberships: []auth.Membership{{Workspace: "acme-analytics", Role: auth.RoleViewer}},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req = req.WithContext(auth.WithIdentityForTesting(req.Context(), identity))
	rec := httptest.NewRecorder()

	handleMe(rec, req)

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if _, present := body["active"]; present {
		t.Errorf("active key present (%v), want omitted", body["active"])
	}
	if body["memberships"] == nil {
		t.Error("memberships should still be returned")
	}
}

// TestHandleMe_NestedFieldsUseLowerCamelCase guards against the casing bug booth-design
// flagged: memberships[].workspace/role and active.workspace/role must serialize
// lowerCamelCase like every other field in this API, not capitalized Go field names.
func TestHandleMe_NestedFieldsUseLowerCamelCase(t *testing.T) {
	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/me", nil), auth.RoleOwner)
	rec := httptest.NewRecorder()

	handleMe(rec, req)

	raw := rec.Body.String()
	if !strings.Contains(raw, `"workspace":"acme-analytics"`) {
		t.Errorf("expected lowerCamelCase \"workspace\" key in response, got: %s", raw)
	}
	if !strings.Contains(raw, `"role":"owner"`) {
		t.Errorf("expected lowerCamelCase \"role\" key in response, got: %s", raw)
	}
	if strings.Contains(raw, `"Workspace"`) || strings.Contains(raw, `"Role"`) {
		t.Errorf("expected no capitalized Workspace/Role keys, got: %s", raw)
	}
}

func TestHandleListModules_HidesAdminNavPathFromNonOwner(t *testing.T) {
	reg := registry.New()
	reg.Put(registry.Module{
		Spec: boothv1alpha1.BoothModuleSpec{
			ID: "storage", DisplayName: "Storage", AdminNavPath: "/storage/admin",
		},
		Status: boothv1alpha1.BoothModuleStatus{Phase: boothv1alpha1.ModulePhaseHealthy},
	})

	handler := handleListModules(reg)

	// Viewer: should not see adminNavPath.
	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/modules", nil), auth.RoleViewer)
	rec := httptest.NewRecorder()
	handler(rec, req)

	var viewerResult []moduleView
	if err := json.NewDecoder(rec.Body).Decode(&viewerResult); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(viewerResult) != 1 {
		t.Fatalf("len = %d, want 1", len(viewerResult))
	}
	if viewerResult[0].AdminNavPath != "" {
		t.Errorf("viewer should not see AdminNavPath, got %q", viewerResult[0].AdminNavPath)
	}

	// Owner: should see adminNavPath.
	req2 := withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/modules", nil), auth.RoleOwner)
	rec2 := httptest.NewRecorder()
	handler(rec2, req2)

	var ownerResult []moduleView
	if err := json.NewDecoder(rec2.Body).Decode(&ownerResult); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if ownerResult[0].AdminNavPath != "/storage/admin" {
		t.Errorf("owner AdminNavPath = %q, want /storage/admin", ownerResult[0].AdminNavPath)
	}
}

// TestHandleListModules_IncludesUIIntegrationMode guards against the bug booth-design
// flagged: without this field the shell can't tell native modules from iframe-proxy
// ones and silently renders everything as native.
func TestHandleListModules_IncludesUIIntegrationMode(t *testing.T) {
	reg := registry.New()
	reg.Put(registry.Module{
		Spec: boothv1alpha1.BoothModuleSpec{
			ID: "superset", DisplayName: "Superset",
			UIIntegrationMode: boothv1alpha1.UIIntegrationModeIframeProxy,
		},
		Status: boothv1alpha1.BoothModuleStatus{Phase: boothv1alpha1.ModulePhaseHealthy},
	})

	handler := handleListModules(reg)
	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/modules", nil), auth.RoleViewer)
	rec := httptest.NewRecorder()
	handler(rec, req)

	var result []moduleView
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("len = %d, want 1", len(result))
	}
	if result[0].UIIntegrationMode != "iframe-proxy" {
		t.Errorf("UIIntegrationMode = %q, want iframe-proxy", result[0].UIIntegrationMode)
	}
}

// ADR 0060: a caller (booth-module-store, on uninstall) needs the module's live namespace back
// from the listing rather than guessing or hardcoding one.
func TestHandleListModules_IncludesNamespace(t *testing.T) {
	reg := registry.New()
	reg.Put(registry.Module{
		Namespace: "booth-superset",
		Spec:      boothv1alpha1.BoothModuleSpec{ID: "superset", DisplayName: "Superset"},
	})
	// A dev-registry-loaded module has no live namespace; it must come back empty, not guessed.
	reg.Put(registry.Module{
		Spec: boothv1alpha1.BoothModuleSpec{ID: "devmod", DisplayName: "Dev Module"},
	})

	handler := handleListModules(reg)
	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/modules", nil), auth.RoleViewer)
	rec := httptest.NewRecorder()
	handler(rec, req)

	var result []moduleView
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	byID := map[string]moduleView{}
	for _, v := range result {
		byID[v.ID] = v
	}
	if byID["superset"].Namespace != "booth-superset" {
		t.Errorf("superset namespace = %q, want booth-superset", byID["superset"].Namespace)
	}
	if byID["devmod"].Namespace != "" {
		t.Errorf("devmod namespace = %q, want empty", byID["devmod"].Namespace)
	}

	// omitempty: a module with no namespace must not even carry the key.
	rec2 := httptest.NewRecorder()
	handler(rec2, withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/modules", nil), auth.RoleViewer))
	var generic []map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &generic); err != nil {
		t.Fatal(err)
	}
	for _, m := range generic {
		if m["id"] == "devmod" {
			if _, present := m["namespace"]; present {
				t.Errorf("devmod JSON carries a namespace key although empty: %v", m)
			}
		}
		if m["id"] == "superset" {
			if m["namespace"] != "booth-superset" {
				t.Errorf("superset JSON namespace = %v", m["namespace"])
			}
		}
	}
}

func TestHandleIframeURL(t *testing.T) {
	tokens := gateway.NewIframeTokenIssuer([]byte("test-secret"))
	issuer := gateway.NewIframeURLIssuer(tokens)
	handler := handleIframeURL(issuer)

	req := withTestIdentity(httptest.NewRequest(http.MethodGet, "/api/modules/superset/iframe-url", nil), auth.RoleEditor)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "superset")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body["url"] == "" {
		t.Error("expected non-empty iframe url")
	}
}
