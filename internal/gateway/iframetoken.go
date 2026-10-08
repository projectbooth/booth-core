package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// IframeClaims is what an iframe-proxy session token carries: enough for the gateway to
// re-derive the caller's identity for the embedded module without a server-side session
// store (so this works the same whether core is running as one replica or many).
type IframeClaims struct {
	ModuleID  string
	Workspace string
	Role      string
	Subject   string
	ExpiresAt time.Time

	// IsOperator carries the caller's real token's /platform/operator claim (ADR 0094's
	// correction, 2026-09-30/2026-10-07) through to the iframe session, since an iframe-proxied
	// module never sees that real token at all — set at IframeURLIssuer.URLFor time from
	// identity.Claims.Groups and threaded through proxyIframeRequest's Mint call so the minted
	// X-Booth-Identity assertion can carry it too.
	IsOperator bool

	// PreferredUsername carries the caller's real token's preferred_username claim through to
	// the iframe session, the same way IsOperator does, so the minted X-Booth-Identity assertion
	// can show a module (e.g. booth-streamlit) something more readable than the raw `sub`. Often
	// empty — not every provider/client scope includes it (auth.Claims' own doc comment).
	PreferredUsername string
}

// IframeTokenIssuer mints and verifies the short-lived, HMAC-signed tokens behind
// ui-integration.md's iframe-proxy auth mechanism: "a short-lived access token passed as
// a query parameter on the initial iframe navigation, plus a scoped cookie for the
// module's own follow-up API calls" (carried forward from OpenDataPlatform per
// ARCHITECTURE.md §6). A signed, self-describing token (rather than a server-side
// session table) means this works unmodified whether core runs as one replica or many.
type IframeTokenIssuer struct {
	secret []byte
}

func NewIframeTokenIssuer(secret []byte) *IframeTokenIssuer {
	return &IframeTokenIssuer{secret: secret}
}

// Issue mints a token valid for ttl. The query-param leg of the flow should use a short
// ttl (order of a minute — it's only alive long enough for the initial navigation); the
// cookie set from it can carry a longer-lived reissued token (see CookieTTL in
// iframeproxy.go).
func (i *IframeTokenIssuer) Issue(claims IframeClaims, ttl time.Duration) (string, error) {
	claims.ExpiresAt = time.Now().Add(ttl)
	payload := encodeClaims(claims)
	sig := i.sign(payload)
	return payload + "." + sig, nil
}

// Verify checks the signature and expiry and returns the embedded claims.
func (i *IframeTokenIssuer) Verify(token string) (IframeClaims, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return IframeClaims{}, errors.New("malformed iframe token")
	}
	payload, sig := parts[0], parts[1]

	if !hmac.Equal([]byte(i.sign(payload)), []byte(sig)) {
		return IframeClaims{}, errors.New("iframe token signature mismatch")
	}

	claims, err := decodeClaims(payload)
	if err != nil {
		return IframeClaims{}, fmt.Errorf("decoding iframe token: %w", err)
	}

	if time.Now().After(claims.ExpiresAt) {
		return IframeClaims{}, errors.New("iframe token expired")
	}

	return claims, nil
}

func (i *IframeTokenIssuer) sign(payload string) string {
	mac := hmac.New(sha256.New, i.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// encodeClaims/decodeClaims use a deliberately simple pipe-delimited format rather than
// JSON+base64 — this token is never parsed by anything but this package. Every field except
// PreferredUsername is guaranteed not to contain "|" (module IDs and workspace slugs are
// already restricted to [a-z0-9-] by contracts/module-manifest.md and decision 0001; role is a
// fixed enum; subject comes from the IdP's `sub` claim, which OIDC guarantees is safe ASCII;
// IsOperator is a fixed "1"/"0"). PreferredUsername has no such guarantee — it's a free-form
// display string an IdP can populate with anything, including "|" — so it alone is
// percent-encoded (url.QueryEscape/QueryUnescape) before joining, which both removes any literal
// "|" and tolerates an empty string unchanged.
func encodeClaims(c IframeClaims) string {
	isOperator := "0"
	if c.IsOperator {
		isOperator = "1"
	}
	raw := strings.Join([]string{
		c.ModuleID, c.Workspace, c.Role, c.Subject, strconv.FormatInt(c.ExpiresAt.Unix(), 10), isOperator,
		url.QueryEscape(c.PreferredUsername),
	}, "|")
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeClaims(payload string) (IframeClaims, error) {
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return IframeClaims{}, err
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 7 {
		return IframeClaims{}, errors.New("wrong number of claim fields")
	}
	expUnix, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return IframeClaims{}, fmt.Errorf("parsing expiry: %w", err)
	}
	preferredUsername, err := url.QueryUnescape(parts[6])
	if err != nil {
		return IframeClaims{}, fmt.Errorf("decoding preferred username: %w", err)
	}
	return IframeClaims{
		ModuleID:          parts[0],
		Workspace:         parts[1],
		Role:              parts[2],
		Subject:           parts[3],
		ExpiresAt:         time.Unix(expUnix, 0),
		IsOperator:        parts[5] == "1",
		PreferredUsername: preferredUsername,
	}, nil
}
