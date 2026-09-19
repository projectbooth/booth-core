package natsauth

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
)

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

func ns(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func TestLoadOrCreate_IsIdempotentAndStable(t *testing.T) {
	c := newFake(t)
	ctx := context.Background()

	a1, err := LoadOrCreate(ctx, c, "booth-system")
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	a2, err := LoadOrCreate(ctx, c, "booth-system")
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if a1.AccountPublicKey() != a2.AccountPublicKey() {
		t.Fatal("keys changed between calls: credentials minted earlier would stop working")
	}

	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-system", Name: AuthConfigMapName}, &cm); err != nil {
		t.Fatalf("server config ConfigMap missing: %v", err)
	}
	if !strings.Contains(cm.Data[AuthConfigKey], "operator:") {
		t.Errorf("ConfigMap doesn't look like a NATS auth fragment: %q", cm.Data[AuthConfigKey])
	}

	// The ConfigMap is readable by anyone who can read ConfigMaps in the namespace, so it
	// must never carry a private seed. NATS seeds are base32 strings starting "SO"/"SA"/"SU".
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-system", Name: KeysSecretName}, &sec); err != nil {
		t.Fatal(err)
	}
	for name, seed := range sec.Data {
		if strings.Contains(cm.Data[AuthConfigKey], string(seed)) {
			t.Errorf("ConfigMap contains the private seed %s", name)
		}
	}
}

func TestLoadOrCreate_RestoresDeletedServerConfig(t *testing.T) {
	c := newFake(t)
	ctx := context.Background()
	if _, err := LoadOrCreate(ctx, c, "booth-system"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "booth-system", Name: AuthConfigMapName}}); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadOrCreate(ctx, c, "booth-system"); err != nil {
		t.Fatalf("LoadOrCreate after ConfigMap deletion: %v", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-system", Name: AuthConfigMapName}, &cm); err != nil {
		t.Fatalf("ConfigMap not restored: %v", err)
	}
}

// End to end through the Kubernetes objects: what core writes must actually make a real
// server accept credentials minted from the keys core reloads.
func TestBootstrappedConfigAdmitsReloadedAuthority(t *testing.T) {
	c := newFake(t)
	ctx := context.Background()
	if _, err := LoadOrCreate(ctx, c, "booth-system"); err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-system", Name: AuthConfigMapName}, &cm); err != nil {
		t.Fatal(err)
	}
	s := startServerWithConf(t, cm.Data[AuthConfigKey])

	// A "restarted core": loads the keys back from the Secret rather than generating.
	reloaded, err := LoadOrCreate(ctx, c, "booth-system")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := reloaded.MintUser("core", CoreGrants(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connect(t, s, cred, nil); err != nil {
		t.Fatalf("server rejected a credential from the reloaded authority: %v", err)
	}
}

func module(id, namespace string, ev *boothv1alpha1.EventBusAccess) *boothv1alpha1.BoothModule {
	return &boothv1alpha1.BoothModule{
		ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: namespace, UID: types.UID("uid-" + id)},
		Spec: boothv1alpha1.BoothModuleSpec{
			ID: id, Events: ev,
			ServiceRef: boothv1alpha1.ServiceReference{Name: id + "-svc", Port: 80},
		},
	}
}

func getCredSecret(t *testing.T, c client.Client, namespace string) (*corev1.Secret, bool) {
	t.Helper()
	var s corev1.Secret
	err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: CredentialsSecretName}, &s)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return &s, true
}

func newProvisioner(t *testing.T, c client.Client) *ModuleProvisioner {
	t.Helper()
	return NewModuleProvisioner(c, newAuthority(t), "nats://booth-core-nats.booth-system.svc.cluster.local:4222")
}

func TestEnsure_ProvisionsScopedCredentialOwnedByModule(t *testing.T) {
	c := newFake(t, ns("booth-superset"))
	p := newProvisioner(t, c)
	m := module("superset", "booth-superset", &boothv1alpha1.EventBusAccess{Publish: []string{"dashboard.*"}})

	if err := p.Ensure(context.Background(), m); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	sec, ok := getCredSecret(t, c, "booth-superset")
	if !ok {
		t.Fatal("credential Secret was not provisioned")
	}
	if sec.StringData[URLKey] != p.URL {
		t.Errorf("url = %q, want %q", sec.StringData[URLKey], p.URL)
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "BoothModule" || sec.OwnerReferences[0].Name != "superset" {
		t.Errorf("OwnerReferences = %v, want the BoothModule (so uninstall garbage-collects the credential)", sec.OwnerReferences)
	}

	// Decode what was minted: the permissions must be exactly the declared ones.
	token, err := jwt.ParseDecoratedJWT([]byte(sec.StringData[CredsKey]))
	if err != nil {
		t.Fatalf("Secret does not contain a valid .creds file: %v", err)
	}
	uc, err := jwt.DecodeUserClaims(token)
	if err != nil {
		t.Fatal(err)
	}
	if uc.Name != "superset" {
		t.Errorf("credential name = %q, want superset", uc.Name)
	}
	var hasDeclared bool
	for _, allowed := range uc.Pub.Allow {
		if allowed == "booth.*.dashboard.*" {
			hasDeclared = true
		}
		for _, forbidden := range []string{">", "booth.>", "$JS.API.>", "$JS.API.STREAM.DELETE.BOOTH_EVENTS"} {
			if allowed == forbidden {
				t.Errorf("publish allow-list unexpectedly contains %q", forbidden)
			}
		}
	}
	if !hasDeclared {
		t.Errorf("publish allow-list %v is missing the declared subject", uc.Pub.Allow)
	}

	var n corev1.Namespace
	if err := c.Get(context.Background(), types.NamespacedName{Name: "booth-superset"}, &n); err != nil {
		t.Fatal(err)
	}
	if n.Labels[ClientNamespaceLabel] != "true" {
		t.Error("module namespace was not labelled for the NATS NetworkPolicy")
	}
}

func TestEnsure_DoesNotReMintWhileCurrent(t *testing.T) {
	c := newFake(t, ns("booth-superset"))
	p := newProvisioner(t, c)
	m := module("superset", "booth-superset", &boothv1alpha1.EventBusAccess{Publish: []string{"dashboard.*"}})

	if err := p.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	first, _ := getCredSecret(t, c, "booth-superset")

	if err := p.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	second, _ := getCredSecret(t, c, "booth-superset")
	if first.StringData[CredsKey] != second.StringData[CredsKey] {
		t.Fatal("credential was re-minted on a no-op reconcile (this runs every few seconds per module)")
	}
}

func TestEnsure_ReMintsWhenDeclaredEventsChange(t *testing.T) {
	c := newFake(t, ns("booth-superset"))
	p := newProvisioner(t, c)
	m := module("superset", "booth-superset", &boothv1alpha1.EventBusAccess{Publish: []string{"dashboard.*"}})
	if err := p.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	first, _ := getCredSecret(t, c, "booth-superset")

	m.Spec.Events.Publish = []string{"dashboard.created"}
	if err := p.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	second, _ := getCredSecret(t, c, "booth-superset")
	if first.StringData[CredsKey] == second.StringData[CredsKey] {
		t.Fatal("credential was not re-minted after the module's declared events changed")
	}
}

func TestEnsure_ReMintsNearExpiry(t *testing.T) {
	c := newFake(t, ns("booth-superset"))
	p := newProvisioner(t, c)
	m := module("superset", "booth-superset", &boothv1alpha1.EventBusAccess{Publish: []string{"dashboard.*"}})
	if err := p.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	first, _ := getCredSecret(t, c, "booth-superset")

	// Jump to within the renewal window.
	p.Now = func() time.Time { return time.Now().Add(p.TTL - p.RenewBefore/2) }
	if err := p.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	second, _ := getCredSecret(t, c, "booth-superset")
	if first.StringData[CredsKey] == second.StringData[CredsKey] {
		t.Fatal("credential was not renewed inside the renewal window")
	}
}

func TestEnsure_ModuleWithoutEventsGetsNoCredential(t *testing.T) {
	c := newFake(t, ns("booth-storage"))
	p := newProvisioner(t, c)

	if err := p.Ensure(context.Background(), module("storage", "booth-storage", nil)); err != nil {
		t.Fatal(err)
	}
	if _, ok := getCredSecret(t, c, "booth-storage"); ok {
		t.Fatal("a module that declares no events was given event-bus credentials")
	}
}

func TestEnsure_RemovesCredentialWhenEventsDropped(t *testing.T) {
	c := newFake(t, ns("booth-superset"))
	p := newProvisioner(t, c)
	m := module("superset", "booth-superset", &boothv1alpha1.EventBusAccess{Publish: []string{"dashboard.*"}})
	if err := p.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}

	m.Spec.Events = nil
	if err := p.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if _, ok := getCredSecret(t, c, "booth-superset"); ok {
		t.Fatal("credential outlived the module's event declaration")
	}
}

func TestEnsure_RejectsWideningPatterns(t *testing.T) {
	c := newFake(t, ns("evil"))
	p := newProvisioner(t, c)

	for _, bad := range []string{">", "*.*", "booth.>", "dashboard.>", "Dashboard.created", "dashboard", "*.created", "a b.c"} {
		m := module("evil", "evil", &boothv1alpha1.EventBusAccess{Publish: []string{bad}})
		if err := p.Ensure(context.Background(), m); err == nil {
			t.Errorf("pattern %q was accepted; it must not be possible to declare wider access than an event type", bad)
		}
	}
	if _, ok := getCredSecret(t, c, "evil"); ok {
		t.Error("a credential was provisioned despite an invalid declaration")
	}
}

func TestEnsure_CrossNamespaceSecretIsNotOwnerReferenced(t *testing.T) {
	c := newFake(t, ns("crds-live-here"), ns("pods-live-here"))
	p := newProvisioner(t, c)
	m := module("superset", "crds-live-here", &boothv1alpha1.EventBusAccess{Publish: []string{"dashboard.*"}})
	m.Spec.ServiceRef.Namespace = "pods-live-here"

	if err := p.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	sec, ok := getCredSecret(t, c, "pods-live-here")
	if !ok {
		t.Fatal("Secret should land in the namespace the module's pods run in")
	}
	if len(sec.OwnerReferences) != 0 {
		t.Errorf("cross-namespace ownerReference is invalid in Kubernetes, got %v", sec.OwnerReferences)
	}
}

// The full path a module takes: manifest -> provisioned Secret -> the exact .creds file a
// module mounts -> a connection a real server accepts and enforces.
func TestProvisionedCredentialWorksAndIsEnforcedByRealServer(t *testing.T) {
	a := newAuthority(t)
	s, coreJS := coreWithStream(t, a)
	c := newFake(t, ns("booth-superset"))
	p := NewModuleProvisioner(c, a, s.ClientURL())

	m := module("superset", "booth-superset", &boothv1alpha1.EventBusAccess{Publish: []string{"dashboard.created"}})
	if err := p.Ensure(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	sec, _ := getCredSecret(t, c, "booth-superset")

	// Rebuild a Credential from the mounted .creds file exactly as a client library would.
	creds := []byte(sec.StringData[CredsKey])
	token, err := jwt.ParseDecoratedJWT(creds)
	if err != nil {
		t.Fatal(err)
	}
	kp, err := jwt.ParseDecoratedUserNKey(creds)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := kp.Seed()
	if err != nil {
		t.Fatal(err)
	}

	ec := &errCollector{}
	nc, err := connect(t, s, &Credential{JWT: token, Seed: string(seed)}, ec)
	if err != nil {
		t.Fatalf("provisioned credential rejected by the server: %v", err)
	}

	// Allowed: exactly the declared event type. Denied: a sibling event type.
	if err := nc.Publish("booth.acme.dashboard.created", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := nc.Publish("booth.acme.dashboard.deleted", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Logf("flush: %v", err)
	}
	if !ec.sawPermissionViolation(2 * time.Second) {
		t.Error("expected a permissions violation for the undeclared sibling event type")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && streamMsgs(t, coreJS) < 1 {
		time.Sleep(25 * time.Millisecond)
	}
	if got := streamMsgs(t, coreJS); got != 1 {
		t.Fatalf("stream holds %d messages, want exactly 1 (only the declared event type)", got)
	}
}
