package api

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/credentialbroker"
	"github.com/projectbooth/booth-core/internal/registry"
)

// credProvider is a real HTTP server implementing the provider side of ADR 0080's contract
// (credentialbroker.ProviderPath), the same "real socket, real request" bar every other proxy
// test in this package already uses (see backend in gateway_workload_test.go) — there is no real
// booth-storage provider endpoint yet to run against (its own docs/decisions/0006 says so
// explicitly), so this is what "tested against a real provider" means until one exists.
type credProvider struct {
	mu       sync.Mutex
	received []credentialbroker.Request // via the wrapper below, minus the requester envelope
	lastAuth string
}

func (c *coreIssuer) addCredentialProvider(t *testing.T, moduleID string, kinds ...string) *credProvider {
	t.Helper()
	p := &credProvider{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != credentialbroker.ProviderPath {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Kind       string          `json:"kind"`
			TTLSeconds int             `json:"ttlSeconds"`
			Access     string          `json:"access"`
			Scope      json.RawMessage `json:"scope"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.received = append(p.received, credentialbroker.Request{Kind: body.Kind, TTLSeconds: body.TTLSeconds, Access: body.Access, Scope: body.Scope})
		p.lastAuth = r.Header.Get("Authorization")
		p.mu.Unlock()

		if body.Scope != nil && string(body.Scope) == `{"backendId":"lake","path":"forbidden"}` {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "scope_not_supported"})
			return
		}

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(credentialbroker.Response{
			LeaseID: "lease-e2e", Kind: body.Kind, ExpiresAt: time.Now().Add(time.Duration(body.TTLSeconds) * time.Second),
			Scope: body.Scope, Credential: json.RawMessage(`{"accessKeyId":"AKIA_TEST","secretAccessKey":"do-not-log-me"}`),
		})
	}))
	t.Cleanup(srv.Close)

	// Namespace deliberately left unset: a devregistry-shaped module (ServiceRef.Name is a
	// directly dialable host, no serviceRef.namespace) is used as-is (registry.Module.BaseURL) —
	// setting one here would make BaseURL() build an unroutable *.svc.cluster.local address, the
	// same mistake ui-integration's own real-cluster routing avoids.
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	c.reg.Put(registry.Module{Spec: boothv1alpha1.BoothModuleSpec{
		ID: moduleID, ServiceRef: boothv1alpha1.ServiceReference{Name: host, Port: int32(n)},
		ProvidesCredentials: &boothv1alpha1.CredentialProviderSpec{Kinds: kinds},
	}})
	return p
}

func (p *credProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.received)
}

type credResult struct {
	status int
	body   credentialbroker.Response
	raw    string
}

func (c *coreIssuer) issueCredential(t *testing.T, bearer, workspace string, reqBody credentialbroker.Request) credResult {
	t.Helper()
	buf, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest(http.MethodPost, c.url+"/api/credentials", bytes.NewReader(buf))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if workspace != "" {
		req.Header.Set(auth.HeaderWorkspace, workspace)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(resp.Body)
	out := credResult{status: resp.StatusCode, raw: raw.String()}
	_ = json.Unmarshal(raw.Bytes(), &out.body)
	return out
}

func credReq(access string) credentialbroker.Request {
	return credentialbroker.Request{Kind: "s3", Access: access, Scope: json.RawMessage(`{"backendId":"lake","path":"t1"}`)}
}

// --- ADR 0084's first ask: the broker must accept workload tokens. -----------------------------

func TestCredentialBroker_AcceptsAWorkloadToken(t *testing.T) {
	c := newCoreIssuer(t)
	provider := c.addCredentialProvider(t, "storage", "s3")
	c.signIn(t, "u-alice", "/workspaces/acme/editor")
	token := c.mustMint(t, "u-alice", "editor")

	req, _ := http.NewRequest(http.MethodPost, c.url+"/api/credentials", jsonBody(credReq(credentialbroker.AccessRead)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(auth.HeaderWorkspace, "acme")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a response that may carry a live credential", got)
	}
	var body credentialbroker.Response
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.LeaseID != "lease-e2e" {
		t.Errorf("LeaseID = %q", body.LeaseID)
	}
	if provider.count() != 1 {
		t.Fatalf("provider received %d requests, want 1", provider.count())
	}
}

func jsonBody(v any) *bytes.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// --- A real human OIDC token works identically. --------------------------------------------------

func TestCredentialBroker_AcceptsAHumanToken(t *testing.T) {
	c := newCoreIssuer(t)
	c.addCredentialProvider(t, "storage", "s3")
	token := c.idp.token(t, "u-alice", map[string]any{"groups": []string{"/workspaces/acme/owner"}})

	got := c.issueCredential(t, token, "acme", credReq(credentialbroker.AccessReadWrite))
	if got.status != http.StatusCreated {
		t.Fatalf("status %d (%s), want 201", got.status, got.raw)
	}
}

// --- Role-gated: a viewer (human or capped workload token) can't get readwrite. ------------------

func TestCredentialBroker_ViewerCannotRequestReadWrite(t *testing.T) {
	c := newCoreIssuer(t)
	c.addCredentialProvider(t, "storage", "s3")
	token := c.idp.token(t, "u-bob", map[string]any{"groups": []string{"/workspaces/acme/viewer"}})

	got := c.issueCredential(t, token, "acme", credReq(credentialbroker.AccessReadWrite))
	if got.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", got.status)
	}

	// read still works for the same caller.
	got = c.issueCredential(t, token, "acme", credReq(credentialbroker.AccessRead))
	if got.status != http.StatusCreated {
		t.Fatalf("read status = %d, want 201", got.status)
	}
}

// A workload token's role is already capped at mint time (ADR 0056/0058) — the broker's own
// authorization reads that capped role, so a run never gets more than its owner (and its
// roleCeiling) allowed, exactly the same invariant the gateway route already has to hold.
func TestCredentialBroker_WorkloadTokenRoleCapIsRespected(t *testing.T) {
	c := newCoreIssuer(t)
	c.addCredentialProvider(t, "storage", "s3")
	c.signIn(t, "u-alice", "/workspaces/acme/owner")
	viewerToken := c.mustMint(t, "u-alice", "viewer") // capped to viewer even though alice is owner

	got := c.issueCredential(t, viewerToken, "acme", credReq(credentialbroker.AccessReadWrite))
	if got.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (the token's capped role, not alice's real one, must govern)", got.status)
	}
}

// --- No provider registered for the kind. ---------------------------------------------------------

func TestCredentialBroker_NoProviderForKind(t *testing.T) {
	c := newCoreIssuer(t)
	token := c.idp.token(t, "u-alice", map[string]any{"groups": []string{"/workspaces/acme/owner"}})

	got := c.issueCredential(t, token, "acme", credentialbroker.Request{
		Kind: "postgres", Access: credentialbroker.AccessRead, Scope: json.RawMessage(`{}`),
	})
	if got.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", got.status)
	}
}

// --- A provider's own refusal (422 scope_not_supported) is relayed through the real router. -------

func TestCredentialBroker_RelaysAProviderRefusal(t *testing.T) {
	c := newCoreIssuer(t)
	c.addCredentialProvider(t, "storage", "s3")
	token := c.idp.token(t, "u-alice", map[string]any{"groups": []string{"/workspaces/acme/owner"}})

	got := c.issueCredential(t, token, "acme", credentialbroker.Request{
		Kind: "s3", Access: credentialbroker.AccessRead, Scope: json.RawMessage(`{"backendId":"lake","path":"forbidden"}`),
	})
	if got.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", got.status)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(got.raw), &body); err != nil || body["error"] != "scope_not_supported" {
		t.Errorf("body = %s", got.raw)
	}
}

// --- Ordinary workspace/token rules still apply: X-Workspace is required, an unknown token 401s. --

func TestCredentialBroker_RequiresWorkspaceAndAToken(t *testing.T) {
	c := newCoreIssuer(t)
	c.addCredentialProvider(t, "storage", "s3")
	token := c.idp.token(t, "u-alice", map[string]any{"groups": []string{"/workspaces/acme/owner"}})

	if got := c.issueCredential(t, token, "", credReq(credentialbroker.AccessRead)); got.status != http.StatusBadRequest {
		t.Errorf("no workspace: status = %d, want 400", got.status)
	}
	if got := c.issueCredential(t, "garbage", "acme", credReq(credentialbroker.AccessRead)); got.status != http.StatusUnauthorized {
		t.Errorf("garbage token: status = %d, want 401", got.status)
	}
	if got := c.issueCredential(t, "", "acme", credReq(credentialbroker.AccessRead)); got.status != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", got.status)
	}
}

// --- The full response body, as it crosses the real router, still never carries anything that
// --- looks like it was written into a log accidentally — this only proves the credential reaches
// --- the caller as designed; internal/credentialbroker's own tests prove it never reaches the audit. --

func TestCredentialBroker_ReturnsTheProviderCredentialToTheCaller(t *testing.T) {
	c := newCoreIssuer(t)
	c.addCredentialProvider(t, "storage", "s3")
	token := c.idp.token(t, "u-alice", map[string]any{"groups": []string{"/workspaces/acme/owner"}})

	got := c.issueCredential(t, token, "acme", credReq(credentialbroker.AccessRead))
	if got.status != http.StatusCreated {
		t.Fatalf("status = %d (%s)", got.status, got.raw)
	}
	var cred map[string]string
	if err := json.Unmarshal(got.body.Credential, &cred); err != nil || cred["secretAccessKey"] != "do-not-log-me" {
		t.Errorf("credential not returned to the caller: %s", got.body.Credential)
	}

	entries := c.credAudit.Entries()
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	if entries[0].LeaseID != "lease-e2e" || entries[0].RequesterSubject != "u-alice" {
		t.Errorf("audit entry = %+v", entries[0])
	}
}

func TestRouter_WithoutCredentialBrokerExposesNothing(t *testing.T) {
	idp := newTestIDP(t)
	router := routerWith(t, idp, nil) // Deps.CredentialBroker left nil
	token := idp.token(t, "u-alice", map[string]any{"groups": []string{"/workspaces/acme/owner"}})

	buf, _ := json.Marshal(credReq(credentialbroker.AccessRead))
	req := httptest.NewRequest(http.MethodPost, "/api/credentials", bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(auth.HeaderWorkspace, "acme")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code == http.StatusCreated {
		t.Fatalf("status = %d, want anything but 201 with the broker off", rec.Code)
	}
}
