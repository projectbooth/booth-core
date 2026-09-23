package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-core/internal/iframeidentity"
)

// registerIframeIdentity mounts ADR 0069's well-known documents at their own path prefix,
// distinct from workload.Service's (which lives at core's root) so the two issuers are never
// reachable at the same URL. Both are public and unauthenticated — this is how a module learns
// to trust core as this token class's issuer, the same discovery mechanism it already uses for
// the deployment's OIDC provider. There is no POST route here: unlike workload tokens, an
// iframe-identity assertion is minted only by core itself, on the iframe-proxy path, never on
// request from a module.
func registerIframeIdentity(r chi.Router, svc *iframeidentity.Service) {
	r.Get(iframeidentity.RoutePrefix+iframeidentity.JWKSPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.JWKS())
	})
	r.Get(iframeidentity.RoutePrefix+iframeidentity.DiscoveryPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.Discovery())
	})
}
