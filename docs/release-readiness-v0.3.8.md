# Release readiness: v0.3.8 (proposed)

Not tagged, not deployed. Prepared for the coordinator ahead of the Wed 28 Oct 09:00
`nfl-weekly-spark` run and the ADR 0122 chain — release timing is the coordinator's call, this
only covers what's actually in the diff.

## 1. What's on master since v0.3.7 (the last tagged release)

`git log v0.3.7..master` — four PRs, three logically separate pieces of work:

| PR | Commit(s) | What |
|---|---|---|
| #25 | `0f059cb` | ADR 0123 confirmations + pins `BOOTH_EVENTS`'s JetStream duplicate window at 2h (was an unconfigured, accidental 2-minute server default) |
| #27 | `dc28fdc` | Fixes a real, pre-existing race in two `internal/sidecar` renewal **tests** — `internal/sidecar/renewal.go` itself is untouched, zero production-code change |
| #26 | `6cab06b` | ADR 0124 design note — docs only, zero code |
| #28 | `2dd229a` | ADR 0124 build: the project registry, bindings, and the iframe-url `?project=` passthrough |

Nothing else is on master since v0.3.7. No chart or `values.yaml` change anywhere in this range
(`git diff v0.3.7..master --stat -- charts/` is empty) — this is a pure application-code release,
no operator-facing Helm changes.

## 2. Proposed version: v0.3.8

This repo's own convention (confirmed: `charts/booth-core/Chart.yaml`'s `version`/`appVersion`
are inert placeholders — the release workflow stamps everything from the git tag name, nothing
in-repo tracks a "current version") increments the patch digit for every release batch
regardless of whether it's a fix or a feature-sized addition (v0.3.4 → v0.3.5 → v0.3.6 → v0.3.7
each did this, including when the batch added a whole new subsystem like service accounts).
Nothing in this batch is a breaking change (see §3/§4), so **v0.3.8** keeps that pattern. A
minor/major bump isn't warranted by anything here — raise it if you want ADR 0124's new API
surface to mark a bigger line than this repo's history treats as the norm.

## 3. Database migrations

**Two new tables, both purely additive, created lazily on first use** — not a startup migration
step. `project.PostgresStore.ready()` runs `CREATE TABLE IF NOT EXISTS` the first time any
project route is actually called (the same lazy pattern `internal/serviceaccount` already uses,
already running in production since v0.3.7's service accounts). Until then, the tables don't
exist at all; nothing in this release forces their creation at startup or during the Helm
upgrade itself.

```sql
booth_projects          (workspace, slug) PRIMARY KEY, name, parent_slug, description, color,
                         icon, archived, created_by, created_at
booth_project_bindings  (workspace, id) PRIMARY KEY, project_slug, kind, backend_id, value,
                         created_by, created_at, UNIQUE (workspace, kind, backend_id, value)
```

- **No existing table changes at all.** No `ALTER TABLE`, no column added/removed/renamed, no
  nullability change, on `booth_service_accounts`, the directory tables, or anything else.
- **Not backward-incompatible in either direction.** v0.3.7's code never queries these two new
  tables (it doesn't know they exist), so running v0.3.7 against a database that already has
  them is a no-op for those rows — they just sit there, unused and harmless.

**Rollback story: a Helm rollback is safe at any time, including after the new tables have been
created and populated.** There is no scenario here that needs the database backup first:
- Rolling back swaps the image/chart back to v0.3.7's code. That code has no SQL referencing
  `booth_projects`/`booth_project_bindings` — it simply never touches them, whether they're
  empty or have real rows in them.
- No data from any *other* table is modified or deleted by this release, so there's nothing to
  restore even in the worst case.
- The only thing a rollback actually loses is forward progress on project data itself (any
  project/binding someone created while v0.3.8 was live disappears from use, though not from the
  database — it's still there, inert, for whenever v0.3.8 is redeployed).

A database backup before the upgrade is still good general practice (it always is), but nothing
in this specific change makes it a prerequisite for a safe rollback.

## 4. Chart/values changes

**None.** Confirmed by diff, not assumed: zero files under `charts/` changed since v0.3.7. No
new chart value, no new required env var, no new Secret/ConfigMap field. The project store reads
the exact same `BOOTH_POSTGRES_DSN`/`BOOTH_POSTGRES_HOST` configuration service accounts already
use — there's nothing for an operator to set.

## 5. Post-upgrade checklist

```
# Registry routes exist and answer correctly
GET  /api/projects                                    -> 200 []            (empty install, real Postgres connected)
POST /api/projects   (as a non-owner)                  -> 403
POST /api/projects   (as an owner) {"slug":"x","name":"X"} -> 201, "parent":null in the body
PATCH /api/projects/x (as a non-owner)                 -> 403
PATCH /api/projects/x (as an owner) {"archived":true}  -> 200

# iframe-url passthrough
GET /api/modules/{id}/iframe-url?project=<slug>        -> 200 {"url": ".../iframe/{id}/?booth_iframe_token=...&project=<slug>"}
GET /api/modules/{id}/iframe-url?project=BAD!SLUG      -> 400
GET /api/modules/{id}/iframe-url    (no project param) -> 200, url unchanged from v0.3.7 — no "project" in it at all

# Existing paths unaffected (regression check, not new behavior)
- Ordinary login / GET /api/me                         -> unchanged
- An existing iframe-proxied module (no ?project=)      -> loads exactly as before
- A native-mode module's own API calls through the gateway -> unaffected (gateway's Director
  never touches query strings on this path; confirmed by reading the code, not just the tests)
```

`GET /api/projects` returning `[]` rather than `null` on empty is a verified code fact, not an
assumption: `handleListProjects` always builds the response from `make([]projectView, 0, ...)`,
which Go's `encoding/json` always renders as `[]`, regardless of whether the query returned zero
rows or a nil slice. The precondition for this is a **real Postgres connection** — if no database
is configured at all (dev-mode / a deployment with no Postgres), every project route 503s
instead, same as service accounts already do.

## 6. Effect on a running pipeline or the event bus during the upgrade

- **The bundled NATS pod is not touched.** No NATS chart value or template changed, so a
  `helm upgrade` here only restarts booth-core's own pod — NATS keeps running, with no
  disruption to the event bus itself.
- **The duplicate-window change (PR #25) is a live JetStream config update, not a stream
  recreation.** Core calls `CreateOrUpdateStream` on every connect/reconnect; this updates
  `BOOTH_EVENTS`'s existing configuration in place. No messages are deleted, no consumer is
  reset, no subject filter changes. The only observable effect is that a publisher retrying the
  same message id (today, nothing retries like this yet — it's forward-looking for booth-api's
  future outbox relay from ADR 0123) gets a longer dedup window than the accidental 2-minute
  default it had before.
- **`booth-core`'s own pod restart is the one real moment of risk, and it's the ordinary kind
  every core release already carries, not something new here.** `replicaCount: 1` by default
  with a configured `readinessProbe` (`/healthz`) means Kubernetes's default rolling-update
  behavior brings up the new pod and waits for it to pass readiness before killing the old one —
  in principle close to zero-downtime, though a resource-constrained homelab node may not have
  headroom for both pods briefly coexisting, in which case there's a short gap where core's
  gateway (and therefore the module proxy and workload-token mint endpoint) is unavailable.
  A pipeline task mid-run whose sidecar tries to renew a lease during exactly that gap is
  unaffected beyond a delayed retry — confirmed by the same test suite PR #27 just hardened:
  `TestRenewer_RenewalFailureNeverDropsTheCurrentLeaseOrExitsRun` is precisely "a renewal failure
  during an outage never drops the current lease or exits the run." Nothing about this release
  changes that guarantee; it's unaffected production code.
- **No part of this release touches `booth-pipeline`, service accounts, or any other module's
  own data path.** The only new consumer of core's API surface is a module that hasn't shipped
  yet (the project-aware list/filter work is still ahead, per ADR 0124's module prompts) — so
  there is no existing caller that starts behaving differently the moment this deploys.

## Still gating the release (not this note's call)

The Wed 28 Oct 09:00 `nfl-weekly-spark` run and the ADR 0122 chain, per your message. Nothing in
this note changes that sequencing.
