package sidecar

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/projectbooth/booth-core/internal/credentialbroker"
)

// DefaultRenewMarginSeconds / DefaultRenewIntervalSeconds match the shape booth-lakehouse's own
// warehouse-credential loop already established (RENEW_MARGIN_SECONDS/RENEW_INTERVAL_SECONDS,
// ADR 0084) rather than inventing new names for the same idea (contracts/credential-sidecar.md).
const (
	DefaultRenewMarginSeconds   = 60
	DefaultRenewIntervalSeconds = 30
)

// Renewer runs the broker call on a timer and hands every successful lease (the first one, and
// every renewal) to OnRenew. It never calls OnRenew with a credential it didn't itself verify
// came from a genuine 2xx broker response — see BrokerClient.Issue.
type Renewer struct {
	Client   *BrokerClient
	Request  credentialbroker.Request
	Margin   time.Duration
	Interval time.Duration

	// OnRenew is called with each new lease, in order, from the single goroutine Run runs in —
	// never concurrently, so a mode (postgres/s3) implementing it doesn't need its own locking
	// around whatever it swaps in.
	OnRenew func(credentialbroker.Response)

	// Now is the clock; overridden in tests. Defaults to time.Now.
	Now func() time.Time

	ready   atomic.Bool
	current atomic.Pointer[credentialbroker.Response]
}

// Ready reports whether at least one lease has ever been obtained — contracts/
// credential-sidecar.md's GET /healthz: "200 once at least one successful lease has been
// obtained, 503 otherwise." Never goes back to false once true, even if every subsequent renewal
// fails and the lease eventually expires — an expired-but-once-valid lease is a different problem
// (the proxy/file layer's own job to handle) from "has this process ever worked at all."
func (r *Renewer) Ready() bool { return r.ready.Load() }

// Current returns the most recently obtained lease, or nil if none yet.
func (r *Renewer) Current() *credentialbroker.Response { return r.current.Load() }

// Run drives the renewal loop until ctx is canceled.
//
// The first lease is special: a RefusalError (the broker will never answer this exact request
// differently — bad scope, bad kind, the caller's role doesn't permit the requested access, no
// provider registered for the kind) is fatal and returned to the caller, which is expected to log
// it and exit non-zero (contracts/credential-sidecar.md's "Failure behavior": "a restart without
// fixing the underlying cause will fail identically, so a crash-loop here is a visible signal, not
// something to retry silently forever"). Any other failure at startup (network, 5xx, a timeout) is
// an outage, retried with backoff — nothing has ever been issued yet, so there is no connection to
// protect, but there's also no reason to give up on a provider that's merely still starting up.
//
// Once at least one lease has been obtained, EVERY subsequent failure — a refusal or an outage —
// is logged and retried on the next poll interval, full stop, never fatal and never dropping the
// current lease: contracts/credential-sidecar.md's renewal section describes exactly this for both
// modes ("does not drop the existing upstream connection... retries on the next poll interval")
// without carving out an exception for a refusal found mid-session (e.g. a role revoked, or a
// module briefly unregistered during a deploy) — a caller losing privilege mid-renewal should see
// new connections refused once its lease actually expires, not have its current, still-valid work
// yanked out from under it the instant one renewal attempt comes back refused.
func (r *Renewer) Run(ctx context.Context) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	if err := r.obtainFirst(ctx); err != nil {
		return err
	}

	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.maybeRenew(ctx)
		}
	}
}

func (r *Renewer) obtainFirst(ctx context.Context) error {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for {
		resp, err := r.Client.Issue(ctx, r.Request)
		if err == nil {
			r.apply(resp)
			return nil
		}
		var refusal *RefusalError
		if errors.As(err, &refusal) {
			return fmt.Errorf("could not obtain an initial %s-kind lease: %w", r.Request.Kind, err)
		}
		log.Printf("sidecar: initial lease attempt failed (%v); retrying in %s", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// maybeRenew checks the current lease against Margin and, if it's due, attempts a renewal. A
// failure here is always non-fatal — see Run's doc comment for why.
func (r *Renewer) maybeRenew(ctx context.Context) {
	cur := r.current.Load()
	if cur != nil && r.Now().Add(r.Margin).Before(cur.ExpiresAt) {
		return // not due yet
	}
	resp, err := r.Client.Issue(ctx, r.Request)
	if err != nil {
		log.Printf("sidecar: renewal failed (keeping the current lease; will retry in %s): %v", r.Interval, err)
		return
	}
	r.apply(resp)
}

func (r *Renewer) apply(resp credentialbroker.Response) {
	r.current.Store(&resp)
	r.ready.Store(true)
	if r.OnRenew != nil {
		r.OnRenew(resp)
	}
}
