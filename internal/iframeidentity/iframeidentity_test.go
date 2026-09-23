package iframeidentity

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/projectbooth/booth-core/internal/auth"
)

const issuer = "http://booth-core.booth-system.svc.cluster.local:8080/iframe-identity"

func newFake(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
}

func newFixture(t *testing.T) *Service {
	t.Helper()
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	return NewService(keys, Options{Issuer: issuer, GroupsClaim: "groups"})
}

// claimsOf verifies raw against svc's own published JWKS — exactly what a module does — and
// returns both the standard and extra claims.
func claimsOf(t *testing.T, svc *Service, raw string) (jwt.Claims, map[string]any) {
	t.Helper()
	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{signingAlg})
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	keys := svc.JWKS().Keys
	if len(keys) != 1 {
		t.Fatalf("JWKS has %d keys", len(keys))
	}
	var std jwt.Claims
	var extra map[string]any
	if err := parsed.Claims(keys[0].Key, &std, &extra); err != nil {
		t.Fatalf("token does not verify against the issuer's own JWKS: %v", err)
	}
	return std, extra
}

func TestMint_ShapeMatchesADR0069(t *testing.T) {
	svc := newFixture(t)
	now := time.Now()
	raw, err := svc.Mint("superset", "acme", "editor", "u-alice")
	if err != nil {
		t.Fatal(err)
	}

	std, extra := claimsOf(t, svc, raw)
	if std.Issuer != issuer {
		t.Errorf("iss = %q, want %q", std.Issuer, issuer)
	}
	if len(std.Audience) != 1 || std.Audience[0] != "superset" {
		t.Errorf("aud = %v, want [superset] (the target module id)", std.Audience)
	}
	if std.Subject != "u-alice" {
		t.Errorf("sub = %q, want the person's own subject", std.Subject)
	}
	if std.ID == "" {
		t.Error("jti is empty")
	}
	if std.Expiry == nil || std.NotBefore == nil {
		t.Fatal("missing exp/nbf")
	}
	lifetime := std.Expiry.Time().Sub(std.NotBefore.Time())
	if lifetime <= 0 || lifetime > 2*time.Minute {
		t.Errorf("lifetime = %v, want (0, 2m] per ADR 0069's exp <= 2 minutes", lifetime)
	}
	if exp := std.Expiry.Time(); exp.Before(now) || exp.After(now.Add(2*time.Minute+time.Second)) {
		t.Errorf("exp = %v, want within 2m of mint time %v", exp, now)
	}

	// Groups claim: ADR 0025's grammar, exactly one entry, and a module's real role-derivation
	// code must read it identically to a human OIDC token's.
	raws, _ := extra["groups"].([]any)
	var groups []string
	for _, g := range raws {
		groups = append(groups, g.(string))
	}
	got := auth.DeriveMemberships(groups)
	want := []auth.Membership{{Workspace: "acme", Role: auth.RoleEditor}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DeriveMemberships(%v) = %v, want %v", groups, got, want)
	}
	if len(got) != 1 {
		t.Errorf("assertion grants %d memberships, want exactly one (it is for one workspace)", len(got))
	}
	if extra[ModuleClaim] != "superset" {
		t.Errorf("%s = %v, want superset", ModuleClaim, extra[ModuleClaim])
	}

	// Never carries anything that could be mistaken for a workload token's shape (ADR 0056), and
	// never carries other person-identifying claims beyond sub.
	if _, has := extra["roleCeiling"]; has {
		t.Error("assertion carries a workload-token-shaped claim")
	}
	if _, has := extra["email"]; has {
		t.Error("assertion carries an email claim")
	}
}

func TestMint_HonoursTheConfiguredGroupsClaimName(t *testing.T) {
	keys, _ := NewKeys()
	svc := NewService(keys, Options{Issuer: issuer, GroupsClaim: "roles"})
	raw, err := svc.Mint("notebooks", "acme", "viewer", "u-bob")
	if err != nil {
		t.Fatal(err)
	}
	_, extra := claimsOf(t, svc, raw)
	if _, ok := extra["roles"]; !ok {
		t.Errorf("claims = %v, want the configured %q claim", extra, "roles")
	}
	if _, ok := extra["groups"]; ok {
		t.Error("emitted a `groups` claim although the deployment's claim is named differently")
	}
}

func TestMint_DefaultGroupsClaimIsGroups(t *testing.T) {
	keys, _ := NewKeys()
	svc := NewService(keys, Options{Issuer: issuer}) // GroupsClaim left empty
	raw, err := svc.Mint("notebooks", "acme", "viewer", "u-bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, extra := claimsOf(t, svc, raw); extra["groups"] == nil {
		t.Errorf("claims = %v, want a default groups claim", extra)
	}
}

func TestMint_RejectsMalformedInputsRatherThanMintingSomethingMeaningless(t *testing.T) {
	svc := newFixture(t)
	for name, args := range map[string][4]string{
		"empty module":       {"", "acme", "editor", "u-alice"},
		"empty workspace":    {"superset", "", "editor", "u-alice"},
		"empty subject":      {"superset", "acme", "editor", ""},
		"empty role":         {"superset", "acme", "", "u-alice"},
		"role isn't a role":  {"superset", "acme", "admin", "u-alice"},
		"role case-mismatch": {"superset", "acme", "Editor", "u-alice"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Mint(args[0], args[1], args[2], args[3]); err == nil {
				t.Fatal("Mint accepted malformed input")
			}
		})
	}
}

func TestIssuerAndDiscoveryAreConsistent(t *testing.T) {
	svc := newFixture(t)
	if svc.Issuer() != issuer {
		t.Errorf("Issuer() = %q, want %q", svc.Issuer(), issuer)
	}
	disc := svc.Discovery()
	if disc["issuer"] != issuer {
		t.Errorf("discovery issuer = %v", disc["issuer"])
	}
	if disc["jwks_uri"] != issuer+JWKSPath {
		t.Errorf("jwks_uri = %v, want %s", disc["jwks_uri"], issuer+JWKSPath)
	}
}

// A trailing slash on the configured issuer must not leave `iss` and the discovery document's
// `issuer` disagreeing (a module's OIDC library checks that identity).
func TestNewService_TrimsTrailingSlashFromIssuer(t *testing.T) {
	keys, _ := NewKeys()
	svc := NewService(keys, Options{Issuer: issuer + "/"})
	if svc.Issuer() != issuer {
		t.Errorf("Issuer() = %q, want the trailing slash trimmed", svc.Issuer())
	}
	raw, err := svc.Mint("superset", "acme", "owner", "u-alice")
	if err != nil {
		t.Fatal(err)
	}
	std, _ := claimsOf(t, svc, raw)
	if std.Issuer != issuer {
		t.Errorf("iss = %q, want %q", std.Issuer, issuer)
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
		t.Fatal("signing key changed between loads: every issued assertion and cached JWKS would break")
	}
	if !reflect.DeepEqual(a.JWKS().Keys[0].Key, b.JWKS().Keys[0].Key) {
		t.Fatal("JWKS differs between loads")
	}
}

func TestLoadOrCreateKeys_RejectsCorruptSecretRatherThanRegenerating(t *testing.T) {
	c := newFake(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "booth-system", Name: KeysSecretName},
		Data:       map[string][]byte{signingKeyKey: []byte("not pem")},
	})
	if _, err := LoadOrCreateKeys(context.Background(), c, "booth-system"); err == nil {
		t.Fatal("silently accepted (or replaced) an unusable key Secret")
	}
}

// The two issuers (this one and workload.Service's) must never be reachable at the same URL or
// share key material — a module trusting one issuer class must not implicitly accept the other,
// since a workload token is defined as never a person while this assertion always is.
func TestKeysAreIndependentOfEachSecretName(t *testing.T) {
	if KeysSecretName == "booth-workload-keys" {
		t.Fatal("iframe-identity and workload keys share a Secret name")
	}
}

func TestLoadOrCreateKeys_TwoReplicasRacingConvergeOnOneKey(t *testing.T) {
	c := newFake(t)
	ctx := context.Background()

	// Simulate the create-then-adopt race directly: both "replicas" try to create the Secret;
	// only one wins, and the other must read back and use the winner's key, never its own.
	k1, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "booth-system", Name: KeysSecretName},
		Data:       k1.secretData(),
	}
	if err := c.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}

	k2, err := LoadOrCreateKeys(ctx, c, "booth-system")
	if err != nil {
		t.Fatal(err)
	}
	if k2.KeyID() != k1.KeyID() {
		t.Fatal("the second replica minted its own key instead of adopting the one already stored")
	}

	var stored corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-system", Name: KeysSecretName}, &stored); err != nil {
		t.Fatal(err)
	}
	if string(stored.Data[signingKeyKey]) != string(k1.secretData()[signingKeyKey]) {
		t.Fatal("the stored Secret was overwritten by the losing replica")
	}
}
