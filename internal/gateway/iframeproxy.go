package gateway

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/projectbooth/booth-core/internal/auth"
)

const (
	// IframeCookieName is scoped to Path=/ deliberately, not Path=/iframe/{id}/ — see
	// IframeFallbackHandler's doc comment for why.
	IframeCookieName = "booth_iframe_session"

	iframeQueryParam = "booth_iframe_token"

	// navigationTokenTTL is intentionally short: the query-param token is only alive
	// long enough for the browser to complete the initial iframe navigation before
	// IframeEntryHandler exchanges it for a cookie and strips it from the URL.
	navigationTokenTTL = time.Minute

	// CookieTTL bounds how long an embedded module session lasts before the shell
	// needs to re-request an iframe URL (which re-validates the caller's current
	// workspace membership from a fresh JWT, not a stale one baked into a long-lived
	// cookie).
	CookieTTL = 15 * time.Minute
)

// IframeURLIssuer mints the URL the shell points an iframe's src at
// (ui-integration.md's iframe-proxy mode). Kept separate from Gateway so the
// shell-facing "give me an iframe URL" API handler (which runs under the ordinary
// auth.Middleware / X-Workspace flow) and the iframe traffic handlers below (which run
// under the iframe token instead) don't share request-auth assumptions.
type IframeURLIssuer struct {
	tokens *IframeTokenIssuer
}

func NewIframeURLIssuer(tokens *IframeTokenIssuer) *IframeURLIssuer {
	return &IframeURLIssuer{tokens: tokens}
}

// URLFor mints a fresh navigation token for identity's active workspace/role and returns the
// iframe src URL, as a path relative to whatever origin the browser is already on (ADR 0069's
// "Implementation notes", bug 2). It used to be built from a configured public base URL
// (BOOTH_PUBLIC_BASE_URL), which no chart value ever set — every real deployment's iframe URL
// silently pointed at the fallback's http://localhost:8080 instead of the deployment's real
// address. A relative URL removes the failure mode entirely rather than making it configurable:
// the shell routes /iframe/ to core itself (ADR 0069 item B), so this resolves correctly against
// whatever origin the browser is already on with no operator configuration at all.
func (i *IframeURLIssuer) URLFor(moduleID string, identity auth.Identity) (string, error) {
	token, err := i.tokens.Issue(IframeClaims{
		ModuleID:  moduleID,
		Workspace: identity.Active.Workspace,
		Role:      string(identity.Active.Role),
		Subject:   identity.Claims.Subject,
	}, navigationTokenTTL)
	if err != nil {
		return "", fmt.Errorf("issuing iframe token: %w", err)
	}
	return fmt.Sprintf("/iframe/%s/?%s=%s", moduleID, iframeQueryParam, token), nil
}

// IframeEntryHandler serves /iframe/{id}/*. First hit (query param present): verify,
// set the session cookie, redirect to the clean URL so the token doesn't linger in
// browser history/referrer headers. Subsequent hits: verify the cookie and proxy.
func (g *Gateway) IframeEntryHandler(tokens *IframeTokenIssuer, idParam func(*http.Request) string, remainderPath func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		moduleID := idParam(r)

		if navToken := r.URL.Query().Get(iframeQueryParam); navToken != "" {
			claims, err := tokens.Verify(navToken)
			if err != nil || claims.ModuleID != moduleID {
				http.Error(w, "invalid or expired iframe token", http.StatusUnauthorized)
				return
			}

			sessionToken, err := tokens.Issue(claims, CookieTTL)
			if err != nil {
				http.Error(w, "issuing session", http.StatusInternalServerError)
				return
			}

			http.SetCookie(w, &http.Cookie{
				Name:     IframeCookieName,
				Value:    sessionToken,
				Path:     "/", // deliberately root-scoped; see IframeFallbackHandler
				HttpOnly: true,
				Secure:   true,
				SameSite: http.SameSiteLaxMode,
				MaxAge:   int(CookieTTL.Seconds()),
			})

			clean := *r.URL
			q := clean.Query()
			q.Del(iframeQueryParam)
			clean.RawQuery = q.Encode()
			http.Redirect(w, r, clean.String(), http.StatusFound)
			return
		}

		claims, ok := g.verifyIframeCookie(r, tokens)
		if !ok || claims.ModuleID != moduleID {
			http.Error(w, "missing or invalid iframe session", http.StatusUnauthorized)
			return
		}

		g.proxyIframeRequest(w, r, claims, remainderPath(r))
	})
}

// IframeFallbackHandler is the general fix for the known failure mode carried over from
// OpenDataPlatform: a third-party UI (Superset, JupyterLab, ...) that issues its own
// follow-up API calls as absolute/root-relative paths (e.g. fetch("/api/v1/chart"))
// escapes any proxy keyed purely on a /iframe/{id}/ path prefix, because the browser
// resolves that path against core's own origin root, not the iframe's mount path.
//
// The fix is to key iframe routing off the session cookie instead of the path: this
// handler is registered as the router's catch-all (lowest priority, matches only once
// nothing else does) and, if a valid IframeCookieName cookie is present, proxies the
// request's original path as-is to that session's module. Because the cookie is
// Path=/-scoped (not Path=/iframe/{id}/), the browser attaches it to exactly these
// escaped root-relative calls too, so they still reach the right module backend.
func (g *Gateway) IframeFallbackHandler(tokens *IframeTokenIssuer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := g.verifyIframeCookie(r, tokens)
		if !ok {
			http.NotFound(w, r)
			return
		}
		// ADR 0069's "Implementation notes", bug 1: the session cookie alone can't distinguish
		// the embedded module's own follow-up call from the person having left the module and
		// asked for something else — a plain page refresh, a shell link opened in a new tab,
		// core hit directly — because the cookie (Path=/) is attached to all of them while the
		// session is live, which item C's renewal now keeps alive for a session's whole
		// duration. Sec-Fetch-Dest tells them apart: "document" is a top-level browser
		// navigation, never something an embedded page's own JS issues (that's "empty", or
		// "iframe" for a nested navigation within the module itself). booth-core doesn't serve
		// the shell — that's booth-design's job, proxied separately — so for a "document"
		// request this handler has no page of its own to hand back; refuse it exactly like
		// having no session cookie at all, rather than silently proxying a top-level navigation
		// into whatever module happens to be running. A browser that omits the header (some
		// older browsers still do) falls through to proxying as before — this is a real
		// hardening, not the sole boundary the cookie's Path=/ scoping already accepts.
		if r.Header.Get(secFetchDestHeader) == secFetchDestDocument {
			http.NotFound(w, r)
			return
		}
		g.proxyIframeRequest(w, r, claims, r.URL.Path)
	})
}

const (
	secFetchDestHeader   = "Sec-Fetch-Dest"
	secFetchDestDocument = "document"
)

func (g *Gateway) verifyIframeCookie(r *http.Request, tokens *IframeTokenIssuer) (IframeClaims, bool) {
	cookie, err := r.Cookie(IframeCookieName)
	if err != nil {
		return IframeClaims{}, false
	}
	claims, err := tokens.Verify(cookie.Value)
	if err != nil {
		return IframeClaims{}, false
	}
	return claims, true
}

func (g *Gateway) proxyIframeRequest(w http.ResponseWriter, r *http.Request, claims IframeClaims, forwardPath string) {
	mod, ok := g.Modules.Get(claims.ModuleID)
	if !ok {
		http.Error(w, fmt.Sprintf("module %q not found in registry", claims.ModuleID), http.StatusNotFound)
		return
	}

	target, err := url.Parse(mod.BaseURL())
	if err != nil {
		http.Error(w, "module has invalid base URL", http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = g.Transport
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.URL.Path = forwardPath
		// The module trusts these the same way it trusts the regular gateway path's
		// headers (core-platform-api.md: modules independently verify identity
		// regardless; these headers are a convenience, not the trust boundary).
		req.Header.Set(auth.HeaderBoothWorkspace, claims.Workspace)
		req.Header.Set(auth.HeaderBoothRole, claims.Role)

		// ADR 0069: the iframe-proxy path has no bearer token to forward at all (a plain
		// iframe navigation, or a third-party UI's own root-relative follow-up call, can't
		// carry one), so this signed, per-request, module-audience-bound assertion is the
		// only thing a module can independently re-verify on this path. Del first — never
		// forward whatever the embedded page's own JS may have set on this header itself —
		// then set a fresh one only if minting succeeds, so a minting failure degrades to no
		// header rather than a stale or client-supplied one reaching the module.
		req.Header.Del(auth.HeaderBoothIdentity)
		if g.IframeIdentity != nil {
			token, err := g.IframeIdentity.Mint(claims.ModuleID, claims.Workspace, claims.Role, claims.Subject)
			if err != nil {
				log.Printf("iframe-proxy identity: minting failed for module %q: %v", claims.ModuleID, err)
			} else {
				req.Header.Set(auth.HeaderBoothIdentity, token)
			}
		}
	}

	proxy.ServeHTTP(w, r)
}
