package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/credentialbroker"
)

// TestCredentialBroker_AgainstRealAPIServer runs ADR 0080's manifest field and provider-credential
// provisioning against a real kube-apiserver — the fake client can't catch what only a real one
// enforces: that the BoothModule schema admits `providesCredentials: {kinds: [...]}` (and keeps
// it, rather than a stale/outdated schema silently pruning it), that a pattern-violating kind is
// rejected before core ever sees it, and that StringData becomes readable Data under the
// documented key.
func TestCredentialBroker_AgainstRealAPIServer(t *testing.T) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := boothv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, name := range []string{"booth-system", "booth-storage", "booth-catalog"} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}

	// The CRD schema must reject a kind that doesn't match the documented pattern before core
	// ever sees it (mirrors the `>` -> events.publish rejection test for ADR 0049).
	bad := &boothv1alpha1.BoothModule{
		ObjectMeta: metav1.ObjectMeta{Name: "evil", Namespace: "booth-catalog"},
		Spec: boothv1alpha1.BoothModuleSpec{
			ID: "evil", DisplayName: "Evil", Version: "0", ContractVersion: "0", HealthCheckPath: "/h",
			ServiceRef:          boothv1alpha1.ServiceReference{Name: "s", Port: 80},
			ProvidesCredentials: &boothv1alpha1.CredentialProviderSpec{Kinds: []string{"S3_Kind!"}},
		},
	}
	if err := c.Create(ctx, bad); err == nil {
		t.Error("the API server accepted a providesCredentials.kinds entry that doesn't match the documented pattern")
	}

	newModule := func(id, ns string, provides *boothv1alpha1.CredentialProviderSpec) *boothv1alpha1.BoothModule {
		return &boothv1alpha1.BoothModule{
			ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: ns},
			Spec: boothv1alpha1.BoothModuleSpec{
				ID: id, DisplayName: id, Version: "0.1.0", ContractVersion: "0.1.0", HealthCheckPath: "/health",
				ServiceRef:          boothv1alpha1.ServiceReference{Name: id, Namespace: ns, Port: 8080},
				ProvidesCredentials: provides,
			},
		}
	}

	storage := newModule("storage", "booth-storage", &boothv1alpha1.CredentialProviderSpec{Kinds: []string{"s3"}})
	if err := c.Create(ctx, storage); err != nil {
		t.Fatalf("creating BoothModule with providesCredentials: %v", err)
	}
	var stored boothv1alpha1.BoothModule
	if err := c.Get(ctx, client.ObjectKeyFromObject(storage), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.ProvidesCredentials == nil || len(stored.Spec.ProvidesCredentials.Kinds) != 1 || stored.Spec.ProvidesCredentials.Kinds[0] != "s3" {
		t.Fatalf("the API server pruned providesCredentials: %+v", stored.Spec.ProvidesCredentials)
	}

	catalog := newModule("catalog", "booth-catalog", nil)
	if err := c.Create(ctx, catalog); err != nil {
		t.Fatal(err)
	}

	keys, err := credentialbroker.NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	p := credentialbroker.NewModuleProvisioner(c, keys)
	if err := p.Ensure(ctx, &stored); err != nil {
		t.Fatalf("Ensure(storage): %v", err)
	}
	if err := p.Ensure(ctx, catalog); err != nil {
		t.Fatalf("Ensure(catalog): %v", err)
	}

	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-storage", Name: credentialbroker.CredentialsSecretName}, &sec); err != nil {
		t.Fatalf("provider-credential Secret: %v", err)
	}
	// A real API server converts StringData to Data.
	if got := string(sec.Data[credentialbroker.KeyCredential]); got != keys.ProviderCredential("storage") {
		t.Errorf("stored credential = %q, want %q", got, keys.ProviderCredential("storage"))
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].UID != stored.UID {
		t.Errorf("ownerReferences = %v, want the BoothModule (uid %s)", sec.OwnerReferences, stored.UID)
	}

	// The module that never declared the field has no Secret to read.
	err = c.Get(ctx, types.NamespacedName{Namespace: "booth-catalog", Name: credentialbroker.CredentialsSecretName}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("module without providesCredentials has a provider-credential Secret (err=%v)", err)
	}

	// Dropping the field withdraws the Secret.
	stored.Spec.ProvidesCredentials = nil
	if err := c.Update(ctx, &stored); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, &stored); err != nil {
		t.Fatal(err)
	}
	err = c.Get(ctx, types.NamespacedName{Namespace: "booth-storage", Name: credentialbroker.CredentialsSecretName}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Secret survived the module dropping providesCredentials (err=%v)", err)
	}
}
