// Package tlsprov provisions the self-signed certificate ADR 0108 item 2(b) describes for the
// bundled install's Ingress: a long-lived CA (so a CA an operator has already imported into
// their browser/OS trust store keeps working across renewals) and a leaf certificate of at
// most one year, which this package re-issues under the same CA once it's within 30 days of
// expiry ("adopt what is there" must not mean "expires silently" -- ADR 0108 condition 4).
//
// Mode (a) (an operator-supplied Secret) and mode (c) (cert-manager) never call this package at
// all -- core must not generate or touch anything in either of those modes. Callers gate on
// config.TLS.SelfSigned (computed by the chart from ingress.tls.secretName/selfSigned) before
// ever calling Ensure.
package tlsprov

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// caSecretSuffix names the CA's own Secret, core-internal only -- an operator never reads
	// this one directly; the CA's public cert is also copied into the leaf Secret's ca.crt key
	// (below) for operator retrieval, so there's one place to look, not two.
	caSecretSuffix = "-ca"

	KeyTLSCert = "tls.crt" // the leaf cert (+ CA, as a chain) -- what the Ingress itself reads
	KeyTLSKey  = "tls.key"
	KeyCACert  = "ca.crt" // the CA's own public cert, for an operator to import/trust

	caValidity   = 10 * 365 * 24 * time.Hour // long-lived: a trusted CA must keep working
	leafValidity = 365 * 24 * time.Hour      // "a leaf of at most one year" (ADR 0108 condition 4)
	renewWithin  = 30 * 24 * time.Hour       // re-issue once within this long of expiry
)

type caKeyPair struct {
	cert *x509.Certificate
	key  *rsa.PrivateKey
	der  []byte // the CA's own raw DER, reused when signing the leaf
}

// Ensure makes the leaf Secret (named by leafSecretName, in namespace) hold a valid
// kubernetes.io/tls certificate for host, self-signed under a CA this package also manages
// (creating either on first run). Safe to call on every reconcile/startup: an existing CA is
// always adopted; an existing leaf is only re-issued when it's missing, within 30 days of
// expiry, or its SAN no longer matches host (ingress.host changed).
func Ensure(ctx context.Context, c client.Client, namespace, leafSecretName, host string) error {
	ca, err := ensureCA(ctx, c, namespace, leafSecretName+caSecretSuffix)
	if err != nil {
		return fmt.Errorf("provisioning the self-signed CA: %w", err)
	}

	leafKey := types.NamespacedName{Namespace: namespace, Name: leafSecretName}
	var existing corev1.Secret
	err = c.Get(ctx, leafKey, &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading %s: %w", leafKey, err)
	}
	if err == nil && leafStillValid(&existing, host) {
		return nil
	}

	certPEM, keyPEM, err := issueLeaf(ca, host, leafValidity)
	if err != nil {
		return fmt.Errorf("issuing the leaf certificate: %w", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.der})

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: leafSecretName}}
	_, err = controllerutil.CreateOrUpdate(ctx, c, secret, func() error {
		secret.Type = corev1.SecretTypeTLS
		secret.Data = map[string][]byte{
			KeyTLSCert: certPEM,
			KeyTLSKey:  keyPEM,
			KeyCACert:  caPEM,
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("writing %s: %w", leafKey, err)
	}
	return nil
}

// leafStillValid reports whether the existing leaf Secret needs no action: present, parseable,
// not within renewWithin of expiry, and its SAN still matches host.
func leafStillValid(s *corev1.Secret, host string) bool {
	raw := secretBytes(s, KeyTLSCert)
	if len(raw) == 0 {
		return false
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	if time.Until(cert.NotAfter) < renewWithin {
		return false
	}
	for _, san := range cert.DNSNames {
		if san == host {
			return true
		}
	}
	return false
}

func ensureCA(ctx context.Context, c client.Client, namespace, caSecretName string) (*caKeyPair, error) {
	key := types.NamespacedName{Namespace: namespace, Name: caSecretName}
	var existing corev1.Secret
	err := c.Get(ctx, key, &existing)
	if err == nil {
		if ca, ok := parseCA(&existing); ok {
			return ca, nil
		}
		return nil, fmt.Errorf("%s exists but doesn't hold a usable CA", key)
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}

	ca, err := generateCA()
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: caSecretName},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			KeyTLSCert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.der}),
			KeyTLSKey:  pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(ca.key)}),
		},
	}
	if err := c.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another replica won the race; adopt what it created.
			if err := c.Get(ctx, key, &existing); err != nil {
				return nil, fmt.Errorf("reading %s: %w", key, err)
			}
			if adopted, ok := parseCA(&existing); ok {
				return adopted, nil
			}
			return nil, fmt.Errorf("%s exists but doesn't hold a usable CA", key)
		}
		return nil, fmt.Errorf("creating %s: %w", key, err)
	}
	return ca, nil
}

func parseCA(s *corev1.Secret) (*caKeyPair, bool) {
	certPEM := secretBytes(s, KeyTLSCert)
	keyPEM := secretBytes(s, KeyTLSKey)
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, false
	}
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, false
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, false
	}
	priv, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, false
	}
	return &caKeyPair{cert: cert, key: priv, der: certBlock.Bytes}, true
}

func generateCA() (*caKeyPair, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generating CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Booth self-signed CA"},
		NotBefore:             now.Add(-time.Hour), // clock skew slack
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("creating CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parsing freshly-created CA certificate: %w", err)
	}
	return &caKeyPair{cert: cert, key: priv, der: der}, nil
}

func issueLeaf(ca *caKeyPair, host string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generating leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &priv.PublicKey, ca.key)
	if err != nil {
		return nil, nil, fmt.Errorf("creating leaf certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	return certPEM, keyPEM, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generating serial number: %w", err)
	}
	return serial, nil
}

// secretBytes reads a key from a Secret whether it arrived as Data (a real API server) or
// still sits in StringData (an object built in-process, as in tests).
func secretBytes(s *corev1.Secret, key string) []byte {
	if v, ok := s.Data[key]; ok {
		return v
	}
	if v, ok := s.StringData[key]; ok {
		return []byte(v)
	}
	return nil
}
