package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

	// HeaderBoothIdentity carries the signed iframe-proxy identity assertion (ADR 0069): a
	// dedicated header, not Authorization, because third-party UIs embedded via iframe-proxy
	// (JupyterHub/jupyter-server, Superset, Metabase) each parse Authorization as their own API
	// token. Set only on the iframe-proxy path (internal/gateway's IframeEntryHandler and
	// IframeFallbackHandler); the ordinary gateway route never touches it.
	HeaderBoothIdentity = "X-Booth-Identity"
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
	observer          ClaimsObserver
	workload          WorkloadVerifier
}

// WorkloadVerifier verifies the tokens core itself mints for unattended runs (ADR 0056), as a
// second trusted issuer next to the deployment's OIDC provider. It must return the same Claims
// shape as Verifier, so nothing downstream can tell which issuer vouched for the caller.
type WorkloadVerifier interface {
	// Issuer is the `iss` of tokens this verifier owns; it is what a request is dispatched on.
	Issuer() string
	Verify(ctx context.Context, rawToken string) (*Claims, error)
}

// WithWorkloadVerifier makes the middleware also accept core's workload tokens (ADR 0059).
//
// Dispatch is by the token's `iss` and is exclusive: a token whose issuer is the workload issuer
// is verified by v and *only* v, and every other token by the OIDC verifier and only it. There is
// no "try one, then the other", so a token that fails its own issuer's checks is never given a
// second chance under a different issuer's rules — the two can't be confused in either direction.
//
// Workload tokens are also never fed to the ClaimsObserver: they represent runs, not people, and
// have no business in the user directory.
//
// This is opt-in per route group, not global: only the gateway route (module-to-module traffic)
// takes it. Core's own privileged routes (install/uninstall, ...) must keep rejecting them.
func WithWorkloadVerifier(v WorkloadVerifier) Option {
	return func(c *middlewareConfig) { c.workload = v }
}

// ClaimsObserver is called once for every request whose token verified, before workspace
// resolution, with the verified claims and the workspace memberships derived from them.
// It exists so the user directory (ADR 0047) can record identities opportunistically from
// the auth path core already runs, without auth depending on the directory. It runs on the
// request path, so it must be fast and must not fail the request.
type ClaimsObserver func(ctx context.Context, claims *Claims, memberships []Membership)

// WithClaimsObserver registers an observer for verified tokens.
func WithClaimsObserver(o ClaimsObserver) Option {
	return func(c *middlewareConfig) { c.observer = o }
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
			token := bearerToken(r)

			var claims *Claims
			fromWorkload := cfg.workload != nil && token != "" && unverifiedIssuer(token) == cfg.workload.Issuer()
			if fromWorkload {
				// Independent of the OIDC provider's availability: a run's token verifies
				// against core's own key, so an unreachable IdP doesn't take module-to-module
				// traffic down with it.
				var err error
				claims, err = cfg.workload.Verify(r.Context(), token)
				if err != nil {
					http.Error(w, "invalid token", http.StatusUnauthorized)
					return
				}
			} else {
				verifier, ready := holder.Get()
				if !ready {
					http.Error(w, "auth is not ready yet (OIDC provider not yet reachable)", http.StatusServiceUnavailable)
					return
				}
				if token == "" {
					http.Error(w, "missing bearer token", http.StatusUnauthorized)
					return
				}
				var err error
				claims, err = verifier.Verify(r.Context(), token)
				if err != nil {
					http.Error(w, "invalid token", http.StatusUnauthorized)
					return
				}
			}

			memberships := DeriveMemberships(claims.Groups)
			if cfg.observer != nil && !fromWorkload {
				cfg.observer(r.Context(), claims, memberships)
			}

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

// unverifiedIssuer reads a JWT's `iss` WITHOUT verifying anything. It is used only to choose
// which verifier gets the token; the chosen verifier then checks the signature and issuer for
// real, so a forged `iss` can at worst send a token to the wrong verifier, which rejects it.
// Anything that isn't a well-formed JWT payload with a string `iss` yields "".
func unverifiedIssuer(raw string) string {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return ""
	}
	return c.Iss
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}
