// Package api assembles booth-core's HTTP surface: its own small API (identity,
// workspace/module listing, iframe-url issuance) plus the gateway's module-proxy and
// iframe-proxy routes, per contracts/core-platform-api.md and contracts/ui-integration.md.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/gateway"
	"github.com/projectbooth/booth-core/internal/lifecycle"
	"github.com/projectbooth/booth-core/internal/registry"
)

// Deps is everything the HTTP layer needs, assembled by cmd/core/main.go.
type Deps struct {
	Verifier     *auth.VerifierHolder
	Registry     *registry.Registry
	Gateway      *gateway.Gateway
	IframeTokens *gateway.IframeTokenIssuer
	IframeURLs   *gateway.IframeURLIssuer
}

// NewRouter builds booth-core's full HTTP router.
func NewRouter(deps Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger) // stdout logging only, per ADR 0022 — no logging API to integrate against
	r.Use(middleware.Recoverer)

	// Unauthenticated: core's own liveness, not a module health check.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	authed := auth.Middleware(deps.Verifier)

	// ADR 0034: GET /api/me is the one route where X-Workspace is optional — it's how a
	// client learns its memberships, so it can't already know a slug. Kept in its own
	// group so no other route inherits the relaxation.
	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(deps.Verifier, auth.WorkspaceOptional))
		r.Get("/api/me", handleMe)
	})

	r.Group(func(r chi.Router) {
		r.Use(authed)

		r.Get("/api/modules", handleListModules(deps.Registry))
		r.Get("/api/modules/{id}/iframe-url", handleIframeURL(deps.IframeURLs))
		r.Post("/api/modules/{id}/install", requireAdmin(handleInstallModule))
		r.Delete("/api/modules/{id}", requireAdmin(handleUninstallModule))

		r.Handle("/modules/{id}/*", deps.Gateway.Handler(
			func(r *http.Request) string { return chi.URLParam(r, "id") },
			func(r *http.Request) string { return "/" + chi.URLParam(r, "*") },
		))
	})

	// Iframe-proxy routes authenticate via the short-lived token / scoped cookie
	// (ui-integration.md), not the main auth.Middleware — a plain iframe navigation
	// can't carry an Authorization header.
	r.Handle("/iframe/{id}/*", deps.Gateway.IframeEntryHandler(deps.IframeTokens,
		func(r *http.Request) string { return chi.URLParam(r, "id") },
		func(r *http.Request) string { return "/" + chi.URLParam(r, "*") },
	))

	// Catch-all fallback for a third-party iframed UI's own root-relative follow-up
	// calls (see gateway.IframeFallbackHandler's doc comment for the failure mode
	// this solves). Must be registered last: chi tries every more specific route
	// first and only falls through to NotFound (wired to this) when nothing else
	// matches.
	r.NotFound(deps.Gateway.IframeFallbackHandler(deps.IframeTokens).ServeHTTP)

	return r
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "no identity", http.StatusUnauthorized)
		return
	}
	body := map[string]any{
		"subject":     identity.Claims.Subject,
		"email":       identity.Claims.Email,
		"memberships": identity.Memberships,
	}
	// ADR 0034: omit "active" entirely when no X-Workspace header was sent — a
	// present-but-empty object would defeat the client's `active ?? fallback` logic.
	if identity.HasActive() {
		body["active"] = identity.Active
	}
	writeJSON(w, http.StatusOK, body)
}

func handleListModules(reg *registry.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}

		modules := reg.List()
		out := make([]moduleView, 0, len(modules))
		for _, m := range modules {
			view := moduleView{
				ID:                m.Spec.ID,
				DisplayName:       m.Spec.DisplayName,
				Icon:              m.Spec.Icon,
				Version:           m.Spec.Version,
				HasOwnUI:          m.Spec.HasOwnUI,
				UIIntegrationMode: string(m.Spec.UIIntegrationMode),
				NavPath:           m.Spec.NavPath,
				NavGroup:          string(m.Spec.NavGroup),
				Phase:             string(m.Status.Phase),
			}
			// ADR 0023: adminNavPath is only surfaced to owners.
			if m.Spec.AdminNavPath != "" && identity.Active.Role.IsAdmin() {
				view.AdminNavPath = m.Spec.AdminNavPath
			}
			out = append(out, view)
		}

		writeJSON(w, http.StatusOK, out)
	}
}

type moduleView struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	Icon              string `json:"icon,omitempty"`
	Version           string `json:"version"`
	HasOwnUI          bool   `json:"hasOwnUi"`
	UIIntegrationMode string `json:"uiIntegrationMode,omitempty"`
	NavPath           string `json:"navPath,omitempty"`
	NavGroup          string `json:"navGroup,omitempty"`
	AdminNavPath      string `json:"adminNavPath,omitempty"`
	Phase             string `json:"phase"`
}

func handleIframeURL(issuer *gateway.IframeURLIssuer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}

		moduleID := chi.URLParam(r, "id")
		url, err := issuer.URLFor(moduleID, identity)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"url": url})
	}
}

// requireAdmin wraps a handler so it 403s for any caller whose active role isn't owner.
// Install/uninstall are exactly the kind of owner-only action ADR 0023's consequences
// section anticipated for booth-storage and, here, for the module lifecycle itself.
func requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}
		if !identity.Active.Role.IsAdmin() {
			http.Error(w, "owner role required", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// installRequest is the body for POST /api/modules/{id}/install. Where the chart
// reference itself comes from (a Module Store catalog, a hand-typed admin form) is left
// to the caller per docs/decisions/0005-module-lifecycle-desired-state.md — this
// endpoint only performs the install/upgrade action.
//
// A caller supplies exactly one of ChartRef (a single string, e.g.
// "oci://host/path/chart-name" — contracts/module-registry-protocol.md's registry-entry
// shape, paired with ChartVersion) or the structured Chart object, per ADR 0028. ChartRef
// exists so a registry-consuming caller (booth-module-store today) can pass a registry
// entry's chartRef/chartVersion straight through unparsed — booth-core is the one place
// that parsing lives now, not every caller independently.
type installRequest struct {
	Namespace    string         `json:"namespace"`
	Chart        chartRefBody   `json:"chart"`
	ChartRef     string         `json:"chartRef,omitempty"`
	ChartVersion string         `json:"chartVersion,omitempty"`
	Values       map[string]any `json:"values"`
}

type chartRefBody struct {
	Path      string `json:"path,omitempty"`
	RepoURL   string `json:"repoUrl,omitempty"`
	ChartName string `json:"chartName,omitempty"`
	Version   string `json:"version,omitempty"`
}

func handleInstallModule(w http.ResponseWriter, r *http.Request) {
	moduleID := chi.URLParam(r, "id")

	var req installRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Namespace == "" {
		http.Error(w, "namespace is required", http.StatusBadRequest)
		return
	}

	ref, err := resolveChartRef(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	mgr, err := lifecycle.NewManager(req.Namespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := mgr.Install(moduleID, ref, req.Values); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// resolveChartRef implements ADR 0028's either/or: a ChartRef string takes precedence
// when present (parsed via lifecycle.ParseChartRef, the same logic lifted from
// booth-module-store's interim parser); otherwise the structured Chart object is used
// as-is, matching pre-ADR-0028 behavior exactly.
func resolveChartRef(req installRequest) (lifecycle.ChartRef, error) {
	if req.ChartRef != "" {
		return lifecycle.ParseChartRef(req.ChartRef, req.ChartVersion)
	}

	ref := lifecycle.ChartRef{
		Path:      req.Chart.Path,
		RepoURL:   req.Chart.RepoURL,
		ChartName: req.Chart.ChartName,
		Version:   req.Chart.Version,
	}
	if ref.IsZero() {
		return lifecycle.ChartRef{}, errChartRequired
	}
	return ref, nil
}

var errChartRequired = errors.New("either chartRef or chart is required")

func handleUninstallModule(w http.ResponseWriter, r *http.Request) {
	moduleID := chi.URLParam(r, "id")
	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		http.Error(w, "namespace query parameter is required", http.StatusBadRequest)
		return
	}

	mgr, err := lifecycle.NewManager(namespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := mgr.Uninstall(moduleID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
