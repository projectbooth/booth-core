// Package keycloakprov provisions the bundled Keycloak's own bootstrap admin credential
// (ADR 0106/0108) — the generated-Secret mechanism ADR 0106 item 3 requires ("never a chart
// default"). This is a separate concern from internal/dbprov's EnsureKeycloak (which
// provisions Keycloak's database on the shared PostgreSQL server): that is a *database*
// credential; this is Keycloak's own application-level bootstrap admin, unrelated to Postgres.
package keycloakprov

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/projectbooth/booth-core/internal/dbprov"
)

const (
	// AdminSecretName holds the bundled Keycloak's bootstrap admin credential; core creates
	// it (see EnsureAdminPassword) and the bundled Keycloak Deployment reads it via
	// KC_BOOTSTRAP_ADMIN_USERNAME/KC_BOOTSTRAP_ADMIN_PASSWORD. Keycloak 26's bootstrap admin
	// is itself temporary (ADR 0106 condition 5) — this Secret is how an operator retrieves
	// the password to create a permanent one, not a credential core itself ever uses again.
	AdminSecretName  = "booth-keycloak-admin"
	AdminUsernameKey = "username"
	AdminPasswordKey = "password"

	// AdminUsername is fixed, not operator-configurable — there is nothing to disambiguate
	// per deployment, and a fixed, documented value is what the operations runbook's
	// port-forward instructions name directly.
	AdminUsername = "admin"
)

// EnsureAdminPassword returns the bundled Keycloak's bootstrap admin password, generating
// and storing it (alongside the fixed username) on first run. Mirrors
// dbprov.EnsureAdminPassword exactly: create-if-absent, safe under several core replicas,
// and an existing Secret is never overwritten — Keycloak's own data directory, once
// initialised, keeps the original password regardless of what this Secret is later set to,
// so this must never regenerate on top of a Keycloak that has already booted once.
func EnsureAdminPassword(ctx context.Context, c client.Client, namespace string) (string, error) {
	key := types.NamespacedName{Namespace: namespace, Name: AdminSecretName}

	var s corev1.Secret
	err := c.Get(ctx, key, &s)
	if err == nil {
		if pw := secretString(&s, AdminPasswordKey); pw != "" {
			return pw, nil
		}
		return "", fmt.Errorf("%s exists but has no %q key", key, AdminPasswordKey)
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("reading %s: %w", key, err)
	}

	pw, err := dbprov.GeneratePassword()
	if err != nil {
		return "", err
	}
	created := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: AdminSecretName},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			AdminUsernameKey: []byte(AdminUsername),
			AdminPasswordKey: []byte(pw),
		},
	}
	if err := c.Create(ctx, created); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another replica won the race; adopt its password.
			if err := c.Get(ctx, key, &s); err != nil {
				return "", fmt.Errorf("reading %s: %w", key, err)
			}
			return secretString(&s, AdminPasswordKey), nil
		}
		return "", fmt.Errorf("creating %s: %w", key, err)
	}
	return pw, nil
}

// secretString reads a key from a Secret whether it arrived as Data (a real API server) or
// still sits in StringData (an object built in-process, as in tests) — mirrors dbprov's own
// helper of the same name/shape.
func secretString(s *corev1.Secret, key string) string {
	if v, ok := s.Data[key]; ok {
		return string(v)
	}
	return s.StringData[key]
}
