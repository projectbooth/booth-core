package workload

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/directory"
	"github.com/projectbooth/booth-core/internal/registry"
)

const (
	// DefaultTokenTTL is how long a minted token is valid (ADR 0056). Renewable by minting again.
	DefaultTokenTTL = 10 * time.Minute

	// DefaultMaxOwnerAge bounds how stale the owner's recorded role may be. See Service.
	DefaultMaxOwnerAge = 7 * 24 * time.Hour

	// JWKSPath is where core publishes its token-verification keys, relative to the issuer URL.
	// Fixed so other modules can configure "trust this issuer" without any per-deployment path.
	JWKSPath = "/.well-known/jwks.json"

	// DiscoveryPath is the standard OIDC discovery document location, served so a module whose
	// JWT library discovers keys the usual way (go-oidc) can add core as an issuer with one line.
	DiscoveryPath = "/.well-known/openid-configuration"

	// MintPath is the minting endpoint, relative to the issuer URL.
	MintPath = "/api/internal/workload-tokens"

	// ModuleClaim names, in the minted token, the module that requested it. Audit only: it lets a
	// downstream log say "pipeline's run job:42", not just "job:42".
	ModuleClaim = "booth_module"
)

var (
	// ErrInvalidRequest wraps a malformed mint request (HTTP 400).
	ErrInvalidRequest = errors.New("invalid mint request")

	// ErrNotEntitled means the calling module does not (or no longer) declare
	// workloadIdentity.mint (HTTP 403).
	ErrNotEntitled = errors.New("module is not entitled to mint workload tokens")

	// ErrOwnerNoAccess means the run's owning user holds no usable role in the workspace right
	// now — never seen there, removed, or too long since core last saw them (HTTP 403). The
	// message deliberately doesn't say which, so a caller can't probe membership with it.
	ErrOwnerNoAccess = errors.New("the run's owner has no current access to that workspace")

	// ErrUnavailable means core can't currently answer (the directory is down) (HTTP 503).
	ErrUnavailable = errors.New("workload identity is temporarily unavailable")
)

// subjectPattern is the required shape of a run identifier: `<kind>:<id>`, e.g. `job:7f3a`.
// ADR 0056 says the subject "must identify the run, e.g. job:<job-id>, never a person"; a
// person's `sub` (a UUID, or `auth0|abc`, or an email) never has this shape, so the format
// keeps a run's identity structurally distinct from a human's in every downstream audit log.
var subjectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}:[A-Za-z0-9._:-]{1,200}$`)

// workspacePattern is ADR 0025's workspace slug grammar.
var workspacePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// Request is the body of POST /api/internal/workload-tokens.
//
// Owner is a contract addition to ADR 0056's `{workspace, subject, roleCeiling}`: the ADR says
// the role is capped by "whatever the run's owning user currently holds", which needs to name
// that user. See docs/decisions/0010-workload-identity.md.
type Request struct {
	Workspace   string `json:"workspace"`
	Subject     string `json:"subject"`
	RoleCeiling string `json:"roleCeiling"`
	Owner       string `json:"owner"`
}

// Token is a minted workload token.
type Token struct {
	JWT       string
	ExpiresAt time.Time
	Role      auth.Role
}

// Modules is the slice of the registry the service needs.
type Modules interface {
	Get(id string) (registry.Module, bool)
}

// Options configures a Service.
type Options struct {
	// Issuer is core's own issuer URL, distinct from the OIDC provider's. It must be reachable
	// by every module that verifies workload tokens (it serves the JWKS).
	Issuer string

	// Audience is what `aud` is set to: the same value every module already expects of a human
	// token (the OIDC client ID). Empty omits the claim.
	Audience string

	// GroupsClaim is the claim carrying the role — the deployment's configured groups claim
	// name, so a module reads it with the exact code it uses for a human's token.
	GroupsClaim string

	TTL         time.Duration
	MaxOwnerAge time.Duration
	Now         func() time.Time
}

// Service mints workload tokens. It holds no state beyond its inputs: every mint re-derives the
// role from the directory at that moment, so nothing cached can outlive a demotion.
type Service struct {
	keys    *Keys
	modules Modules
	users   directory.Store
	opts    Options
}

// NewService builds a Service. Defaults: 10-minute tokens, 7-day owner-recency bound, the
// system clock, and a groups claim named "groups".
func NewService(keys *Keys, modules Modules, users directory.Store, opts Options) *Service {
	opts.Issuer = strings.TrimRight(opts.Issuer, "/")
	if opts.TTL <= 0 {
		opts.TTL = DefaultTokenTTL
	}
	if opts.MaxOwnerAge <= 0 {
		opts.MaxOwnerAge = DefaultMaxOwnerAge
	}
	if opts.GroupsClaim == "" {
		opts.GroupsClaim = "groups"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{keys: keys, modules: modules, users: users, opts: opts}
}

// Issuer returns core's issuer URL (no trailing slash).
func (s *Service) Issuer() string { return s.opts.Issuer }

// JWKS returns the public key set published at JWKSPath.
func (s *Service) JWKS() jose.JSONWebKeySet { return s.keys.JWKS() }

// Discovery returns the OIDC discovery document published at DiscoveryPath.
func (s *Service) Discovery() map[string]any {
	return map[string]any{
		"issuer":                                s.opts.Issuer,
		"jwks_uri":                              s.opts.Issuer + JWKSPath,
		"id_token_signing_alg_values_supported": []string{string(signingAlg)},
		"subject_types_supported":               []string{"public"},
		"response_types_supported":              []string{"id_token"},
	}
}

// AuthenticateModule turns a presented minting credential into the module it was issued to,
// and confirms that module is still entitled: it exists in the registry and *currently*
// declares workloadIdentity.mint. The second check is what makes "omit the field and the module
// cannot mint" hold for a module that once declared it and a copy of whose credential survives.
func (s *Service) AuthenticateModule(presented string) (string, error) {
	id, ok := s.keys.ModuleForCredential(presented)
	if !ok {
		return "", errInvalidCredential
	}
	m, found := s.modules.Get(id)
	if !found || m.Spec.WorkloadIdentity == nil || !m.Spec.WorkloadIdentity.Mint {
		return "", ErrNotEntitled
	}
	return id, nil
}

// errInvalidCredential is the credential wasn't one core issued (HTTP 401).
var errInvalidCredential = errors.New("invalid minting credential")

// IsInvalidCredential reports whether err means the presented credential was not one core issued.
func IsInvalidCredential(err error) bool { return errors.Is(err, errInvalidCredential) }

// Mint issues a token for a run of moduleID (already authenticated by AuthenticateModule).
//
// The token's role is the lesser of req.RoleCeiling and the owner's role in the workspace *as
// the directory holds it at this instant* — never a value carried in the request or remembered
// from an earlier mint. A demoted owner's next mint comes out lower, a removed owner's is
// refused, and there is nothing to revoke.
func (s *Service) Mint(ctx context.Context, moduleID string, req Request) (Token, error) {
	if err := validate(req); err != nil {
		return Token{}, err
	}

	// Re-check entitlement rather than trusting the caller to have just called
	// AuthenticateModule: cheap, and it keeps this method safe to call on its own.
	if m, ok := s.modules.Get(moduleID); !ok || m.Spec.WorkloadIdentity == nil || !m.Spec.WorkloadIdentity.Mint {
		return Token{}, ErrNotEntitled
	}

	ownerRole, err := s.ownerRole(ctx, req.Owner, req.Workspace)
	if err != nil {
		return Token{}, err
	}
	role := auth.LesserRole(auth.Role(req.RoleCeiling), ownerRole)
	if role == "" {
		return Token{}, ErrOwnerNoAccess
	}

	now := s.opts.Now()
	exp := now.Add(s.opts.TTL)
	signer, err := s.keys.signer()
	if err != nil {
		return Token{}, fmt.Errorf("preparing signer: %w", err)
	}

	claims := jwt.Claims{
		Issuer:    s.opts.Issuer,
		Subject:   req.Subject,
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		Expiry:    jwt.NewNumericDate(exp),
		ID:        randomID(),
	}
	if s.opts.Audience != "" {
		claims.Audience = jwt.Audience{s.opts.Audience}
	}
	// The groups claim is exactly what a human's token carries (ADR 0025's grammar), so a
	// module's existing role derivation reads it with no new code. One entry: this token is for
	// one workspace, and cannot be replayed into another.
	extra := map[string]any{
		s.opts.GroupsClaim: []string{fmt.Sprintf("/workspaces/%s/%s", req.Workspace, role)},
		ModuleClaim:        moduleID,
	}
	raw, err := jwt.Signed(signer).Claims(claims).Claims(extra).Serialize()
	if err != nil {
		return Token{}, fmt.Errorf("signing token: %w", err)
	}

	// Audit trail (ADR 0056). Never logs the token itself.
	log.Printf("workload identity: module=%s subject=%s workspace=%s owner=%s ceiling=%s granted=%s",
		moduleID, req.Subject, req.Workspace, req.Owner, req.RoleCeiling, role)

	return Token{JWT: raw, ExpiresAt: exp, Role: role}, nil
}

// ownerRole looks up the owner's current role in workspace. The directory is the only source
// core has for it (roles exist nowhere but in tokens — ADR 0025), so this is "the role in the
// owner's most recent verified token", bounded: an owner not seen within MaxOwnerAge has no
// usable role, which is what stops a user who was removed and never signed in again from
// keeping their scheduled jobs privileged forever.
func (s *Service) ownerRole(ctx context.Context, owner, workspace string) (auth.Role, error) {
	u, found, err := s.users.Get(ctx, owner, workspace)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !found {
		return "", ErrOwnerNoAccess
	}
	if s.opts.Now().Sub(u.LastSeenAt) > s.opts.MaxOwnerAge {
		return "", ErrOwnerNoAccess
	}
	role := auth.Role(u.Roles[workspace])
	if auth.RoleRank(role) == 0 {
		return "", ErrOwnerNoAccess
	}
	return role, nil
}

func validate(req Request) error {
	switch {
	case !workspacePattern.MatchString(req.Workspace):
		return fmt.Errorf("%w: workspace must match %s", ErrInvalidRequest, workspacePattern)
	case !subjectPattern.MatchString(req.Subject):
		return fmt.Errorf("%w: subject must identify the run as <kind>:<id> (e.g. job:42), not a person", ErrInvalidRequest)
	case auth.RoleRank(auth.Role(req.RoleCeiling)) == 0:
		return fmt.Errorf("%w: roleCeiling must be owner, editor, or viewer", ErrInvalidRequest)
	case req.Owner == "" || len(req.Owner) > 512:
		return fmt.Errorf("%w: owner (the run's owning user's sub) is required", ErrInvalidRequest)
	}
	return nil
}

// randomID is a unique `jti`, so any one minted token can be told apart in an audit log.
func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err)) // the process can't safely continue
	}
	return hex.EncodeToString(b)
}

// verifyLeeway tolerates clock skew between the replica that minted a token and the one
// checking it.
const verifyLeeway = 5 * time.Second

// Verify checks a workload token against core's own signing key and returns the same auth.Claims
// a human's token yields from auth.Verifier, so the gateway (ADR 0059) derives workspace and role
// from it with exactly the code it uses for a person. It is what makes core the second trusted
// issuer on its own gateway route, mirroring what every module does via OIDC discovery.
//
// It checks, and rejects on any failure: RS256 only (nothing else parses), a signature by this
// deployment's key, `iss` equal to core's issuer, `aud` including the configured audience (when one
// is set), expiry and not-before, a non-empty `sub`, and the requesting-module claim that only
// Mint ever writes. A token any other issuer signed — including the deployment's IdP — fails the
// signature check no matter what it claims about itself.
func (s *Service) Verify(_ context.Context, rawToken string) (*auth.Claims, error) {
	parsed, err := jwt.ParseSigned(rawToken, []jose.SignatureAlgorithm{signingAlg})
	if err != nil {
		return nil, fmt.Errorf("parsing workload token: %w", err)
	}
	var std jwt.Claims
	var extra map[string]any
	if err := parsed.Claims(&s.keys.signing.PublicKey, &std, &extra); err != nil {
		return nil, fmt.Errorf("workload token signature: %w", err)
	}

	expected := jwt.Expected{Issuer: s.opts.Issuer, Time: s.opts.Now()}
	if s.opts.Audience != "" {
		expected.AnyAudience = jwt.Audience{s.opts.Audience}
	}
	if err := std.ValidateWithLeeway(expected, verifyLeeway); err != nil {
		return nil, fmt.Errorf("workload token claims: %w", err)
	}
	if std.Subject == "" {
		return nil, errors.New("workload token has no subject")
	}
	if m, _ := extra[ModuleClaim].(string); m == "" {
		return nil, errors.New("workload token was not minted for a module")
	}

	claims := &auth.Claims{Subject: std.Subject}
	if list, ok := extra[s.opts.GroupsClaim].([]any); ok {
		for _, item := range list {
			if g, ok := item.(string); ok {
				claims.Groups = append(claims.Groups, g)
			}
		}
	}
	return claims, nil
}
