# Confirmations: booth-core's part of ADR 0123 (event triggers for pipelines)

Per `agent-briefs/adr-0123-build-prompts.md`'s booth-core entry, against the accepted ADR
including its Revision section. No chart change; one code gap found and fixed (below).

## 1. NATS server is 2.10 or later

Confirmed, in both places that matter:

- **Production**: `charts/booth-core/Chart.yaml` pins the vendored `nats` subchart at
  `2.14.6` (`charts/booth-core/charts/nats-2.14.6.tgz`), whose own `values.yaml` sets the
  server image tag `nats:2.14.6-alpine`.
- **Tests**: `go.mod` pins `github.com/nats-io/nats-server/v2 v2.10.22`, the embedded
  server `internal/natsauth/authority_test.go` and the new `internal/eventbus/nats_test.go`
  both start in-process.

Both are past 2.10 (needed for a single consumer with two filter subjects, per the ADR's own
note — `booth-pipeline`'s shared `booth-pipeline-triggers` consumer on
`storage.object.created` and `api.write.committed`).

## 2. Only booth-storage and booth-api can hold the publish permission

True today, and true the same way every other event type in the fleet already is: there is
no registry or admission check in `booth-core` that reserves an event-type pattern to one
module id (unlike, for example, the `apiwrite:` workload-token subject-kind reservation —
a different threat model, since that one guards a request-time identity string against
spoofing by a caller, while an event-type publish grant is only ever set by a module's own
manifest, authored in that module's own repo). Checked every `BoothModule` manifest in the
fleet today (`grep -A6 events: **/boothmodule.yaml` across all fourteen module repos):
`booth-catalog` subscribes to `dashboard.*`/`table.*`/`database.table.*`; `booth-database`
publishes `database.table.*`; `booth-lakehouse` publishes `table.*`; `booth-streamlit`
publishes `dashboard.*`. Nobody declares `storage.*`, `api.*`, `storage.object.created`, or
`api.write.committed`, and neither `booth-storage` nor `booth-api` declares any events yet.
Once each adds its one line, each is the sole holder of its own event type, same as every
existing publisher above — not because core enforces it, but because nothing else asks for
it. Flag, not a gap: this is organizational discipline (code review on each manifest PR), not
an automated guarantee, consistent with how `database.table.*`/`table.*` have always worked.

## 3. NATS is reachable from booth-api's namespace

Not yet, but automatically once booth-api's manifest declares `events.publish`, with no
`booth-core` change needed. `charts/booth-core/templates/networkpolicy-nats.yaml` admits
ingress to the NATS pod only from core itself, other NATS pods, and namespaces labelled
`booth.projectbooth.io/event-bus-client: "true"`; `internal/natsauth/provision.go`'s
`ModuleProvisioner.Ensure` applies that label to a module's namespace only when its manifest
declares at least one event (empty grants → no credential, no label). Today `booth-api`
declares none, so its namespace isn't labelled and isn't admitted. The moment its chart adds
`publish: [api.write.committed]`, the next reconcile labels it and NATS becomes reachable —
the same self-service path `booth-database`/`booth-lakehouse`/`booth-streamlit`/`booth-catalog`
already went through. Checked `booth-api`'s own chart for an egress `NetworkPolicy` that
could block the other direction: it has none, so nothing on its side needs to change either.

## 4. `BOOTH_EVENTS` has a duplicate window and keeps 7-day retention

7-day retention: unchanged, confirmed (`internal/eventbus/nats.go`, `MaxAge: 7 * 24 *
time.Hour`, `Retention: LimitsPolicy`).

Duplicate window: **gap found and fixed.** `internal/eventbus/nats.go`'s `StreamConfig` never
set `Duplicates`, so the stream ran on whatever the server defaults to — confirmed by a
mutation test to be exactly 2 minutes, not configured, just inherited. That's short against
`booth-api`'s own accepted design (an outbox relay that can retry for up to 24 hours while the
bus is down, and — the real risk — could retry the same already-published outbox row's
message id again after a crash between the publish ack and marking the row done across a slow
restart). At-least-once delivery and duplicate-tolerant consumers (ADR 0123 point 1) make this
a correctness non-issue either way — `booth-pipeline`'s own consumer dedupes by stream
position regardless — but an *unconfigured, accidental* 2-minute window serves none of its
intended purpose (trimming the common crash-recovery case before it ever reaches a consumer)
for a relay whose own retry horizon is measured in hours. Fixed by pinning `Duplicates: 2 *
time.Hour` explicitly on `BOOTH_EVENTS`, generous enough for a slow restart without an
unbounded in-memory dedup table. `CreateOrUpdateStream` already applies config changes to an
existing stream on every `Connect`, so this takes effect on the next deploy with no migration.

Added `internal/eventbus/nats_test.go`: asserts the stream carries the explicit 2-hour window
and the unchanged 7-day/`LimitsPolicy` retention, and that reconnecting against an
already-provisioned stream keeps the window (not just first creation). Mutation-tested: reverting
the `Duplicates` line makes the first assertion fail with the measured 2-minute default,
confirming the test catches the actual gap rather than a tautology.

## On acceptance: `contracts/core-platform-api.md`

Not edited here (out of scope — it lives in `booth-architecture`, and this session doesn't
edit other repos). The event-bus section should gain `storage.object.created` and
`api.write.committed` alongside the existing `dashboard.*`/`table.*` listing, the same way ADR
0046 updated it for `dashboard.*`'s payload shape.

## Differences from the ADR

Everything above matches the ADR's Revision section except one addition beyond what it asked
to confirm: the `Duplicates` window was implicit, not configured, and is now pinned explicitly
at 2 hours. Nothing else needed code; the ADR's "no change to booth-core code or chart" line
holds other than this one fix, which the ADR's own confirmation ask surfaced rather than
contradicted.
