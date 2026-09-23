// Package iframeidentity implements ADR 0069: on every iframe-proxied request (ADR 0005's
// uiIntegrationMode: iframe-proxy), core mints a short-lived, per-module-audience signed
// identity assertion — X-Booth-Identity — because that path has no bearer token to forward at
// all. The browser's own OIDC token lives only in memory (ADR 0032) and a plain iframe
// navigation, or a third-party UI's own follow-up call, can't carry it; a workspace/role header
// alone is exactly what ADR 0041 already forbids trusting as sole authority.
//
// This is a second, independent issuer from workload.Service's (ADR 0056/0058) — distinct key,
// distinct issuer URL, distinct claim shape (`aud` is the target module's id here, not a fixed
// client id) — precisely because a workload token is defined as never representing a person, and
// a module trusting one issuer class must not implicitly accept the other. Keys mirrors
// workload.Keys' bootstrap; Service mirrors workload.Service's JWKS/discovery surface. The HTTP
// routes live in internal/api; the minting call site is internal/gateway's iframe-proxy handlers.
package iframeidentity

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"

	jose "github.com/go-jose/go-jose/v4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// KeysSecretName holds core's iframe-identity signing key, separate from
	// workload.KeysSecretName so the two issuers never share key material. Only booth-core reads
	// it. Deleting it regenerates the key, invalidating every outstanding assertion — harmless,
	// since one lives at most DefaultTTL and is minted fresh on the very next proxied request.
	KeysSecretName = "booth-iframe-identity-keys"

	signingKeyKey = "signing.pem"

	// signingAlg is RS256, the one algorithm every JWT library's default OIDC verification
	// already accepts, so a module trusts this as a second issuer with no algorithm config.
	signingAlg = jose.RS256
)

// Keys is core's iframe-identity signing key. Safe for concurrent use; immutable once built.
type Keys struct {
	signing *rsa.PrivateKey
	keyID   string
}

// NewKeys generates a fresh key.
func NewKeys() (*Keys, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generating iframe-identity signing key: %w", err)
	}
	return newKeys(priv)
}

func newKeys(priv *rsa.PrivateKey) (*Keys, error) {
	jwk := jose.JSONWebKey{Key: &priv.PublicKey}
	thumb, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("computing key id: %w", err)
	}
	return &Keys{signing: priv, keyID: base64.RawURLEncoding.EncodeToString(thumb)}, nil
}

// LoadOrCreateKeys returns this deployment's Keys, generating and persisting them on first run.
// Idempotent and safe under several core replicas: the first to create the Secret wins and the
// rest adopt it (the same create-then-adopt pattern as workload.LoadOrCreateKeys and the event-bus
// keys).
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
			Data:       k.secretData(),
		}
		cerr := c.Create(ctx, &sec)
		if cerr == nil {
			return k, nil
		}
		if !apierrors.IsAlreadyExists(cerr) {
			return nil, fmt.Errorf("creating %s: %w", key, cerr)
		}
		// Another replica won the race; adopt its keys rather than ours.
		if err = c.Get(ctx, key, &sec); err != nil {
			return nil, fmt.Errorf("reading %s: %w", key, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}

	k, err := keysFromSecret(sec.Data)
	if err != nil {
		return nil, fmt.Errorf("iframe-identity keys in %s are unusable: %w", key, err)
	}
	return k, nil
}

func (k *Keys) secretData() map[string][]byte {
	der, _ := x509.MarshalPKCS8PrivateKey(k.signing) // cannot fail for an RSA key
	return map[string][]byte{
		signingKeyKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
	}
}

func keysFromSecret(data map[string][]byte) (*Keys, error) {
	block, _ := pem.Decode(data[signingKeyKey])
	if block == nil {
		return nil, fmt.Errorf("no PEM block in %s", signingKeyKey)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", signingKeyKey, err)
	}
	priv, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an RSA key", signingKeyKey)
	}
	return newKeys(priv)
}

// KeyID is the `kid` of the signing key, published in the JWKS.
func (k *Keys) KeyID() string { return k.keyID }

// JWKS is core's public key set for this issuer: what a module fetches to verify an assertion.
func (k *Keys) JWKS() jose.JSONWebKeySet {
	return jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &k.signing.PublicKey, KeyID: k.keyID, Algorithm: string(signingAlg), Use: "sig",
	}}}
}

func (k *Keys) signer() (jose.Signer, error) {
	return jose.NewSigner(
		jose.SigningKey{Algorithm: signingAlg, Key: k.signing},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", k.keyID),
	)
}
