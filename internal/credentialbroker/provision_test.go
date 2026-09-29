package credentialbroker

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
)

func moduleSpec(id string, provides *boothv1alpha1.CredentialProviderSpec) boothv1alpha1.BoothModuleSpec {
	return boothv1alpha1.BoothModuleSpec{
		ID: id, DisplayName: id, Version: "0.1.0", ContractVersion: "0.1.0", HealthCheckPath: "/h",
		ServiceRef:          boothv1alpha1.ServiceReference{Name: id, Port: 80},
		ProvidesCredentials: provides,
	}
}

var providesS3 = &boothv1alpha1.CredentialProviderSpec{Kinds: []string{"s3"}}

func boothModule(id, ns, svcNS string, provides *boothv1alpha1.CredentialProviderSpec) *boothv1alpha1.BoothModule {
	spec := moduleSpec(id, provides)
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

func stringOf(s *corev1.Secret, k string) string {
	if v, ok := s.Data[k]; ok {
		return string(v)
	}
	return s.StringData[k]
}

func TestProvisioner_DeliversCredentialOnlyToModulesThatDeclareIt(t *testing.T) {
	c := newFake(t)
	keys, _ := NewKeys()
	p := NewModuleProvisioner(c, keys)
	ctx := context.Background()

	if err := p.Ensure(ctx, boothModule("storage", "booth-storage", "", providesS3)); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, boothModule("catalog", "booth-catalog", "", nil)); err != nil {
		t.Fatal(err)
	}

	s, err := getSecret(t, c, "booth-storage")
	if err != nil {
		t.Fatalf("entitled module got no Secret: %v", err)
	}
	if got := stringOf(s, KeyCredential); got != keys.ProviderCredential("storage") {
		t.Errorf("delivered credential = %q, want %q", got, keys.ProviderCredential("storage"))
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Kind != "BoothModule" {
		t.Errorf("Secret beside its BoothModule should be owned by it: %+v", s.OwnerReferences)
	}

	if _, err := getSecret(t, c, "booth-catalog"); !apierrors.IsNotFound(err) {
		t.Fatalf("module without providesCredentials received a Secret (err=%v)", err)
	}
}

func TestProvisioner_IsIdempotentAndSelfHealing(t *testing.T) {
	c := newFake(t)
	keys, _ := NewKeys()
	p := NewModuleProvisioner(c, keys)
	ctx := context.Background()
	mod := boothModule("storage", "booth-storage", "", providesS3)

	if err := p.Ensure(ctx, mod); err != nil {
		t.Fatal(err)
	}
	before, _ := getSecret(t, c, "booth-storage")
	if err := p.Ensure(ctx, mod); err != nil {
		t.Fatal(err)
	}
	after, _ := getSecret(t, c, "booth-storage")
	if before.ResourceVersion != after.ResourceVersion {
		t.Error("reconciling an up-to-date Secret rewrote it")
	}

	if err := c.Delete(ctx, after); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, mod); err != nil {
		t.Fatal(err)
	}
	if _, err := getSecret(t, c, "booth-storage"); err != nil {
		t.Fatalf("Secret not restored: %v", err)
	}

	rotated, _ := NewKeys()
	p.Keys = rotated
	if err := p.Ensure(ctx, mod); err != nil {
		t.Fatal(err)
	}
	s, _ := getSecret(t, c, "booth-storage")
	if got := stringOf(s, KeyCredential); got != rotated.ProviderCredential("storage") {
		t.Error("credential was not re-issued after the keys changed")
	}
}

func TestProvisioner_RemovesItsSecretWhenTheFieldIsDroppedButNotOthers(t *testing.T) {
	c := newFake(t)
	keys, _ := NewKeys()
	p := NewModuleProvisioner(c, keys)
	ctx := context.Background()

	if err := p.Ensure(ctx, boothModule("storage", "booth-storage", "", providesS3)); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, boothModule("storage", "booth-storage", "", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := getSecret(t, c, "booth-storage"); !apierrors.IsNotFound(err) {
		t.Fatalf("Secret survived the module dropping providesCredentials (err=%v)", err)
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
	p := NewModuleProvisioner(c, keys)
	ctx := context.Background()

	if err := p.Ensure(ctx, boothModule("a", "ns-of-a", "", providesS3)); err != nil {
		t.Fatal(err)
	}
	if _, err := getSecret(t, c, "ns-of-a"); err != nil {
		t.Errorf("default namespace not applied: %v", err)
	}
	if err := p.Ensure(ctx, boothModule("b", "ns-of-b", "svc-ns-b", providesS3)); err != nil {
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

func TestProvisioner_EmptyKindsListIsTheSameAsOmittingTheField(t *testing.T) {
	c := newFake(t)
	keys, _ := NewKeys()
	p := NewModuleProvisioner(c, keys)
	ctx := context.Background()

	if err := p.Ensure(ctx, boothModule("storage", "booth-storage", "", &boothv1alpha1.CredentialProviderSpec{Kinds: nil})); err != nil {
		t.Fatal(err)
	}
	if _, err := getSecret(t, c, "booth-storage"); !apierrors.IsNotFound(err) {
		t.Fatalf("an empty kinds list still received a Secret (err=%v)", err)
	}
}
