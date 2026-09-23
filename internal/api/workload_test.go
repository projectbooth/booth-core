package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/config"
	"github.com/projectbooth/booth-core/internal/directory"
	"github.com/projectbooth/booth-core/internal/gateway"
	"github.com/projectbooth/booth-core/internal/registry"
	"github.com/projectbooth/booth-core/internal/workload"
)

// coreIssuer is a real core router served over HTTP, with workload identity on, next to a
// separate test IdP standing in for Keycloak. The issuer URL must be the server's own address
// (it serves its JWKS), so the server is created first and the handler attached after.
type coreIssuer struct {
	url   string
	idp   *testIDP
	keys  *workload.Keys
	users *directory.MemoryStore
	reg   *registry.Registry
	http  *httptest.Server
	svc   *workload.Service

	// now is the clock the workload service reads; tests move it to mint an already-old token.
	now func() time.Time
}

func newCoreIssuer(t *testing.T) *coreIssuer {
	t.Helper()
	idp := newTestIDP(t)
	keys, err := workload.NewKeys()
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(nil)
	c := &coreIssuer{
		url: "http://" + srv.Listener.Addr().String(), idp: idp, keys: keys,
		users: directory.NewMemoryStore(), reg: registry.New(), http: srv, now: time.Now,
	}

	// The same wiring cmd/core does: the recorder feeds the directory from verified tokens.
	recorder := directory.NewRecorder(c.users)
	svc := workload.NewService(keys, c.reg, c.users, workload.Options{
		Issuer: c.url, Audience: "c", GroupsClaim: "groups", Now: func() time.Time { return c.now() },
	})
	c.svc = svc
	tokens := gateway.NewIframeTokenIssuer([]byte("s"))
	srv.Config.Handler = NewRouter(Deps{
		Verifier: idp.holder, Registry: c.reg, Gateway: gateway.New(c.reg),
		IframeTokens: tokens, IframeURLs: gateway.NewIframeURLIssuer(tokens),
		Directory: c.users, DirectoryRecorder: recorder, Workload: svc,
	})
	srv.Start()
	t.Cleanup(srv.Close)

	// pipeline declares workloadIdentity; notes doesn't.
	c.reg.Put(registry.Module{Namespace: "ns", Spec: boothv1alpha1.BoothModuleSpec{
		ID: "pipeline", WorkloadIdentity: &boothv1alpha1.WorkloadIdentityRequirement{Mint: true},
	}})
	c.reg.Put(registry.Module{Namespace: "ns", Spec: boothv1alpha1.BoothModuleSpec{ID: "notes"}})
	return c
}

func (i *testIDP) verifier(t *testing.T) *auth.Verifier {
	t.Helper()
	v, ok := i.holder.Get()
	if !ok {
		t.Fatal("test IdP verifier not ready")
	}
	return v
}

// signIn makes a human request through core with a token carrying groups, so the directory
// records the user exactly as it does in production.
func (c *coreIssuer) signIn(t *testing.T, sub string, groups ...string) {
	t.Helper()
	tok := c.idp.token(t, sub, map[string]any{"groups": groups})
	req, _ := http.NewRequest(http.MethodGet, c.url+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in as %s: status %d", sub, resp.StatusCode)
	}
}

type mintResult struct {
	status int
	body   mintResponse
	raw    string
}

func (c *coreIssuer) mint(t *testing.T, bearer string, body any) mintResult {
	t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case string:
		buf.WriteString(b)
	default:
		_ = json.NewEncoder(&buf).Encode(b)
	}
	req, _ := http.NewRequest(http.MethodPost, c.url+workload.MintPath, &buf)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(resp.Body)
	out := mintResult{status: resp.StatusCode, raw: raw.String()}
	_ = json.Unmarshal(raw.Bytes(), &out.body)
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a token response", resp.Header.Get("Cache-Control"))
	}
	return out
}

func mintBody(owner, ceiling string) map[string]string {
	return map[string]string{"workspace": "acme", "subject": "job:42", "roleCeiling": ceiling, "owner": owner}
}

// --- Regression: a token from a different issuer (Keycloak) is not accepted as a minting credential.

func TestMintEndpoint_RejectsAnyTokenThatIsNotACoreIssuedCredential(t *testing.T) {
	c := newCoreIssuer(t)
	c.signIn(t, "u-alice", "/workspaces/acme/owner")

	// A perfectly valid token from the deployment's IdP — the strongest thing a caller could
	// present that core otherwise trusts — must not open the minting endpoint. Every variant
	// below is one a careless auth check could accept.
	valid := c.idp.token(t, "u-alice", map[string]any{"groups": []string{"/workspaces/acme/owner"}})
	claimingToBeThePipeline := c.idp.token(t, "pipeline", map[string]any{
		"groups": []string{"/workspaces/acme/owner"}, "booth_module": "pipeline", "azp": "pipeline",
	})
	// A token minted by core itself (the *right* issuer for workload tokens) must not bootstrap
	// more minting either: only the module's provisioned credential may.
	own := c.mint(t, c.keys.Credential("pipeline"), mintBody("u-alice", "editor"))
	if own.status != http.StatusOK {
		t.Fatalf("setup: minting with the real credential = %d %s", own.status, own.raw)
	}
	parts := strings.Split(valid, ".")

	for name, bearer := range map[string]string{
		"a valid OIDC token from the IdP":              valid,
		"an IdP token claiming to be the pipeline":     claimingToBeThePipeline,
		"a workload token core minted":                 own.body.Token,
		"no credential":                                "",
		"garbage":                                      "not-a-credential",
		"a JWT dressed with the credential tag":        "bwmc." + parts[1] + "." + parts[2],
		"a credential of a module with a wrong secret": "bwmc.pipeline.AAAA",
	} {
		t.Run(name, func(t *testing.T) {
			got := c.mint(t, bearer, mintBody("u-alice", "viewer"))
			if got.status != http.StatusUnauthorized {
				t.Fatalf("status = %d (%s), want 401", got.status, got.raw)
			}
			if got.body.Token != "" {
				t.Fatal("a token was minted for an unauthenticated caller")
			}
		})
	}
}

// --- Regression: a module that never declared workloadIdentity is rejected. -------------------

func TestMintEndpoint_RejectsModuleThatNeverDeclaredWorkloadIdentity(t *testing.T) {
	c := newCoreIssuer(t)
	c.signIn(t, "u-alice", "/workspaces/acme/owner")

	// notes is installed but declared nothing; it was never provisioned a credential. Even a
	// well-formed credential for it (the strongest case) mints nothing.
	got := c.mint(t, c.keys.Credential("notes"), mintBody("u-alice", "viewer"))
	if got.status != http.StatusForbidden || got.body.Token != "" {
		t.Fatalf("undeclared module: status %d, body %s; want 403 and no token", got.status, got.raw)
	}

	// pipeline's credential can't be turned into notes' identity by editing the module name.
	stolen := strings.Replace(c.keys.Credential("pipeline"), ".pipeline.", ".notes.", 1)
	if got := c.mint(t, stolen, mintBody("u-alice", "viewer")); got.status != http.StatusUnauthorized {
		t.Fatalf("re-labelled credential: status %d, want 401", got.status)
	}

	// The control: the module that did declare it mints fine.
	if got := c.mint(t, c.keys.Credential("pipeline"), mintBody("u-alice", "viewer")); got.status != http.StatusOK {
		t.Fatalf("declared module: status %d %s, want 200", got.status, got.raw)
	}
}

// --- Regression: the role never exceeds the owner's, and follows them live — through the real
// --- auth path that populates the directory, not a hand-seeded one.

func TestMintEndpoint_RoleFollowsTheOwnersLiveRoleThroughTheRealAuthPath(t *testing.T) {
	c := newCoreIssuer(t)
	cred := c.keys.Credential("pipeline")

	// Alice signs in as an editor; the run asks for owner.
	c.signIn(t, "u-alice", "/workspaces/acme/editor")
	got := c.mint(t, cred, mintBody("u-alice", "owner"))
	if got.status != http.StatusOK || got.body.Role != "editor" {
		t.Fatalf("status %d role %q (%s); want 200 and editor — never more than the owner holds", got.status, got.body.Role, got.raw)
	}

	// She is demoted; her next request through core records it, and the very next mint drops.
	c.signIn(t, "u-alice", "/workspaces/acme/viewer")
	got = c.mint(t, cred, mintBody("u-alice", "owner"))
	if got.status != http.StatusOK || got.body.Role != "viewer" {
		t.Fatalf("after demotion: status %d role %q; want viewer", got.status, got.body.Role)
	}

	// She is removed from the workspace (her token no longer lists it): mints are refused.
	c.signIn(t, "u-alice", "/workspaces/elsewhere/owner")
	got = c.mint(t, cred, mintBody("u-alice", "viewer"))
	if got.status != http.StatusForbidden || got.body.Token != "" {
		t.Fatalf("after removal: status %d (%s); want 403 and no token", got.status, got.raw)
	}

	// Someone core has never seen can't be named as an owner to borrow a role.
	if got := c.mint(t, cred, mintBody("u-mallory", "viewer")); got.status != http.StatusForbidden {
		t.Fatalf("unknown owner: status %d, want 403", got.status)
	}
}

// --- The token is verified the same way every module verifies a human's. ----------------------

// A module trusts core as a second issuer with the same generic OIDC code it already has: point
// a verifier at core's issuer URL. This uses core's real auth.Verifier (go-oidc discovery, JWKS,
// issuer, audience, expiry), then reads the role with the same function used for human tokens —
// and checks it lands identically to a human's.
func TestMintedToken_VerifiesAndDerivesRoleExactlyLikeAHumans(t *testing.T) {
	c := newCoreIssuer(t)
	c.signIn(t, "u-alice", "/workspaces/acme/editor")
	minted := c.mint(t, c.keys.Credential("pipeline"), mintBody("u-alice", "owner"))
	if minted.status != http.StatusOK {
		t.Fatalf("mint: %d %s", minted.status, minted.raw)
	}

	// RequireAudience: the token must carry the audience human tokens do.
	workloadVerifier, err := auth.NewVerifier(context.Background(), config.OIDCConfig{
		IssuerURL: c.url, ClientID: "c", RequireAudience: true, GroupsClaim: "groups",
	})
	if err != nil {
		t.Fatalf("a module could not add core as an issuer via OIDC discovery: %v", err)
	}
	claims, err := workloadVerifier.Verify(context.Background(), minted.body.Token)
	if err != nil {
		t.Fatalf("token did not verify against core's issuer: %v", err)
	}
	if claims.Subject != "job:42" {
		t.Errorf("sub = %q, want job:42", claims.Subject)
	}

	human := c.idp.token(t, "u-alice", map[string]any{"groups": []string{"/workspaces/acme/editor"}})
	humanClaims, err := c.idp.verifier(t).Verify(context.Background(), human)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := auth.DeriveMemberships(claims.Groups), auth.DeriveMemberships(humanClaims.Groups); !reflect.DeepEqual(got, want) {
		t.Fatalf("workload token derives %v, a human's derives %v — they must be identical", got, want)
	}
	if role, ok := auth.RoleFor(auth.DeriveMemberships(claims.Groups), "acme"); !ok || role != auth.RoleEditor {
		t.Errorf("role for acme = %q, %v; want editor", role, ok)
	}
	if _, ok := auth.RoleFor(auth.DeriveMemberships(claims.Groups), "labs"); ok {
		t.Error("token grants a role in a workspace it wasn't minted for")
	}

	// The trust is one-way: each issuer's verifier rejects the other's tokens, so core's own
	// human-facing routes never accept a workload token, and the IdP's tokens never verify as core's.
	if _, err := workloadVerifier.Verify(context.Background(), human); err == nil {
		t.Error("core's workload verifier accepted a token from the IdP")
	}
	if _, err := c.idp.verifier(t).Verify(context.Background(), minted.body.Token); err == nil {
		t.Error("the IdP verifier accepted a token core minted")
	}
	req, _ := http.NewRequest(http.MethodGet, c.url+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+minted.body.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api/me with a workload token = %d, want 401", resp.StatusCode)
	}
}

func TestWellKnownDocuments(t *testing.T) {
	c := newCoreIssuer(t)

	resp, err := http.Get(c.url + "/.well-known/jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("JWKS status = %d", resp.StatusCode)
	}
	var jwks struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil || len(jwks.Keys) != 1 {
		t.Fatalf("JWKS = %+v, %v", jwks, err)
	}
	k := jwks.Keys[0]
	if k["kty"] != "RSA" || k["alg"] != "RS256" || k["use"] != "sig" || k["kid"] != c.keys.KeyID() {
		t.Errorf("unexpected key: %v", k)
	}
	if _, private := k["d"]; private {
		t.Error("JWKS exposes a private key")
	}

	resp2, err := http.Get(c.url + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var disc map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&disc)
	if disc["issuer"] != c.url || disc["jwks_uri"] != c.url+"/.well-known/jwks.json" {
		t.Errorf("discovery = %v", disc)
	}
}

func TestMintEndpoint_RejectsMalformedBodies(t *testing.T) {
	c := newCoreIssuer(t)
	c.signIn(t, "u-alice", "/workspaces/acme/owner")
	cred := c.keys.Credential("pipeline")

	for name, body := range map[string]any{
		"not json":              "nope",
		"empty":                 "",
		"an unknown field":      `{"workspace":"acme","subject":"job:1","roleCeiling":"viewer","owner":"u-alice","role":"owner"}`,
		"a person as subject":   map[string]string{"workspace": "acme", "subject": "u-alice", "roleCeiling": "viewer", "owner": "u-alice"},
		"an invalid ceiling":    mintBody("u-alice", "root"),
		"a missing owner":       map[string]string{"workspace": "acme", "subject": "job:1", "roleCeiling": "viewer"},
		"an oversized document": `{"workspace":"` + strings.Repeat("a", 8192) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := c.mint(t, cred, body); got.status != http.StatusBadRequest {
				t.Fatalf("status = %d (%s), want 400", got.status, got.raw)
			}
		})
	}
}

func TestRouter_WithoutWorkloadIdentityExposesNothing(t *testing.T) {
	idp := newTestIDP(t)
	router := routerWith(t, idp, nil)
	for _, path := range []string{"/.well-known/jwks.json", "/.well-known/openid-configuration", workload.MintPath} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code == http.StatusOK {
				t.Errorf("%s %s = 200 with workload identity off", method, path)
			}
		}
	}
}
