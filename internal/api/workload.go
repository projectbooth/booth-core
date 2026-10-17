package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-core/internal/workload"
)

const maxMintRequestBytes = 4 << 10

// registerWorkload mounts ADR 0056's routes. None of them go through auth.Middleware: that
// middleware verifies a *human's* OIDC token against the deployment's IdP, and these routes are
// deliberately outside it — the minting endpoint has its own credential, and the two
// well-known documents are public (they are how other modules learn to trust core's issuer).
func registerWorkload(r chi.Router, svc *workload.Service) {
	r.Get(workload.JWKSPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.JWKS())
	})
	r.Get(workload.DiscoveryPath, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.Discovery())
	})
	r.Post(workload.MintPath, handleMintWorkloadToken(svc))
}

// mintResponse is the body returned by POST /api/internal/workload-tokens.
type mintResponse struct {
	Token     string    `json:"token"`
	TokenType string    `json:"tokenType"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Role is the role actually granted — the lesser of the requested ceiling and the run
	// owner's current role — so a caller can see when it got less than it asked for.
	Role string `json:"role"`
}

func handleMintWorkloadToken(svc *workload.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Tokens must never be cached by anything between core and the module.
		w.Header().Set("Cache-Control", "no-store")

		presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if presented == "" || presented == r.Header.Get("Authorization") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="booth-workload-minting"`)
			http.Error(w, "missing minting credential", http.StatusUnauthorized)
			return
		}
		moduleID, err := svc.AuthenticateModule(presented)
		if err != nil {
			mintError(w, err)
			return
		}

		var req workload.Request
		body := http.MaxBytesReader(w, r.Body, maxMintRequestBytes)
		dec := json.NewDecoder(body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		tok, err := svc.Mint(r.Context(), moduleID, req)
		if err != nil {
			mintError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, mintResponse{
			Token: tok.JWT, TokenType: "Bearer", ExpiresAt: tok.ExpiresAt.UTC(), Role: string(tok.Role),
		})
	}
}

// mintErrorResponse is the body for a mint refusal that has a machine-readable code (ADR 0119
// amendment 3 point 5) — only the two causes workload.RefusalCode recognizes get one; every
// other refusal below stays a plain-text body with no code (ADR 0058's anti-probing rule).
type mintErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func mintError(w http.ResponseWriter, err error) {
	switch {
	case workload.IsInvalidCredential(err):
		w.Header().Set("WWW-Authenticate", `Bearer realm="booth-workload-minting"`)
		http.Error(w, "invalid minting credential", http.StatusUnauthorized)
		return
	case errors.Is(err, workload.ErrInvalidRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, workload.ErrUnavailable):
		log.Printf("workload identity: %v", err)
		http.Error(w, "workload identity is temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	// Everything left refuses with 403. Only the two service-account causes get a
	// machine-readable `code` in a JSON body — a person owner's lack of access and a module's
	// lost entitlement stay one indistinguishable plain-text 403, same as before this ADR.
	if code, ok := workload.RefusalCode(err); ok {
		writeJSON(w, http.StatusForbidden, mintErrorResponse{Error: err.Error(), Code: code})
		return
	}
	if errors.Is(err, workload.ErrNotEntitled) || errors.Is(err, workload.ErrOwnerNoAccess) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	log.Printf("workload identity: minting failed: %v", err)
	http.Error(w, "minting failed", http.StatusInternalServerError)
}
