package natsauth

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// KeysSecretName holds the private keys behind the bus's trust chain. Only booth-core
	// reads it. Deleting it regenerates every key, which invalidates every issued
	// credential and orphans the existing JetStream data (it lives under the old account).
	KeysSecretName = "booth-nats-keys"

	// AuthConfigMapName holds the generated NATS server config fragment. The bundled NATS
	// chart mounts it and includes it from nats.conf (see charts/booth-core/values.yaml);
	// its name is therefore a contract between this package and that chart. It contains
	// only public material (JWTs), no seeds.
	AuthConfigMapName = "booth-nats-auth"

	// AuthConfigKey is the file name inside AuthConfigMapName.
	AuthConfigKey = "booth-auth.conf"

	accountKeyAnnotation = "booth.projectbooth.io/account-public-key"
)

// LoadOrCreate returns the Authority for this deployment, generating and persisting its
// keys on first run. It's idempotent and safe to run from several core replicas at once:
// the first to create the keys Secret wins and the rest adopt it. It also (re)writes the
// server config ConfigMap when it's missing or was rendered from different keys, so the
// NATS server and core always agree on the trust root.
//
// It runs before core's HTTP server starts and does not depend on NATS being up — indeed
// the NATS pod can't start until the ConfigMap this creates exists, so core must never
// wait on the bus before calling it.
func LoadOrCreate(ctx context.Context, c client.Client, namespace string) (*Authority, error) {
	seeds, err := loadSeeds(ctx, c, namespace)
	if apierrors.IsNotFound(err) {
		seeds, err = createSeeds(ctx, c, namespace)
	}
	if err != nil {
		return nil, err
	}

	a, err := NewAuthority(seeds)
	if err != nil {
		return nil, fmt.Errorf("event bus keys in %s/%s are unusable: %w", namespace, KeysSecretName, err)
	}

	if err := ensureServerConfig(ctx, c, namespace, a); err != nil {
		return nil, err
	}
	return a, nil
}

func loadSeeds(ctx context.Context, c client.Client, namespace string) (Seeds, error) {
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: KeysSecretName}, &sec); err != nil {
		return Seeds{}, err
	}
	return Seeds{
		Operator: string(sec.Data["operator.seed"]),
		Account:  string(sec.Data["account.seed"]),
		Signing:  string(sec.Data["signing.seed"]),
		System:   string(sec.Data["system.seed"]),
	}, nil
}

func createSeeds(ctx context.Context, c client.Client, namespace string) (Seeds, error) {
	seeds, err := GenerateSeeds()
	if err != nil {
		return Seeds{}, err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: KeysSecretName},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"operator.seed": []byte(seeds.Operator),
			"account.seed":  []byte(seeds.Account),
			"signing.seed":  []byte(seeds.Signing),
			"system.seed":   []byte(seeds.System),
		},
	}
	if err := c.Create(ctx, sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another replica won the race; adopt its keys rather than ours.
			return loadSeeds(ctx, c, namespace)
		}
		return Seeds{}, fmt.Errorf("creating %s/%s: %w", namespace, KeysSecretName, err)
	}
	return seeds, nil
}

func ensureServerConfig(ctx context.Context, c client.Client, namespace string, a *Authority) error {
	key := types.NamespacedName{Namespace: namespace, Name: AuthConfigMapName}

	var existing corev1.ConfigMap
	err := c.Get(ctx, key, &existing)
	if err == nil && existing.Annotations[accountKeyAnnotation] == a.AccountPublicKey() && existing.Data[AuthConfigKey] != "" {
		return nil // already rendered from these keys
	}
	missing := apierrors.IsNotFound(err)
	if err != nil && !missing {
		return fmt.Errorf("reading %s: %w", key, err)
	}

	conf, err := a.ServerConfig()
	if err != nil {
		return err
	}
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Name:        AuthConfigMapName,
			Annotations: map[string]string{accountKeyAnnotation: a.AccountPublicKey()},
		},
		Data: map[string]string{AuthConfigKey: conf},
	}

	if missing {
		if cerr := c.Create(ctx, desired); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return fmt.Errorf("creating %s: %w", key, cerr)
		}
		return nil
	}

	existing.Annotations = desired.Annotations
	existing.Data = desired.Data
	if uerr := c.Update(ctx, &existing); uerr != nil {
		return fmt.Errorf("updating %s: %w", key, uerr)
	}
	return nil
}
