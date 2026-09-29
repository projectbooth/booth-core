package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/credentialbroker"
)

const maxCredentialRequestBytes = 16 << 10

// handleIssueCredential backs POST /api/credentials (ADR 0080). It runs in the same
// workload-token-trusting router group as the gateway route (see NewRouter) — its caller is
// "a task or module," per the contract, exactly the population that already presents workload
// tokens on the ordinary gateway path.
func handleIssueCredential(svc *credentialbroker.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store") // a response body may carry a live credential

		identity, ok := auth.FromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusUnauthorized)
			return
		}

		var req credentialbroker.Request
		body := http.MaxBytesReader(w, r.Body, maxCredentialRequestBytes)
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		resp, err := svc.Issue(r.Context(), identity.Claims.Subject, identity.Active.Workspace, identity.Active.Role, req)
		if err != nil {
			if !errors.Is(err, credentialbroker.ErrAuditRecordFailed) {
				credentialError(w, err)
				return
			}
			// A real credential was minted; it must still reach the caller (see
			// ErrAuditRecordFailed's doc comment) — only the audit write failed, loudly logged
			// here since the audit trail itself is meant to stay distinct from ordinary logs.
			log.Printf("credential broker: %v", err)
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

func credentialError(w http.ResponseWriter, err error) {
	var providerErr *credentialbroker.ProviderError
	switch {
	case errors.As(err, &providerErr):
		// The provider's own considered refusal (e.g. 422 scope_not_supported per ADR 0080's
		// "refuse rather than widen") — relayed verbatim, status and body both, never
		// reinterpreted. A provider must never put a credential in a refusal body; the broker
		// doesn't inspect it either way, only forwards it.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(providerErr.StatusCode)
		_, _ = w.Write(providerErr.Body)
	case errors.Is(err, credentialbroker.ErrInvalidRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, credentialbroker.ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, credentialbroker.ErrNoProvider):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, credentialbroker.ErrAmbiguousProvider):
		log.Printf("credential broker: %v", err)
		http.Error(w, "credential broker is misconfigured for this kind", http.StatusInternalServerError)
	case errors.Is(err, credentialbroker.ErrProviderUnavailable):
		log.Printf("credential broker: %v", err)
		http.Error(w, "credential provider is temporarily unavailable", http.StatusServiceUnavailable)
	default:
		log.Printf("credential broker: unexpected error: %v", err)
		http.Error(w, "credential issuance failed", http.StatusInternalServerError)
	}
}
