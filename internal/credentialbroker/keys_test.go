package credentialbroker

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFake(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
}

func TestProviderCredential_BindsToTheModule(t *testing.T) {
	k, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	cred := k.ProviderCredential("storage")
	if id, ok := k.ModuleForProviderCredential(cred); !ok || id != "storage" {
		t.Fatalf("round trip = %q, %v", id, ok)
	}

	parts := strings.Split(cred, ".")
	for name, bad := range map[string]string{
		"another module's name, same mac": parts[0] + ".database." + parts[2],
		"truncated mac":                   cred[:len(cred)-4],
		"empty":                           "",
		"no dots":                         "storage",
		"empty module":                    parts[0] + ".." + parts[2],
		"extra segment":                   cred + ".x",
		"wrong prefix":                    "bwmc." + parts[1] + "." + parts[2], // workload's own prefix
	} {
		if id, ok := k.ModuleForProviderCredential(bad); ok {
			t.Errorf("%s: accepted as module %q", name, id)
		}
	}

	other, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := other.ModuleForProviderCredential(cred); ok {
		t.Error("a credential from another deployment's keys was accepted")
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
	if b.ProviderCredential("storage") != a.ProviderCredential("storage") {
		t.Fatal("credentials differ between replicas loading the same Secret")
	}
}

func TestLoadOrCreateKeys_TwoReplicasRacingConvergeOnOneKey(t *testing.T) {
	c := newFake(t)
	ctx := context.Background()

	k1, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "booth-system", Name: KeysSecretName},
		Data:       map[string][]byte{credentialKeyKey: k1.credKey},
	}
	if err := c.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}

	k2, err := LoadOrCreateKeys(ctx, c, "booth-system")
	if err != nil {
		t.Fatal(err)
	}
	if k2.ProviderCredential("storage") != k1.ProviderCredential("storage") {
		t.Fatal("the second replica minted its own key instead of adopting the one already stored")
	}

	var stored corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-system", Name: KeysSecretName}, &stored); err != nil {
		t.Fatal(err)
	}
	if string(stored.Data[credentialKeyKey]) != string(k1.credKey) {
		t.Fatal("the stored Secret was overwritten by the losing replica")
	}
}

func TestLoadOrCreateKeys_RejectsCorruptSecretRatherThanRegenerating(t *testing.T) {
	c := newFake(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "booth-system", Name: KeysSecretName},
		Data:       map[string][]byte{credentialKeyKey: []byte("too-short")},
	})
	if _, err := LoadOrCreateKeys(context.Background(), c, "booth-system"); err == nil {
		t.Fatal("silently accepted (or replaced) an unusable key Secret")
	}
}
