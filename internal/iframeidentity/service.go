package iframeidentity

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/projectbooth/booth-core/internal/auth"
)

const (
	// RoutePrefix is where this issuer's well-known documents (and, by construction, its issuer
	// URL) live under core's own base URL — distinct from the workload-token issuer, which is
	// published at core's root, so the two are never reachable at the same URL even if a
	// deployment's public base URL is otherwise identical.
	RoutePrefix = "/iframe-identity"

	// JWKSPath and DiscoveryPath are relative to Options.Issuer, mirroring workload.Service's
	// well-known layout.
	JWKSPath      = "/.well-known/jwks.json"
	DiscoveryPath = "/.well-known/openid-configuration"

	// DefaultTTL is an assertion's lifetime. ADR 0069 requires "exp ≤ 2 minutes"; this is the
	// only value Service ever issues (see NewService) — there's no per-deployment reason to make
	// it longer, since a fresh assertion is minted on literally every proxied request rather than
	// reused for a whole session.
	DefaultTTL = 2 * time.Minute

	// ModuleClaim names, in the assertion, the module it was minted for — the same value as
	// `aud`, carried as its own claim purely for a downstream log line to read without decoding
	// `aud`'s array-or-string ambiguity.
	ModuleClaim = "booth_module"
)

// Options configures a Service.
type Options struct {
	// Issuer is core's own issuer URL for iframe-proxy assertions — distinct from both the
	// deployment's OIDC provider and workload.Service's issuer (ADR 0069). Conventionally
	// <core's base URL> + RoutePrefix. Must be reachable by every iframe-proxy module (it serves
	// the JWKS).
	Issuer string

	// GroupsClaim is the claim carrying role — the deployment's configured groups claim name, so
	// a module reads this assertion with the exact code it already uses for a human's OIDC token.
	GroupsClaim string

	Now func() time.Time
}

// Service mints iframe-proxy identity assertions and publishes the key material to verify them.
type Service struct {
	keys *Keys
	opts Options
}

// NewService builds a Service. GroupsClaim defaults to "groups"; the clock defaults to the
// system clock.
func NewService(keys *Keys, opts Options) *Service {
	opts.Issuer = strings.TrimRight(opts.Issuer, "/")
	if opts.GroupsClaim == "" {
		opts.GroupsClaim = "groups"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{keys: keys, opts: opts}
}

// Issuer returns core's iframe-identity issuer URL (no trailing slash).
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

// errInvalidAssertionInput guards against minting a broken assertion from malformed inputs; it
// should never actually trigger given callers only ever pass an already-validated IframeClaims,
// but a signed, well-formed-but-meaningless assertion is a worse failure mode than refusing to
// mint at all.
var errInvalidAssertionInput = errors.New("cannot mint an iframe-identity assertion")

// Mint issues a fresh assertion scoped to one iframe-proxied request: `aud` = moduleID, `sub` =
// the person's own subject (never a workload-shaped one — this path exists only for a human's
// browser session), and the groups claim shaped exactly like ADR 0025's human-token grammar
// (`/workspaces/<workspace>/<role>`), so a module's existing ADR 0041 role-derivation code reads
// it with no new logic. Lifetime is fixed at DefaultTTL.
func (s *Service) Mint(moduleID, workspace, role, subject string) (string, error) {
	if moduleID == "" || workspace == "" || subject == "" || auth.RoleRank(auth.Role(role)) == 0 {
		return "", fmt.Errorf("%w: moduleID=%q workspace=%q role=%q subject=%q",
			errInvalidAssertionInput, moduleID, workspace, role, subject)
	}

	now := s.opts.Now()
	signer, err := s.keys.signer()
	if err != nil {
		return "", fmt.Errorf("preparing signer: %w", err)
	}

	claims := jwt.Claims{
		Issuer:    s.opts.Issuer,
		Subject:   subject,
		Audience:  jwt.Audience{moduleID},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		Expiry:    jwt.NewNumericDate(now.Add(DefaultTTL)),
		ID:        randomID(),
	}
	extra := map[string]any{
		s.opts.GroupsClaim: []string{fmt.Sprintf("/workspaces/%s/%s", workspace, role)},
		ModuleClaim:        moduleID,
	}
	raw, err := jwt.Signed(signer).Claims(claims).Claims(extra).Serialize()
	if err != nil {
		return "", fmt.Errorf("signing assertion: %w", err)
	}
	return raw, nil
}

// randomID is a unique `jti`, in case a downstream log wants to tell two assertions apart.
func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err)) // the process can't safely continue
	}
	return hex.EncodeToString(b)
}
