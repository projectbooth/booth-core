// Package gateway implements booth-core's synchronous request routing (ADR 0007): both
// UI traffic and module-to-module calls are reverse-proxied through core, which attaches
// verified identity before forwarding. There is no direct arbitrary module-to-module
// network path — everything synchronous goes through here.
package gateway

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/registry"
)

// ModuleLookup resolves a module ID to its current registry entry. Implemented by
// *registry.Registry; declared as an interface here so gateway doesn't need to know
// about the registry's internals beyond this one method.
type ModuleLookup interface {
	Get(id string) (registry.Module, bool)
}

// Gateway routes a request path of the form /modules/{id}/... to that module's
// BaseURL, stripping the /modules/{id} prefix and attaching the caller's verified
// identity as headers the module can trust (in addition to independently re-verifying
// the forwarded JWT, per core-platform-api.md's defense-in-depth requirement).
type Gateway struct {
	Modules ModuleLookup

	// Transport, if set, carries proxied requests instead of http.DefaultTransport. Nil in
	// production; tests inject one to observe exactly what the gateway sends and where,
	// without cluster DNS.
	Transport http.RoundTripper

	// IframeIdentity, if set, mints the signed X-Booth-Identity assertion attached to every
	// iframe-proxied request (ADR 0069; see iframeproxy.go). Nil leaves the header unset — the
	// pre-ADR-0069 workspace/role-only behavior — which is what every existing test in this
	// package (constructed via New, with no iframe-identity issuer configured) still exercises.
	// It's never consulted on the ordinary /modules/{id}/* gateway route.
	IframeIdentity IframeIdentityMinter
}

// IframeIdentityMinter mints the assertion carried as X-Booth-Identity on the iframe-proxy path
// (ADR 0069). Implemented by iframeidentity.Service; declared as an interface here so gateway
// doesn't need to import the JOSE/JWT libraries directly.
type IframeIdentityMinter interface {
	Mint(moduleID, workspace, role, subject string) (string, error)
}

func New(modules ModuleLookup) *Gateway {
	return &Gateway{Modules: modules}
}

// ServeHTTP expects auth.Middleware to have already run and attached an auth.Identity to
// the request context, and expects the module ID and forwarded path to already be
// resolved onto the request context by the caller's router (see Handler below) — this
// method does the actual proxying once both are known.
func (g *Gateway) proxyTo(moduleID string, forwardPath string, rawToken string) (http.Handler, error) {
	mod, ok := g.Modules.Get(moduleID)
	if !ok {
		return nil, fmt.Errorf("module %q not found in registry", moduleID)
	}

	target, err := url.Parse(mod.BaseURL())
	if err != nil {
		return nil, fmt.Errorf("module %q has invalid base URL %q: %w", moduleID, mod.BaseURL(), err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = g.Transport
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalDirector(r)
		r.URL.Path = forwardPath
		r.Header.Set("Authorization", "Bearer "+rawToken)
	}

	return proxy, nil
}

// Handler builds the http.Handler for the gateway's module-proxy route, expected to be
// mounted at a path like "/modules/{id}/*" by the caller's router, with idParam and
// pathParam functions extracting the {id} and the remainder-of-path from the request
// (kept router-agnostic rather than importing a specific mux here).
func (g *Gateway) Handler(idParam func(*http.Request) string, remainderPath func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity attached to request", http.StatusUnauthorized)
			return
		}

		moduleID := idParam(r)
		forwardPath := remainderPath(r)

		token := bearerTokenFromRequest(r)

		proxy, err := g.proxyTo(moduleID, forwardPath, token)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		// Attach the resolved workspace/role for the module to trust, per decision
		// 0001 item 6 — sent as headers the module can read without needing to
		// re-derive membership from the groups claim itself.
		r.Header.Set(auth.HeaderBoothWorkspace, identity.Active.Workspace)
		r.Header.Set(auth.HeaderBoothRole, string(identity.Active.Role))

		proxy.ServeHTTP(w, r)
	})
}

func bearerTokenFromRequest(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && h[:len(prefix)] == prefix {
		return h[len(prefix):]
	}
	return ""
}
