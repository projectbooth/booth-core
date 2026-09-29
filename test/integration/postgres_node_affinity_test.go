package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/dbprov"
)

// TestPinBundledPostgresNode_AgainstRealAPIServer runs ADR 0083's node-pinning bootstrap against
// a real kube-apiserver. The fake client used elsewhere can't catch what only a real API server
// enforces: whether a StatefulSet's spec.template.spec.nodeSelector is genuinely mutable
// post-creation via a merge patch (several other StatefulSet spec fields are immutable and a real
// server rejects changes to them), and that the patch doesn't need any RBAC beyond what core's
// existing broad ClusterRole already grants.
func TestPinBundledPostgresNode_AgainstRealAPIServer(t *testing.T) {
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

	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "booth-system"}}); err != nil {
		t.Fatal(err)
	}

	// The bundled chart's real shape: StatefulSet + its ordinal-0 pod, same name prefix.
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "booth-system", Name: "booth-core-postgresql"},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: "booth-core-postgresql",
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": "postgresql"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "postgresql"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "postgresql", Image: "postgres:16-alpine"}}},
			},
		},
	}
	if err := c.Create(ctx, sts); err != nil {
		t.Fatalf("creating StatefulSet: %v", err)
	}

	// envtest runs no real scheduler/kubelet, so nothing assigns a pod's node automatically —
	// simulating that (setting spec.nodeName directly at creation) is the same thing a real
	// scheduler's binding does, and is exactly what EnsureNodeAffinity reads.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "booth-system", Name: "booth-core-postgresql-0"},
		Spec: corev1.PodSpec{
			NodeName:   "kind-worker",
			Containers: []corev1.Container{{Name: "postgresql", Image: "postgres:16-alpine"}},
		},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("creating pod: %v", err)
	}

	pinned, err := dbprov.EnsureNodeAffinity(ctx, c, "booth-system", "booth-core-postgresql")
	if err != nil {
		t.Fatalf("EnsureNodeAffinity: %v", err)
	}
	if !pinned {
		t.Fatal("pinned = false, want true")
	}

	var got appsv1.StatefulSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-system", Name: "booth-core-postgresql"}, &got); err != nil {
		t.Fatal(err)
	}
	if node := got.Spec.Template.Spec.NodeSelector[dbprov.NodeHostnameLabel]; node != "kind-worker" {
		t.Fatalf("nodeSelector[%s] = %q, want kind-worker", dbprov.NodeHostnameLabel, node)
	}

	// Idempotent against a real server too: a second call is a no-op, not a conflicting write.
	if pinned, err = dbprov.EnsureNodeAffinity(ctx, c, "booth-system", "booth-core-postgresql"); err != nil || !pinned {
		t.Fatalf("second call: pinned=%v err=%v", pinned, err)
	}
}
