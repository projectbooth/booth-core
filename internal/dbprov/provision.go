package dbprov

import (
	"context"
	"fmt"
	"strconv"
	"sync"
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
	// CredentialsSecretName is the Secret written into a module's namespace (ADR 0020: the
	// module reads provisioned config the ordinary Kubernetes way, never via a live API).
	// Proposed convention, pending an architecture ADR — see docs/decisions/0008.
	CredentialsSecretName = "booth-database-credentials"

	// CoreCredentialsSecretName holds core's own database credentials, in core's namespace.
	CoreCredentialsSecretName = "booth-core-database"

	// AdminSecretName holds the bundled PostgreSQL server's superuser password; core creates
	// it (see EnsureAdminPassword) and the bundled StatefulSet reads it.
	AdminSecretName = "booth-postgres-admin"
	AdminSecretKey  = "password"

	// ClientNamespaceLabel marks a namespace as hosting database clients; the bundled
	// PostgreSQL NetworkPolicy admits traffic only from namespaces carrying it.
	ClientNamespaceLabel = "booth.projectbooth.io/database-client"

	databaseAnnotation = "booth.projectbooth.io/database"

	// recheckInterval bounds how often an already-provisioned database is re-verified
	// against the server. Reconcile runs every few seconds per module; the server check is
	// only to self-heal a server that lost its data, so it doesn't need to be that eager.
	recheckInterval = 5 * time.Minute
)

// Secret keys. `dsn` is the one most modules want; the parts are there for those that build
// their own connection (or need to change sslmode/options).
const (
	KeyDSN      = "dsn"
	KeyHost     = "host"
	KeyPort     = "port"
	KeyDatabase = "database"
	KeyUsername = "username"
	KeyPassword = "password"
)

// Provisioner delivers each module's database credentials (ADR 0053).
type Provisioner struct {
	Client client.Client
	Admin  *Admin
	Now    func() time.Time

	mu       sync.Mutex
	verified map[string]time.Time
}

// NewProvisioner builds a Provisioner.
func NewProvisioner(c client.Client, a *Admin) *Provisioner {
	return &Provisioner{Client: c, Admin: a, Now: time.Now, verified: map[string]time.Time{}}
}

// Ensure makes a module's database and credential Secret match its manifest: if it signals
// `database.enabled`, it gets a database, a role, and a Secret; if it doesn't (or stopped),
// a Secret this package wrote is removed — but the database and role are left in place, so
// re-enabling or reinstalling reconnects to the same data.
func (p *Provisioner) Ensure(ctx context.Context, mod *boothv1alpha1.BoothModule) error {
	ns := mod.Spec.ServiceRef.Namespace
	if ns == "" {
		ns = mod.Namespace
	}
	key := types.NamespacedName{Namespace: ns, Name: CredentialsSecretName}

	if mod.Spec.Database == nil || !mod.Spec.Database.Enabled {
		return p.removeOurs(ctx, key)
	}

	name, err := DatabaseName(mod.Spec.ID)
	if err != nil {
		return fmt.Errorf("module %q: %w", mod.Spec.ID, err)
	}

	// ownerReferences can't cross namespaces; own the Secret (so it's garbage-collected on
	// uninstall) only when it lives beside the BoothModule.
	var owner *metav1.OwnerReference
	if ns == mod.Namespace {
		owner = &metav1.OwnerReference{
			APIVersion: boothv1alpha1.GroupVersion.String(),
			Kind:       "BoothModule",
			Name:       mod.Name,
			UID:        mod.UID,
		}
	}

	if _, err := p.ensure(ctx, key, name, owner); err != nil {
		return fmt.Errorf("provisioning database for module %q: %w", mod.Spec.ID, err)
	}
	return p.labelNamespace(ctx, ns)
}

// EnsureCore provisions booth-core's own database and returns its DSN. It's the same
// mechanism modules use, so core's user directory (ADR 0047) gets real persistence on a
// default install without an operator supplying a DSN.
func (p *Provisioner) EnsureCore(ctx context.Context, namespace string) (string, error) {
	key := types.NamespacedName{Namespace: namespace, Name: CoreCredentialsSecretName}
	return p.ensure(ctx, key, CoreDatabaseName, nil)
}

// ensure provisions database `name` and returns its DSN, keeping the credential Secret at
// key. The Secret is the source of truth for the password: an existing one is honoured (so
// running consumers keep working), and only a missing one triggers a new password.
func (p *Provisioner) ensure(ctx context.Context, key types.NamespacedName, name string, owner *metav1.OwnerReference) (string, error) {
	var existing corev1.Secret
	err := p.Client.Get(ctx, key, &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("reading %s: %w", key, err)
	}
	found := err == nil

	password := ""
	if found {
		password = secretString(&existing, KeyPassword)
	}

	if password == "" {
		// No usable credential yet: mint one. Set on the server *before* it's published, so a
		// failure in between leaves a role whose password nobody has been handed — the next
		// attempt simply sets a new one.
		password, err = GeneratePassword()
		if err != nil {
			return "", err
		}
		if err := p.Admin.Ensure(ctx, name, password); err != nil {
			return "", err
		}
		p.markVerified(name)
	} else if !p.recentlyVerified(name) {
		// A credential exists. Confirm the server still has the role and database — it may
		// have been restored from an older backup or recreated — and if not, rebuild them
		// with the *existing* password so running consumers don't need new credentials.
		ok, err := p.Admin.Exists(ctx, name)
		if err != nil {
			return "", err
		}
		if !ok {
			if err := p.Admin.Ensure(ctx, name, password); err != nil {
				return "", err
			}
		}
		p.markVerified(name)
	}

	dsn := p.Admin.DSN(name, password)
	if found && secretString(&existing, KeyDSN) == dsn && existing.Annotations[databaseAnnotation] == name {
		return dsn, nil
	}

	host, port := p.Admin.Endpoint()
	err = secrets.NewProvisioner(p.Client).ProvisionSecretWith(ctx, key.Namespace, key.Name,
		map[string]string{
			KeyDSN:      dsn,
			KeyHost:     host,
			KeyPort:     strconv.Itoa(port),
			KeyDatabase: name,
			KeyUsername: name,
			KeyPassword: password,
		},
		map[string]string{databaseAnnotation: name},
		owner,
	)
	if err != nil {
		return "", err
	}
	return dsn, nil
}

// removeOurs deletes the credential Secret if this package wrote it. The database and role
// are intentionally left alone.
func (p *Provisioner) removeOurs(ctx context.Context, key types.NamespacedName) error {
	var s corev1.Secret
	if err := p.Client.Get(ctx, key, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", key, err)
	}
	if s.Annotations[databaseAnnotation] == "" {
		return nil // not ours
	}
	if err := p.Client.Delete(ctx, &s); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("removing %s: %w", key, err)
	}
	return nil
}

func (p *Provisioner) recentlyVerified(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	at, ok := p.verified[name]
	return ok && p.Now().Sub(at) < recheckInterval
}

func (p *Provisioner) markVerified(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verified[name] = p.Now()
}

// labelNamespace marks the module's namespace as a database client so the bundled
// PostgreSQL NetworkPolicy admits it. Labels are never removed (another module may share
// the namespace, and a stale label only widens reachability to a namespace that already
// holds credentials).
func (p *Provisioner) labelNamespace(ctx context.Context, ns string) error {
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

// secretString reads a key from a Secret whether it arrived as Data (a real API server) or
// still sits in StringData (an object built in-process, as in tests).
func secretString(s *corev1.Secret, key string) string {
	if v, ok := s.Data[key]; ok {
		return string(v)
	}
	return s.StringData[key]
}

// EnsureAdminPassword returns the bundled PostgreSQL server's superuser password,
// generating and storing it on first run. The bundled StatefulSet reads the same Secret, so
// the server's first initialisation and core agree. Like the NATS keys, it's create-if-absent
// and safe under several core replicas. Deleting the Secret after the server has initialised
// its data directory leaves core unable to authenticate (the server keeps the old password).
func EnsureAdminPassword(ctx context.Context, c client.Client, namespace string) (string, error) {
	key := types.NamespacedName{Namespace: namespace, Name: AdminSecretName}

	var s corev1.Secret
	err := c.Get(ctx, key, &s)
	if err == nil {
		if pw := secretString(&s, AdminSecretKey); pw != "" {
			return pw, nil
		}
		return "", fmt.Errorf("%s exists but has no %q key", key, AdminSecretKey)
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("reading %s: %w", key, err)
	}

	pw, err := GeneratePassword()
	if err != nil {
		return "", err
	}
	created := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: AdminSecretName},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{AdminSecretKey: []byte(pw)},
	}
	if err := c.Create(ctx, created); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another replica won the race; adopt its password.
			if err := c.Get(ctx, key, &s); err != nil {
				return "", fmt.Errorf("reading %s: %w", key, err)
			}
			return secretString(&s, AdminSecretKey), nil
		}
		return "", fmt.Errorf("creating %s: %w", key, err)
	}
	return pw, nil
}
