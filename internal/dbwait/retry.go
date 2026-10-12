// Package dbwait retries a database's first connection at process startup.
// ARCHITECTURE.md item 66 (found 2026-10-12 on the homelab): a freshly-scheduled pod's first
// connection to the bundled PostgreSQL can be refused for a few seconds while k3s's kube-router
// finishes adding the new pod's IP to the NetworkPolicy allow set. core used to exit on that
// first refusal, crash-looping until the restart happened to land after the window closed.
// Retrying for about a minute rides out that transient window instead; a server still
// unreachable after that long is a real outage, not a scheduling race, so Retry's caller should
// treat its error as fatal.
package dbwait

import (
	"context"
	"fmt"
	"log"
	"time"
)

// DefaultMaxElapsed bounds the total time Retry spends retrying before giving up: comfortably
// longer than the few-second refusal window item 66 observed, short enough that a genuinely
// unreachable server still fails loudly within about a minute rather than hanging indefinitely.
const DefaultMaxElapsed = 60 * time.Second

const (
	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 8 * time.Second
)

// Retry calls fn, which should attempt exactly one connection or connection-dependent
// operation, retrying with exponential backoff (starting at 500ms, doubling up to 8s) until it
// succeeds, ctx is canceled, or maxElapsed has passed since the first attempt — whichever comes
// first. Each retry is logged with what and the error that triggered it, so an operator
// watching logs at boot sees a bounded wait, not a silent hang or an immediate crash. A non-nil
// return is either ctx's error or fn's last error, wrapped with the number of attempts made.
func Retry(ctx context.Context, what string, maxElapsed time.Duration, fn func() error) error {
	deadline := time.Now().Add(maxElapsed)
	backoff := initialBackoff

	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}

		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s: still failing after %s (%d attempts): %w", what, maxElapsed, attempt, err)
		}

		log.Printf("%s: not ready yet (attempt %d: %v); retrying in %s", what, attempt, err, backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}
