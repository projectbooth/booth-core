package dbprov

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newStatefulSet(namespace, name string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "x"}}},
			},
		},
	}
}

func newScheduledPod(namespace, name, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.PodSpec{NodeName: nodeName},
	}
}

func getStatefulSet(t *testing.T, c client.Client, namespace, name string) *appsv1.StatefulSet {
	t.Helper()
	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, &sts); err != nil {
		t.Fatalf("getting StatefulSet: %v", err)
	}
	return &sts
}

// ADR 0083: nothing to pin to yet, on a fresh install before the pod exists at all.
func TestEnsureNodeAffinity_PodDoesNotExistYet(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithObjects(newStatefulSet("booth-system", "booth-core-postgresql")).Build()

	pinned, err := EnsureNodeAffinity(ctx, c, "booth-system", "booth-core-postgresql")
	if err != nil {
		t.Fatalf("err = %v, want nil (not scheduled yet is not a failure)", err)
	}
	if pinned {
		t.Error("pinned = true with no pod at all")
	}
}

// The pod object exists (e.g. Pending) but the scheduler hasn't assigned it a node yet.
func TestEnsureNodeAffinity_PodExistsButNotYetScheduled(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithObjects(
		newStatefulSet("booth-system", "booth-core-postgresql"),
		newScheduledPod("booth-system", "booth-core-postgresql-0", ""),
	).Build()

	pinned, err := EnsureNodeAffinity(ctx, c, "booth-system", "booth-core-postgresql")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if pinned {
		t.Error("pinned = true with no node assigned")
	}
}

// The core case: once the pod has a node, the StatefulSet gets pinned to it, using the standard
// kubernetes.io/hostname label ADR 0083 names.
func TestEnsureNodeAffinity_PinsOnceTheNodeIsKnown(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithObjects(
		newStatefulSet("booth-system", "booth-core-postgresql"),
		newScheduledPod("booth-system", "booth-core-postgresql-0", "node-a"),
	).Build()

	pinned, err := EnsureNodeAffinity(ctx, c, "booth-system", "booth-core-postgresql")
	if err != nil {
		t.Fatal(err)
	}
	if !pinned {
		t.Fatal("pinned = false, want true")
	}
	sts := getStatefulSet(t, c, "booth-system", "booth-core-postgresql")
	if got := sts.Spec.Template.Spec.NodeSelector[NodeHostnameLabel]; got != "node-a" {
		t.Errorf("nodeSelector[%s] = %q, want node-a", NodeHostnameLabel, got)
	}
}

// Idempotent: a second call once already pinned reports pinned=true and doesn't even issue a
// write — the StatefulSet's ResourceVersion must not move.
func TestEnsureNodeAffinity_IdempotentOnceAlreadyPinned(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithObjects(
		newStatefulSet("booth-system", "booth-core-postgresql"),
		newScheduledPod("booth-system", "booth-core-postgresql-0", "node-a"),
	).Build()

	if _, err := EnsureNodeAffinity(ctx, c, "booth-system", "booth-core-postgresql"); err != nil {
		t.Fatal(err)
	}
	before := getStatefulSet(t, c, "booth-system", "booth-core-postgresql").ResourceVersion

	pinned, err := EnsureNodeAffinity(ctx, c, "booth-system", "booth-core-postgresql")
	if err != nil {
		t.Fatal(err)
	}
	if !pinned {
		t.Error("pinned = false on the already-pinned call")
	}
	after := getStatefulSet(t, c, "booth-system", "booth-core-postgresql").ResourceVersion
	if before != after {
		t.Errorf("StatefulSet was rewritten although already pinned correctly: %s -> %s", before, after)
	}
}

// Self-healing: if the pod is ever found on a different node than the current pin (e.g. an
// operator manually migrated the local-path volume), the next call re-pins to match reality
// rather than leaving a stale, now-wrong constraint in place.
func TestEnsureNodeAffinity_RepinsWhenThePodIsOnADifferentNode(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithObjects(
		newStatefulSet("booth-system", "booth-core-postgresql"),
		newScheduledPod("booth-system", "booth-core-postgresql-0", "node-a"),
	).Build()
	if _, err := EnsureNodeAffinity(ctx, c, "booth-system", "booth-core-postgresql"); err != nil {
		t.Fatal(err)
	}

	var pod corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-system", Name: "booth-core-postgresql-0"}, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Spec.NodeName = "node-b"
	if err := c.Update(ctx, &pod); err != nil {
		t.Fatal(err)
	}

	pinned, err := EnsureNodeAffinity(ctx, c, "booth-system", "booth-core-postgresql")
	if err != nil {
		t.Fatal(err)
	}
	if !pinned {
		t.Fatal("pinned = false, want true")
	}
	sts := getStatefulSet(t, c, "booth-system", "booth-core-postgresql")
	if got := sts.Spec.Template.Spec.NodeSelector[NodeHostnameLabel]; got != "node-b" {
		t.Errorf("nodeSelector[%s] = %q, want node-b (must follow the pod's real node)", NodeHostnameLabel, got)
	}
}

// A scheduled pod with no matching StatefulSet is a real error (the chart guarantees they
// coexist), not "not scheduled yet" — it must not be silently swallowed the same way.
func TestEnsureNodeAffinity_ErrorsWhenTheStatefulSetIsMissing(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithObjects(
		newScheduledPod("booth-system", "booth-core-postgresql-0", "node-a"),
	).Build()

	if _, err := EnsureNodeAffinity(ctx, c, "booth-system", "booth-core-postgresql"); err == nil {
		t.Fatal("expected an error when the StatefulSet doesn't exist")
	}
}

// Never touches any other field on the StatefulSet — this must be safe to run against a live,
// Helm-managed object without fighting Helm's own reconciliation of everything else.
func TestEnsureNodeAffinity_TouchesOnlyTheNodeSelector(t *testing.T) {
	ctx := context.Background()
	sts := newStatefulSet("booth-system", "booth-core-postgresql")
	sts.Spec.Template.Spec.Containers[0].Image = "postgres:16-alpine"
	sts.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
	c := fake.NewClientBuilder().WithObjects(sts, newScheduledPod("booth-system", "booth-core-postgresql-0", "node-a")).Build()

	if _, err := EnsureNodeAffinity(ctx, c, "booth-system", "booth-core-postgresql"); err != nil {
		t.Fatal(err)
	}
	got := getStatefulSet(t, c, "booth-system", "booth-core-postgresql")
	if got.Spec.Template.Spec.Containers[0].Image != "postgres:16-alpine" {
		t.Errorf("container image changed: %q", got.Spec.Template.Spec.Containers[0].Image)
	}
	if got.Labels["app.kubernetes.io/managed-by"] != "Helm" {
		t.Errorf("Helm's own label was disturbed: %v", got.Labels)
	}
}
