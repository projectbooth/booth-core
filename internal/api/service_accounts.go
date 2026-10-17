package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/serviceaccount"
)

const maxServiceAccountRequestBytes = 4 << 10

// serviceAccountView is the wire shape of a service account (ADR 0120). CreatedBy is included —
// unlike a directory entry's workspace list, it is not itself a way to enumerate anything the
// caller couldn't already see by listing this same endpoint.
type serviceAccountView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Role        string    `json:"role"`
	Description string    `json:"description"`
	Disabled    bool      `json:"disabled"`
	CreatedBy   string    `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
}

func toServiceAccountView(a serviceaccount.Account) serviceAccountView {
	return serviceAccountView{
		ID: a.ID(), Name: a.Name, Role: string(a.Role), Description: a.Description,
		Disabled: a.Disabled, CreatedBy: a.CreatedBy, CreatedAt: a.CreatedAt,
	}
}

// serviceAccountBadgeView is the trimmed wire shape GET /api/service-accounts/{name} returns to
// any workspace member (ADR 0119 amendment 3 point 5: "a single account's {name, role, disabled}
// for any member") — enough for booth-pipeline to render its "owner disabled" badge, without
// handing every member description/createdBy/createdAt. The full view (toServiceAccountView) is
// reserved for the list route, which is owner-only.
type serviceAccountBadgeView struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled"`
}

func toServiceAccountBadgeView(a serviceaccount.Account) serviceAccountBadgeView {
	return serviceAccountBadgeView{Name: a.Name, Role: string(a.Role), Disabled: a.Disabled}
}

// handleListServiceAccounts lists every account in the caller's active workspace, with full
// detail (role, description, disabled, createdBy, createdAt) — owner-only (requireAdmin, wired
// by the router), per ADR 0119 amendment 3 point 5: the picker a workspace owner uses to assign
// an account to a pipeline. Any member instead gets a single account's trimmed badge view via
// handleGetServiceAccount below.
func handleListServiceAccounts(store serviceaccount.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}
		accounts, err := store.List(r.Context(), identity.Active.Workspace)
		if err != nil {
			serviceAccountError(w, err)
			return
		}
		out := make([]serviceAccountView, 0, len(accounts))
		for _, a := range accounts {
			out = append(out, toServiceAccountView(a))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// handleGetServiceAccount resolves one account by name in the caller's active workspace, open to
// any member — the trimmed badge view only (ADR 0119 amendment 3 point 5), e.g. for
// booth-pipeline to show "owner disabled" without exposing description/createdBy/createdAt to
// every member.
func handleGetServiceAccount(store serviceaccount.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}
		name, err := url.PathUnescape(chi.URLParam(r, "name"))
		if err != nil || name == "" {
			http.Error(w, "invalid name", http.StatusBadRequest)
			return
		}
		a, found, err := store.Get(r.Context(), identity.Active.Workspace, name)
		if err != nil {
			serviceAccountError(w, err)
			return
		}
		if !found {
			http.Error(w, "service account not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, toServiceAccountBadgeView(a))
	}
}

// createServiceAccountRequest is the body of POST /api/service-accounts.
type createServiceAccountRequest struct {
	Name        string `json:"name"`
	Role        string `json:"role"`
	Description string `json:"description"`
}

// handleCreateServiceAccount creates a new account in the caller's active workspace. Wrapped in
// requireAdmin by the router (ADR 0120 #3: owner-only create).
func handleCreateServiceAccount(store serviceaccount.Store, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}

		var req createServiceAccountRequest
		body := http.MaxBytesReader(w, r.Body, maxServiceAccountRequestBytes)
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		role := auth.Role(req.Role)
		if err := serviceaccount.ValidateNew(req.Name, role); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		a := serviceaccount.Account{
			Workspace: identity.Active.Workspace, Name: req.Name, Role: role, Description: req.Description,
			CreatedBy: identity.Claims.Subject, CreatedAt: now().UTC(),
		}
		if err := store.Create(r.Context(), a); err != nil {
			serviceAccountError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, toServiceAccountView(a))
	}
}

// patchServiceAccountRequest is the body of PATCH /api/service-accounts/{name}. A field is a
// pointer so "omitted" (leave it alone) is distinguishable from "set to false"/"set to empty".
type patchServiceAccountRequest struct {
	Role        *string `json:"role"`
	Description *string `json:"description"`
	Disabled    *bool   `json:"disabled"`
}

// handlePatchServiceAccount edits role/description/disabled. Wrapped in requireAdmin by the
// router (ADR 0120 #3: owner-only edit/disable).
func handlePatchServiceAccount(store serviceaccount.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}
		name, err := url.PathUnescape(chi.URLParam(r, "name"))
		if err != nil || name == "" {
			http.Error(w, "invalid name", http.StatusBadRequest)
			return
		}

		var req patchServiceAccountRequest
		body := http.MaxBytesReader(w, r.Body, maxServiceAccountRequestBytes)
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		patch := serviceaccount.Patch{Description: req.Description, Disabled: req.Disabled}
		if req.Role != nil {
			role := auth.Role(*req.Role)
			patch.Role = &role
		}
		if err := patch.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		a, err := store.Update(r.Context(), identity.Active.Workspace, name, patch)
		if err != nil {
			serviceAccountError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toServiceAccountView(a))
	}
}

// handleDeleteServiceAccount removes an account. Wrapped in requireAdmin by the router. Refuses
// (409) an account that isn't disabled yet — the coordinator's v0 ruling for ADR 0120 in place of
// a usage registry (addendum point 5): disabling already fails every future mint closed, so by
// the time a delete is possible nothing can still be depending on the account working.
func handleDeleteServiceAccount(store serviceaccount.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}
		name, err := url.PathUnescape(chi.URLParam(r, "name"))
		if err != nil || name == "" {
			http.Error(w, "invalid name", http.StatusBadRequest)
			return
		}
		if err := store.Delete(r.Context(), identity.Active.Workspace, name); err != nil {
			serviceAccountError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func serviceAccountError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, serviceaccount.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, serviceaccount.ErrAlreadyExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, serviceaccount.ErrMustDisableBeforeDelete):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, serviceaccount.ErrUnavailable):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		http.Error(w, "service account request failed", http.StatusInternalServerError)
	}
}
