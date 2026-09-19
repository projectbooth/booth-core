package natsauth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/eventbus"
)

// These tests run a real, in-process NATS server configured from exactly the fragment
// Authority.ServerConfig renders for production, then attempt real publishes and API calls
// with real minted credentials. They verify enforcement, not just that we generated
// permission strings.

func newAuthority(t *testing.T) *Authority {
	t.Helper()
	seeds, err := GenerateSeeds()
	if err != nil {
		t.Fatalf("GenerateSeeds: %v", err)
	}
	a, err := NewAuthority(seeds)
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}
	return a
}

func startServer(t *testing.T, a *Authority) *server.Server {
	t.Helper()
	auth, err := a.ServerConfig()
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	return startServerWithConf(t, auth)
}

// startServerWithConf starts a server whose auth fragment is the given config text.
func startServerWithConf(t *testing.T, auth string) *server.Server {
	t.Helper()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "booth-auth.conf"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	// Mirrors the chart: the main config includes the generated fragment.
	main := "listen: 127.0.0.1:-1\njetstream { store_dir: \"" + filepath.ToSlash(filepath.Join(dir, "js")) + "\" }\ninclude ./booth-auth.conf\n"
	confPath := filepath.Join(dir, "nats.conf")
	if err := os.WriteFile(confPath, []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}

	opts, err := server.ProcessConfigFile(confPath)
	if err != nil {
		t.Fatalf("ProcessConfigFile: %v", err)
	}
	opts.NoLog, opts.NoSigs = os.Getenv("BOOTH_TEST_NATS_LOG") == "", true

	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if !opts.NoLog {
		s.ConfigureLogger()
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server did not become ready")
	}
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	return s
}

// errCollector captures asynchronous server errors (permission violations are async).
type errCollector struct {
	mu   sync.Mutex
	errs []error
}

func (c *errCollector) handler(_ *nats.Conn, _ *nats.Subscription, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs = append(c.errs, err)
}

func (c *errCollector) sawPermissionViolation(wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, e := range c.errs {
			if strings.Contains(strings.ToLower(e.Error()), "permissions violation") {
				c.mu.Unlock()
				return true
			}
		}
		c.mu.Unlock()
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func connect(t *testing.T, s *server.Server, cred *Credential, ec *errCollector) (*nats.Conn, error) {
	t.Helper()
	opts := []nats.Option{nats.NoReconnect(), nats.Timeout(3 * time.Second)}
	if cred != nil {
		opts = append(opts, nats.UserJWTAndSeed(cred.JWT, cred.Seed))
	}
	if ec != nil {
		opts = append(opts, nats.ErrorHandler(ec.handler))
	}
	nc, err := nats.Connect(s.ClientURL(), opts...)
	if err == nil {
		t.Cleanup(nc.Close)
	}
	return nc, err
}

// coreWithStream starts a server, connects as core, and creates the events stream.
func coreWithStream(t *testing.T, a *Authority) (*server.Server, jetstream.JetStream) {
	t.Helper()
	s := startServer(t, a)
	cred, err := a.MintUser("core", CoreGrants(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := connect(t, s, cred, nil)
	if err != nil {
		t.Fatalf("core connect: %v", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: eventbus.StreamName, Subjects: []string{eventbus.StreamSubjectFilter},
	}); err != nil {
		t.Fatalf("core creating stream: %v", err)
	}
	return s, js
}

func streamMsgs(t *testing.T, coreJS jetstream.JetStream) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := coreJS.Stream(ctx, eventbus.StreamName)
	if err != nil {
		t.Fatalf("stream lookup: %v", err)
	}
	info, err := st.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Msgs
}

func mustGrants(t *testing.T, ev *boothv1alpha1.EventBusAccess) Grants {
	t.Helper()
	g, err := GrantsFor(ev)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestUnauthenticatedConnectionRefused(t *testing.T) {
	a := newAuthority(t)
	s := startServer(t, a)

	if _, err := connect(t, s, nil, nil); err == nil {
		t.Fatal("expected an anonymous connection to be refused, but it succeeded")
	}
}

func TestCredentialFromAnotherAuthorityRefused(t *testing.T) {
	s := startServer(t, newAuthority(t))
	rogue := newAuthority(t) // different seeds -> not trusted by this server

	cred, err := rogue.MintUser("rogue", CoreGrants(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connect(t, s, cred, nil); err == nil {
		t.Fatal("expected a credential signed by an untrusted authority to be refused")
	}
}

func TestExpiredCredentialRefused(t *testing.T) {
	a := newAuthority(t)
	s := startServer(t, a)

	cred, err := a.MintUser("old", CoreGrants(), -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connect(t, s, cred, nil); err == nil {
		t.Fatal("expected an expired credential to be refused")
	}
}

// The demonstrated gap from ADR 0049: a module forging an event type it doesn't own.
func TestPublisherCannotForgeSubjectsItDoesNotOwn(t *testing.T) {
	a := newAuthority(t)
	s, coreJS := coreWithStream(t, a)

	cred, err := a.MintUser("superset", mustGrants(t, &boothv1alpha1.EventBusAccess{
		Publish: []string{"dashboard.*"},
	}), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ec := &errCollector{}
	nc, err := connect(t, s, cred, ec)
	if err != nil {
		t.Fatalf("module connect: %v", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Legitimate: a dashboard event, in any workspace.
	if _, err := js.Publish(ctx, "booth.acme-analytics.dashboard.created", []byte(`{}`)); err != nil {
		t.Fatalf("legitimate publish was refused: %v", err)
	}

	// Forgery: an event type this module doesn't own.
	forgeCtx, forgeCancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer forgeCancel()
	if _, err := js.Publish(forgeCtx, "booth.acme-analytics.dataset.written", []byte(`{}`)); err == nil {
		t.Fatal("forged publish to an unowned event type was acknowledged")
	}
	if !ec.sawPermissionViolation(2 * time.Second) {
		t.Error("expected the server to report a permissions violation for the forged publish")
	}

	// The only durable proof: the forged message never reached the stream.
	if got := streamMsgs(t, coreJS); got != 1 {
		t.Fatalf("stream holds %d messages, want exactly 1 (the legitimate one)", got)
	}
}

func TestSubscribeOnlyModuleCannotPublishEvents(t *testing.T) {
	a := newAuthority(t)
	s, coreJS := coreWithStream(t, a)

	cred, err := a.MintUser("catalog", mustGrants(t, &boothv1alpha1.EventBusAccess{
		Subscribe: []string{"dashboard.*"},
	}), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := connect(t, s, cred, &errCollector{})
	if err != nil {
		t.Fatal(err)
	}
	js, _ := jetstream.New(nc)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := js.Publish(ctx, "booth.acme-analytics.dashboard.deleted", []byte(`{}`)); err == nil {
		t.Fatal("a subscribe-only module was able to publish an event")
	}
	if got := streamMsgs(t, coreJS); got != 0 {
		t.Fatalf("stream holds %d messages, want 0", got)
	}
}

func TestSubscriberCanConsumeButCannotDestroyTheStream(t *testing.T) {
	a := newAuthority(t)
	s, coreJS := coreWithStream(t, a)

	// Seed one event via core.
	seedCtx, seedCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer seedCancel()
	if _, err := coreJS.Publish(seedCtx, "booth.acme-analytics.dashboard.created", []byte(`{"id":"d1"}`)); err != nil {
		t.Fatal(err)
	}

	cred, err := a.MintUser("catalog", mustGrants(t, &boothv1alpha1.EventBusAccess{
		Subscribe: []string{"dashboard.*"},
	}), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := connect(t, s, cred, &errCollector{})
	if err != nil {
		t.Fatal(err)
	}
	js, _ := jetstream.New(nc)

	// Legitimate: durable consumer, fetch the event, ack it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cons, err := js.CreateOrUpdateConsumer(ctx, eventbus.StreamName, jetstream.ConsumerConfig{
		Durable: "catalog", FilterSubject: "booth.*.dashboard.*", AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("subscriber was refused a consumer: %v", err)
	}
	batch, err := cons.Fetch(1, jetstream.FetchMaxWait(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var got int
	for msg := range batch.Messages() {
		got++
		if err := msg.Ack(); err != nil {
			t.Errorf("ack: %v", err)
		}
	}
	if got != 1 {
		t.Fatalf("consumed %d messages, want 1", got)
	}

	// Forbidden: tampering with the stream itself.
	for name, do := range map[string]func(context.Context) error{
		"delete stream": func(c context.Context) error { return js.DeleteStream(c, eventbus.StreamName) },
		"purge stream": func(c context.Context) error {
			st, err := js.Stream(c, eventbus.StreamName)
			if err != nil {
				return err
			}
			return st.Purge(c)
		},
	} {
		c, cc := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		if err := do(c); err == nil {
			t.Errorf("subscriber was able to %s", name)
		}
		cc()
	}
	if n := streamMsgs(t, coreJS); n != 1 {
		t.Fatalf("stream holds %d messages after tampering attempts, want 1", n)
	}
}

func TestPublisherWithoutSubscribeCannotCreateConsumers(t *testing.T) {
	a := newAuthority(t)
	s, _ := coreWithStream(t, a)

	cred, _ := a.MintUser("superset", mustGrants(t, &boothv1alpha1.EventBusAccess{
		Publish: []string{"dashboard.*"},
	}), time.Hour)
	nc, err := connect(t, s, cred, &errCollector{})
	if err != nil {
		t.Fatal(err)
	}
	js, _ := jetstream.New(nc)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := js.CreateOrUpdateConsumer(ctx, eventbus.StreamName, jetstream.ConsumerConfig{
		Durable: "spy", AckPolicy: jetstream.AckExplicitPolicy,
	}); err == nil {
		t.Fatal("a publish-only module was able to create a consumer and read the stream")
	}
}

// Core connects with a credential minted per (re)connect; prove that path against a real
// server, including that a reconnect (which mints a new one) still works.
func TestConnectOptionAuthenticatesAndSurvivesReconnect(t *testing.T) {
	a := newAuthority(t)
	s := startServer(t, a)

	reconnected := make(chan struct{}, 1)
	nc, err := nats.Connect(s.ClientURL(),
		a.ConnectOption("core", CoreGrants()),
		nats.ReconnectWait(50*time.Millisecond),
		nats.ReconnectHandler(func(*nats.Conn) { reconnected <- struct{}{} }),
	)
	if err != nil {
		t.Fatalf("connect with ConnectOption: %v", err)
	}
	t.Cleanup(nc.Close)

	// Force a reconnect by kicking the client server-side.
	s.DisconnectClientByID(mustClientID(t, s))

	select {
	case <-reconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not reconnect with a freshly minted credential")
	}
}

func mustClientID(t *testing.T, s *server.Server) uint64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cz, err := s.Connz(&server.ConnzOptions{}); err == nil && len(cz.Conns) > 0 {
			return cz.Conns[0].Cid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no client connection found on the server")
	return 0
}
