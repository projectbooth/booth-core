package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projectbooth/booth-core/internal/credentialbroker"
)

// fakeBroker is a real HTTP server standing in for POST /api/credentials — a genuine socket, not
// a mocked interface, matching this repo's established testing bar for anything that calls out
// over HTTP (see internal/credentialbroker's own realProvider for the precedent this follows).
type fakeBroker struct {
	*httptest.Server
	mu       sync.Mutex
	calls    int
	respond  func(call int, req credentialbroker.Request) (status int, body any)
	lastAuth string
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	b := &fakeBroker{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req credentialbroker.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.mu.Lock()
		b.calls++
		call := b.calls
		b.lastAuth = r.Header.Get("Authorization")
		respond := b.respond
		b.mu.Unlock()

		status, body := http.StatusCreated, any(credentialbroker.Response{
			LeaseID: "lease-1", Kind: req.Kind, ExpiresAt: time.Now().Add(5 * time.Minute),
			Scope: req.Scope, Credential: json.RawMessage(`{"secret":"shh"}`),
		})
		if respond != nil {
			status, body = respond(call, req)
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(b.Server.Close)
	return b
}

func (b *fakeBroker) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func newClient(broker *fakeBroker) *BrokerClient {
	return NewBrokerClient(broker.URL, "acme", StaticToken("test-token"))
}

func testRequest() credentialbroker.Request {
	return credentialbroker.Request{Kind: "s3", Access: credentialbroker.AccessRead, Scope: json.RawMessage(`{"backendId":"x"}`)}
}

// --- BrokerClient --------------------------------------------------------------------------------

func TestBrokerClient_Issue_SendsTheDocumentedHeadersAndBody(t *testing.T) {
	broker := newFakeBroker(t)
	client := newClient(broker)
	resp, err := client.Issue(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.LeaseID != "lease-1" {
		t.Errorf("LeaseID = %q", resp.LeaseID)
	}
	if broker.lastAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q", broker.lastAuth)
	}
}

func TestBrokerClient_Issue_ClassifiesRefusalsDistinctFromOutages(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			broker := newFakeBroker(t)
			broker.respond = func(int, credentialbroker.Request) (int, any) {
				return status, map[string]string{"error": "nope"}
			}
			_, err := newClient(broker).Issue(context.Background(), testRequest())
			var refusal *RefusalError
			if !errors.As(err, &refusal) {
				t.Fatalf("status %d: err = %v, want *RefusalError", status, err)
			}
			if refusal.StatusCode != status {
				t.Errorf("StatusCode = %d, want %d", refusal.StatusCode, status)
			}
		})
	}

	broker := newFakeBroker(t)
	broker.respond = func(int, credentialbroker.Request) (int, any) { return 503, map[string]string{} }
	_, err := newClient(broker).Issue(context.Background(), testRequest())
	var refusal *RefusalError
	if errors.As(err, &refusal) {
		t.Fatalf("a 503 was classified as a refusal, want a plain (retryable) error: %v", err)
	}
	if err == nil {
		t.Fatal("expected an error for a 503")
	}
}

func TestBrokerClient_Issue_RejectsAnIncompleteResponse(t *testing.T) {
	broker := newFakeBroker(t)
	broker.respond = func(int, credentialbroker.Request) (int, any) {
		return 201, map[string]string{"kind": "s3"} // no leaseId/expiresAt/credential
	}
	if _, err := newClient(broker).Issue(context.Background(), testRequest()); err == nil {
		t.Fatal("accepted a response missing leaseId/expiresAt/credential")
	}
}

// --- Renewer: the renewal-timing contract ---------------------------------------------------------

// fakeClock lets a test move time forward deterministically rather than racing a real one.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestRenewer_RenewsOnlyOnceWithinTheMargin(t *testing.T) {
	broker := newFakeBroker(t)
	clock := &fakeClock{now: time.Now()}
	// The broker's issued expiresAt must be relative to the SAME clock the renewer checks it
	// against — a real broker's clock and the renewer's own clock are the same clock in
	// production; only this test's fake one needs to say so explicitly.
	broker.respond = func(int, credentialbroker.Request) (int, any) {
		return http.StatusCreated, credentialbroker.Response{
			LeaseID: "lease-1", Kind: "s3", ExpiresAt: clock.Now().Add(5 * time.Minute), Credential: json.RawMessage(`{}`),
		}
	}
	var renewals atomic.Int32
	r := &Renewer{
		Client: newClient(broker), Request: testRequest(),
		Margin: time.Minute, Interval: time.Millisecond,
		Now:     clock.Now,
		OnRenew: func(credentialbroker.Response) { renewals.Add(1) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()

	waitForCalls(t, broker, 1) // initial lease
	if !r.Ready() {
		t.Fatal("not ready after the initial lease")
	}

	// expiresAt is 5 minutes out; well outside the 1-minute margin, so ticks must not renew.
	for i := 0; i < 20; i++ {
		time.Sleep(2 * time.Millisecond)
	}
	if got := broker.callCount(); got != 1 {
		t.Errorf("calls = %d, want 1 (renewal not due yet)", got)
	}

	// Move the clock to inside the margin: the very next tick must renew exactly once.
	clock.Advance(4*time.Minute + 30*time.Second)
	waitForCalls(t, broker, 2)
	time.Sleep(20 * time.Millisecond) // give a few more ticks a chance to over-fire
	if renewals.Load() != 2 {
		t.Errorf("OnRenew called %d times, want exactly 2 (initial + one renewal)", renewals.Load())
	}

	cancel()
	<-done
}

func waitForCalls(t *testing.T, b *fakeBroker, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.callCount() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d broker call(s), got %d", n, b.callCount())
}

func TestRenewer_UsesTheRealExpiresAtNotTheRequestedTTL(t *testing.T) {
	broker := newFakeBroker(t)
	clock := &fakeClock{now: time.Now()}
	broker.respond = func(int, credentialbroker.Request) (int, any) {
		// The provider clamped the TTL up (e.g. MinIO's 15-minute floor), far past any
		// ttlSeconds the sidecar might have requested — the renewer must key off this.
		return 201, credentialbroker.Response{
			LeaseID: "lease-1", Kind: "s3", ExpiresAt: clock.Now().Add(15 * time.Minute),
			Credential: json.RawMessage(`{}`),
		}
	}
	r := &Renewer{Client: newClient(broker), Request: testRequest(), Margin: time.Minute, Interval: time.Millisecond, Now: clock.Now}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	waitForCalls(t, broker, 1)

	clock.Advance(10 * time.Minute) // well past a hypothetical 5m ttlSeconds, nowhere near 15m expiresAt
	time.Sleep(30 * time.Millisecond)
	if got := broker.callCount(); got != 1 {
		t.Errorf("calls = %d, want 1 (still not within margin of the REAL expiresAt)", got)
	}
	cancel()
	<-done
}

// --- Regression: a refusal is fatal only before any lease has ever been obtained. ------------------

func TestRenewer_RefusalAtStartupIsFatal(t *testing.T) {
	broker := newFakeBroker(t)
	broker.respond = func(int, credentialbroker.Request) (int, any) {
		return 403, map[string]string{"error": "forbidden"}
	}
	r := &Renewer{Client: newClient(broker), Request: testRequest(), Margin: time.Minute, Interval: time.Millisecond}
	err := r.Run(context.Background())
	var refusal *RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("Run() = %v, want a *RefusalError", err)
	}
	if r.Ready() {
		t.Error("Ready() = true after a startup refusal")
	}
}

func TestRenewer_OutageAtStartupRetriesRatherThanFailing(t *testing.T) {
	broker := newFakeBroker(t)
	var attempt atomic.Int32
	broker.respond = func(int, credentialbroker.Request) (int, any) {
		if attempt.Add(1) <= 2 {
			return 503, map[string]string{}
		}
		return 201, credentialbroker.Response{LeaseID: "lease-1", Kind: "s3", ExpiresAt: time.Now().Add(time.Hour), Credential: json.RawMessage(`{}`)}
	}
	r := &Renewer{Client: newClient(broker), Request: testRequest(), Margin: time.Minute, Interval: time.Hour}
	// obtainFirst's own backoff starts at 1s; shrink it for the test by calling it directly isn't
	// exported, so instead run with a context timeout generous enough for 2 real-second retries.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("Run returned early (should still be retrying the outage, then settle into steady state): %v", err)
	case <-time.After(4 * time.Second):
	}
	if !r.Ready() {
		t.Error("Ready() = false after the outage should have resolved on retry")
	}
	cancel()
}

// --- The hard requirement the task calls out by name: a renewal failure never drops the lease. -----

func TestRenewer_RenewalFailureNeverDropsTheCurrentLeaseOrExitsRun(t *testing.T) {
	broker := newFakeBroker(t)
	clock := &fakeClock{now: time.Now()}
	var failRenewals atomic.Bool
	broker.respond = func(call int, req credentialbroker.Request) (int, any) {
		if call > 1 && failRenewals.Load() {
			// A refusal specifically — the stricter case: even a "permanent" failure during
			// renewal must not disturb the existing lease (docs/decisions/0015's reasoning).
			return 403, map[string]string{"error": "role revoked"}
		}
		return 201, credentialbroker.Response{
			LeaseID: "lease-1", Kind: "s3", ExpiresAt: clock.Now().Add(5 * time.Minute), Credential: json.RawMessage(`{}`),
		}
	}
	r := &Renewer{Client: newClient(broker), Request: testRequest(), Margin: time.Minute, Interval: time.Millisecond, Now: clock.Now}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitForCalls(t, broker, 1)
	before := r.Current()

	failRenewals.Store(true)
	clock.Advance(4*time.Minute + 30*time.Second) // inside the margin: a renewal attempt fires
	waitForCalls(t, broker, 2)
	time.Sleep(50 * time.Millisecond)

	select {
	case err := <-done:
		t.Fatalf("Run exited after a renewal refusal, want it to keep running: %v", err)
	default:
	}
	if r.Current() != before {
		t.Error("the lease changed after a failed renewal; it must be left exactly alone")
	}
	if !r.Ready() {
		t.Error("Ready() flipped false after a renewal failure")
	}

	cancel()
	<-done
}
