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

// HasActive reports whether an active workspace was resolved for this request. It's
// always true behind the default Middleware; false only on WorkspaceOptional routes when
// the client sent no X-Workspace header.
func (i Identity) HasActive() bool {
	return i.Active.Workspace != ""
}

// FromContext retrieves the Identity attached by Middleware. The second return value is
// false if no identity was attached (the middleware wasn't run, or the request is
// intentionally unauthenticated).
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityContextKey).(Identity)
	return id, ok
}

// Option customizes Middleware's behavior for a specific route group.
type Option func(*middlewareConfig)

type middlewareConfig struct {
	workspaceOptional bool
}

// WorkspaceOptional makes the X-Workspace header optional (ADR 0034): when it's absent,
// Identity.Active is left unresolved (zero value; see Identity.HasActive) instead of the
// request being rejected with 400. When the header IS present it's validated exactly as
// usual. This exists solely for GET /api/me, the bootstrapping call a client makes to
// learn its memberships and so can't already know a workspace slug — it is not a general
// relaxation, and every other authenticated route must keep the default (mandatory) behavior.
var WorkspaceOptional Option = func(c *middlewareConfig) { c.workspaceOptional = true }

// Middleware verifies the request's bearer token, resolves the requested active
// workspace against the token's memberships, and attaches the result to the request
// context for downstream handlers (core's own API and the gateway's proxy handler both
// use this). It takes a *VerifierHolder rather than a *Verifier directly so the HTTP
// server can start accepting requests before the configured OIDC provider is reachable —
// see VerifierHolder's doc comment.
func Middleware(holder *VerifierHolder, opts ...Option) func(http.Handler) http.Handler {
	var cfg middlewareConfig
	for _, o := range opts {
		o(&cfg)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			verifier, ready := holder.Get()
			if !ready {
				http.Error(w, "auth is not ready yet (OIDC provider not yet reachable)", http.StatusServiceUnavailable)
				return
			}

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

			var active Membership
			requestedWorkspace := r.Header.Get(HeaderWorkspace)
			switch {
			case requestedWorkspace == "" && cfg.workspaceOptional:
				// ADR 0034: leave active unresolved.
			case requestedWorkspace == "":
				http.Error(w, "missing "+HeaderWorkspace+" header", http.StatusBadRequest)
				return
			default:
				var err error
				active, err = ResolveActiveWorkspace(memberships, requestedWorkspace)
				if err != nil {
					if errors.Is(err, ErrNoMembership) {
						http.Error(w, "no membership in requested workspace", http.StatusForbidden)
						return
					}
					http.Error(w, "workspace resolution failed", http.StatusInternalServerError)
					return
				}
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
