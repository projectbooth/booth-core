package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/gateway"
	"github.com/projectbooth/booth-core/internal/registry"
)

func routerWith(t *testing.T, idp *testIDP, mod func(*Deps)) http.Handler {
	t.Helper()
	tokens := gateway.NewIframeTokenIssuer([]byte("s"))
	reg := registry.New()
	deps := Deps{
		Verifier: idp.holder, Registry: reg, Gateway: gateway.New(reg),
		IframeTokens: tokens, IframeURLs: gateway.NewIframeURLIssuer(tokens),
	}
	if mod != nil {
		mod(&deps)
	}
	return NewRouter(deps)
}

func get(router http.Handler, token, path, workspace string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if workspace != "" {
		req.Header.Set(auth.HeaderWorkspace, workspace)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestRouter_OnlyAPIMeIsWorkspaceHeaderExempt pins ADR 0034's wiring through the real
// router and a real verifier: GET /api/me works without X-Workspace (and omits "active"),
// while every other authenticated route still 400s without it.
func TestRouter_OnlyAPIMeIsWorkspaceHeaderExempt(t *testing.T) {
	idp := newTestIDP(t)
	router := routerWith(t, idp, nil)
	token := idp.token(t, "u1", map[string]any{"groups": []string{"/workspaces/acme-analytics/owner"}})

	// /api/me without the header: 200, memberships present, active omitted.
	rec := get(router, token, "/api/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/me without header: status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, present := body["active"]; present {
		t.Errorf("active present without header: %v", body["active"])
	}
	if ms, _ := body["memberships"].([]any); len(ms) != 1 {
		t.Errorf("memberships = %v, want 1 entry", body["memberships"])
	}

	// /api/me with the header: active populated as before.
	rec = get(router, token, "/api/me", "acme-analytics")
	body = nil
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if rec.Code != http.StatusOK || body["active"] == nil {
		t.Errorf("/api/me with header: status = %d, active = %v; want 200 with active", rec.Code, body["active"])
	}

	// Every other authenticated route still requires the header.
	if rec := get(router, token, "/api/modules", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("/api/modules without header: status = %d, want 400", rec.Code)
	}
	if rec := get(router, token, "/api/modules", "acme-analytics"); rec.Code != http.StatusOK {
		t.Errorf("/api/modules with header: status = %d, want 200", rec.Code)
	}
}
