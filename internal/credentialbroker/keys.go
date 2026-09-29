// Package credentialbroker implements ADR 0080: a cross-cutting capability, distinct from
// workload identity's platform_access/grant() (ADR 0056/0058), for handing a task or module a
// short-lived native-protocol credential — an object-storage credential, a Postgres connection —
// that can't be proxied through the gateway's ordinary HTTP path the way a module API call can.
//
// Core is the broker, not a secrets vault: it authenticates and authorizes a request using the
// same identity/workspace/role the gateway already resolves, then routes it to whichever module
// declared it provides that credential *kind* (ADR 0080's `providesCredentials` manifest field),
// and relays back exactly what that provider mints. See contracts/credential-broker.md and
// docs/decisions/0014-credential-broker.md for the full design.
package credentialbroker

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// KeysSecretName holds the key core derives every provider's broker-calling credential from.
	// Only booth-core reads it. Deleting it invalidates every provider's delivered credential —
	// the provisioner re-issues a fresh one for each declaring module on its next reconcile, the
	// same self-healing property workload.Keys and its Secret already have.
	KeysSecretName = "booth-credential-broker-keys"

	credentialKeyKey = "credential.key"

	// credentialPrefix tags a provider credential so it's never mistaken for a JWT (workload
	// tokens, human tokens) or for a workload minting credential (workload.credentialPrefix),
	// and so a leaked one is recognisable in a log scan.
	credentialPrefix = "bcbp"
)

// Keys is core's credential-broker key material: the seed every provider module's own
// broker-calling credential is derived from. Safe for concurrent use; immutable once built.
type Keys struct {
	credKey []byte
}

// NewKeys generates fresh key material.
func NewKeys() (*Keys, error) {
	credKey := make([]byte, 32)
	if _, err := rand.Read(credKey); err != nil {
		return nil, fmt.Errorf("generating credential-broker key: %w", err)
	}
	return newKeys(credKey)
}

func newKeys(credKey []byte) (*Keys, error) {
	if len(credKey) < 32 {
		return nil, fmt.Errorf("credential-broker key must be at least 32 bytes, got %d", len(credKey))
	}
	return &Keys{credKey: credKey}, nil
}

// LoadOrCreateKeys returns this deployment's Keys, generating and persisting them on first run.
// Idempotent and safe under several core replicas: the first to create the Secret wins and the
// rest adopt it (the same create-then-adopt pattern as workload.LoadOrCreateKeys).
func LoadOrCreateKeys(ctx context.Context, c client.Client, namespace string) (*Keys, error) {
	key := types.NamespacedName{Namespace: namespace, Name: KeysSecretName}

	var sec corev1.Secret
	err := c.Get(ctx, key, &sec)
	if apierrors.IsNotFound(err) {
		k, gerr := NewKeys()
		if gerr != nil {
			return nil, gerr
		}
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: KeysSecretName},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{credentialKeyKey: k.credKey},
		}
		cerr := c.Create(ctx, &sec)
		if cerr == nil {
			return k, nil
		}
		if !apierrors.IsAlreadyExists(cerr) {
			return nil, fmt.Errorf("creating %s: %w", key, cerr)
		}
		// Another replica won the race; adopt its key rather than ours.
		if err = c.Get(ctx, key, &sec); err != nil {
			return nil, fmt.Errorf("reading %s: %w", key, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}

	k, err := newKeys(sec.Data[credentialKeyKey])
	if err != nil {
		return nil, fmt.Errorf("credential-broker key in %s is unusable: %w", key, err)
	}
	return k, nil
}

// ProviderCredential is the credential core delivers to a provider module (as the Secret
// `booth-credential-broker-provider-credentials`) and presents on every outbound call to it:
// `bcbp.<module-id>.<mac>`. Derived rather than stored per-module, so any core replica can
// authenticate a provider call with no shared state, and dropping `providesCredentials` from a
// module's manifest revokes it immediately (the provisioner deletes the Secret; core also
// re-checks entitlement live before ever calling out, the same belt-and-suspenders pattern
// workload.Service.AuthenticateModule uses).
func (k *Keys) ProviderCredential(moduleID string) string {
	return credentialPrefix + "." + moduleID + "." + base64.RawURLEncoding.EncodeToString(k.mac(moduleID))
}

func (k *Keys) mac(moduleID string) []byte {
	m := hmac.New(sha256.New, k.credKey)
	m.Write([]byte("booth-credential-broker-provider-credential\x00" + moduleID))
	return m.Sum(nil)
}

// ModuleForProviderCredential authenticates a presented provider credential and returns the
// module it was issued to. Core itself never calls this (core only ever presents this credential
// outbound, never verifies one inbound), but a real provider implementation needs the exact same
// derivation to check an inbound call really came from core — kept here, alongside
// ProviderCredential, so that verification lives in one place rather than being re-derived
// per-provider from scratch.
func (k *Keys) ModuleForProviderCredential(presented string) (string, bool) {
	parts := strings.Split(presented, ".")
	if len(parts) != 3 || parts[0] != credentialPrefix || parts[1] == "" {
		return "", false
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(mac, k.mac(parts[1])) {
		return "", false
	}
	return parts[1], true
}
