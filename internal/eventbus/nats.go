package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Bus wraps a JetStream context, providing booth-core's publish/subscribe surface (part
// of core-platform-api.md's event bus contract) over the raw NATS client. JetStream is
// enabled specifically so a briefly-down consumer (e.g. booth-catalog restarting) doesn't
// silently miss events published while it was offline (ADR 0021).
type Bus struct {
	conn *nats.Conn
	js   jetstream.JetStream
}

// Connect dials the configured NATS server and ensures the shared BOOTH_EVENTS stream
// exists (creating it if this is a fresh deployment). Extra options carry authentication
// (ADR 0049).
func Connect(ctx context.Context, url string, opts ...nats.Option) (*Bus, error) {
	conn, err := nats.Connect(url, append([]nats.Option{nats.Name("booth-core")}, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %s: %w", url, err)
	}

	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("creating JetStream context: %w", err)
	}

	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      StreamName,
		Subjects:  []string{StreamSubjectFilter},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    7 * 24 * time.Hour,
		Storage:   jetstream.FileStorage,
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ensuring stream %s: %w", StreamName, err)
	}

	return &Bus{conn: conn, js: js}, nil
}

func (b *Bus) Close() {
	b.conn.Close()
}

// Publish sends one event. publishedBy is the publishing module's manifest ID.
func (b *Bus) Publish(ctx context.Context, workspace, eventType, publishedBy string, data map[string]any) error {
	subject, err := Subject(workspace, eventType)
	if err != nil {
		return err
	}

	envelope := Envelope{
		Workspace:   workspace,
		EventType:   eventType,
		PublishedAt: time.Now().UTC(),
		PublishedBy: publishedBy,
		Data:        data,
	}

	payload, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshaling event envelope: %w", err)
	}

	if _, err := b.js.Publish(ctx, subject, payload); err != nil {
		return fmt.Errorf("publishing to %s: %w", subject, err)
	}
	return nil
}

// Handler processes one received event. Returning an error leaves the message unacked,
// so JetStream redelivers it (at-least-once delivery, per ADR 0021).
type Handler func(ctx context.Context, envelope Envelope) error

// Subscribe creates (or resumes) a durable JetStream consumer named consumerName,
// filtered to subjectFilter (e.g. "booth.acme-analytics.dashboard.*" or "booth.*.dashboard.created"
// per decision 0002's wildcard patterns), and delivers matching events to handler until
// ctx is canceled.
//
// consumerName must be stable across a subscriber's restarts — it's what makes the
// subscription durable: JetStream remembers this consumer's delivery position, so a
// consumer that was offline gets everything it missed on reconnect instead of only new
// events (ADR 0021's whole reason for choosing JetStream over plain NATS pub/sub).
func (b *Bus) Subscribe(ctx context.Context, consumerName, subjectFilter string, handler Handler) error {
	consumer, err := b.js.CreateOrUpdateConsumer(ctx, StreamName, jetstream.ConsumerConfig{
		Durable:       consumerName,
		FilterSubject: subjectFilter,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return fmt.Errorf("creating consumer %s: %w", consumerName, err)
	}

	_, err = consumer.Consume(func(msg jetstream.Msg) {
		var envelope Envelope
		if err := json.Unmarshal(msg.Data(), &envelope); err != nil {
			_ = msg.Nak()
			return
		}

		if err := handler(ctx, envelope); err != nil {
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("starting consume loop for %s: %w", consumerName, err)
	}

	return nil
}
