// Package secrets implements ADR 0020's config/secrets delivery: core (or a granting
// module, e.g. booth-storage handing out storage credentials) provisions a module's
// configuration and credentials as ordinary Kubernetes Secret/ConfigMap objects ahead of
// time. There is deliberately no live, on-demand "give me a credential" API here — that's
// the trust boundary ADR 0020 exists to preserve (ARCHITECTURE.md §6's "no live
// credential-write capability" lesson). A module reads what's provisioned the ordinary
// Kubernetes way: mounted files or env vars from its own pod spec.
package secrets

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Provisioner writes Secret/ConfigMap objects into a module's namespace as part of a
// reconciliation loop (ADR 0020's "likely the same controller machinery as ADR 0019's
// manifest watching, extended to also manage Secrets/ConfigMaps"). It performs
// create-or-update, never delete — removing a module's Helm release removes its
// namespace's objects as a normal side effect of that, not something this package drives.
type Provisioner struct {
	client client.Client
}

func NewProvisioner(c client.Client) *Provisioner {
	return &Provisioner{client: c}
}

// ProvisionSecret creates or updates a Secret named `name` in `namespace` with the given
// string data, owned by `owner` so it's garbage-collected if the owner is deleted.
// data is intentionally map[string]string (not []byte) — every caller in this codebase
// is composing credentials/config from Go strings, and Kubernetes handles the
// base64 encoding of the underlying Secret's StringData field for us.
func (p *Provisioner) ProvisionSecret(ctx context.Context, namespace, name string, data map[string]string, owner metav1.OwnerReference) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, p.client, secret, func() error {
		secret.StringData = data
		secret.OwnerReferences = []metav1.OwnerReference{owner}
		return nil
	})
	if err != nil {
		return fmt.Errorf("provisioning secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

// ProvisionConfigMap creates or updates a ConfigMap named `name` in `namespace`.
func (p *Provisioner) ProvisionConfigMap(ctx context.Context, namespace, name string, data map[string]string, owner metav1.OwnerReference) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, p.client, cm, func() error {
		cm.Data = data
		cm.OwnerReferences = []metav1.OwnerReference{owner}
		return nil
	})
	if err != nil {
		return fmt.Errorf("provisioning configmap %s/%s: %w", namespace, name, err)
	}
	return nil
}
