package workload

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
	// CredentialsSecretName is the Secret written into an entitled module's namespace (ADR 0056,
	// delivered by the ADR 0020 mechanism: the module reads it the ordinary Kubernetes way).
	CredentialsSecretName = "booth-workload-minting-credentials"

	// Secret keys.
	KeyCredential = "credential" // the minting credential; send as `Authorization: Bearer <it>`
	KeyURL        = "url"        // the full URL of the minting endpoint
	KeyIssuer     = "issuer"     // the `iss` of tokens core mints: what a verifier must trust

	workloadAnnotation = "booth.projectbooth.io/workload-identity"
)

// ModuleProvisioner writes each entitled module's minting credential Secret.
type ModuleProvisioner struct {
	Client client.Client
	Keys   *Keys

	// Issuer is core's issuer URL; the mint endpoint is Issuer + MintPath.
	Issuer string
}

// NewModuleProvisioner builds a ModuleProvisioner.
func NewModuleProvisioner(c client.Client, k *Keys, issuer string) *ModuleProvisioner {
	return &ModuleProvisioner{Client: c, Keys: k, Issuer: trimSlash(issuer)}
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// Ensure makes the module's minting-credential Secret match its manifest: written if the module
// declares `workloadIdentity.mint`, and removed (if this package wrote it) if it doesn't. A module
// that never declared the field gets nothing, and so cannot mint.
func (p *ModuleProvisioner) Ensure(ctx context.Context, mod *boothv1alpha1.BoothModule) error {
	ns := mod.Spec.ServiceNamespace(mod.Namespace)
	key := types.NamespacedName{Namespace: ns, Name: CredentialsSecretName}

	var existing corev1.Secret
	getErr := p.Client.Get(ctx, key, &existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return fmt.Errorf("reading %s: %w", key, getErr)
	}
	found := getErr == nil

	if mod.Spec.WorkloadIdentity == nil || !mod.Spec.WorkloadIdentity.Mint {
		if found && existing.Annotations[workloadAnnotation] != "" {
			if err := p.Client.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("removing stale credential %s: %w", key, err)
			}
		}
		return nil
	}

	want := map[string]string{
		KeyCredential: p.Keys.Credential(mod.Spec.ID),
		KeyURL:        p.Issuer + MintPath,
		KeyIssuer:     p.Issuer,
	}
	if found && existing.Annotations[workloadAnnotation] == mod.Spec.ID && matches(existing, want) {
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
	return secrets.NewProvisioner(p.Client).ProvisionSecretWith(ctx, ns, CredentialsSecretName, want,
		map[string]string{workloadAnnotation: mod.Spec.ID}, owner)
}

func matches(s corev1.Secret, want map[string]string) bool {
	for k, v := range want {
		if got, ok := s.Data[k]; ok {
			if string(got) != v {
				return false
			}
		} else if s.StringData[k] != v {
			return false
		}
	}
	return true
}
