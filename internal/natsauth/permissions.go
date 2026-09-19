// Package natsauth authenticates the event bus and scopes what each participant may
// publish (ADR 0049). It uses NATS's JWT operator/account model: core holds one account's
// signing key and mints a user credential per module, with subject permissions derived
// from that module's manifest. Because permissions live in the signed credential, the
// NATS server needs no reconfiguration when a module is installed or removed.
//
// The permission model is a default-deny allow-list: anything not listed below is
// refused by the server — including every JetStream stream-management API, so no module
// can delete or purge the shared events stream.
package natsauth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
	"github.com/projectbooth/booth-core/internal/eventbus"
)

// eventPatternRe mirrors the CRD's validation and docs/decisions/0002's event-type
// grammar: two or more dotted tokens, the first a literal name (so a module can never
// declare "*.*" or ">" and reach every event type), later tokens a name or "*".
var eventPatternRe = regexp.MustCompile(`^[a-z][a-z0-9]*(\.([a-z][a-z0-9]*|\*))+$`)

// ValidateEventPattern rejects anything that isn't a well-formed event-type pattern.
// The CRD schema enforces the same rule at admission; this is the second check, so a
// resource created before the schema existed (or with validation bypassed) still can't
// widen its own permissions.
func ValidateEventPattern(p string) error {
	if !eventPatternRe.MatchString(p) {
		return fmt.Errorf("invalid event pattern %q: want dotted lowercase tokens (a * is allowed for any token after the first), e.g. dashboard.created or dashboard.*", p)
	}
	return nil
}

// Grants is the set of NATS subjects a credential may publish to and subscribe to.
type Grants struct {
	PublishAllow   []string
	SubscribeAllow []string
}

// IsEmpty reports whether the grants allow nothing at all.
func (g Grants) IsEmpty() bool {
	return len(g.PublishAllow) == 0 && len(g.SubscribeAllow) == 0
}

// Hash is a stable fingerprint of the grants, used to decide whether an already
// provisioned credential still matches the manifest or must be re-minted.
func (g Grants) Hash() string {
	pub := append([]string(nil), g.PublishAllow...)
	sub := append([]string(nil), g.SubscribeAllow...)
	sort.Strings(pub)
	sort.Strings(sub)
	sum := sha256.Sum256([]byte(strings.Join(pub, "\n") + "\x00" + strings.Join(sub, "\n")))
	return hex.EncodeToString(sum[:])
}

// jetStreamConsumerAPI lists the JetStream API subjects a *subscriber* needs against the
// shared events stream: read the stream's info, create/inspect consumers, pull messages,
// and ack. Notably absent: STREAM.DELETE/PURGE/UPDATE, MSG.DELETE, and CONSUMER.DELETE —
// a credentialed module can read, but not destroy or tamper with, the stream.
//
// Known limit (documented in docs/decisions/0006): consumer-API subjects aren't scoped to
// a consumer name, so one subscriber could in principle interfere with another's durable
// consumer, and a consumer's filter subject is the client's choice — the declared
// subscribe patterns act as a capability gate rather than per-subject read confinement.
func jetStreamConsumerAPI() []string {
	s := eventbus.StreamName
	return []string{
		"$JS.API.CONSUMER.CREATE." + s + ".>",
		"$JS.API.CONSUMER.DURABLE.CREATE." + s + ".>",
		"$JS.API.CONSUMER.INFO." + s + ".>",
		"$JS.API.CONSUMER.MSG.NEXT." + s + ".>",
		"$JS.ACK." + s + ".>",
		"$JS.FC." + s + ".>",
	}
}

// GrantsFor derives a module's NATS grants from its manifest's events declaration.
// A module with no declared events gets empty grants (and therefore no credential).
func GrantsFor(ev *boothv1alpha1.EventBusAccess) (Grants, error) {
	if ev == nil || (len(ev.Publish) == 0 && len(ev.Subscribe) == 0) {
		return Grants{}, nil
	}

	var g Grants

	for _, p := range ev.Publish {
		if err := ValidateEventPattern(p); err != nil {
			return Grants{}, err
		}
		g.PublishAllow = append(g.PublishAllow, "booth.*."+p)
	}

	for _, p := range ev.Subscribe {
		// Validated for the same reason as publish patterns, even though the pattern
		// itself isn't yet embedded in a subject (see jetStreamConsumerAPI's note).
		if err := ValidateEventPattern(p); err != nil {
			return Grants{}, err
		}
	}
	if len(ev.Subscribe) > 0 {
		g.PublishAllow = append(g.PublishAllow, jetStreamConsumerAPI()...)
	}

	// Anyone using JetStream needs to see the stream exist, and to receive replies
	// (publish acks, consumer API responses, pull deliveries) on a private inbox.
	g.PublishAllow = append(g.PublishAllow, "$JS.API.INFO", "$JS.API.STREAM.INFO."+eventbus.StreamName)
	g.SubscribeAllow = []string{"_INBOX.>"}

	g.PublishAllow = dedupe(g.PublishAllow)
	return g, nil
}

// CoreGrants is what booth-core itself may do: everything, within its own account.
// Core owns the stream (it creates it at startup) so it needs the management API.
func CoreGrants() Grants {
	return Grants{PublishAllow: []string{">"}, SubscribeAllow: []string{">"}}
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
