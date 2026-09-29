package credentialbroker

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/secrets"
)

const (
	// CredentialsSecretName is the Secret written into a provider module's namespace (ADR 0080,
	// via the ADR 0020 mechanism: the module reads it the ordinary Kubernetes way).
	CredentialsSecretName = "booth-credential-broker-provider-credentials"

	// KeyCredential holds the value core presents as `Authorization: Bearer <it>` on every
	// outbound broker call to this provider.
	KeyCredential = "credential"

	providerAnnotation = "booth.projectbooth.io/credential-provider"
)

// ModuleProvisioner writes each declaring module's broker-calling-credential Secret. Mirrors
// workload.ModuleProvisioner almost exactly — same Ensure-on-every-reconcile shape, same
// create-or-remove-based-on-the-manifest-field logic — but delivers a credential core presents
// *outbound*, not one a module presents inbound to core.
type ModuleProvisioner struct {
	Client client.Client
	Keys   *Keys
}

// NewModuleProvisioner builds a ModuleProvisioner.
func NewModuleProvisioner(c client.Client, k *Keys) *ModuleProvisioner {
	return &ModuleProvisioner{Client: c, Keys: k}
}

// Ensure makes the module's provider-credential Secret match its manifest: written if the module
// declares `providesCredentials`, removed (if this package wrote it) if it doesn't. A module that
// never declared the field gets nothing, and so is never routed to.
func (p *ModuleProvisioner) Ensure(ctx context.Context, mod *boothv1alpha1.BoothModule) error {
	ns := mod.Spec.ServiceNamespace(mod.Namespace)
	key := types.NamespacedName{Namespace: ns, Name: CredentialsSecretName}

	var existing corev1.Secret
	getErr := p.Client.Get(ctx, key, &existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return fmt.Errorf("reading %s: %w", key, getErr)
	}
	found := getErr == nil

	if mod.Spec.ProvidesCredentials == nil || len(mod.Spec.ProvidesCredentials.Kinds) == 0 {
		if found && existing.Annotations[providerAnnotation] != "" {
			if err := p.Client.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("removing stale credential %s: %w", key, err)
			}
		}
		return nil
	}

	want := p.Keys.ProviderCredential(mod.Spec.ID)
	if found && existing.Annotations[providerAnnotation] == mod.Spec.ID && matches(existing, want) {
		return nil
	}

	// ownerReferences can't cross namespaces: own the Secret (so it's garbage-collected on
	// uninstall) only when it lives alongside the BoothModule.
	var owner *metav1.OwnerReference
	if ns == mod.Namespace {
		owner = &metav1.OwnerReference{
			APIVersion: boothv1alpha1.GroupVersion.String(),
			Kind:       "BoothModule",
			Name:       mod.Name,
			UID:        mod.UID,
		}
	}
	return secrets.NewProvisioner(p.Client).ProvisionSecretWith(ctx, ns, CredentialsSecretName,
		map[string]string{KeyCredential: want},
		map[string]string{providerAnnotation: mod.Spec.ID}, owner)
}

func matches(s corev1.Secret, want string) bool {
	if got, ok := s.Data[KeyCredential]; ok {
		return string(got) == want
	}
	return s.StringData[KeyCredential] == want
}
