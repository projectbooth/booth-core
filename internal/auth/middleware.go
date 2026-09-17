package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// HeaderWorkspace is the client-supplied header naming the active workspace (decision
// 0001, item 6). HeaderRole/HeaderWorkspaceOut are what the gateway forwards downstream
// to a module once the request is validated — a module trusts these only because
// core-platform-api.md requires it to also independently re-verify the JWT
// (defense in depth), not because the header alone is trusted network-wide.
const (
	HeaderWorkspace      = "X-Workspace"
	HeaderBoothWorkspace = "X-Booth-Workspace"
	HeaderBoothRole      = "X-Booth-Role"
)

type contextKey string

const identityContextKey contextKey = "booth-identity"

// Identity is the fully-resolved caller identity attached to a request's context by
// Middleware: who they are, every workspace they belong to, and which one is active for
// this specific request.
type Identity struct {
	Claims      *Claims
	Memberships []Membership
	Active      Membership
}

// FromContext retrieves the Identity attached by Middleware. The second return value is
// false if no identity was attached (the middleware wasn't run, or the request is
// intentionally unauthenticated).
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityContextKey).(Identity)
	return id, ok
}

// Middleware verifies the request's bearer token, resolves the requested active
// workspace against the token's memberships, and attaches the result to the request
// context for downstream handlers (core's own API and the gateway's proxy handler both
// use this).
func Middleware(verifier *Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}

			claims, err := verifier.Verify(r.Context(), token)
			if err != nil {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}

			memberships := DeriveMemberships(claims.Groups)

			requestedWorkspace := r.Header.Get(HeaderWorkspace)
			if requestedWorkspace == "" {
				http.Error(w, "missing "+HeaderWorkspace+" header", http.StatusBadRequest)
				return
			}

			active, err := ResolveActiveWorkspace(memberships, requestedWorkspace)
			if err != nil {
				if errors.Is(err, ErrNoMembership) {
					http.Error(w, "no membership in requested workspace", http.StatusForbidden)
					return
				}
				http.Error(w, "workspace resolution failed", http.StatusInternalServerError)
				return
			}

			identity := Identity{Claims: claims, Memberships: memberships, Active: active}
			ctx := context.WithValue(r.Context(), identityContextKey, identity)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// WithIdentityForTesting attaches an Identity to a context the same way Middleware does,
// for use by other packages' tests that need to exercise a handler downstream of
// Middleware without standing up a real OIDC provider. Not for production use.
func WithIdentityForTesting(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityContextKey, identity)
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}
