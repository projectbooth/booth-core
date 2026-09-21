package workload

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/directory"
	"github.com/projectbooth/booth-core/internal/registry"
)

const issuer = "http://booth-core.booth-system.svc.cluster.local:8080"

func newFake(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := boothv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
}

func moduleSpec(id string, wi *boothv1alpha1.WorkloadIdentityRequirement) boothv1alpha1.BoothModuleSpec {
	return boothv1alpha1.BoothModuleSpec{
		ID: id, DisplayName: id, Version: "0.1.0", ContractVersion: "0.1.0", HealthCheckPath: "/h",
		ServiceRef:       boothv1alpha1.ServiceReference{Name: id, Port: 80},
		WorkloadIdentity: wi,
	}
}

var mints = &boothv1alpha1.WorkloadIdentityRequirement{Mint: true}

// agingStore makes every directory entry look `age` older than it was recorded, so a test can
// say "the owner was last seen N days ago" without a clock inside the store.
type agingStore struct {
	directory.Store
	age time.Duration
}

func (a *agingStore) Get(ctx context.Context, sub, workspace string) (directory.User, bool, error) {
	u, ok, err := a.Store.Get(ctx, sub, workspace)
	u.LastSeenAt = u.LastSeenAt.Add(-a.age)
	return u, ok, err
}

// fixture is a Service over a real registry and a real (memory) directory, with a movable clock.
type fixture struct {
	svc   *Service
	keys  *Keys
	reg   *registry.Registry
	users *directory.MemoryStore
	aged  *agingStore
	now   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{keys: keys, reg: registry.New(), users: directory.NewMemoryStore(), now: time.Now()}
	f.aged = &agingStore{Store: f.users}
	f.svc = NewService(keys, f.reg, f.aged, Options{
		Issuer: issuer, Audience: "booth-ui", GroupsClaim: "groups",
		Now: func() time.Time { return f.now },
	})
	// "pipeline" declares workloadIdentity; "notes" never did.
	f.reg.Put(registry.Module{Namespace: "ns", Spec: moduleSpec("pipeline", mints)})
	f.reg.Put(registry.Module{Namespace: "ns", Spec: moduleSpec("notes", nil)})
	return f
}

// seen records that owner was last seen with role in workspace, as the recorder would.
func (f *fixture) seen(t *testing.T, owner, workspace string, role auth.Role) {
	t.Helper()
	err := f.users.Upsert(context.Background(), directory.User{
		Sub: owner, Workspaces: []string{workspace}, Roles: map[string]string{workspace: string(role)},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) mint(module string, req Request) (Token, error) {
	return f.svc.Mint(context.Background(), module, req)
}

func req(owner, ceiling string) Request {
	return Request{Workspace: "acme", Subject: "job:42", RoleCeiling: ceiling, Owner: owner}
}

// claimsOf verifies tok's signature against the published JWKS — exactly what a module does —
// and returns its claims.
func (f *fixture) claimsOf(t *testing.T, tok Token) map[string]any {
	t.Helper()
	parsed, err := jwt.ParseSigned(tok.JWT, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	std := jwt.Claims{}
	keys := f.svc.JWKS().Keys
	if len(keys) != 1 {
		t.Fatalf("JWKS has %d keys", len(keys))
	}
	if err := parsed.Claims(keys[0].Key, &out, &std); err != nil {
		t.Fatalf("token does not verify against core's own JWKS: %v", err)
	}
	if err := std.ValidateWithLeeway(jwt.Expected{Issuer: issuer, Time: f.now}, time.Second); err != nil {
		t.Fatalf("standard claims invalid: %v", err)
	}
	return out
}

// --- Regression: a module that never declared workloadIdentity cannot mint. -------------------

func TestMint_ModuleThatNeverDeclaredWorkloadIdentityIsRejected(t *testing.T) {
	f := newFixture(t)
	f.seen(t, "u-alice", "acme", auth.RoleOwner)

	for name, module := range map[string]string{
		"registered without the field": "notes",
		"not registered at all":        "ghost",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.mint(module, req("u-alice", "editor")); !errors.Is(err, ErrNotEntitled) {
				t.Fatalf("Mint = %v, want ErrNotEntitled", err)
			}
			// Even holding a *validly signed* credential for that module (say a copy from
			// before it dropped the field) authenticates to nothing.
			if _, err := f.svc.AuthenticateModule(f.keys.Credential(module)); !errors.Is(err, ErrNotEntitled) {
				t.Fatalf("AuthenticateModule = %v, want ErrNotEntitled", err)
			}
		})
	}

	t.Run("mint: false is the same as omitting the field", func(t *testing.T) {
		f.reg.Put(registry.Module{Spec: moduleSpec("off", &boothv1alpha1.WorkloadIdentityRequirement{Mint: false})})
		if _, err := f.mint("off", req("u-alice", "editor")); !errors.Is(err, ErrNotEntitled) {
			t.Fatalf("Mint = %v, want ErrNotEntitled", err)
		}
	})

	t.Run("dropping the field revokes a module that used to mint", func(t *testing.T) {
		cred := f.keys.Credential("pipeline")
		if _, err := f.svc.AuthenticateModule(cred); err != nil {
			t.Fatalf("declared module rejected: %v", err)
		}
		f.reg.Put(registry.Module{Namespace: "ns", Spec: moduleSpec("pipeline", nil)})
		if _, err := f.svc.AuthenticateModule(cred); !errors.Is(err, ErrNotEntitled) {
			t.Fatalf("after dropping the field: %v, want ErrNotEntitled", err)
		}
		if _, err := f.mint("pipeline", req("u-alice", "editor")); !errors.Is(err, ErrNotEntitled) {
			t.Fatalf("Mint after dropping the field = %v, want ErrNotEntitled", err)
		}
	})
}

// --- Regression: the role never exceeds what the owner currently holds. ----------------------

func TestMint_RoleIsTheLesserOfCeilingAndOwnersCurrentRole(t *testing.T) {
	cases := []struct {
		owner, ceiling, want auth.Role
	}{
		{auth.RoleViewer, auth.RoleOwner, auth.RoleViewer}, // asked for more than the owner has
		{auth.RoleViewer, auth.RoleEditor, auth.RoleViewer},
		{auth.RoleEditor, auth.RoleOwner, auth.RoleEditor},
		{auth.RoleOwner, auth.RoleViewer, auth.RoleViewer}, // the ceiling still binds an owner
		{auth.RoleOwner, auth.RoleEditor, auth.RoleEditor},
		{auth.RoleOwner, auth.RoleOwner, auth.RoleOwner},
		{auth.RoleEditor, auth.RoleEditor, auth.RoleEditor},
	}
	for _, c := range cases {
		t.Run("owner "+string(c.owner)+" ceiling "+string(c.ceiling), func(t *testing.T) {
			f := newFixture(t)
			f.seen(t, "u-alice", "acme", c.owner)
			tok, err := f.mint("pipeline", req("u-alice", string(c.ceiling)))
			if err != nil {
				t.Fatal(err)
			}
			if tok.Role != c.want {
				t.Errorf("Role = %q, want %q", tok.Role, c.want)
			}
			// The token itself, not just the return value, carries the capped role.
			groups := f.claimsOf(t, tok)["groups"]
			if want := []any{"/workspaces/acme/" + string(c.want)}; !reflect.DeepEqual(groups, want) {
				t.Errorf("groups = %v, want %v", groups, want)
			}
		})
	}
}

// --- Regression: the role is re-derived at every mint, never cached. --------------------------

func TestMint_RoleIsReDerivedLiveAtEveryMint(t *testing.T) {
	f := newFixture(t)
	f.seen(t, "u-alice", "acme", auth.RoleOwner)

	tok, err := f.mint("pipeline", req("u-alice", "owner"))
	if err != nil || tok.Role != auth.RoleOwner {
		t.Fatalf("first mint = %+v, %v; want owner", tok, err)
	}

	// Alice is demoted. The very next mint — same module, same run, same request — comes out lower.
	f.seen(t, "u-alice", "acme", auth.RoleViewer)
	tok, err = f.mint("pipeline", req("u-alice", "owner"))
	if err != nil || tok.Role != auth.RoleViewer {
		t.Fatalf("mint after demotion = %+v, %v; want viewer", tok, err)
	}

	// Re-promoted: it follows back up, so nothing was latched down either.
	f.seen(t, "u-alice", "acme", auth.RoleEditor)
	if tok, err = f.mint("pipeline", req("u-alice", "owner")); err != nil || tok.Role != auth.RoleEditor {
		t.Fatalf("mint after re-promotion = %+v, %v; want editor", tok, err)
	}

	// Removed from the workspace (the next token they present lists no membership in it): refused.
	if err := f.users.Upsert(context.Background(), directory.User{Sub: "u-alice", Workspaces: nil, Roles: nil}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.mint("pipeline", req("u-alice", "viewer")); !errors.Is(err, ErrOwnerNoAccess) {
		t.Fatalf("mint after removal = %v, want ErrOwnerNoAccess", err)
	}
}

func TestMint_RefusesWhenOwnerHasNoUsableRole(t *testing.T) {
	f := newFixture(t)
	f.seen(t, "u-alice", "acme", auth.RoleOwner)

	t.Run("owner core has never seen", func(t *testing.T) {
		if _, err := f.mint("pipeline", req("u-nobody", "viewer")); !errors.Is(err, ErrOwnerNoAccess) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("owner belongs to a different workspace only", func(t *testing.T) {
		f.seen(t, "u-dave", "labs", auth.RoleOwner)
		if _, err := f.mint("pipeline", req("u-dave", "viewer")); !errors.Is(err, ErrOwnerNoAccess) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a recorded role that isn't a real role grants nothing", func(t *testing.T) {
		f.seen(t, "u-eve", "acme", auth.Role("superadmin"))
		if _, err := f.mint("pipeline", req("u-eve", "owner")); !errors.Is(err, ErrOwnerNoAccess) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a user recorded before roles existed has no role, not a default one", func(t *testing.T) {
		if err := f.users.Upsert(context.Background(), directory.User{Sub: "u-old", Workspaces: []string{"acme"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.mint("pipeline", req("u-old", "viewer")); !errors.Is(err, ErrOwnerNoAccess) {
			t.Fatalf("got %v", err)
		}
	})
}

// A user removed from the IdP who never signs in again is never re-recorded, so their last role
// would otherwise stand forever. The recency bound is what ends it.
func TestMint_OwnerNotSeenWithinMaxAgeLosesTheirRole(t *testing.T) {
	f := newFixture(t)
	f.seen(t, "u-alice", "acme", auth.RoleOwner)

	f.aged.age = DefaultMaxOwnerAge - time.Hour
	if _, err := f.mint("pipeline", req("u-alice", "editor")); err != nil {
		t.Fatalf("just inside the bound: %v", err)
	}
	f.aged.age = DefaultMaxOwnerAge + time.Hour
	if _, err := f.mint("pipeline", req("u-alice", "editor")); !errors.Is(err, ErrOwnerNoAccess) {
		t.Fatalf("just outside the bound: %v, want ErrOwnerNoAccess", err)
	}
	// Signing in again (the recorder re-stamps the entry) restores it.
	f.aged.age = 0
	f.seen(t, "u-alice", "acme", auth.RoleOwner)
	if _, err := f.mint("pipeline", req("u-alice", "editor")); err != nil {
		t.Fatalf("after the owner is seen again: %v", err)
	}
}

// --- Token shape. -----------------------------------------------------------------------------

// The groups claim is the whole reason no other module needs new authorization code: it must
// parse, through the very same function core uses on a human's token, into the same Membership.
func TestMint_GroupsClaimIsShapedLikeAHumanTokens(t *testing.T) {
	f := newFixture(t)
	f.seen(t, "u-alice", "acme", auth.RoleEditor)
	tok, err := f.mint("pipeline", req("u-alice", "owner"))
	if err != nil {
		t.Fatal(err)
	}
	claims := f.claimsOf(t, tok)

	raw, _ := claims["groups"].([]any)
	var groups []string
	for _, g := range raw {
		groups = append(groups, g.(string))
	}
	got := auth.DeriveMemberships(groups)
	if want := []auth.Membership{{Workspace: "acme", Role: auth.RoleEditor}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DeriveMemberships(%v) = %v, want %v", groups, got, want)
	}
	if len(got) != 1 {
		t.Errorf("token grants %d memberships, want exactly one (it is for one workspace)", len(got))
	}

	if claims["iss"] != issuer {
		t.Errorf("iss = %v, want core's own issuer %s", claims["iss"], issuer)
	}
	if claims["sub"] != "job:42" {
		t.Errorf("sub = %v, want the run's identifier", claims["sub"])
	}
	if claims["aud"] != "booth-ui" {
		t.Errorf("aud = %v, want what human tokens carry (the client id)", claims["aud"])
	}
	if claims[ModuleClaim] != "pipeline" {
		t.Errorf("%s = %v, want the requesting module", ModuleClaim, claims[ModuleClaim])
	}
	if _, has := claims["email"]; has {
		t.Error("a run's token must not carry identity claims of a person")
	}
	if got := tok.ExpiresAt.Sub(f.now); got != 10*time.Minute {
		t.Errorf("lifetime = %v, want 10m", got)
	}
}

func TestMint_HonoursTheConfiguredGroupsClaimName(t *testing.T) {
	f := newFixture(t)
	f.svc = NewService(f.keys, f.reg, f.users, Options{Issuer: issuer, GroupsClaim: "roles", Now: func() time.Time { return f.now }})
	f.seen(t, "u-alice", "acme", auth.RoleViewer)
	tok, err := f.mint("pipeline", req("u-alice", "viewer"))
	if err != nil {
		t.Fatal(err)
	}
	claims := f.claimsOf(t, tok)
	if _, ok := claims["roles"]; !ok {
		t.Errorf("claims = %v, want the configured %q claim", claims, "roles")
	}
	if _, ok := claims["groups"]; ok {
		t.Error("emitted a `groups` claim although the deployment's claim is named differently")
	}
	if _, has := claims["aud"]; has {
		t.Error("aud set although no audience is configured")
	}
}

func TestMint_ValidatesTheRequest(t *testing.T) {
	f := newFixture(t)
	f.seen(t, "u-alice", "acme", auth.RoleOwner)
	good := req("u-alice", "editor")

	mutate := func(fn func(*Request)) Request { r := good; fn(&r); return r }
	for name, r := range map[string]Request{
		"empty workspace":                mutate(func(r *Request) { r.Workspace = "" }),
		"uppercase workspace":            mutate(func(r *Request) { r.Workspace = "Acme" }),
		"workspace with a path in it":    mutate(func(r *Request) { r.Workspace = "acme/owner" }),
		"empty subject":                  mutate(func(r *Request) { r.Subject = "" }),
		"subject that is a person's sub": mutate(func(r *Request) { r.Subject = "3f2b8c1e-aaaa-bbbb-cccc-0123456789ab" }),
		"subject that is an email":       mutate(func(r *Request) { r.Subject = "alice@example.com" }),
		"subject that is the owner":      mutate(func(r *Request) { r.Subject = "u-alice" }),
		"unknown ceiling":                mutate(func(r *Request) { r.RoleCeiling = "admin" }),
		"empty ceiling":                  mutate(func(r *Request) { r.RoleCeiling = "" }),
		"no owner":                       mutate(func(r *Request) { r.Owner = "" }),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.mint("pipeline", r); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Mint = %v, want ErrInvalidRequest", err)
			}
		})
	}
	if _, err := f.mint("pipeline", good); err != nil {
		t.Fatalf("the unmutated request should mint: %v", err)
	}
}

// --- Credentials. -----------------------------------------------------------------------------

func TestCredential_BindsToTheModule(t *testing.T) {
	k, _ := NewKeys()
	cred := k.Credential("pipeline")
	if id, ok := k.ModuleForCredential(cred); !ok || id != "pipeline" {
		t.Fatalf("round trip = %q, %v", id, ok)
	}

	parts := strings.Split(cred, ".")
	for name, bad := range map[string]string{
		"another module's name, same mac": parts[0] + ".notes." + parts[2],
		"truncated mac":                   cred[:len(cred)-4],
		"empty":                           "",
		"no dots":                         "pipeline",
		"empty module":                    parts[0] + ".." + parts[2],
		"extra segment":                   cred + ".x",
		"wrong prefix":                    "jwt." + parts[1] + "." + parts[2],
	} {
		if id, ok := k.ModuleForCredential(bad); ok {
			t.Errorf("%s: accepted as module %q", name, id)
		}
	}

	other, _ := NewKeys()
	if _, ok := other.ModuleForCredential(cred); ok {
		t.Error("a credential from another deployment's keys was accepted")
	}
}

// --- Key storage. -----------------------------------------------------------------------------

func TestLoadOrCreateKeys_IsStableAcrossCallsAndReplicas(t *testing.T) {
	c := newFake(t)
	ctx := context.Background()
	a, err := LoadOrCreateKeys(ctx, c, "booth-system")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateKeys(ctx, c, "booth-system")
	if err != nil {
		t.Fatal(err)
	}
	if a.KeyID() != b.KeyID() {
		t.Fatal("signing key changed between loads: every issued token and credential would break")
	}
	if b.Credential("pipeline") != a.Credential("pipeline") {
		t.Fatal("credentials differ between replicas loading the same Secret")
	}
	// What one replica signs, another verifies via the same published key.
	if !reflect.DeepEqual(a.JWKS().Keys[0].Key, b.JWKS().Keys[0].Key) {
		t.Fatal("JWKS differs between loads")
	}
}

func TestLoadOrCreateKeys_RejectsCorruptSecretRatherThanRegenerating(t *testing.T) {
	c := newFake(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "booth-system", Name: KeysSecretName},
		Data:       map[string][]byte{signingKeyKey: []byte("not pem"), credentialKeyKey: []byte("x")},
	})
	if _, err := LoadOrCreateKeys(context.Background(), c, "booth-system"); err == nil {
		t.Fatal("silently accepted (or replaced) an unusable key Secret")
	}
}

func TestPublishedKeysCarryNoPrivateMaterial(t *testing.T) {
	k, _ := NewKeys()
	jwks := k.JWKS()
	b, err := jwks.Keys[0].MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{`"d"`, `"p"`, `"q"`, `"dp"`, `"dq"`, `"qi"`} {
		if strings.Contains(string(b), private+":") {
			t.Errorf("JWKS contains private RSA parameter %s", private)
		}
	}
	if !jwks.Keys[0].IsPublic() {
		t.Error("published key is not a public key")
	}
}

// --- Provisioning. ----------------------------------------------------------------------------

func boothModule(id, ns, svcNS string, wi *boothv1alpha1.WorkloadIdentityRequirement) *boothv1alpha1.BoothModule {
	spec := moduleSpec(id, wi)
	spec.ServiceRef.Namespace = svcNS
	return &boothv1alpha1.BoothModule{
		ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: ns, UID: types.UID("uid-" + id)},
		Spec:       spec,
	}
}

func getSecret(t *testing.T, c client.Client, ns string) (*corev1.Secret, error) {
	t.Helper()
	var s corev1.Secret
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: CredentialsSecretName}, &s)
	return &s, err
}

func TestProvisioner_DeliversCredentialOnlyToModulesThatDeclareIt(t *testing.T) {
	c := newFake(t)
	keys, _ := NewKeys()
	p := NewModuleProvisioner(c, keys, issuer+"/")
	ctx := context.Background()

	if err := p.Ensure(ctx, boothModule("pipeline", "booth-pipeline", "", mints)); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, boothModule("storage", "booth-storage", "", nil)); err != nil {
		t.Fatal(err)
	}

	s, err := getSecret(t, c, "booth-pipeline")
	if err != nil {
		t.Fatalf("entitled module got no Secret: %v", err)
	}
	if s.Name != "booth-workload-minting-credentials" {
		t.Errorf("Secret name = %q", s.Name)
	}
	if id, ok := keys.ModuleForCredential(stringOf(s, KeyCredential)); !ok || id != "pipeline" {
		t.Errorf("delivered credential authenticates as %q, %v; want pipeline", id, ok)
	}
	if got := stringOf(s, KeyURL); got != issuer+"/api/internal/workload-tokens" {
		t.Errorf("url = %q", got)
	}
	if got := stringOf(s, KeyIssuer); got != issuer {
		t.Errorf("issuer = %q (trailing slash must be normalised away)", got)
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Kind != "BoothModule" {
		t.Errorf("Secret beside its BoothModule should be owned by it: %+v", s.OwnerReferences)
	}

	// A module that never declared the field gets nothing at all.
	if _, err := getSecret(t, c, "booth-storage"); !apierrors.IsNotFound(err) {
		t.Fatalf("module without workloadIdentity received a minting Secret (err=%v)", err)
	}
}

func stringOf(s *corev1.Secret, k string) string {
	if v, ok := s.Data[k]; ok {
		return string(v)
	}
	return s.StringData[k]
}

func TestProvisioner_IsIdempotentAndSelfHealing(t *testing.T) {
	c := newFake(t)
	keys, _ := NewKeys()
	p := NewModuleProvisioner(c, keys, issuer)
	ctx := context.Background()
	mod := boothModule("pipeline", "booth-pipeline", "", mints)

	if err := p.Ensure(ctx, mod); err != nil {
		t.Fatal(err)
	}
	before, _ := getSecret(t, c, "booth-pipeline")
	if err := p.Ensure(ctx, mod); err != nil {
		t.Fatal(err)
	}
	after, _ := getSecret(t, c, "booth-pipeline")
	if before.ResourceVersion != after.ResourceVersion {
		t.Error("reconciling an up-to-date Secret rewrote it (this runs every few seconds per module)")
	}

	// Someone deletes it, or the keys were rotated: the next reconcile restores a working one.
	if err := c.Delete(ctx, after); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, mod); err != nil {
		t.Fatal(err)
	}
	if _, err := getSecret(t, c, "booth-pipeline"); err != nil {
		t.Fatalf("Secret not restored: %v", err)
	}
	newKeysForRotation, _ := NewKeys()
	p.Keys = newKeysForRotation
	if err := p.Ensure(ctx, mod); err != nil {
		t.Fatal(err)
	}
	s, _ := getSecret(t, c, "booth-pipeline")
	if id, ok := newKeysForRotation.ModuleForCredential(stringOf(s, KeyCredential)); !ok || id != "pipeline" {
		t.Error("credential was not re-issued after the keys changed")
	}
}

func TestProvisioner_RemovesItsSecretWhenTheFieldIsDroppedButNotOthers(t *testing.T) {
	c := newFake(t)
	keys, _ := NewKeys()
	p := NewModuleProvisioner(c, keys, issuer)
	ctx := context.Background()

	if err := p.Ensure(ctx, boothModule("pipeline", "booth-pipeline", "", mints)); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, boothModule("pipeline", "booth-pipeline", "", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := getSecret(t, c, "booth-pipeline"); !apierrors.IsNotFound(err) {
		t.Fatalf("Secret survived the module dropping workloadIdentity (err=%v)", err)
	}

	// A same-named Secret core didn't write (no annotation) is not core's to delete.
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "booth-other", Name: CredentialsSecretName},
		Data:       map[string][]byte{"mine": []byte("x")},
	}
	if err := c.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, boothModule("other", "booth-other", "", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := getSecret(t, c, "booth-other"); err != nil {
		t.Fatalf("deleted a Secret it did not create: %v", err)
	}
}

func TestProvisioner_UsesServiceNamespaceWithTheDocumentedDefault(t *testing.T) {
	c := newFake(t)
	keys, _ := NewKeys()
	p := NewModuleProvisioner(c, keys, issuer)
	ctx := context.Background()

	// serviceRef.namespace unset: the BoothModule's own namespace.
	if err := p.Ensure(ctx, boothModule("a", "ns-of-a", "", mints)); err != nil {
		t.Fatal(err)
	}
	if _, err := getSecret(t, c, "ns-of-a"); err != nil {
		t.Errorf("default namespace not applied: %v", err)
	}
	// Explicit and different from the resource's: delivered there, and (as owner references
	// can't cross namespaces) unowned.
	if err := p.Ensure(ctx, boothModule("b", "ns-of-b", "svc-ns-b", mints)); err != nil {
		t.Fatal(err)
	}
	s, err := getSecret(t, c, "svc-ns-b")
	if err != nil {
		t.Fatalf("explicit namespace not honoured: %v", err)
	}
	if len(s.OwnerReferences) != 0 {
		t.Errorf("cross-namespace Secret has an owner reference: %+v", s.OwnerReferences)
	}
}

// --- Verify (the gateway's second issuer, ADR 0059). ------------------------------------------

func TestVerify_AcceptsWhatMintProducesAndYieldsAHumanShapedClaims(t *testing.T) {
	f := newFixture(t)
	f.seen(t, "u-alice", "acme", auth.RoleEditor)
	tok, err := f.mint("pipeline", req("u-alice", "owner"))
	if err != nil {
		t.Fatal(err)
	}
	claims, err := f.svc.Verify(context.Background(), tok.JWT)
	if err != nil {
		t.Fatalf("Verify rejected a freshly minted token: %v", err)
	}
	if claims.Subject != "job:42" {
		t.Errorf("Subject = %q", claims.Subject)
	}
	if got, want := auth.DeriveMemberships(claims.Groups), []auth.Membership{{Workspace: "acme", Role: auth.RoleEditor}}; !reflect.DeepEqual(got, want) {
		t.Errorf("memberships = %v, want %v", got, want)
	}
	if claims.Email != "" || claims.Name != "" || claims.PreferredUsername != "" {
		t.Errorf("a run carries person claims: %+v", claims)
	}
}

// forge signs claims with an arbitrary RSA key, the way any other issuer would.
func forge(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVerify_RejectsEverythingCoreDidNotMintAsAValidRun(t *testing.T) {
	f := newFixture(t)
	f.seen(t, "u-alice", "acme", auth.RoleOwner)
	minted, _ := f.mint("pipeline", req("u-alice", "viewer"))

	valid := func() map[string]any {
		return map[string]any{
			"iss": issuer, "sub": "job:1", "aud": "booth-ui", ModuleClaim: "pipeline",
			"exp": f.now.Add(time.Minute).Unix(), "iat": f.now.Unix(),
			"groups": []string{"/workspaces/acme/owner"},
		}
	}
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	// The reverse-direction confusion: a token core's OWN key signed, but which claims another
	// issuer (the IdP) — the workload verifier must refuse it on the issuer alone.
	relabelled := f.keys.signRaw(t, mut(valid(), "iss", "https://idp.example/realms/booth"))
	// A token from the IdP (any other key) that claims to be core's.
	idpClaimingCore := forge(t, otherKey, valid())

	hs256, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte("0123456789abcdef0123456789abcdef")}, nil)
	hsTok, _ := jwt.Signed(hs256).Claims(valid()).Serialize()

	parts := strings.Split(minted.JWT, ".")
	tampered := parts[0] + "." + b64(`{"iss":"`+issuer+`","sub":"job:42","aud":"booth-ui","booth_module":"pipeline","exp":`+itoa(f.now.Add(time.Hour).Unix())+`,"groups":["/workspaces/acme/owner"]}`) + "." + parts[2]

	for name, tok := range map[string]string{
		"signed by another key, claiming core's issuer":    idpClaimingCore,
		"signed by core's key but claiming another issuer": relabelled,
		"core's key, wrong audience":                       f.keys.signRaw(t, mut(valid(), "aud", "someone-else")),
		"core's key, no audience":                          f.keys.signRaw(t, without(valid(), "aud")),
		"expired":                                          f.keys.signRaw(t, mut(valid(), "exp", f.now.Add(-time.Hour).Unix())),
		"not yet valid":                                    f.keys.signRaw(t, mut(valid(), "nbf", f.now.Add(time.Hour).Unix())),
		"no subject":                                       f.keys.signRaw(t, without(valid(), "sub")),
		"no requesting module (not something Mint made)":   f.keys.signRaw(t, without(valid(), ModuleClaim)),
		"HS256":                        hsTok,
		"payload edited after signing": tampered,
		"not a jwt":                    "nope",
		"empty":                        "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.svc.Verify(context.Background(), tok); err == nil {
				t.Fatal("token was accepted")
			}
		})
	}

	// The control: the same hand-built claims, signed by core's key with all the right values, pass —
	// so the failures above are each down to the one thing that was wrong.
	if _, err := f.svc.Verify(context.Background(), f.keys.signRaw(t, valid())); err != nil {
		t.Fatalf("control token rejected: %v", err)
	}
}

func TestVerify_ExpiryFollowsTheClock(t *testing.T) {
	f := newFixture(t)
	f.seen(t, "u-alice", "acme", auth.RoleOwner)
	tok, _ := f.mint("pipeline", req("u-alice", "viewer"))

	f.now = f.now.Add(DefaultTokenTTL - time.Minute)
	if _, err := f.svc.Verify(context.Background(), tok.JWT); err != nil {
		t.Fatalf("still-valid token rejected: %v", err)
	}
	f.now = f.now.Add(2 * time.Minute)
	if _, err := f.svc.Verify(context.Background(), tok.JWT); err == nil {
		t.Fatal("expired token accepted")
	}
}

// signRaw signs arbitrary claims with core's own key — the position of an attacker who somehow
// held it, or of a bug in Mint — so Verify is tested independent of what Mint happens to emit.
func (k *Keys) signRaw(t *testing.T, claims map[string]any) string {
	t.Helper()
	s, err := k.signer()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(s).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mut(m map[string]any, k string, v any) map[string]any { m[k] = v; return m }
func without(m map[string]any, k string) map[string]any    { delete(m, k); return m }
func b64(s string) string                                  { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func itoa(n int64) string                                  { return strconv.FormatInt(n, 10) }
