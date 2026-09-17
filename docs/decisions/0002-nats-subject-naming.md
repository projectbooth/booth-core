# booth-core decision 0002: NATS subject naming convention

Status: **promoted, accepted as-is** —
`../../../booth-architecture/decisions/0026-nats-subject-naming.md` (2026-09-17). No
rework needed; this file is kept as the detailed reasoning behind that ADR.

## Context

ADR 0021 picked NATS with JetStream as the event bus broker but left the actual subject
string format open. `booth-core`'s v0 needs a concrete answer to build the event bus
wiring (JetStream stream/consumer setup) and document it in
`contracts/core-platform-api.md`. At least one real use case already exists to design
against: `dashboard.created`/`updated`/`deleted` (ADR 0018), published by three modules,
consumed by `booth-catalog`.

## Decision

**Subject format**: `booth.<workspace>.<event-type>`, where `<event-type>` is the
dotted event name a module publishes (e.g. `dashboard.created`) verbatim, and
`<workspace>` is the workspace slug (decision 0001's grammar, `[a-z0-9-]+`).

Example: a `dashboard.created` event in workspace `acme-analytics` publishes to subject
`booth.acme-analytics.dashboard.created`.

**Why workspace-namespaced from the start**: ADR 0008 fixed multi-workspace tenancy for
v0, and every module's data is workspace-scoped. An event about a dashboard, a dataset,
or anything else is inherently scoped to the workspace it happened in — a consumer that
only cares about one workspace (the common case: `booth-catalog` re-indexing) should be
able to subscribe with a subject wildcard like `booth.acme-analytics.>` instead of
receiving every workspace's events and filtering client-side.

**Wildcard subscription patterns this enables**:
- `booth.acme-analytics.>` — everything in one workspace.
- `booth.*.dashboard.created` — one event type across every workspace (useful for a
  cross-workspace admin view, if one is ever needed — not a v0 requirement, just a
  reason this shape is worth keeping over the alternative below).
- `booth.acme-analytics.dashboard.*` — every dashboard lifecycle event in one workspace.

**JetStream stream layout**: one stream, `BOOTH_EVENTS`, with subject filter
`booth.>`, capturing every workspace and event type. Splitting into per-workspace
streams is not done in v0 — workspace count is expected to be small enough that a single
stream's retention/replay semantics are simpler to reason about than N dynamically
created streams, one per workspace. Revisit if workspace count or event volume ever makes
this a real bottleneck.

**Payload envelope**: every event's JSON payload carries a minimal common envelope this
decision does fix, even though full per-event-type payload schemas (e.g. dashboard
event's exact shape, ADR 0018) are out of scope here:

```json
{
  "workspace": "acme-analytics",
  "eventType": "dashboard.created",
  "publishedAt": "2026-09-17T18:00:00Z",
  "publishedBy": "superset",
  "data": { }
}
```

`publishedBy` is the publishing module's manifest `id` — lets a consumer log/debug
without parsing the subject string. `data` is the event-type-specific payload, whose
schema is each event type's own concern to define (e.g. ADR 0018's follow-up for
`dashboard.*`).

## Consequences

- `contracts/core-platform-api.md`'s event bus section gets this subject grammar and
  envelope shape once promoted to an ADR.
- Every publishing module needs the active workspace slug at publish time (already true —
  every module request already carries `X-Booth-Workspace` per decision 0001).
- `booth-catalog` and the three dashboard modules can now agree on transport mechanics
  (subject shape, envelope) independent of finalizing `dashboard.*`'s `data` payload
  shape, unblocking them from each other.

## Flagged back

Resolved: promoted to `booth-architecture` ADR 0026, accepted exactly as proposed here.
Any module can now build its publish/subscribe wiring against this shape as settled.
