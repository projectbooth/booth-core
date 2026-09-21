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
	"github.com/projectbooth/booth-core/internal/workload"
)

// TestWorkloadIdentity_AgainstRealAPIServer runs ADR 0056's key bootstrap and credential
// provisioning against a real kube-apiserver. The fake client can't catch what only a real one
// enforces: that the BoothModule schema admits `workloadIdentity: {mint: true}` (and prunes
// nothing it should keep), that StringData becomes readable Data under the documented keys, that
// two replicas racing to create the signing key converge on one, and that the ownerReference is
// one Kubernetes accepts.
func TestWorkloadIdentity_AgainstRealAPIServer(t *testing.T) {
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

	for _, name := range []string{"booth-system", "booth-pipeline", "booth-storage"} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}

	// Signing key: created once, then adopted, and it survives a round trip through a real Secret.
	k1, err := workload.LoadOrCreateKeys(ctx, c, "booth-system")
	if err != nil {
		t.Fatalf("LoadOrCreateKeys: %v", err)
	}
	k2, err := workload.LoadOrCreateKeys(ctx, c, "booth-system")
	if err != nil {
		t.Fatal(err)
	}
	if k1.KeyID() != k2.KeyID() || k1.Credential("pipeline") != k2.Credential("pipeline") {
		t.Fatal("keys are not stable across loads against a real API server")
	}

	newModule := func(id, ns string, wi *boothv1alpha1.WorkloadIdentityRequirement) *boothv1alpha1.BoothModule {
		return &boothv1alpha1.BoothModule{
			ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: ns},
			Spec: boothv1alpha1.BoothModuleSpec{
				ID: id, DisplayName: id, Version: "0.1.0", ContractVersion: "0.1.0", HealthCheckPath: "/health",
				ServiceRef:       boothv1alpha1.ServiceReference{Name: id, Namespace: ns, Port: 8080},
				WorkloadIdentity: wi,
			},
		}
	}

	// The CRD must admit the field and keep it (an outdated schema would silently prune it, and
	// the module would never get a credential).
	pipeline := newModule("pipeline", "booth-pipeline", &boothv1alpha1.WorkloadIdentityRequirement{Mint: true})
	if err := c.Create(ctx, pipeline); err != nil {
		t.Fatalf("creating BoothModule with workloadIdentity: %v", err)
	}
	var stored boothv1alpha1.BoothModule
	if err := c.Get(ctx, client.ObjectKeyFromObject(pipeline), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.WorkloadIdentity == nil || !stored.Spec.WorkloadIdentity.Mint {
		t.Fatalf("the API server pruned workloadIdentity: %+v", stored.Spec.WorkloadIdentity)
	}

	storage := newModule("storage", "booth-storage", nil)
	if err := c.Create(ctx, storage); err != nil {
		t.Fatal(err)
	}

	issuer := "http://booth-core.booth-system.svc.cluster.local:8080"
	p := workload.NewModuleProvisioner(c, k1, issuer)
	if err := p.Ensure(ctx, &stored); err != nil {
		t.Fatalf("Ensure(pipeline): %v", err)
	}
	if err := p.Ensure(ctx, storage); err != nil {
		t.Fatalf("Ensure(storage): %v", err)
	}

	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-pipeline", Name: "booth-workload-minting-credentials"}, &sec); err != nil {
		t.Fatalf("minting Secret: %v", err)
	}
	// A real API server converts StringData to Data.
	if id, ok := k1.ModuleForCredential(string(sec.Data[workload.KeyCredential])); !ok || id != "pipeline" {
		t.Errorf("stored credential authenticates as %q, %v; want pipeline", id, ok)
	}
	if got := string(sec.Data[workload.KeyURL]); got != issuer+"/api/internal/workload-tokens" {
		t.Errorf("url = %q", got)
	}
	if got := string(sec.Data[workload.KeyIssuer]); got != issuer {
		t.Errorf("issuer = %q", got)
	}
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].UID != stored.UID {
		t.Errorf("ownerReferences = %v, want the BoothModule (uid %s)", sec.OwnerReferences, stored.UID)
	}

	// The module that never declared the field has no Secret to read.
	err = c.Get(ctx, types.NamespacedName{Namespace: "booth-storage", Name: "booth-workload-minting-credentials"}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("module without workloadIdentity has a minting Secret (err=%v)", err)
	}

	// Dropping the field withdraws the Secret.
	stored.Spec.WorkloadIdentity = nil
	if err := c.Update(ctx, &stored); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, &stored); err != nil {
		t.Fatal(err)
	}
	err = c.Get(ctx, types.NamespacedName{Namespace: "booth-pipeline", Name: "booth-workload-minting-credentials"}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Secret survived the module dropping workloadIdentity (err=%v)", err)
	}
}
