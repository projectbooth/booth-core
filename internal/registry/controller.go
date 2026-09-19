package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
)

// healthPollInterval is how often a healthy/unhealthy module is re-checked. This is
// implemented as a reconcile requeue rather than a separate goroutine/ticker, so it
// shares the controller's existing work queue, rate limiting, and shutdown handling.
const healthPollInterval = 15 * time.Second

// healthCheckTimeout bounds a single health-check HTTP call so one unresponsive module
// can't stall the controller's work queue.
const healthCheckTimeout = 5 * time.Second

// Controller reconciles booth-core's Registry from the cluster's live BoothModule
// resources (ADR 0019) — no push endpoint, no polling loop for manifest discovery
// itself, only for the health-check half of the job.
type Controller struct {
	client.Client
	Registry   *Registry
	HTTPClient *http.Client

	// HealthCheckURL builds the URL to poll for a module's health. Defaults to the
	// real in-cluster Service DNS name; overridable in tests to point at an
	// httptest server instead.
	HealthCheckURL func(spec boothv1alpha1.BoothModuleSpec) string

	// EventBus, if set, provisions each module's event-bus credentials from its manifest
	// on every reconcile (ADR 0049). Nil means event-bus auth is disabled.
	EventBus EventBusProvisioner

	// Database, if set, provisions each module's PostgreSQL database and credentials from
	// its manifest on every reconcile (ADR 0053). Nil means database provisioning is off.
	Database DatabaseProvisioner
}

// DatabaseProvisioner makes a module's database and credential Secret match its manifest.
// Implemented by dbprov.Provisioner.
type DatabaseProvisioner interface {
	Ensure(ctx context.Context, mod *boothv1alpha1.BoothModule) error
}

// EventBusProvisioner makes a module's event-bus credentials match its manifest.
// Implemented by natsauth.ModuleProvisioner; an interface here so this package doesn't
// depend on the NATS libraries.
type EventBusProvisioner interface {
	Ensure(ctx context.Context, mod *boothv1alpha1.BoothModule) error
}

// NewController wires a Controller with a sane default HTTP client and in-cluster
// health-check URL builder.
func NewController(c client.Client, reg *Registry) *Controller {
	return &Controller{
		Client:     c,
		Registry:   reg,
		HTTPClient: &http.Client{Timeout: healthCheckTimeout},
		HealthCheckURL: func(spec boothv1alpha1.BoothModuleSpec) string {
			return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d%s",
				spec.ServiceRef.Name, spec.ServiceRef.Namespace, spec.ServiceRef.Port, spec.HealthCheckPath)
		},
	}
}

// Reconcile implements the controller-runtime reconcile loop: on every BoothModule
// add/update/delete (and on the healthPollInterval requeue), it re-derives the
// Registry's entry and re-checks health.
func (c *Controller) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var mod boothv1alpha1.BoothModule
	if err := c.Get(ctx, req.NamespacedName, &mod); err != nil {
		if apierrors.IsNotFound(err) {
			// Uninstalled: the chart removed the BoothModule resource. Since ID is the
			// registry's key and req.Name is the Kubernetes object name (not
			// necessarily identical if a chart names its resource differently), we
			// can't recover the module ID from the request alone once the object is
			// gone. Charts are expected to name the BoothModule resource after the
			// module ID precisely so this cleanup path works.
			c.Registry.Delete(req.Name)
			logger.Info("module uninstalled, removed from registry", "module", req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching BoothModule %s: %w", req.NamespacedName, err)
	}

	status := c.checkHealth(ctx, mod.Spec)
	status.ObservedGeneration = mod.Generation

	c.Registry.Put(Module{Spec: mod.Spec, Status: status})

	if statusChanged(mod.Status, status) {
		mod.Status = status
		if err := c.Status().Update(ctx, &mod); err != nil {
			logger.Error(err, "updating BoothModule status", "module", mod.Spec.ID)
			// Don't fail reconciliation over a status-write conflict; the in-memory
			// registry (what the gateway/API actually read from) is already correct,
			// and the next poll will retry the status write.
		}
	}

	// Provision after the registry/health work so a provisioning problem never hides a
	// module's health. Errors are returned (joined, so one failing doesn't skip the other),
	// which requeues with backoff; the next attempt also re-runs the health check, so
	// polling continues while this is failing.
	var provisionErrs []error
	if c.EventBus != nil {
		if err := c.EventBus.Ensure(ctx, &mod); err != nil {
			provisionErrs = append(provisionErrs, fmt.Errorf("event-bus credentials: %w", err))
		}
	}
	if c.Database != nil {
		if err := c.Database.Ensure(ctx, &mod); err != nil {
			provisionErrs = append(provisionErrs, fmt.Errorf("database: %w", err))
		}
	}
	if err := errors.Join(provisionErrs...); err != nil {
		return ctrl.Result{}, fmt.Errorf("provisioning for module %q: %w", mod.Spec.ID, err)
	}

	return ctrl.Result{RequeueAfter: healthPollInterval}, nil
}

func (c *Controller) checkHealth(ctx context.Context, spec boothv1alpha1.BoothModuleSpec) boothv1alpha1.BoothModuleStatus {
	now := metav1.Now()

	url := c.HealthCheckURL(spec)

	reqCtx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return boothv1alpha1.BoothModuleStatus{
			Phase:               boothv1alpha1.ModulePhaseUnreachable,
			Message:             fmt.Sprintf("building health check request: %v", err),
			LastHealthCheckTime: &now,
		}
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return boothv1alpha1.BoothModuleStatus{
			Phase:               boothv1alpha1.ModulePhaseUnreachable,
			Message:             err.Error(),
			LastHealthCheckTime: &now,
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return boothv1alpha1.BoothModuleStatus{
			Phase:               boothv1alpha1.ModulePhaseHealthy,
			LastHealthCheckTime: &now,
		}
	}

	return boothv1alpha1.BoothModuleStatus{
		Phase:               boothv1alpha1.ModulePhaseUnhealthy,
		Message:             fmt.Sprintf("health check returned status %d", resp.StatusCode),
		LastHealthCheckTime: &now,
	}
}

// statusChanged compares everything except the timestamp, so a Status().Update isn't
// fired on every single poll when nothing actually changed — only the timestamp would
// differ on a steady-state healthy module, which isn't worth an API server write every
// 15s.
func statusChanged(old, new boothv1alpha1.BoothModuleStatus) bool {
	return old.Phase != new.Phase || old.Message != new.Message || old.ObservedGeneration != new.ObservedGeneration
}

// SetupWithManager registers this controller with a controller-runtime Manager, watching
// BoothModule resources cluster-wide.
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&boothv1alpha1.BoothModule{}).
		Complete(c)
}
