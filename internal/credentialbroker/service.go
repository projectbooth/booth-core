package credentialbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/registry"
)

// apiWriteSubjectPrefix marks a workload token minted for exactly one purpose: booth-api's
// per-(API, granting owner) write sidecar (ADR 0117 reconciled design item 3, subject
// "apiwrite:<api-id>"). The prefix alone is the signal, needing no new stored state — ADR 0056's
// subject grammar already makes a "<kind>:<id>" prefix structurally meaningful (see
// internal/workload/service.go's subjectPattern), and this token's one job is a table-scoped
// write lease, never anything broader, even though its role is capped at "editor" (ADR 0117:
// "an apiwrite: token can request only tablewrite; a readwrite request from it is refused" —
// without this, an editor-capped apiwrite token could ask the broker for a whole-database
// readwrite, exactly the hole ADR 0117's "Reconciled design" overruled booth-core's first design
// note for).
const apiWriteSubjectPrefix = "apiwrite:"

const (
	// DefaultMaxTTL is the ceiling on ttlSeconds a caller may request — ADR 0080's "mandatory
	// short TTL... a stricter default ceiling than an HTTP grant() token's" (workload.DefaultTokenTTL
	// is 10 minutes). A request asking for more is clamped down to this, not refused: the caller
	// asked for "at most this long," same spirit as a workload token's roleCeiling never being
	// treated as an error when it exceeds what's actually granted. A *provider* may still need to
	// clamp back up to its own floor (e.g. MinIO's expiring service accounts can't go below 15
	// minutes) — that's the provider's own documented capability limit, not a violation of this
	// ceiling; see docs/decisions/0014.
	DefaultMaxTTL = 5 * time.Minute

	// ProviderPath is the fixed path every provider module implements on its own HTTP surface —
	// part of this contract, the same way workload.MintPath is fixed for the minting endpoint.
	// Core calls exactly this path on whichever module's BaseURL() is registered for the
	// requested kind.
	ProviderPath = "/internal/credentials"

	// providerCallTimeout bounds a single provider call, so one slow/hung provider can't stall
	// the broker route indefinitely.
	providerCallTimeout = 10 * time.Second

	// AccessRead / AccessReadWrite / AccessTableWrite are the values Request.Access accepts.
	// Shared, broker-level vocabulary (unlike Scope, which is opaque per kind) specifically
	// because the broker's own authorization decision needs to read it regardless of kind — see
	// docs/decisions/0014.
	AccessRead      = "read"
	AccessReadWrite = "readwrite"

	// AccessTableWrite is a distinct top-level value (ADR 0117 "Reconciled design" item 4), never
	// translated to/from AccessReadWrite anywhere in this package. readwrite still means a lease
	// in the whole-database _rw group; tablewrite means a lease in a per-grant write group scoped
	// to the named tables in scope.write, and nothing else. Keeping them distinct, rather than
	// letting readwrite's existing meaning stretch to cover "sometimes narrower," is what lets
	// booth-database's provider refuse a tablewrite request with no scope.write (and a
	// read/readwrite request that carries one) rather than silently guessing which was meant.
	AccessTableWrite = "tablewrite"
)

var (
	// ErrInvalidRequest wraps a malformed request body (HTTP 400).
	ErrInvalidRequest = errors.New("invalid credential request")

	// ErrForbidden means the caller's resolved role doesn't permit the requested access (HTTP 403).
	ErrForbidden = errors.New("caller's role does not permit the requested access")

	// ErrNoProvider means no installed module declares providesCredentials for this kind (HTTP 404).
	ErrNoProvider = errors.New("no module provides this credential kind")

	// ErrAmbiguousProvider means more than one module declares the same kind — a fleet
	// misconfiguration core refuses to guess through (HTTP 500).
	ErrAmbiguousProvider = errors.New("more than one module provides this credential kind")

	// ErrProviderUnavailable wraps a network failure, timeout, or malformed response from the
	// provider (HTTP 503) — distinct from ProviderError, which is the provider's own considered
	// refusal (e.g. 422 scope_not_supported), relayed as-is rather than mapped to this.
	ErrProviderUnavailable = errors.New("credential provider is unavailable")

	// ErrAuditRecordFailed wraps a failure to record an issuance's audit entry, returned
	// *alongside* a valid Response — the credential was genuinely minted and must still reach the
	// caller (an already-issued credential can't be safely un-issued, and refusing to return it
	// would just cause a retry that mints a redundant second one). Callers of Issue must check
	// for this specifically rather than discarding Response whenever err != nil.
	ErrAuditRecordFailed = errors.New("credential issued but its audit entry could not be recorded")
)

// ProviderError is the provider's own non-2xx response to an otherwise-valid, authorized
// request — e.g. ADR 0080's "refuse rather than widen" (a 422 scope_not_supported). Body is the
// provider's raw response, relayed to the original caller verbatim: providers must never put a
// credential value in a refusal response, and the broker doesn't try to launder it either way —
// it only ever forwards it, the same as it forwards a success response's credential unread.
type ProviderError struct {
	StatusCode int
	Body       []byte
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider refused: status %d", e.StatusCode)
}

var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Request is a credential request. Adopted, with one deliberate deviation, from booth-lakehouse's
// provisional design (tested against real MinIO/Lakekeeper before this contract existed):
// `access` is a shared, top-level field rather than nested inside the opaque `scope` object,
// because the broker's own authorization decision needs to read it regardless of kind — see
// docs/decisions/0014 for the reasoning.
type Request struct {
	Kind       string          `json:"kind"`
	TTLSeconds int             `json:"ttlSeconds,omitempty"`
	Access     string          `json:"access"`
	Scope      json.RawMessage `json:"scope"`
	Options    json.RawMessage `json:"options,omitempty"`

	// OnBehalfOf names the identity booth-api is acting for (ADR 0117 reconciled design item 4),
	// e.g. the app subject a declared write came from. Passed through to the provider's
	// Requester.OnBehalfOf verbatim, for that provider's own audit record only — authorize()
	// never reads it, and it never widens or narrows what the already-resolved (subject,
	// workspace, role) may do. Optional; every existing caller that omits it is unaffected.
	OnBehalfOf string `json:"onBehalfOf,omitempty"`
}

// Response is what's returned to the requester — the provider's own minted response, relayed
// through unread except for the fields the broker itself needs (LeaseID, ExpiresAt) to build its
// audit record. Credential is never inspected, logged, or otherwise touched beyond this pass-through.
type Response struct {
	LeaseID    string          `json:"leaseId"`
	Kind       string          `json:"kind"`
	ExpiresAt  time.Time       `json:"expiresAt"`
	Scope      json.RawMessage `json:"scope"`
	Credential json.RawMessage `json:"credential"`
}

// providerRequest is what the broker sends to a provider — Request plus the already-authorized
// caller's identity, which the provider trusts outright (ADR 0080: "a provider never re-derives
// trust itself, it trusts the broker's forwarded, already-authorized request") and uses only for
// its own audit logging, never for any further authorization decision.
type providerRequest struct {
	Kind       string            `json:"kind"`
	TTLSeconds int               `json:"ttlSeconds"`
	Access     string            `json:"access"`
	Scope      json.RawMessage   `json:"scope"`
	Options    json.RawMessage   `json:"options,omitempty"`
	Requester  providerRequester `json:"requester"`
}

type providerRequester struct {
	Subject    string `json:"subject"`
	Workspace  string `json:"workspace"`
	Role       string `json:"role"`
	OnBehalfOf string `json:"onBehalfOf,omitempty"`
}

// Modules is the slice of the registry the broker needs: every installed module, to find whoever
// declared providesCredentials for a kind. Implemented by *registry.Registry.
type Modules interface {
	List() []registry.Module
}

// Options configures a Service.
type Options struct {
	MaxTTL     time.Duration
	HTTPClient *http.Client
	Now        func() time.Time
}

// Service is the credential broker (ADR 0080): authenticates nothing itself (that's
// auth.Middleware's job, same as the gateway), authorizes an already-resolved caller against a
// requested kind/access, routes to the one module that provides that kind, and relays what it
// mints — recording an audit entry for every genuine issuance, never for a refusal.
type Service struct {
	keys    *Keys
	modules Modules
	audit   AuditStore
	opts    Options
}

// NewService builds a Service. Defaults: DefaultMaxTTL, a 10s-per-call HTTP client, the system
// clock.
func NewService(keys *Keys, modules Modules, audit AuditStore, opts Options) *Service {
	if opts.MaxTTL <= 0 {
		opts.MaxTTL = DefaultMaxTTL
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: providerCallTimeout}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{keys: keys, modules: modules, audit: audit, opts: opts}
}

// Issue authorizes and routes a credential request for a caller already resolved by
// auth.Middleware (the same identity/workspace/role chain the gateway and workload-token minting
// already use) — subject, workspace and role are exactly Identity.Claims.Subject,
// Identity.Active.Workspace, Identity.Active.Role.
func (s *Service) Issue(ctx context.Context, subject, workspace string, role auth.Role, req Request) (Response, error) {
	if err := validate(req); err != nil {
		return Response{}, err
	}
	if !authorize(subject, role, req.Access) {
		return Response{}, ErrForbidden
	}

	mod, err := s.findProvider(req.Kind)
	if err != nil {
		return Response{}, err
	}

	ttl := req.TTLSeconds
	if ttl <= 0 || time.Duration(ttl)*time.Second > s.opts.MaxTTL {
		ttl = int(s.opts.MaxTTL.Seconds())
	}

	body, err := json.Marshal(providerRequest{
		Kind: req.Kind, TTLSeconds: ttl, Access: req.Access, Scope: req.Scope, Options: req.Options,
		Requester: providerRequester{Subject: subject, Workspace: workspace, Role: string(role), OnBehalfOf: req.OnBehalfOf},
	})
	if err != nil {
		return Response{}, fmt.Errorf("%w: encoding provider request: %v", ErrProviderUnavailable, err)
	}

	// ADR 0117 "Reconciled design" item 4: booth-database's revoke request (tablewrite,
	// options.revoke=true, tables: []) answers 201 with credential: null — a genuine success,
	// not an incomplete response, and the one case this layer must not reject the way it rejects
	// every other provider that forgets to include a credential.
	resp, err := s.callProvider(ctx, mod, body, isRevokeRequest(req))
	if err != nil {
		return Response{}, err
	}

	scope := resp.Scope
	if scope == nil {
		scope = req.Scope
	}
	if err := s.audit.Record(ctx, Entry{
		LeaseID: resp.LeaseID, RequesterSubject: subject, Workspace: workspace, Role: string(role),
		Kind: req.Kind, Access: req.Access, Scope: scope, ProviderModuleID: mod.Spec.ID,
		IssuedAt: s.opts.Now(), ExpiresAt: resp.ExpiresAt,
	}); err != nil {
		// The credential is already minted and about to be handed to the caller; a failure to
		// record the audit entry must not un-issue it or make the caller re-request (which would
		// mint a second, redundant credential) — see ErrAuditRecordFailed's own doc comment.
		return resp, fmt.Errorf("%w: issued %s credential (lease %s): %v", ErrAuditRecordFailed, req.Kind, resp.LeaseID, err)
	}
	return resp, nil
}

func validate(req Request) error {
	switch {
	case !kindPattern.MatchString(req.Kind):
		return fmt.Errorf("%w: kind must match %s", ErrInvalidRequest, kindPattern)
	case req.Access != AccessRead && req.Access != AccessReadWrite && req.Access != AccessTableWrite:
		return fmt.Errorf("%w: access must be %q, %q or %q", ErrInvalidRequest, AccessRead, AccessReadWrite, AccessTableWrite)
	case len(req.Scope) == 0 || string(req.Scope) == "null":
		return fmt.Errorf("%w: scope is required", ErrInvalidRequest)
	case req.TTLSeconds < 0:
		return fmt.Errorf("%w: ttlSeconds must not be negative", ErrInvalidRequest)
	}
	return nil
}

// authorize applies ADR 0048's already-established precedent (editor/owner write, viewer reads)
// to the broker's one shared, cross-kind field: a viewer may request read-scoped credentials, but
// never readwrite or tablewrite ones, regardless of kind. read and readwrite's own rules are
// exactly as they were before tablewrite existed — unaffected by the subject-prefix branch below.
//
// An apiwrite:-subject token (ADR 0117 reconciled design item 4) is further restricted to
// tablewrite alone, even though its role is capped at editor: that token exists for exactly one
// purpose (booth-api's write sidecar requesting its own table-scoped lease), and nothing about
// holding an editor-capped role should let it also ask for a whole-database readwrite, or widen
// into read. This is a stricter rule for one subject shape, layered on top of the role check, not
// a replacement for it — the role check still applies within that one allowed access value.
func authorize(subject string, role auth.Role, access string) bool {
	if strings.HasPrefix(subject, apiWriteSubjectPrefix) {
		return access == AccessTableWrite && auth.RoleRank(role) >= auth.RoleRank(auth.RoleEditor)
	}
	switch access {
	case AccessReadWrite, AccessTableWrite:
		return auth.RoleRank(role) >= auth.RoleRank(auth.RoleEditor)
	default:
		return auth.RoleRank(role) > 0
	}
}

func (s *Service) findProvider(kind string) (registry.Module, error) {
	var found []registry.Module
	for _, m := range s.modules.List() {
		if m.Spec.ProvidesCredentials == nil {
			continue
		}
		for _, k := range m.Spec.ProvidesCredentials.Kinds {
			if k == kind {
				found = append(found, m)
				break
			}
		}
	}
	switch len(found) {
	case 0:
		return registry.Module{}, ErrNoProvider
	case 1:
		return found[0], nil
	default:
		return registry.Module{}, ErrAmbiguousProvider
	}
}

// isRevokeRequest reports whether req is booth-database's documented revoke shape (ADR 0117
// "Reconciled design" item 4 / docs/design-app-writes.md §3 in booth-database: access
// "tablewrite", options: {"revoke": true}) — the one request shape a provider's 201 may legally
// answer with credential: null. req.Options is opaque to the broker beyond this one check; a
// malformed or absent options value simply means "not a revoke," never an error here — validate()
// already rejected anything else wrong with the request before this is reached.
func isRevokeRequest(req Request) bool {
	if req.Access != AccessTableWrite || len(req.Options) == 0 {
		return false
	}
	var opts struct {
		Revoke bool `json:"revoke"`
	}
	if err := json.Unmarshal(req.Options, &opts); err != nil {
		return false
	}
	return opts.Revoke
}

// isNullOrAbsent reports whether raw is JSON null or simply wasn't sent at all — the two shapes
// that mean "no credential" rather than "a credential the broker should pass through unread."
func isNullOrAbsent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || string(trimmed) == "null"
}

func (s *Service) callProvider(ctx context.Context, mod registry.Module, body []byte, allowNullCredential bool) (Response, error) {
	callCtx, cancel := context.WithTimeout(ctx, providerCallTimeout)
	defer cancel()

	url := strings.TrimRight(mod.BaseURL(), "/") + ProviderPath
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+s.keys.ProviderCredential(mod.Spec.ID))

	httpResp, err := s.opts.HTTPClient.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("%w: calling %s: %v", ErrProviderUnavailable, mod.Spec.ID, err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return Response{}, fmt.Errorf("%w: reading response from %s: %v", ErrProviderUnavailable, mod.Spec.ID, err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return Response{}, &ProviderError{StatusCode: httpResp.StatusCode, Body: raw}
	}

	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Response{}, fmt.Errorf("%w: %s returned an unparseable response: %v", ErrProviderUnavailable, mod.Spec.ID, err)
	}
	if resp.LeaseID == "" || resp.ExpiresAt.IsZero() {
		return Response{}, fmt.Errorf("%w: %s returned an incomplete response (missing leaseId/expiresAt)", ErrProviderUnavailable, mod.Spec.ID)
	}
	if isNullOrAbsent(resp.Credential) && !allowNullCredential {
		return Response{}, fmt.Errorf("%w: %s returned an incomplete response (missing credential)", ErrProviderUnavailable, mod.Spec.ID)
	}
	return resp, nil
}
