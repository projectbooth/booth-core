package credentialbroker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/registry"
)

// stubModules is a Modules implementation backed by a plain slice, so tests can set up whatever
// registry shape they need without a real (or fake-client) registry.Registry.
type stubModules struct{ modules []registry.Module }

func (s stubModules) List() []registry.Module { return s.modules }

// realProvider is a genuine HTTP server standing in for a provider module's /internal/credentials
// endpoint — a real TCP listener, real request/response marshaling, not a Go interface mock. It's
// the closest thing to "a real provider" available today: booth-storage's own provider-side
// endpoint doesn't exist yet (see its docs/decisions/0006 — "not building the provider endpoint
// itself... booth-core's broker routing landing first is the correct order"), so this is what
// "routes correctly, tested against a real provider" means until one does.
type realProvider struct {
	*httptest.Server
	// received records every request this provider actually received, headers and body both, so
	// a test can assert on exactly what the broker sent over the wire.
	received []receivedRequest
	// respond is called per-request to build the response; defaults to a canned success.
	respond func(providerRequest) (status int, body any)
}

type receivedRequest struct {
	authHeader string
	path       string
	body       providerRequest
}

func newRealProvider(t *testing.T) *realProvider {
	t.Helper()
	p := &realProvider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body providerRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.received = append(p.received, receivedRequest{authHeader: r.Header.Get("Authorization"), path: r.URL.Path, body: body})

		respond := p.respond
		if respond == nil {
			respond = func(providerRequest) (int, any) {
				return http.StatusCreated, Response{
					LeaseID: "lease-abc", Kind: body.Kind, ExpiresAt: time.Now().Add(time.Duration(body.TTLSeconds) * time.Second),
					Scope: body.Scope, Credential: json.RawMessage(`{"accessKeyId":"AKIA...","secretAccessKey":"top-secret-value"}`),
				}
			}
		}
		status, out := respond(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(p.Server.Close)
	return p
}

func registerProvider(reg *stubModules, id, host string, kinds ...string) {
	u := strings.TrimPrefix(host, "http://")
	name, port := splitHostPort(u)
	reg.modules = append(reg.modules, registry.Module{Spec: boothv1alpha1.BoothModuleSpec{
		ID:                  id,
		ServiceRef:          boothv1alpha1.ServiceReference{Name: name, Port: port},
		ProvidesCredentials: &boothv1alpha1.CredentialProviderSpec{Kinds: kinds},
	}})
}

func splitHostPort(hostport string) (string, int32) {
	i := strings.LastIndex(hostport, ":")
	host := hostport[:i]
	var port int
	for _, c := range hostport[i+1:] {
		port = port*10 + int(c-'0')
	}
	return host, int32(port)
}

func newFixture(t *testing.T, provider *realProvider, kinds ...string) (*Service, *MemoryStore, *stubModules) {
	t.Helper()
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	mods := &stubModules{}
	if provider != nil {
		registerProvider(mods, "storage", provider.URL, kinds...)
	}
	audit := NewMemoryStore()
	svc := NewService(keys, mods, audit, Options{})
	return svc, audit, mods
}

func req(kind, access string) Request {
	return Request{Kind: kind, Access: access, Scope: json.RawMessage(`{"backendId":"lake","path":"t1"}`)}
}

// --- Real routing, real HTTP, real provider (not a mock). ---------------------------------------

func TestIssue_RoutesToTheRegisteredProviderOverRealHTTP(t *testing.T) {
	provider := newRealProvider(t)
	svc, audit, _ := newFixture(t, provider, "s3")

	resp, err := svc.Issue(context.Background(), "u-alice", "acme", auth.RoleEditor, req("s3", AccessReadWrite))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if resp.LeaseID != "lease-abc" {
		t.Errorf("LeaseID = %q", resp.LeaseID)
	}
	if len(provider.received) != 1 {
		t.Fatalf("provider received %d requests, want 1", len(provider.received))
	}
	got := provider.received[0]
	if got.path != ProviderPath {
		t.Errorf("path = %q, want %q", got.path, ProviderPath)
	}

	entries := audit.Entries()
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	if entries[0].LeaseID != "lease-abc" || entries[0].RequesterSubject != "u-alice" || entries[0].Kind != "s3" {
		t.Errorf("audit entry = %+v", entries[0])
	}
}

// The provider must be able to authenticate the call as genuinely from core, and the requester's
// already-authorized identity must reach it — it trusts this outright (ADR 0080), never re-deriving it.
func TestIssue_AuthenticatesToTheProviderAndForwardsRequesterIdentity(t *testing.T) {
	provider := newRealProvider(t)
	svc, _, _ := newFixture(t, provider, "s3")
	keys := svc.keys

	if _, err := svc.Issue(context.Background(), "u-alice", "acme", auth.RoleOwner, req("s3", AccessRead)); err != nil {
		t.Fatal(err)
	}
	got := provider.received[0]
	want := "Bearer " + keys.ProviderCredential("storage")
	if got.authHeader != want {
		t.Errorf("Authorization = %q, want %q", got.authHeader, want)
	}
	if got.body.Requester.Subject != "u-alice" || got.body.Requester.Workspace != "acme" || got.body.Requester.Role != "owner" {
		t.Errorf("requester = %+v", got.body.Requester)
	}
	if got.body.Access != AccessRead {
		t.Errorf("access = %q", got.body.Access)
	}
}

// --- Regression: authorization by role. ---------------------------------------------------------

func TestIssue_ReadWriteRequiresEditorOrOwner(t *testing.T) {
	provider := newRealProvider(t)
	svc, _, _ := newFixture(t, provider, "s3")

	for _, tc := range []struct {
		role  auth.Role
		allow bool
	}{
		{auth.RoleViewer, false}, {auth.RoleEditor, true}, {auth.RoleOwner, true},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			provider.received = nil
			_, err := svc.Issue(context.Background(), "u-x", "acme", tc.role, req("s3", AccessReadWrite))
			if tc.allow && err != nil {
				t.Fatalf("role %s should be allowed readwrite: %v", tc.role, err)
			}
			if !tc.allow {
				if !errors.Is(err, ErrForbidden) {
					t.Fatalf("role %s: err = %v, want ErrForbidden", tc.role, err)
				}
				if len(provider.received) != 0 {
					t.Errorf("role %s: a forbidden request still reached the provider", tc.role)
				}
			}
		})
	}
}

func TestIssue_ReadIsAllowedForAnyRealRole(t *testing.T) {
	provider := newRealProvider(t)
	svc, _, _ := newFixture(t, provider, "s3")

	for _, role := range []auth.Role{auth.RoleViewer, auth.RoleEditor, auth.RoleOwner} {
		if _, err := svc.Issue(context.Background(), "u-x", "acme", role, req("s3", AccessRead)); err != nil {
			t.Errorf("role %s: read should be allowed: %v", role, err)
		}
	}
}

func TestIssue_UnknownRoleIsForbidden(t *testing.T) {
	provider := newRealProvider(t)
	svc, _, _ := newFixture(t, provider, "s3")
	if _, err := svc.Issue(context.Background(), "u-x", "acme", auth.Role("nonsense"), req("s3", AccessRead)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

// --- Regression: routing / discovery. ------------------------------------------------------------

func TestIssue_NoProviderForKind(t *testing.T) {
	svc, _, _ := newFixture(t, nil)
	if _, err := svc.Issue(context.Background(), "u-x", "acme", auth.RoleOwner, req("postgres", AccessRead)); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("err = %v, want ErrNoProvider", err)
	}
}

func TestIssue_AmbiguousProviderRefusesToGuess(t *testing.T) {
	p1, p2 := newRealProvider(t), newRealProvider(t)
	keys, _ := NewKeys()
	mods := &stubModules{}
	registerProvider(mods, "storage-a", p1.URL, "s3")
	registerProvider(mods, "storage-b", p2.URL, "s3")
	svc := NewService(keys, mods, NewMemoryStore(), Options{})

	if _, err := svc.Issue(context.Background(), "u-x", "acme", auth.RoleOwner, req("s3", AccessRead)); !errors.Is(err, ErrAmbiguousProvider) {
		t.Fatalf("err = %v, want ErrAmbiguousProvider", err)
	}
	if len(p1.received) != 0 || len(p2.received) != 0 {
		t.Error("an ambiguous kind still reached a provider")
	}
}

// --- Regression: request validation. --------------------------------------------------------------

func TestIssue_ValidatesTheRequest(t *testing.T) {
	provider := newRealProvider(t)
	svc, _, _ := newFixture(t, provider, "s3")

	for name, r := range map[string]Request{
		"empty kind":          {Kind: "", Access: AccessRead, Scope: json.RawMessage(`{}`)},
		"uppercase kind":      {Kind: "S3", Access: AccessRead, Scope: json.RawMessage(`{}`)},
		"unknown access":      {Kind: "s3", Access: "delete", Scope: json.RawMessage(`{}`)},
		"empty access":        {Kind: "s3", Access: "", Scope: json.RawMessage(`{}`)},
		"missing scope":       {Kind: "s3", Access: AccessRead},
		"null scope":          {Kind: "s3", Access: AccessRead, Scope: json.RawMessage(`null`)},
		"negative ttlSeconds": {Kind: "s3", Access: AccessRead, Scope: json.RawMessage(`{}`), TTLSeconds: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Issue(context.Background(), "u-x", "acme", auth.RoleOwner, r); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

// --- Regression: TTL is clamped to the ceiling, never widened by the provider's own floor. --------

func TestIssue_TTLIsClampedToTheCeiling(t *testing.T) {
	provider := newRealProvider(t)
	svc, _, _ := newFixture(t, provider, "s3")
	svc.opts.MaxTTL = 2 * time.Minute

	r := req("s3", AccessRead)
	r.TTLSeconds = 3600
	if _, err := svc.Issue(context.Background(), "u-x", "acme", auth.RoleOwner, r); err != nil {
		t.Fatal(err)
	}
	if got := provider.received[0].body.TTLSeconds; got != 120 {
		t.Errorf("ttlSeconds sent to provider = %d, want 120 (clamped)", got)
	}
}

func TestIssue_ZeroOrOmittedTTLDefaultsToTheCeiling(t *testing.T) {
	provider := newRealProvider(t)
	svc, _, _ := newFixture(t, provider, "s3")
	svc.opts.MaxTTL = 90 * time.Second

	r := req("s3", AccessRead) // TTLSeconds left at zero
	if _, err := svc.Issue(context.Background(), "u-x", "acme", auth.RoleOwner, r); err != nil {
		t.Fatal(err)
	}
	if got := provider.received[0].body.TTLSeconds; got != 90 {
		t.Errorf("ttlSeconds sent to provider = %d, want 90", got)
	}
}

func TestDefaultMaxTTL_IsStricterThanAWorkloadToken(t *testing.T) {
	// ADR 0080: "a stricter default ceiling than an HTTP grant() token's." Pinned here so a
	// change to either constant is caught rather than silently drifting past the requirement.
	if DefaultMaxTTL >= 10*time.Minute {
		t.Errorf("DefaultMaxTTL = %s, want stricter (shorter) than workload's 10-minute token", DefaultMaxTTL)
	}
}

// --- Regression: a provider's considered refusal is relayed verbatim, not translated. -------------

func TestIssue_ProviderRefusalIsRelayedVerbatim(t *testing.T) {
	provider := newRealProvider(t)
	provider.respond = func(providerRequest) (int, any) {
		return http.StatusUnprocessableEntity, map[string]string{"error": "scope_not_supported"}
	}
	svc, audit, _ := newFixture(t, provider, "s3")

	_, err := svc.Issue(context.Background(), "u-x", "acme", auth.RoleOwner, req("s3", AccessRead))
	var provErr *ProviderError
	if !errors.As(err, &provErr) {
		t.Fatalf("err = %v, want *ProviderError", err)
	}
	if provErr.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("StatusCode = %d", provErr.StatusCode)
	}
	var body map[string]string
	if err := json.Unmarshal(provErr.Body, &body); err != nil || body["error"] != "scope_not_supported" {
		t.Errorf("Body = %s", provErr.Body)
	}
	if len(audit.Entries()) != 0 {
		t.Error("a refusal was recorded as an issuance")
	}
}

// --- Regression: a broken/unreachable/malformed provider maps to ErrProviderUnavailable, and
// --- nothing is ever recorded as issued when it wasn't. --------------------------------------------

func TestIssue_UnreachableProviderIsHandled(t *testing.T) {
	keys, _ := NewKeys()
	mods := &stubModules{}
	registerProvider(mods, "storage", "http://127.0.0.1:1", "s3") // nothing listens here
	audit := NewMemoryStore()
	svc := NewService(keys, mods, audit, Options{})

	_, err := svc.Issue(context.Background(), "u-x", "acme", auth.RoleOwner, req("s3", AccessRead))
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	if len(audit.Entries()) != 0 {
		t.Error("recorded an issuance for a call that never reached a provider")
	}
}

func TestIssue_MalformedProviderResponseIsHandled(t *testing.T) {
	provider := newRealProvider(t)
	provider.respond = func(providerRequest) (int, any) { return http.StatusCreated, "not-a-valid-response-object" }
	svc, audit, _ := newFixture(t, provider, "s3")

	_, err := svc.Issue(context.Background(), "u-x", "acme", auth.RoleOwner, req("s3", AccessRead))
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	if len(audit.Entries()) != 0 {
		t.Error("recorded an issuance for a malformed response")
	}
}

func TestIssue_IncompleteProviderResponseIsHandled(t *testing.T) {
	provider := newRealProvider(t)
	provider.respond = func(providerRequest) (int, any) {
		return http.StatusCreated, Response{Kind: "s3"} // no leaseId/expiresAt/credential
	}
	svc, audit, _ := newFixture(t, provider, "s3")

	_, err := svc.Issue(context.Background(), "u-x", "acme", auth.RoleOwner, req("s3", AccessRead))
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	if len(audit.Entries()) != 0 {
		t.Error("recorded an issuance for an incomplete response")
	}
}

// --- The one hard requirement above all others: the credential value is never in the audit trail. --

func TestIssue_AuditEntryNeverContainsTheCredential(t *testing.T) {
	provider := newRealProvider(t)
	svc, audit, _ := newFixture(t, provider, "s3")

	resp, err := svc.Issue(context.Background(), "u-x", "acme", auth.RoleOwner, req("s3", AccessRead))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Credential), "top-secret-value") {
		t.Fatal("test setup: the response should carry the secret so this test can prove it isn't audited")
	}

	entries := audit.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	// Entry has no Credential field at all — reflect over it to make this assertion resilient to
	// field reordering, and to fail loudly (a compile error, in effect) if one is ever added.
	v := reflect.ValueOf(entries[0])
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		val := v.Field(i)
		s, ok := val.Interface().(string)
		if !ok {
			continue
		}
		if strings.Contains(s, "top-secret-value") {
			t.Errorf("audit entry field %s contains the credential value: %q", f.Name, s)
		}
	}
	if strings.Contains(string(entries[0].Scope), "top-secret-value") {
		t.Error("audit entry's Scope contains the credential value")
	}
}

func TestEntry_HasNoCredentialField(t *testing.T) {
	// A structural guard, not just a behavioural one: Entry must never even have a field capable
	// of holding a credential, so a future change can't accidentally start recording one.
	typ := reflect.TypeOf(Entry{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(name, "credential") || strings.Contains(name, "secret") {
			t.Errorf("Entry has a field named %q, which could hold a credential value", typ.Field(i).Name)
		}
	}
}
