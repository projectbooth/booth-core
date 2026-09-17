// Package eventbus implements booth-core's asynchronous pub/sub mechanism (ADR 0007),
// backed by NATS with JetStream (ADR 0021), using the subject naming convention and
// payload envelope proposed in docs/decisions/0002-nats-subject-naming.md.
package eventbus

import (
	"fmt"
	"regexp"
	"time"
)

// StreamName is the single JetStream stream capturing every workspace and event type
// (decision 0002's "one stream, not per-workspace" call).
const StreamName = "BOOTH_EVENTS"

// StreamSubjectFilter is the wildcard subject filter StreamName is configured with.
const StreamSubjectFilter = "booth.>"

var eventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)+$`)
var workspacePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// Subject builds the NATS subject for an event in a given workspace, per decision
// 0002: booth.<workspace>.<event-type>.
func Subject(workspace, eventType string) (string, error) {
	if !workspacePattern.MatchString(workspace) {
		return "", fmt.Errorf("invalid workspace slug %q", workspace)
	}
	if !eventTypePattern.MatchString(eventType) {
		return "", fmt.Errorf("invalid event type %q, want dotted form like \"dashboard.created\"", eventType)
	}
	return fmt.Sprintf("booth.%s.%s", workspace, eventType), nil
}

// Envelope is the common wrapper every event's payload is published with, per decision
// 0002. Data carries the event-type-specific payload, whose schema is that event type's
// own concern (e.g. ADR 0018's dashboard.* payload, still to be finalized).
type Envelope struct {
	Workspace   string         `json:"workspace"`
	EventType   string         `json:"eventType"`
	PublishedAt time.Time      `json:"publishedAt"`
	PublishedBy string         `json:"publishedBy"`
	Data        map[string]any `json:"data"`
}
