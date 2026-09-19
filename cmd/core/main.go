// Command core is booth-core's entrypoint: the mandatory control plane binary running
// auth, the module registry controller, the gateway, and event bus wiring in one process.
package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/api"
	"github.com/projectbooth/booth-core/internal/auth"
	"github.com/projectbooth/booth-core/internal/config"
	"github.com/projectbooth/booth-core/internal/devregistry"
	"github.com/projectbooth/booth-core/internal/eventbus"
	"github.com/projectbooth/booth-core/internal/gateway"
	"github.com/projectbooth/booth-core/internal/natsauth"
	"github.com/projectbooth/booth-core/internal/registry"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	reg := registry.New()

	// authority is non-nil only when event-bus authentication is on and we're running in a
	// real cluster (ADR 0049); it's used both to mint core's own bus credential and, via
	// the registry controller, each module's.
	var authority *natsauth.Authority

	// Module discovery: real BoothModule CRD watch in a real cluster (ADR 0019), or a
	// static file for local development without one (ADR 0019's noted convenience,
	// agent-briefs/core.md's third open question).
	if cfg.DevRegistryPath != "" {
		modules, err := devregistry.Load(cfg.DevRegistryPath)
		if err != nil {
			return fmt.Errorf("loading dev registry: %w", err)
		}
		for _, m := range modules {
			reg.Put(m)
		}
		log.Printf("dev mode: loaded %d module(s) from %s (no CRD watch)", len(modules), cfg.DevRegistryPath)
	} else {
		scheme, err := newScheme()
		if err != nil {
			return err
		}

		var provisioner registry.EventBusProvisioner
		if cfg.EventBusAuth {
			// A direct (uncached) client: the manager's cache would otherwise hold every
			// Secret in the cluster in memory just to read a few of ours.
			direct, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
			if err != nil {
				return fmt.Errorf("creating Kubernetes client for event-bus auth: %w", err)
			}
			// Must complete before anything waits on NATS: the NATS pod can't start until
			// the ConfigMap this writes exists. Fatal on failure — running with the bus
			// authentication the operator asked for silently absent would be worse.
			authority, err = natsauth.LoadOrCreate(ctx, direct, cfg.KubeNamespace)
			if err != nil {
				return fmt.Errorf("bootstrapping event-bus authentication: %w", err)
			}
			provisioner = natsauth.NewModuleProvisioner(direct, authority, cfg.NATSModuleURL)
			log.Printf("event-bus authentication enabled (account %s)", authority.AccountPublicKey())
		}

		mgr, err := startRegistryController(scheme, reg, provisioner)
		if err != nil {
			return fmt.Errorf("starting registry controller: %w", err)
		}
		go func() {
			if err := mgr.Start(ctx); err != nil {
				log.Printf("registry controller stopped: %v", err)
			}
		}()
	}

	// The OIDC provider (e.g. Keycloak) may not be reachable the instant core boots —
	// nothing in the Helm chart guarantees ordering between them, especially on a fresh
	// cluster bring-up. Rather than crash-looping until it happens to be up, retry in
	// the background and let auth.Middleware serve 503s on auth-gated routes until a
	// verifier is ready; /healthz and unauthenticated routes work immediately either way.
	verifierHolder := &auth.VerifierHolder{}
	go initVerifierWithRetry(ctx, cfg.OIDC, verifierHolder)

	iframeSecret, err := iframeSigningSecret()
	if err != nil {
		return fmt.Errorf("preparing iframe token secret: %w", err)
	}
	iframeTokens := gateway.NewIframeTokenIssuer(iframeSecret)
	iframeURLs := gateway.NewIframeURLIssuer(iframeTokens, publicBaseURL())

	gw := gateway.New(reg)

	// Event bus: NATS/JetStream (ADR 0021). Connected in the background: NATS may not be
	// up yet (with auth on, its pod is waiting on the ConfigMap core just wrote), and a
	// developer running only the HTTP surface locally shouldn't need NATS at all.
	var busOpts []nats.Option
	if authority != nil {
		busOpts = append(busOpts, authority.ConnectOption("booth-core", natsauth.CoreGrants()))
	} else {
		log.Print("WARNING: event-bus authentication is OFF (BOOTH_EVENTBUS_AUTH is not true or this is dev mode); " +
			"any pod that can reach NATS can publish any event (ADR 0049)")
	}
	go maintainBus(ctx, cfg.NATSURL, busOpts)

	router := api.NewRouter(api.Deps{
		Verifier:     verifierHolder,
		Registry:     reg,
		Gateway:      gw,
		IframeTokens: iframeTokens,
		IframeURLs:   iframeURLs,
	})

	server := &http.Server{Addr: cfg.HTTPAddr, Handler: router}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("booth-core listening on %s", cfg.HTTPAddr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// initVerifierWithRetry retries auth.NewVerifier with backoff until it succeeds or ctx
// is canceled, storing the result in holder once ready. If no issuer URL is configured
// at all (the local-dev-without-OIDC case config.Load() already permits), it logs once
// and returns rather than retrying forever against an empty URL.
func initVerifierWithRetry(ctx context.Context, cfg config.OIDCConfig, holder *auth.VerifierHolder) {
	if cfg.IssuerURL == "" {
		log.Print("no BOOTH_OIDC_ISSUER_URL configured; /api routes requiring auth will always 503")
		return
	}

	const maxBackoff = 30 * time.Second
	backoff := time.Second

	for {
		verifier, err := auth.NewVerifier(ctx, cfg)
		if err == nil {
			holder.Store(verifier)
			log.Print("OIDC verifier ready")
			return
		}

		log.Printf("OIDC verifier not ready yet (%v); retrying in %s", err, backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// maintainBus connects to the event bus, retrying with backoff until it succeeds, then
// holds the connection until ctx is canceled.
func maintainBus(ctx context.Context, url string, opts []nats.Option) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for {
		bus, err := eventbus.Connect(ctx, url, opts...)
		if err == nil {
			log.Print("event bus connected")
			<-ctx.Done()
			bus.Close()
			return
		}
		log.Printf("event bus not ready yet (%v); retrying in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func newScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := boothv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return scheme, nil
}

func startRegistryController(scheme *runtime.Scheme, reg *registry.Registry, eventBus registry.EventBusProvisioner) (ctrl.Manager, error) {
	// Metrics and health-probe servers are both disabled: controller-runtime's
	// manager defaults its metrics server to :8080, which collides with booth-core's
	// own HTTP server in this same process — this bit us for real (see git history),
	// not a hypothetical. Nothing currently scrapes controller-runtime's own metrics
	// endpoint separately from booth-core's own /healthz.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		return nil, fmt.Errorf("creating controller manager: %w", err)
	}

	controller := registry.NewController(mgr.GetClient(), reg)
	controller.EventBus = eventBus
	if err := controller.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("setting up registry controller: %w", err)
	}

	return mgr, nil
}

// iframeSigningSecret loads the HMAC key for gateway.IframeTokenIssuer from the
// standard env var a real deployment's Helm-provisioned Secret populates (ADR 0020);
// falls back to a random per-process key for local development, where losing sessions
// on restart is a non-issue.
func iframeSigningSecret() ([]byte, error) {
	if v := os.Getenv("BOOTH_IFRAME_SIGNING_KEY"); v != "" {
		return []byte(v), nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	log.Print("BOOTH_IFRAME_SIGNING_KEY not set; using an ephemeral key (fine for local dev, not for a real deployment with more than one replica)")
	return key, nil
}

func publicBaseURL() string {
	if v := os.Getenv("BOOTH_PUBLIC_BASE_URL"); v != "" {
		return v
	}
	return "http://localhost:8080"
}

const shutdownTimeout = 10 * time.Second
