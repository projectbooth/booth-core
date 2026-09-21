package natsauth

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/secrets"
)

const (
	// CredentialsSecretName is the Secret core writes into a module's namespace. Its name
	// and keys are what a module's chart mounts or reads (ADR 0020: the module reads
	// provisioned config the ordinary Kubernetes way, never via a live API). Proposed
	// convention, pending an architecture ADR — see docs/decisions/0006.
	CredentialsSecretName = "booth-event-bus-credentials"

	// CredsKey holds a standard NATS ".creds" file (nats.UserCredentials reads it as-is).
	CredsKey = "nats.creds"

	// URLKey holds the in-cluster URL to connect to.
	URLKey = "url"

	// ClientNamespaceLabel marks a namespace as hosting event-bus clients. The bundled
	// NATS NetworkPolicy admits traffic only from namespaces carrying it.
	ClientNamespaceLabel = "booth.projectbooth.io/event-bus-client"

	grantsHashAnnotation = "booth.projectbooth.io/grants-hash"
	expiresAnnotation    = "booth.projectbooth.io/expires-at"

	// DefaultTTL bounds how long a credential is valid. Credentials are renewed well
	// before this while the module exists, so the expiry is what eventually revokes a
	// module that has been uninstalled (there is no online revocation in v0).
	DefaultTTL = 90 * 24 * time.Hour

	// DefaultRenewBefore is how long before expiry a credential is re-minted.
	DefaultRenewBefore = 30 * 24 * time.Hour
)

// ModuleProvisioner writes each module's event-bus credential Secret, scoped by that
// module's manifest (BoothModule.Spec.Events).
type ModuleProvisioner struct {
	Client    client.Client
	Authority *Authority

	// URL is the in-cluster address modules connect to; it goes into the Secret.
	URL string

	TTL         time.Duration
	RenewBefore time.Duration
	Now         func() time.Time
}

// NewModuleProvisioner builds a ModuleProvisioner with default lifetimes.
func NewModuleProvisioner(c client.Client, a *Authority, url string) *ModuleProvisioner {
	return &ModuleProvisioner{
		Client: c, Authority: a, URL: url,
		TTL: DefaultTTL, RenewBefore: DefaultRenewBefore, Now: time.Now,
	}
}

// Ensure makes the module's credential Secret match its manifest: minting one if absent,
// if the declared events changed, or if it's near expiry; leaving it alone otherwise; and
// removing one this package wrote if the module no longer declares any events. A module
// that never declared events gets nothing — and so cannot connect to the bus at all.
func (p *ModuleProvisioner) Ensure(ctx context.Context, mod *boothv1alpha1.BoothModule) error {
	grants, err := GrantsFor(mod.Spec.Events)
	if err != nil {
		return fmt.Errorf("module %q: %w", mod.Spec.ID, err)
	}

	ns := mod.Spec.ServiceNamespace(mod.Namespace)
	key := types.NamespacedName{Namespace: ns, Name: CredentialsSecretName}

	var existing corev1.Secret
	getErr := p.Client.Get(ctx, key, &existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return fmt.Errorf("reading %s: %w", key, getErr)
	}
	found := getErr == nil

	if grants.IsEmpty() {
		if found && existing.Annotations[grantsHashAnnotation] != "" {
			if err := p.Client.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("removing stale credential %s: %w", key, err)
			}
		}
		return nil
	}

	if found && p.isCurrent(&existing, grants) {
		return p.labelNamespace(ctx, ns)
	}

	cred, err := p.Authority.MintUser(mod.Spec.ID, grants, p.TTL)
	if err != nil {
		return fmt.Errorf("minting credential for module %q: %w", mod.Spec.ID, err)
	}

	// ownerReferences can't cross namespaces: own the Secret (so it's garbage-collected
	// on uninstall) only when it lives alongside the BoothModule.
	var owner *metav1.OwnerReference
	if ns == mod.Namespace {
		owner = &metav1.OwnerReference{
			APIVersion: boothv1alpha1.GroupVersion.String(),
			Kind:       "BoothModule",
			Name:       mod.Name,
			UID:        mod.UID,
		}
	}

	err = secrets.NewProvisioner(p.Client).ProvisionSecretWith(ctx, ns, CredentialsSecretName,
		map[string]string{CredsKey: string(cred.Creds), URLKey: p.URL},
		map[string]string{
			grantsHashAnnotation: grants.Hash(),
			expiresAnnotation:    cred.ExpiresAt.UTC().Format(time.RFC3339),
		},
		owner,
	)
	if err != nil {
		return err
	}
	return p.labelNamespace(ctx, ns)
}

// isCurrent reports whether an existing Secret still matches the manifest and has enough
// life left, so reconciling every few seconds doesn't mint a new credential each time.
func (p *ModuleProvisioner) isCurrent(s *corev1.Secret, g Grants) bool {
	if s.Annotations[grantsHashAnnotation] != g.Hash() {
		return false
	}
	exp, err := time.Parse(time.RFC3339, s.Annotations[expiresAnnotation])
	if err != nil || !exp.After(p.Now().Add(p.RenewBefore)) {
		return false
	}
	_, hasData := s.Data[CredsKey]
	_, hasStringData := s.StringData[CredsKey]
	return hasData || hasStringData
}

// labelNamespace marks the module's namespace as an event-bus client so the bundled NATS
// NetworkPolicy admits it. Labels are never removed: another module in the same
// namespace may still need the bus, and a stale label only ever widens reachability to a
// namespace that already holds a valid credential holder, never beyond.
func (p *ModuleProvisioner) labelNamespace(ctx context.Context, ns string) error {
	var n corev1.Namespace
	if err := p.Client.Get(ctx, types.NamespacedName{Name: ns}, &n); err != nil {
		return fmt.Errorf("reading namespace %s: %w", ns, err)
	}
	if n.Labels[ClientNamespaceLabel] == "true" {
		return nil
	}
	patch := client.MergeFrom(n.DeepCopy())
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	n.Labels[ClientNamespaceLabel] = "true"
	if err := p.Client.Patch(ctx, &n, patch); err != nil {
		return fmt.Errorf("labelling namespace %s: %w", ns, err)
	}
	return nil
}
