package eventbus

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
)

// startJetStreamServer runs a real, in-process NATS server with JetStream enabled and no
// auth — Connect's stream setup doesn't depend on ADR 0049's credential layer, so these
// tests exercise that in isolation rather than through natsauth's heavier fixture.
func startJetStreamServer(t *testing.T) string {
	t.Helper()
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server did not become ready")
	}
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	return s.ClientURL()
}

// TestConnect_StreamHasAnExplicitDuplicateWindowAndSevenDayRetention pins the two
// BOOTH_EVENTS properties ADR 0123's build prompt asks booth-core to confirm: a configured
// duplicate window (not just whatever the server defaults to) and the existing 7-day
// retention, unchanged by adding the window.
func TestConnect_StreamHasAnExplicitDuplicateWindowAndSevenDayRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bus, err := Connect(ctx, startJetStreamServer(t))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer bus.Close()

	stream, err := bus.js.Stream(ctx, StreamName)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	cfg := stream.CachedInfo().Config

	if cfg.Duplicates != 2*time.Hour {
		t.Fatalf("Duplicates = %v, want 2h", cfg.Duplicates)
	}
	if cfg.MaxAge != 7*24*time.Hour {
		t.Fatalf("MaxAge = %v, want 7 days", cfg.MaxAge)
	}
	if cfg.Retention != jetstream.LimitsPolicy {
		t.Fatalf("Retention = %v, want LimitsPolicy", cfg.Retention)
	}
}

// TestConnect_ReconnectingKeepsTheConfiguredDuplicateWindow exercises the
// CreateOrUpdateStream path a second time against an already-provisioned stream (what
// happens on every booth-core restart), confirming the explicit Duplicates value sticks
// rather than only applying on first creation.
func TestConnect_ReconnectingKeepsTheConfiguredDuplicateWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := startJetStreamServer(t)

	first, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("Connect (first): %v", err)
	}
	first.Close()

	second, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("Connect (second): %v", err)
	}
	defer second.Close()

	stream, err := second.js.Stream(ctx, StreamName)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := stream.CachedInfo().Config.Duplicates; got != 2*time.Hour {
		t.Fatalf("Duplicates = %v, want 2h", got)
	}
}
