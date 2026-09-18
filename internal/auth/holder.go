package auth

import "sync/atomic"

// VerifierHolder holds a *Verifier that may not be ready yet. Booth-core's HTTP server
// starts listening (and answers /healthz) immediately at boot even if the configured
// OIDC provider isn't reachable yet — nothing guarantees an external Keycloak (or any
// other IdP) is up before core is, especially on a fresh cluster bring-up, and the
// control plane crash-looping until it happens to be is worse than serving 503s on
// auth-gated routes for a while. cmd/core retries auth.NewVerifier in the background and
// calls Store once it succeeds.
type VerifierHolder struct {
	v atomic.Pointer[Verifier]
}

// Store makes v the verifier Middleware uses for all subsequent requests.
func (h *VerifierHolder) Store(v *Verifier) {
	h.v.Store(v)
}

// Get returns the current verifier, or ok=false if none has been stored yet.
func (h *VerifierHolder) Get() (*Verifier, bool) {
	v := h.v.Load()
	return v, v != nil
}
