// Package workload implements ADR 0056: booth-core as a second trusted token issuer, for one
// narrow purpose — short-lived, workspace-scoped tokens that represent a job/run rather than a
// person, minted on request by modules that declared `workloadIdentity: {mint: true}`.
//
// The pieces: Keys (the signing key and the secret minting credentials derive from), Service
// (validates a mint request and signs the token), ModuleProvisioner (delivers each entitled
// module its minting credential), and the well-known JWKS/discovery documents that let every
// other module trust core's issuer the same generic way it trusts the OIDC provider's.
// The HTTP surface lives in internal/api.
package workload

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"

	jose "github.com/go-jose/go-jose/v4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// KeysSecretName holds core's workload-token signing key and the secret minting credentials
	// derive from. Only booth-core reads it. Deleting it regenerates both, which invalidates
	// every outstanding token (at most 10 minutes' worth) and every minting credential — the
	// provisioner re-issues the latter on its next reconcile, so this self-heals.
	KeysSecretName = "booth-workload-keys"

	signingKeyKey    = "signing.pem"
	credentialKeyKey = "credential.key"

	// signingAlg is RS256 deliberately: it's the one algorithm every JWT library's default
	// accepts (go-oidc's default set is RS256 only), so a module can add core as a second
	// issuer without configuring anything about algorithms.
	signingAlg = jose.RS256

	// credentialPrefix tags a minting credential so it can never be mistaken for a JWT (which
	// is also three dot-separated segments) and so a leaked one is recognisable in a log scan.
	credentialPrefix = "bwmc"
)

// Keys is core's workload-identity key material. Safe for concurrent use; immutable once built.
type Keys struct {
	signing *rsa.PrivateKey
	keyID   string

	// credKey is the HMAC key minting credentials derive from. Deriving (rather than storing a
	// random credential per module) means any core replica can verify a credential with no shared
	// state, and nothing has to be revoked when a module is removed — verification also requires
	// the module to *currently* declare workloadIdentity, checked live against the registry.
	credKey []byte
}

// NewKeys generates fresh key material.
func NewKeys() (*Keys, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generating workload signing key: %w", err)
	}
	credKey := make([]byte, 32)
	if _, err := rand.Read(credKey); err != nil {
		return nil, fmt.Errorf("generating credential key: %w", err)
	}
	return newKeys(priv, credKey)
}

func newKeys(priv *rsa.PrivateKey, credKey []byte) (*Keys, error) {
	if len(credKey) < 32 {
		return nil, fmt.Errorf("credential key must be at least 32 bytes, got %d", len(credKey))
	}
	jwk := jose.JSONWebKey{Key: &priv.PublicKey}
	thumb, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("computing key id: %w", err)
	}
	return &Keys{signing: priv, keyID: base64.RawURLEncoding.EncodeToString(thumb), credKey: credKey}, nil
}

// LoadOrCreateKeys returns this deployment's Keys, generating and persisting them on first run.
// Idempotent and safe under several core replicas: the first to create the Secret wins and the
// rest adopt it (the same create-then-adopt pattern as the event-bus keys).
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
		return nil, fmt.Errorf("workload keys in %s are unusable: %w", key, err)
	}
	return k, nil
}

func (k *Keys) secretData() map[string][]byte {
	der, _ := x509.MarshalPKCS8PrivateKey(k.signing) // cannot fail for an RSA key
	return map[string][]byte{
		signingKeyKey:    pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		credentialKeyKey: k.credKey,
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
	return newKeys(priv, data[credentialKeyKey])
}

// KeyID is the `kid` of the signing key, published in the JWKS.
func (k *Keys) KeyID() string { return k.keyID }

// JWKS is core's public key set: what a module fetches to verify a workload token.
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

// Credential is the minting credential for moduleID: `bwmc.<module>.<mac>`. It authorizes one
// thing — asking core to mint a workload token — and only as that module.
func (k *Keys) Credential(moduleID string) string {
	return credentialPrefix + "." + moduleID + "." + base64.RawURLEncoding.EncodeToString(k.credentialMAC(moduleID))
}

func (k *Keys) credentialMAC(moduleID string) []byte {
	m := hmac.New(sha256.New, k.credKey)
	m.Write([]byte("booth-workload-minting-credential\x00" + moduleID))
	return m.Sum(nil)
}

// ModuleForCredential authenticates a presented minting credential and returns the module it
// was issued to. It says nothing about whether that module is *still entitled* to mint — the
// caller must check the registry (Service does).
//
// Anything that isn't a credential core issued fails here, whatever else it is: a valid OIDC
// token from the deployment's IdP, a workload token core itself minted, a credential for a
// different module. There is deliberately no path from "a JWT" to "may mint".
func (k *Keys) ModuleForCredential(presented string) (string, bool) {
	parts := strings.Split(presented, ".")
	if len(parts) != 3 || parts[0] != credentialPrefix || parts[1] == "" {
		return "", false
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(mac, k.credentialMAC(parts[1])) {
		return "", false
	}
	return parts[1], true
}
