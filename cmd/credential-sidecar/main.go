// Command credential-sidecar implements ADR 0095 / contracts/credential-sidecar.md: a small,
// standalone binary that calls booth-core's credential broker (ADR 0080/0088) on a timer and
// exposes the result to a pod's main container as either a localhost Postgres wire-protocol proxy
// (--kind=postgres) or a refreshed AWS shared-credentials file (--kind=s3) — so native database/
// lakehouse access never requires that container's own code to know the credential broker exists.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/projectbooth/booth-core/internal/credentialbroker"
	"github.com/projectbooth/booth-core/internal/sidecar"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var (
		kind              = flag.String("kind", "", "credential kind to request (postgres, s3)")
		scope             = flag.String("scope", "", "raw JSON passed through to the broker's scope field")
		options           = flag.String("options", "", "raw JSON passed through to the broker's options field")
		access            = flag.String("access", credentialbroker.AccessReadWrite, "read or readwrite, forwarded to the broker")
		listen            = flag.String("listen", "127.0.0.1:5432", "postgres mode: address (or unix socket path prefixed unix://) the wire-protocol proxy binds")
		credentialsFile   = flag.String("credentials-file", "", "s3 mode: path the refreshed AWS shared-credentials-file is written to")
		healthListen      = flag.String("health-listen", "127.0.0.1:8080", "s3 mode: address the standalone /healthz listener binds (postgres mode serves /healthz on --listen itself)")
		profile           = flag.String("profile", "default", "s3 mode: the credentials-file section name")
		coreURL           = flag.String("core-url", os.Getenv("BOOTH_CORE_URL"), "booth-core base URL (env BOOTH_CORE_URL)")
		workspace         = flag.String("workspace", os.Getenv("BOOTH_WORKSPACE"), "the X-Workspace to request against (env BOOTH_WORKSPACE)")
		token             = flag.String("token", os.Getenv("BOOTH_TOKEN"), "bearer token to present to the broker (env BOOTH_TOKEN)")
		tokenFile         = flag.String("token-file", os.Getenv("BOOTH_TOKEN_FILE"), "path to re-read the bearer token from on every call, instead of --token (env BOOTH_TOKEN_FILE)")
		renewMarginSecs   = flag.Int("renew-margin-seconds", envIntOr("RENEW_MARGIN_SECONDS", sidecar.DefaultRenewMarginSeconds), "renew this many seconds before the lease's real expiresAt (env RENEW_MARGIN_SECONDS); unless set, postgres mode's default is half the lease's own real lifetime instead of a fixed margin (ADR 0095 fifth amendment) — s3 mode always uses this fixed margin")
		renewIntervalSecs = flag.Int("renew-interval-seconds", envIntOr("RENEW_INTERVAL_SECONDS", sidecar.DefaultRenewIntervalSeconds), "how often to check whether renewal is due (env RENEW_INTERVAL_SECONDS)")
	)
	flag.Parse()

	// "Explicitly given" covers both the flag and its env var — either means the operator made a
	// real choice that postgres mode's half-lifetime default (below) must not override.
	marginExplicit := os.Getenv("RENEW_MARGIN_SECONDS") != ""
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "renew-margin-seconds" {
			marginExplicit = true
		}
	})

	if err := validateFlags(*kind, *scope, *access, *credentialsFile); err != nil {
		return err
	}
	if *coreURL == "" {
		return errors.New("--core-url (or BOOTH_CORE_URL) is required")
	}
	if *workspace == "" {
		return errors.New("--workspace (or BOOTH_WORKSPACE) is required")
	}

	var tokenSource sidecar.TokenSource
	switch {
	case *tokenFile != "":
		tokenSource = sidecar.FileToken(*tokenFile)
	case *token != "":
		tokenSource = sidecar.StaticToken(*token)
	default:
		return errors.New("one of --token/BOOTH_TOKEN or --token-file/BOOTH_TOKEN_FILE is required")
	}

	var rawScope, rawOptions json.RawMessage
	if *scope != "" {
		rawScope = json.RawMessage(*scope)
	}
	if *options != "" {
		rawOptions = json.RawMessage(*options)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := sidecar.NewBrokerClient(*coreURL, *workspace, tokenSource)
	renewer := &sidecar.Renewer{
		Client: client,
		Request: credentialbroker.Request{
			Kind: *kind, Access: *access, Scope: rawScope, Options: rawOptions,
		},
		Margin:   secondsDuration(*renewMarginSecs),
		Interval: secondsDuration(*renewIntervalSecs),
	}

	switch *kind {
	case "postgres":
		if !marginExplicit {
			renewer.HalfLifetime = true
		}
		proxy := sidecar.NewPostgresProxy(renewer)
		network, addr := listenNetworkAddr(*listen)
		go func() {
			if err := renewer.Run(ctx); err != nil {
				log.Printf("sidecar: fatal: %v", err)
				stop()
				os.Exit(1)
			}
		}()
		log.Printf("credential-sidecar: postgres mode, listening on %s", *listen)
		return proxy.Listen(ctx, network, addr)

	case "s3":
		writer := sidecar.NewS3FileWriter(*credentialsFile, *profile)
		renewer.OnRenew = writer.OnRenew
		errCh := make(chan error, 2)
		go func() {
			errCh <- renewer.Run(ctx)
		}()
		go func() {
			log.Printf("credential-sidecar: s3 mode, healthz on %s, credentials file %s", *healthListen, *credentialsFile)
			errCh <- sidecar.ServeHealthz(ctx, *healthListen, renewer.Ready)
		}()
		return <-errCh

	default:
		return fmt.Errorf("unreachable: unknown kind %q", *kind)
	}
}

func validateFlags(kind, scope, access, credentialsFile string) error {
	switch kind {
	case "postgres":
	case "s3":
		if credentialsFile == "" {
			return errors.New("--credentials-file is required for --kind=s3")
		}
	case "":
		return errors.New("--kind is required (postgres or s3)")
	default:
		return fmt.Errorf("unknown --kind %q (want postgres or s3)", kind)
	}
	if scope == "" {
		return errors.New("--scope is required")
	}
	if access != credentialbroker.AccessRead && access != credentialbroker.AccessReadWrite {
		return fmt.Errorf("--access must be %q or %q, got %q", credentialbroker.AccessRead, credentialbroker.AccessReadWrite, access)
	}
	return nil
}

// listenNetworkAddr turns --listen's value into (network, address) for net.Listen: a
// "unix://<path>" prefix selects a Unix socket (contracts/credential-sidecar.md allows either);
// anything else is a TCP address.
func listenNetworkAddr(listen string) (network, addr string) {
	const unixPrefix = "unix://"
	if len(listen) > len(unixPrefix) && listen[:len(unixPrefix)] == unixPrefix {
		return "unix", listen[len(unixPrefix):]
	}
	return "tcp", listen
}

func envIntOr(name string, fallback int) int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func secondsDuration(n int) time.Duration { return time.Duration(n) * time.Second }
