package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	josejwt "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/gateway"
	"github.com/projectbooth/booth-core/internal/registry"
)

// backend is a module behind the gateway that records what actually reached it.
type backend struct {
	mu   sync.Mutex
	hits []http.Header
}

func (b *backend) last() (http.Header, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.hits) == 0 {
		return nil, false
	}
	return b.hits[len(b.hits)-1], true
}

func (b *backend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.hits)
}

// addBackend registers module id, served by a real HTTP server, in core's registry. A module with
// no serviceRef.namespace and a host:port name is dialled as-is (registry.Module.BaseURL).
func (c *coreIssuer) addBackend(t *testing.T, id string) *backend {
	t.Helper()
	b := &backend{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.hits = append(b.hits, r.Header.Clone())
		b.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached " + r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	c.reg.Put(registry.Module{Spec: boothv1alpha1.BoothModuleSpec{
		ID: id, ServiceRef: boothv1alpha1.ServiceReference{Name: host, Port: int32(n)},
	}})
	return b
}

// viaGateway makes a request to /modules/<id>/<path> as bearer, in workspace.
func (c *coreIssuer) viaGateway(t *testing.T, bearer, workspace, path string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, c.url+"/modules/"+path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if workspace != "" {
		req.Header.Set(auth.HeaderWorkspace, workspace)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func (c *coreIssuer) mustMint(t *testing.T, owner, ceiling string) string {
	t.Helper()
	got := c.mint(t, c.keys.Credential("pipeline"), mintBody(owner, ceiling))
	if got.status != http.StatusOK {
		t.Fatalf("mint: %d %s", got.status, got.raw)
	}
	return got.body.Token
}

// --- ADR 0059: a workload token reaches a module through the gateway. -------------------------

func TestGateway_WorkloadTokenReachesModuleWithCorrectForwardedIdentity(t *testing.T) {
	c := newCoreIssuer(t)
	storage := c.addBackend(t, "storage")
	c.signIn(t, "u-alice", "/workspaces/acme/editor")
	token := c.mustMint(t, "u-alice", "owner") // capped to editor by alice's own role

	status, body := c.viaGateway(t, token, "acme", "storage/files/list", nil)
	if status != http.StatusOK || body != "reached /files/list" {
		t.Fatalf("status %d body %q; want the module's response", status, body)
	}
	h, _ := storage.last()
	if got := h.Get(auth.HeaderBoothWorkspace); got != "acme" {
		t.Errorf("X-Booth-Workspace = %q, want acme", got)
	}
	if got := h.Get(auth.HeaderBoothRole); got != "editor" {
		t.Errorf("X-Booth-Role = %q, want editor", got)
	}
	// The module re-verifies the token itself (defense in depth); it must receive the same one.
	if got := h.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("forwarded Authorization = %q, want the workload token unchanged", got)
	}
}

// A run's role is capped at what it was minted with — which may be lower than what a human holds
// in that very workspace — and the gateway forwards exactly that, never the human's.
func TestGateway_CappedRoleIsForwardedAsIsNeverUpgraded(t *testing.T) {
	c := newCoreIssuer(t)
	storage := c.addBackend(t, "storage")
	c.signIn(t, "u-alice", "/workspaces/acme/owner")
	humanToken := c.idp.token(t, "u-alice", map[string]any{"groups": []string{"/workspaces/acme/owner"}})

	roleSeenBy := func(bearer string) string {
		t.Helper()
		if status, body := c.viaGateway(t, bearer, "acme", "storage/x", nil); status != http.StatusOK {
			t.Fatalf("status %d (%s)", status, body)
		}
		h, _ := storage.last()
		return h.Get(auth.HeaderBoothRole)
	}

	// The same person, in the same workspace, through the same gateway route:
	if got := roleSeenBy(humanToken); got != "owner" {
		t.Fatalf("human role = %q, want owner (test precondition)", got)
	}
	for ceiling, want := range map[string]string{"viewer": "viewer", "editor": "editor", "owner": "owner"} {
		if got := roleSeenBy(c.mustMint(t, "u-alice", ceiling)); got != want {
			t.Errorf("run with ceiling %s: forwarded role = %q, want %q", ceiling, got, want)
		}
	}
}

func TestGateway_WorkloadTokenIsScopedToItsWorkspaceAndHeaderRulesStillApply(t *testing.T) {
	c := newCoreIssuer(t)
	storage := c.addBackend(t, "storage")
	c.signIn(t, "u-alice", "/workspaces/acme/owner", "/workspaces/labs/owner")
	token := c.mustMint(t, "u-alice", "viewer") // minted for acme only

	if status, _ := c.viaGateway(t, token, "labs", "storage/x", nil); status != http.StatusForbidden {
		t.Errorf("token for acme used against labs: status %d, want 403", status)
	}
	if status, _ := c.viaGateway(t, token, "", "storage/x", nil); status != http.StatusBadRequest {
		t.Errorf("no X-Workspace: status %d, want 400 (same as a human)", status)
	}
	if storage.count() != 0 {
		t.Fatalf("refused requests reached the module %d time(s)", storage.count())
	}

	// A caller-supplied role header never survives: the module sees what core resolved.
	status, _ := c.viaGateway(t, token, "acme", "storage/x", map[string]string{
		auth.HeaderBoothRole: "owner", auth.HeaderBoothWorkspace: "labs",
	})
	h, _ := storage.last()
	if status != http.StatusOK || h.Get(auth.HeaderBoothRole) != "viewer" || h.Get(auth.HeaderBoothWorkspace) != "acme" {
		t.Errorf("status %d, forwarded role %q workspace %q; want viewer/acme regardless of forged headers",
			status, h.Get(auth.HeaderBoothRole), h.Get(auth.HeaderBoothWorkspace))
	}
}

// --- A token from neither issuer is still rejected, and the two can't be confused. ------------

func TestGateway_RejectsTokensFromNeitherIssuer(t *testing.T) {
	c := newCoreIssuer(t)
	storage := c.addBackend(t, "storage")
	c.signIn(t, "u-alice", "/workspaces/acme/owner")
	good := c.mustMint(t, "u-alice", "viewer")

	strangerKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	signer, _ := josejwt.NewSigner(josejwt.SigningKey{Algorithm: josejwt.RS256, Key: strangerKey}, (&josejwt.SignerOptions{}).WithType("JWT"))
	stranger := func(iss string) string {
		raw, err := jwt.Signed(signer).Claims(map[string]any{
			"iss": iss, "sub": "x", "aud": "c", "exp": time.Now().Add(time.Hour).Unix(),
			"groups": []string{"/workspaces/acme/owner"},
		}).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	parts := strings.Split(good, ".")

	// A token core minted, but a while ago: the clock moves, the token's own expiry ends it.
	c.now = func() time.Time { return time.Now().Add(-time.Hour) }
	old := c.mustMint(t, "u-alice", "viewer")
	c.now = time.Now

	for name, tok := range map[string]string{
		"no token":                           "",
		"garbage":                            "not-a-token",
		"a stranger's key, claiming core":    stranger(c.url),
		"a stranger's key, claiming the IdP": stranger(c.idp.url),
		"a stranger's key, claiming nobody":  stranger("https://nobody.example"),
		"a minted token, payload edited":     parts[0] + "." + parts[1] + "A." + parts[2],
		"an expired workload token":          old,
		"a minting credential":               c.keys.Credential("pipeline"),
	} {
		t.Run(name, func(t *testing.T) {
			if status, _ := c.viaGateway(t, tok, "acme", "storage/x", nil); status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", status)
			}
		})
	}
	if storage.count() != 0 {
		t.Fatalf("a rejected request reached the module %d time(s)", storage.count())
	}
}

func TestGateway_IssuersCannotBeConfusedWithEachOther(t *testing.T) {
	c := newCoreIssuer(t)
	storage := c.addBackend(t, "storage")
	c.signIn(t, "u-alice", "/workspaces/acme/viewer")

	// An IdP-signed token that *says* core issued it (an attempt to be treated as a run, or to
	// pick up workload-token handling): refused, because core checks it against its own key.
	forgedAsCore := c.idp.token(t, "job:1", map[string]any{"iss": c.url, "groups": []string{"/workspaces/acme/owner"}, "booth_module": "pipeline"})
	if status, _ := c.viaGateway(t, forgedAsCore, "acme", "storage/x", nil); status != http.StatusUnauthorized {
		t.Errorf("IdP token relabelled as core's: status %d, want 401", status)
	}

	// A workload token can't be replayed as the IdP's: the IdP verifier rejects it outright.
	minted := c.mustMint(t, "u-alice", "viewer")
	if _, err := c.idp.verifier(t).Verify(context.Background(), minted); err == nil {
		t.Error("the OIDC verifier accepted a workload token")
	}

	// And a real human token still verifies as a human's, unchanged by the second issuer.
	human := c.idp.token(t, "u-alice", map[string]any{"groups": []string{"/workspaces/acme/viewer"}})
	if status, _ := c.viaGateway(t, human, "acme", "storage/x", nil); status != http.StatusOK {
		t.Errorf("human token through the gateway: status %d, want 200", status)
	}
	if storage.count() != 1 {
		t.Errorf("module was reached %d times, want exactly the one legitimate request", storage.count())
	}
}

// --- The second issuer is trusted on the gateway route only. ----------------------------------

// Core's own privileged routes must keep refusing a workload token: it is trusted to be *forwarded*
// as a caller identity, not to act as a person against core itself (install/uninstall in
// particular are owner-gated, and a run's token can carry the owner role).
func TestWorkloadTokensAreRejectedByEveryCoreRouteExceptTheGateway(t *testing.T) {
	c := newCoreIssuer(t)
	c.addBackend(t, "storage")
	c.signIn(t, "u-alice", "/workspaces/acme/owner")
	token := c.mustMint(t, "u-alice", "owner")

	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/modules"},
		{http.MethodGet, "/api/users"},
		{http.MethodGet, "/api/modules/storage/iframe-url"},
		{http.MethodPost, "/api/modules/storage/install"},
		{http.MethodDelete, "/api/modules/storage?namespace=x"},
	} {
		req, _ := http.NewRequest(rt.method, c.url+rt.path, strings.NewReader(`{"namespace":"x","chartRef":"oci://x/y"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(auth.HeaderWorkspace, "acme")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with a workload token = %d, want 401", rt.method, rt.path, resp.StatusCode)
		}
	}

	// The control: the gateway route, same token, works.
	if status, _ := c.viaGateway(t, token, "acme", "storage/x", nil); status != http.StatusOK {
		t.Fatalf("gateway with the same token = %d, want 200", status)
	}
}

func TestGateway_WorkloadTokensWorkWhenTheIdPIsDownAndNeverEnterTheDirectory(t *testing.T) {
	c := newCoreIssuer(t)
	c.addBackend(t, "storage")
	c.signIn(t, "u-alice", "/workspaces/acme/editor")
	token := c.mustMint(t, "u-alice", "viewer")

	// The same core, but whose OIDC provider has never been reachable (verifier never stored).
	tokens := gateway.NewIframeTokenIssuer([]byte("s"))
	router := NewRouter(Deps{
		Verifier: &auth.VerifierHolder{}, Registry: c.reg, Gateway: gateway.New(c.reg),
		IframeTokens: tokens, IframeURLs: gateway.NewIframeURLIssuer(tokens),
		Directory: c.users, Workload: c.svc,
	})
	call := func(bearer string) int {
		req := httptest.NewRequest(http.MethodGet, "/modules/storage/x", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set(auth.HeaderWorkspace, "acme")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := call(token); got != http.StatusOK {
		t.Errorf("workload token with the IdP down = %d, want 200", got)
	}
	if got := call(c.idp.token(t, "u-alice", map[string]any{"groups": []string{"/workspaces/acme/editor"}})); got != http.StatusServiceUnavailable {
		t.Errorf("human token with the IdP down = %d, want 503", got)
	}

	// Runs are not people: nothing about job:42 was recorded (the recorder is on the real router).
	c.viaGateway(t, token, "acme", "storage/x", nil)
	if u, found, _ := c.users.Get(context.Background(), "job:42", "acme"); found {
		t.Errorf("a run was recorded in the user directory: %+v", u)
	}
}

func TestGateway_WithoutWorkloadIdentityStillRejectsCoreIssuedLookingTokens(t *testing.T) {
	idp := newTestIDP(t)
	router := routerWith(t, idp, nil) // Workload nil: the second issuer is simply not configured
	reg := registry.New()
	_ = reg
	req := httptest.NewRequest(http.MethodGet, "/modules/storage/x", nil)
	req.Header.Set("Authorization", "Bearer "+idp.token(t, "x", map[string]any{"iss": "http://core"}))
	req.Header.Set(auth.HeaderWorkspace, "acme")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
