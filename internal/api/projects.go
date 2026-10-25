package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/project"
)

const maxProjectRequestBytes = 4 << 10

// projectView is the wire shape of a project, fixed by the merged contract
// (booth-architecture/contracts/projects.md): {slug, name, parent, description, color, icon,
// archived, createdBy, createdAt}, every field always present (no omitempty — an unset parent/
// color/icon is "", not absent). Open to any member — a project carries no authorization
// meaning, and the picker/grouping UI needs the full list for every role.
type projectView struct {
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Parent      string    `json:"parent"`
	Description string    `json:"description"`
	Color       string    `json:"color"`
	Icon        string    `json:"icon"`
	Archived    bool      `json:"archived"`
	CreatedBy   string    `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
}

func toProjectView(p project.Project) projectView {
	return projectView{
		Slug: p.Slug, Name: p.Name, Parent: p.Parent, Description: p.Description,
		Color: p.Color, Icon: p.Icon, Archived: p.Archived, CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt,
	}
}

// handleListProjects lists every project in the caller's active workspace, open to any member.
func handleListProjects(store project.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}
		projects, err := store.List(r.Context(), identity.Active.Workspace)
		if err != nil {
			projectError(w, err)
			return
		}
		out := make([]projectView, 0, len(projects))
		for _, p := range projects {
			out = append(out, toProjectView(p))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleGetProject resolves one project by slug in the caller's active workspace, open to any
// member.
func handleGetProject(store project.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}
		slug, err := url.PathUnescape(chi.URLParam(r, "slug"))
		if err != nil || slug == "" {
			http.Error(w, "invalid slug", http.StatusBadRequest)
			return
		}
		p, found, err := store.Get(r.Context(), identity.Active.Workspace, slug)
		if err != nil {
			projectError(w, err)
			return
		}
		if !found {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, toProjectView(p))
	}
}

// createProjectRequest is the body of POST /api/projects.
type createProjectRequest struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Parent      string `json:"parent"`
	Description string `json:"description"`
	Color       string `json:"color"`
	Icon        string `json:"icon"`
}

// handleCreateProject creates a new project in the caller's active workspace. Wrapped in
// requireAdmin by the router (ADR 0124: owner-only writes, open reads — not stated explicitly in
// the ADR, ruled by the coordinator on the design note).
func handleCreateProject(store project.Store, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}

		var req createProjectRequest
		body := http.MaxBytesReader(w, r.Body, maxProjectRequestBytes)
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		if err := project.ValidateNew(req.Slug, req.Name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		p := project.Project{
			Workspace: identity.Active.Workspace, Slug: req.Slug, Name: req.Name, Parent: req.Parent,
			Description: req.Description, Color: req.Color, Icon: req.Icon,
			CreatedBy: identity.Claims.Subject, CreatedAt: now().UTC(),
		}
		if err := store.Create(r.Context(), p); err != nil {
			projectError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, toProjectView(p))
	}
}

// patchProjectRequest is the body of PATCH /api/projects/{slug}. A field is a pointer so
// "omitted" is distinguishable from "set to empty"/"set to false" — Parent's pointed-to value
// being "" means "clear back to top-level", not "leave alone" (project.Patch's own convention).
type patchProjectRequest struct {
	Name        *string `json:"name"`
	Parent      *string `json:"parent"`
	Description *string `json:"description"`
	Color       *string `json:"color"`
	Icon        *string `json:"icon"`
	Archived    *bool   `json:"archived"`
}

// handlePatchProject edits name/parent/description/color/icon/archived. Wrapped in requireAdmin
// by the router.
func handlePatchProject(store project.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}
		slug, err := url.PathUnescape(chi.URLParam(r, "slug"))
		if err != nil || slug == "" {
			http.Error(w, "invalid slug", http.StatusBadRequest)
			return
		}

		var req patchProjectRequest
		body := http.MaxBytesReader(w, r.Body, maxProjectRequestBytes)
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		patch := project.Patch{
			Name: req.Name, Parent: req.Parent, Description: req.Description,
			Color: req.Color, Icon: req.Icon, Archived: req.Archived,
		}
		if err := patch.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		p, err := store.Update(r.Context(), identity.Active.Workspace, slug, patch)
		if err != nil {
			projectError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toProjectView(p))
	}
}

// bindingView is the wire shape of a binding, fixed by the merged contract
// (booth-architecture/contracts/projects.md): {id, project, kind, value, backendId, createdBy,
// createdAt}, with backendId present only for a storage_prefix binding (project.Binding.BackendID
// is already "" for the other two kinds — see project.ValidateNewBinding — so omitempty here is
// exactly that rule, not a separate decision).
type bindingView struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Kind      string    `json:"kind"`
	Value     string    `json:"value"`
	BackendID string    `json:"backendId,omitempty"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

func toBindingView(b project.Binding) bindingView {
	return bindingView{
		ID: b.ID, Project: b.ProjectSlug, Kind: string(b.Kind), BackendID: b.BackendID,
		Value: b.Value, CreatedBy: b.CreatedBy, CreatedAt: b.CreatedAt,
	}
}

// handleListProjectBindings lists bindings for one project (GET /api/projects/{slug}/bindings),
// open to any member.
func handleListProjectBindings(store project.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}
		slug, err := url.PathUnescape(chi.URLParam(r, "slug"))
		if err != nil || slug == "" {
			http.Error(w, "invalid slug", http.StatusBadRequest)
			return
		}
		bindings, err := store.ListBindings(r.Context(), identity.Active.Workspace, project.BindingFilter{ProjectSlug: slug})
		if err != nil {
			projectError(w, err)
			return
		}
		out := make([]bindingView, 0, len(bindings))
		for _, b := range bindings {
			out = append(out, toBindingView(b))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleListWorkspaceBindings lists every binding in the workspace, optionally filtered by kind
// (GET /api/projects/bindings?kind=...), open to any member — the coordinator's addition beyond
// the ADR's literal per-project route, so a module like booth-database can fetch every
// postgres_schema binding in one call rather than per project.
func handleListWorkspaceBindings(store project.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}
		kind := project.BindingKind(r.URL.Query().Get("kind"))
		bindings, err := store.ListBindings(r.Context(), identity.Active.Workspace, project.BindingFilter{Kind: kind})
		if err != nil {
			projectError(w, err)
			return
		}
		out := make([]bindingView, 0, len(bindings))
		for _, b := range bindings {
			out = append(out, toBindingView(b))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// createBindingRequest is the body of POST /api/projects/{slug}/bindings.
type createBindingRequest struct {
	Kind      string `json:"kind"`
	BackendID string `json:"backendId"`
	Value     string `json:"value"`
}

// handleCreateProjectBinding creates a binding on one project. Wrapped in requireAdmin by the
// router.
func handleCreateProjectBinding(store project.Store, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}
		slug, err := url.PathUnescape(chi.URLParam(r, "slug"))
		if err != nil || slug == "" {
			http.Error(w, "invalid slug", http.StatusBadRequest)
			return
		}

		var req createBindingRequest
		body := http.MaxBytesReader(w, r.Body, maxProjectRequestBytes)
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		kind := project.BindingKind(req.Kind)
		if err := project.ValidateNewBinding(kind, req.BackendID, req.Value); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		b, err := store.CreateBinding(r.Context(), project.Binding{
			Workspace: identity.Active.Workspace, ProjectSlug: slug, Kind: kind,
			BackendID: req.BackendID, Value: req.Value,
			CreatedBy: identity.Claims.Subject, CreatedAt: now().UTC(),
		})
		if err != nil {
			projectError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, toBindingView(b))
	}
}

// handleDeleteProjectBinding removes one binding by id, at the path the merged contract fixes —
// DELETE /api/projects/{slug}/bindings/{id} (booth-architecture/contracts/projects.md) — not the
// slug-less /api/projects/bindings/{id} this package built first. A binding id is already unique
// per workspace (project.Store.DeleteBinding takes no slug), so {slug} isn't needed to find the
// row, but it is still checked: deleting an id that exists but belongs to a different project
// than the URL names is refused as a 404, the same "the URL's claim must be true" posture a
// mismatched path segment gets everywhere else in this API, rather than silently deleting
// whatever the id happens to resolve to. Wrapped in requireAdmin by the router.
func handleDeleteProjectBinding(store project.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}
		slug, err := url.PathUnescape(chi.URLParam(r, "slug"))
		if err != nil || slug == "" {
			http.Error(w, "invalid slug", http.StatusBadRequest)
			return
		}
		id, err := url.PathUnescape(chi.URLParam(r, "id"))
		if err != nil || id == "" {
			http.Error(w, "invalid id", http.StatusBadRequest)
			return
		}

		bindings, err := store.ListBindings(r.Context(), identity.Active.Workspace, project.BindingFilter{ProjectSlug: slug})
		if err != nil {
			projectError(w, err)
			return
		}
		found := false
		for _, b := range bindings {
			if b.ID == id {
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "binding not found on this project", http.StatusNotFound)
			return
		}

		if err := store.DeleteBinding(r.Context(), identity.Active.Workspace, id); err != nil {
			projectError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func projectError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, project.ErrNotFound), errors.Is(err, project.ErrBindingNotFound), errors.Is(err, project.ErrParentNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, project.ErrAlreadyExists), errors.Is(err, project.ErrBindingConflict),
		errors.Is(err, project.ErrParentHasAParent), errors.Is(err, project.ErrHasChildren):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, project.ErrUnavailable):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		http.Error(w, "project request failed", http.StatusInternalServerError)
	}
}
