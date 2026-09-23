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
	"github.com/projectbooth/booth-core/internal/dbprov"
	"github.com/projectbooth/booth-core/internal/devregistry"
	"github.com/projectbooth/booth-core/internal/directory"
	"github.com/projectbooth/booth-core/internal/eventbus"
	"github.com/projectbooth/booth-core/internal/gateway"
	"github.com/projectbooth/booth-core/internal/iframeidentity"
	"github.com/projectbooth/booth-core/internal/natsauth"
	"github.com/projectbooth/booth-core/internal/registry"
	"github.com/projectbooth/booth-core/internal/workload"
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

	// dbProv is non-nil only when database provisioning is on in a real cluster (ADR 0053);
	// core uses it for its own database too.
	var dbProv *dbprov.Provisioner

	// workloadKeys / workloadProvisioner are non-nil only when workload identity is on in a real
	// cluster (ADR 0056).
	var workloadKeys *workload.Keys
	var workloadProvisioner registry.WorkloadProvisioner

	// iframeIdentityKeys is non-nil only when the iframe-proxy identity-assertion issuer is
	// configured (ADR 0069) — persisted via a Secret in a real cluster, ephemeral in dev mode
	// (see the two bootstrap sites below), same trade-off as iframeSigningSecret's dev fallback.
	var iframeIdentityKeys *iframeidentity.Keys

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

		if cfg.IframeIdentity.IssuerURL != "" {
			// No Kubernetes Secret to persist to in dev mode; an ephemeral per-process key is
			// fine here the same way iframeSigningSecret's dev fallback is — losing it on
			// restart just means every existing iframe-proxy assertion stops verifying, and a
			// fresh one is minted on the very next proxied request.
			iframeIdentityKeys, err = iframeidentity.NewKeys()
			if err != nil {
				return fmt.Errorf("preparing iframe-identity keys: %w", err)
			}
			log.Printf("dev mode: iframe-proxy identity issuer enabled with an ephemeral key (issuer %s)", cfg.IframeIdentity.IssuerURL)
		}
	} else {
		scheme, err := newScheme()
		if err != nil {
			return err
		}

		// A direct (uncached) client for provisioning: the manager's cache would otherwise
		// hold every Secret in the cluster in memory just to read a few of ours.
		var direct client.Client
		if cfg.EventBusAuth || cfg.Postgres.Host != "" || cfg.Workload.IssuerURL != "" || cfg.IframeIdentity.IssuerURL != "" {
			direct, err = client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
			if err != nil {
				return fmt.Errorf("creating Kubernetes client for provisioning: %w", err)
			}
		}

		var busProvisioner registry.EventBusProvisioner
		if cfg.EventBusAuth {
			// Must complete before anything waits on NATS: the NATS pod can't start until
			// the ConfigMap this writes exists. Fatal on failure — running with the bus
			// authentication the operator asked for silently absent would be worse.
			authority, err = natsauth.LoadOrCreate(ctx, direct, cfg.KubeNamespace)
			if err != nil {
				return fmt.Errorf("bootstrapping event-bus authentication: %w", err)
			}
			busProvisioner = natsauth.NewModuleProvisioner(direct, authority, cfg.NATSModuleURL)
			log.Printf("event-bus authentication enabled (account %s)", authority.AccountPublicKey())
		}

		// Database provisioning (ADR 0053). Same ordering rule as the bus: for the bundled
		// server the admin-password Secret must exist before its pod can start, so this
		// runs before anything waits on Postgres — and construction never contacts it.
		var dbProvisioner registry.DatabaseProvisioner
		if cfg.Postgres.Host != "" {
			pw := cfg.Postgres.AdminPassword
			if cfg.Postgres.Bundled {
				pw, err = dbprov.EnsureAdminPassword(ctx, direct, cfg.KubeNamespace)
				if err != nil {
					return fmt.Errorf("bootstrapping the bundled PostgreSQL admin credential: %w", err)
				}
			}
			if bc := cfg.Postgres.BackupClaim; cfg.Postgres.Bundled && bc.Name != "" {
				if err := dbprov.EnsureBackupClaim(ctx, direct, cfg.KubeNamespace, bc.Name, bc.Size, bc.StorageClass); err != nil {
					return fmt.Errorf("preparing the PostgreSQL backup volume: %w", err)
				}
			}
			admin, err := dbprov.NewAdmin(ctx, dbprov.Config{
				Host: cfg.Postgres.Host, Port: cfg.Postgres.Port, ModuleHost: cfg.Postgres.ModuleHost,
				AdminUser: cfg.Postgres.AdminUser, AdminPassword: pw, AdminDatabase: cfg.Postgres.AdminDatabase,
				SSLMode: cfg.Postgres.SSLMode, RestrictMaintenanceAccess: cfg.Postgres.RestrictMaintenanceAccess,
			})
			if err != nil {
				return fmt.Errorf("configuring database provisioning: %w", err)
			}
			defer admin.Close()
			for _, w := range cfg.Postgres.StartupWarnings() {
				log.Print("WARNING: " + w)
			}
			dbProv = dbprov.NewProvisioner(direct, admin)
			dbProvisioner = dbProv
			log.Printf("database provisioning enabled (server %s:%d, bundled=%v)", cfg.Postgres.Host, cfg.Postgres.Port, cfg.Postgres.Bundled)
		} else {
			log.Print("BOOTH_POSTGRES_HOST is not set; module databases will not be provisioned (ADR 0053)")
		}

		// Workload identity (ADR 0056): core's own signing key, and per-module minting credentials.
		if cfg.Workload.IssuerURL != "" {
			workloadKeys, err = workload.LoadOrCreateKeys(ctx, direct, cfg.KubeNamespace)
			if err != nil {
				return fmt.Errorf("preparing workload identity keys: %w", err)
			}
			workloadProvisioner = workload.NewModuleProvisioner(direct, workloadKeys, cfg.Workload.IssuerURL)
			log.Printf("workload identity enabled (issuer %s, key %s)", cfg.Workload.IssuerURL, workloadKeys.KeyID())
		} else {
			log.Print("BOOTH_WORKLOAD_ISSUER_URL is not set; workload identity is off and no module will receive a minting credential (ADR 0056)")
		}

		// Iframe-proxy identity issuer (ADR 0069): its own signing key, no per-module Secret to
		// provision — every iframe-proxy module trusts it purely via the public discovery
		// document/JWKS, the same way it trusts the deployment's OIDC provider.
		if cfg.IframeIdentity.IssuerURL != "" {
			iframeIdentityKeys, err = iframeidentity.LoadOrCreateKeys(ctx, direct, cfg.KubeNamespace)
			if err != nil {
				return fmt.Errorf("preparing iframe-identity keys: %w", err)
			}
			log.Printf("iframe-proxy identity issuer enabled (issuer %s, key %s)", cfg.IframeIdentity.IssuerURL, iframeIdentityKeys.KeyID())
		} else {
			log.Print("BOOTH_IFRAME_IDENTITY_ISSUER_URL is not set; the iframe-proxy path will not carry a signed identity assertion (ADR 0069)")
		}

		mgr, err := startRegistryController(scheme, reg, busProvisioner, dbProvisioner, workloadProvisioner)
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
	iframeURLs := gateway.NewIframeURLIssuer(iframeTokens)

	gw := gateway.New(reg)

	// User directory (ADR 0047). It always starts serving from memory, and switches to
	// PostgreSQL as soon as a database is available: immediately for an explicit DSN, or once
	// core has provisioned its own database on the shared server (ADR 0053), which can take
	// a while on a fresh install. Memory loses entries on restart but repopulates itself as
	// users make authenticated requests, so it degrades rather than breaks.
	users := directory.NewSwitchable(directory.NewMemoryStore())
	recorder := directory.NewRecorder(users)
	switch {
	case cfg.PostgresDSN != "":
		pg, err := directory.NewPostgresStore(ctx, cfg.PostgresDSN)
		if err != nil {
			return fmt.Errorf("configuring user directory: %w", err)
		}
		defer pg.Close()
		users.Swap(pg)
		log.Print("user directory: using the database from BOOTH_POSTGRES_DSN")
	case dbProv != nil:
		go persistDirectory(ctx, dbProv, cfg.KubeNamespace, users, recorder)
	default:
		log.Print("no database is configured (BOOTH_POSTGRES_DSN / BOOTH_POSTGRES_HOST); the user directory is " +
			"in-memory and will be empty after a restart until users make authenticated requests again (ADR 0047)")
	}

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

	var workloadSvc *workload.Service
	if workloadKeys != nil {
		workloadSvc = workload.NewService(workloadKeys, reg, users, workload.Options{
			Issuer:      cfg.Workload.IssuerURL,
			Audience:    cfg.OIDC.ClientID,
			GroupsClaim: cfg.OIDC.GroupsClaim,
			MaxOwnerAge: cfg.Workload.OwnerMaxAge,
		})
		if cfg.PostgresDSN == "" && dbProv == nil {
			log.Print("WARNING: workload identity is on but the user directory is in-memory: after a core restart, " +
				"no run can be minted a token until its owner has made an authenticated request again (ADR 0056)")
		}
	}

	var iframeIdentitySvc *iframeidentity.Service
	if iframeIdentityKeys != nil {
		iframeIdentitySvc = iframeidentity.NewService(iframeIdentityKeys, iframeidentity.Options{
			Issuer:      cfg.IframeIdentity.IssuerURL,
			GroupsClaim: cfg.OIDC.GroupsClaim,
		})
		// Deliberately not `gw.IframeIdentity = iframeIdentitySvc` unconditionally: assigning a
		// nil *iframeidentity.Service to the IframeIdentityMinter interface field produces a
		// non-nil interface holding a nil pointer, which g.IframeIdentity != nil in
		// proxyIframeRequest would then treat as configured and panic on Mint. Guarding on
		// iframeIdentityKeys != nil here keeps the field genuinely nil when the issuer is off.
		gw.IframeIdentity = iframeIdentitySvc
	}

	router := api.NewRouter(api.Deps{
		Verifier:     verifierHolder,
		Registry:     reg,
		Gateway:      gw,
		IframeTokens: iframeTokens,
		IframeURLs:   iframeURLs,

		Directory:         users,
		DirectoryRecorder: recorder,
		Workload:          workloadSvc,
		IframeIdentity:    iframeIdentitySvc,
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

// persistDirectory provisions core's own database and switches the user directory onto it,
// retrying with backoff until the server is reachable (the bundled one may still be starting).
func persistDirectory(ctx context.Context, p *dbprov.Provisioner, namespace string, users *directory.Switchable, recorder *directory.Recorder) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for {
		dsn, err := p.EnsureCore(ctx, namespace)
		if err == nil {
			var pg *directory.PostgresStore
			if pg, err = directory.NewPostgresStore(ctx, dsn); err == nil {
				users.Swap(pg)
				recorder.Reset() // the new store is empty; re-record users on their next request
				log.Print("user directory is now persistent (core database provisioned)")
				<-ctx.Done()
				pg.Close()
				return
			}
		}
		log.Printf("core database not ready yet (%v); retrying in %s", err, backoff)
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

func startRegistryController(scheme *runtime.Scheme, reg *registry.Registry, eventBus registry.EventBusProvisioner, database registry.DatabaseProvisioner, workloadIdentity registry.WorkloadProvisioner) (ctrl.Manager, error) {
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
	controller.Database = database
	controller.Workload = workloadIdentity
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

const shutdownTimeout = 10 * time.Second
