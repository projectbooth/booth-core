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

	// HalfLifetime, when true, overrides Margin: a lease is renewed once half of its own real
	// lifetime (ExpiresAt minus the time this Renewer locally observed obtaining it) has elapsed,
	// instead of a fixed margin before expiry. Set by postgres mode's wiring (cmd/credential-
	// sidecar) when --renew-margin-seconds/RENEW_MARGIN_SECONDS was not explicitly given (ADR
	// 0095 fifth amendment, 2026-10-06, docs/decisions/0015): the sidecar's default 60-second
	// fixed margin meant a client connection opened just before a renewal swap was only
	// guaranteed to outlive that swap by about a minute, not the lease's nominal hour. s3 mode
	// never sets this — there is no long-lived connection to protect, only a request in flight,
	// so the fixed default margin stays.
	HalfLifetime bool

	// OnRenew is called with each new lease, in order, from the single goroutine Run runs in —
	// never concurrently, so a mode (postgres/s3) implementing it doesn't need its own locking
	// around whatever it swaps in.
	OnRenew func(credentialbroker.Response)

	// Now is the clock; overridden in tests. Defaults to time.Now.
	Now func() time.Time

	ready   atomic.Bool
	current atomic.Pointer[leaseState]
}

// leaseState pairs a lease with this Renewer's own local observation of when it obtained it
// (there is no server-provided "issued at" in credentialbroker.Response — IssuedAt is an
// audit-only, server-side field never put on the wire). Storing both together in one atomic
// pointer, written once per successful Issue call by Run's single goroutine, means a reader on
// another goroutine (healthz, the postgres proxy) never observes one half updated without the
// other.
type leaseState struct {
	resp     credentialbroker.Response
	issuedAt time.Time
}

// Ready reports whether at least one lease has ever been obtained — contracts/
// credential-sidecar.md's GET /healthz: "200 once at least one successful lease has been
// obtained, 503 otherwise." Never goes back to false once true, even if every subsequent renewal
// fails and the lease eventually expires — an expired-but-once-valid lease is a different problem
// (the proxy/file layer's own job to handle) from "has this process ever worked at all."
func (r *Renewer) Ready() bool { return r.ready.Load() }

// Current returns the most recently obtained lease, or nil if none yet.
func (r *Renewer) Current() *credentialbroker.Response {
	st := r.current.Load()
	if st == nil {
		return nil
	}
	return &st.resp
}

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

// maybeRenew checks the current lease against its margin and, if it's due, attempts a renewal. A
// failure here is always non-fatal — see Run's doc comment for why.
func (r *Renewer) maybeRenew(ctx context.Context) {
	st := r.current.Load()
	if st != nil && r.Now().Add(r.marginFor(st)).Before(st.resp.ExpiresAt) {
		return // not due yet
	}
	resp, err := r.Client.Issue(ctx, r.Request)
	if err != nil {
		log.Printf("sidecar: renewal failed (keeping the current lease; will retry in %s): %v", r.Interval, err)
		return
	}
	r.apply(resp)
}

// marginFor returns the margin to renew st's lease by: half of its own real lifetime
// (ExpiresAt - issuedAt) when HalfLifetime is set, the fixed Margin otherwise.
func (r *Renewer) marginFor(st *leaseState) time.Duration {
	if !r.HalfLifetime {
		return r.Margin
	}
	return st.resp.ExpiresAt.Sub(st.issuedAt) / 2
}

func (r *Renewer) apply(resp credentialbroker.Response) {
	r.current.Store(&leaseState{resp: resp, issuedAt: r.Now()})
	r.ready.Store(true)
	if r.OnRenew != nil {
		r.OnRenew(resp)
	}
}
