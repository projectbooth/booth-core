package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/directory"
)

const (
	defaultUserSearchLimit = 25
	maxUserSearchLimit     = 100
	maxUserSearchQuery     = 100
)

// userView is the wire shape of a directory entry (ADR 0047). Deliberately omits the
// entry's workspace list: it's used to scope who may see an entry, and returning it would
// leak the user's other workspace memberships.
type userView struct {
	Sub               string    `json:"sub"`
	PreferredUsername string    `json:"preferredUsername,omitempty"`
	Name              string    `json:"name,omitempty"`
	Email             string    `json:"email,omitempty"`
	DisplayName       string    `json:"displayName"`
	LastSeenAt        time.Time `json:"lastSeenAt"`
}

func toUserView(u directory.User) userView {
	return userView{
		Sub: u.Sub, PreferredUsername: u.PreferredUsername, Name: u.Name, Email: u.Email,
		DisplayName: u.DisplayName(), LastSeenAt: u.LastSeenAt,
	}
}

// handleGetUser resolves one `sub` to display claims. A user outside the caller's active
// workspace is indistinguishable from one that doesn't exist (404 either way), so this
// can't be used to probe which subs are known to the deployment.
func handleGetUser(store directory.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}

		// chi routes on the raw path, so a sub containing reserved characters (some IdPs use
		// "auth0|abc", "user@tenant") arrives still percent-encoded.
		sub, err := url.PathUnescape(chi.URLParam(r, "sub"))
		if err != nil || sub == "" {
			http.Error(w, "invalid sub", http.StatusBadRequest)
			return
		}

		u, found, err := store.Get(r.Context(), sub, identity.Active.Workspace)
		if err != nil {
			http.Error(w, "user directory unavailable", http.StatusServiceUnavailable)
			return
		}
		if !found {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, toUserView(u))
	}
}

// handleSearchUsers backs a picker: users in the caller's active workspace whose name,
// username, or email contains ?q= (empty q lists them all), capped by ?limit=.
func handleSearchUsers(store directory.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}

		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if len(q) > maxUserSearchQuery {
			http.Error(w, "q is too long", http.StatusBadRequest)
			return
		}

		limit := defaultUserSearchLimit
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
				return
			}
			limit = min(n, maxUserSearchLimit)
		}

		users, err := store.Search(r.Context(), identity.Active.Workspace, q, limit)
		if err != nil {
			http.Error(w, "user directory unavailable", http.StatusServiceUnavailable)
			return
		}

		out := make([]userView, 0, len(users))
		for _, u := range users {
			out = append(out, toUserView(u))
		}
		writeJSON(w, http.StatusOK, out)
	}
}
