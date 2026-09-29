package dbprov

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NodeHostnameLabel is the standard, built-in node label every distribution sets (k3s included)
// — used rather than a custom one so pinning needs no separate step that labels Node objects
// themselves (ADR 0083 names this label explicitly as the intended mechanism).
const NodeHostnameLabel = "kubernetes.io/hostname"

// EnsureNodeAffinity implements ADR 0083: pins the bundled Postgres StatefulSet's pod template to
// the node its `local-path-provisioner` volume actually lives on, once that's known. `local-path`
// binds a PersistentVolume to whichever specific node first created it; nothing else in the chart
// keeps the pod on that same node, so an unpinned reschedule (a rollout restart, a `helm upgrade`
// touching the pod spec, a node drain) can strand the pod on a node that can't mount its data —
// not one module's database, but the single shared instance every opted-in module's lives on.
//
// This can't be a static value in the chart: the node isn't known until the pod has actually been
// scheduled once, so pinning happens here, after the fact, against the live cluster — the same
// "core bootstraps its own bundled-Postgres infra directly, not via Helm" trust boundary as
// EnsureAdminPassword/EnsureBackupClaim. statefulSetName is also the ordinal-0 pod's name prefix
// (the bundled chart names the Service, StatefulSet, and pod all "<release>-postgresql").
//
// Returns pinned=true once the StatefulSet's nodeSelector matches the pod's actual node (whether
// this call just set it or it already matched — idempotent and safe to call repeatedly, including
// concurrently from several core replicas: a patch that loses a race just gets recomputed and
// retried the same as any other transient error). pinned=false with a nil error means there's
// nothing to pin to yet (the pod doesn't exist, or exists but hasn't been assigned a node) — not a
// failure, just "too early"; the caller is expected to retry.
func EnsureNodeAffinity(ctx context.Context, c client.Client, namespace, statefulSetName string) (pinned bool, err error) {
	var pod corev1.Pod
	podKey := types.NamespacedName{Namespace: namespace, Name: statefulSetName + "-0"}
	if err := c.Get(ctx, podKey, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s: %w", podKey, err)
	}
	if pod.Spec.NodeName == "" {
		return false, nil
	}

	var sts appsv1.StatefulSet
	stsKey := types.NamespacedName{Namespace: namespace, Name: statefulSetName}
	if err := c.Get(ctx, stsKey, &sts); err != nil {
		return false, fmt.Errorf("reading %s: %w", stsKey, err)
	}
	if sts.Spec.Template.Spec.NodeSelector[NodeHostnameLabel] == pod.Spec.NodeName {
		return true, nil
	}

	patch := client.MergeFrom(sts.DeepCopy())
	if sts.Spec.Template.Spec.NodeSelector == nil {
		sts.Spec.Template.Spec.NodeSelector = map[string]string{}
	}
	sts.Spec.Template.Spec.NodeSelector[NodeHostnameLabel] = pod.Spec.NodeName
	if err := c.Patch(ctx, &sts, patch); err != nil {
		return false, fmt.Errorf("pinning %s to node %q: %w", stsKey, pod.Spec.NodeName, err)
	}
	return true, nil
}
