// Package sidecar implements the credential sidecar (ADR 0095, contracts/credential-sidecar.md):
// a small, standalone binary that makes native database/lakehouse access invisible to a module's
// own pod by calling the credential broker (internal/credentialbroker, ADR 0080/0088) on a timer
// and exposing the result as either a localhost Postgres wire-protocol proxy or a refreshed AWS
// shared-credentials file. It shares nothing with the broker's own server-side code beyond the
// wire types (credentialbroker.Request/Response) — this package is a client, not a second broker.
package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/projectbooth/booth-core/internal/credentialbroker"
)

// TokenSource returns the bearer token to present to the broker, read fresh on every call so an
// externally-rotated token (e.g. a human session token refreshed by the browser) is always
// picked up — the sidecar never caches or owns the pod's identity itself (contracts/
// credential-sidecar.md's "Identity" section: it authenticates with whatever is already mounted).
type TokenSource func() (string, error)

// StaticToken returns a TokenSource for a token that never changes for the process's lifetime
// (the common case: a workload token minted once for an unattended run).
func StaticToken(token string) TokenSource {
	return func() (string, error) { return token, nil }
}

// FileToken returns a TokenSource that re-reads path on every call, for an identity some other
// process in the pod keeps current on disk.
func FileToken(path string) TokenSource {
	return func() (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading token file %s: %w", path, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
}

// RefusalError is the broker's own considered refusal — a config error (bad scope, bad kind, the
// caller's role doesn't permit the requested access, no provider registered) that will fail
// identically on any retry. Mirrors credentialbroker.ProviderError's "never log the body as if it
// might contain a credential" property: a refusal body is diagnostic text, never a secret.
type RefusalError struct {
	StatusCode int
	Body       string
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("broker refused the request: status %d: %s", e.StatusCode, e.Body)
}

// refusalStatus reports whether status is one of the broker's documented refusal codes
// (contracts/credential-sidecar.md: "422/403/404 — bad scope, bad kind, no provider registered")
// plus 400/401, which are the same class of "this exact request can never succeed" failure.
func refusalStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// BrokerClient calls POST /api/credentials (contracts/credential-broker.md).
type BrokerClient struct {
	BaseURL    string
	Workspace  string
	Token      TokenSource
	HTTPClient *http.Client
}

// NewBrokerClient builds a BrokerClient with a sane per-call timeout.
func NewBrokerClient(baseURL, workspace string, token TokenSource) *BrokerClient {
	return &BrokerClient{
		BaseURL: strings.TrimRight(baseURL, "/"), Workspace: workspace, Token: token,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Issue makes one credential request. Returns a *RefusalError for a response the broker will
// never answer differently on retry; any other non-nil error (network failure, timeout, a 5xx, an
// unparseable body) is a transient outage the caller should retry with backoff rather than give
// up on — see docs/decisions/0015's renewal-loop design for exactly where that line is drawn.
func (c *BrokerClient) Issue(ctx context.Context, req credentialbroker.Request) (credentialbroker.Response, error) {
	token, err := c.Token()
	if err != nil {
		return credentialbroker.Response{}, fmt.Errorf("reading identity token: %w", err)
	}
	if token == "" {
		return credentialbroker.Response{}, errors.New("no identity token available")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return credentialbroker.Response{}, fmt.Errorf("encoding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/credentials", bytes.NewReader(body))
	if err != nil {
		return credentialbroker.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("X-Workspace", c.Workspace)

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return credentialbroker.Response{}, fmt.Errorf("calling broker: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return credentialbroker.Response{}, fmt.Errorf("reading broker response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if refusalStatus(resp.StatusCode) {
			return credentialbroker.Response{}, &RefusalError{StatusCode: resp.StatusCode, Body: string(raw)}
		}
		return credentialbroker.Response{}, fmt.Errorf("broker returned status %d", resp.StatusCode)
	}

	var out credentialbroker.Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return credentialbroker.Response{}, fmt.Errorf("unparseable broker response: %w", err)
	}
	if out.LeaseID == "" || out.ExpiresAt.IsZero() || len(out.Credential) == 0 {
		return credentialbroker.Response{}, errors.New("broker response missing leaseId/expiresAt/credential")
	}
	return out, nil
}
