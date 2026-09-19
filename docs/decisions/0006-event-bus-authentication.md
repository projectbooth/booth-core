# booth-core decision 0006: How the event bus is authenticated (ADR 0049)

Status: **implemented; two contract additions flagged back to booth-architecture** — the
`events` manifest field and the credential Secret convention below. ADR 0049 mandates the
outcome (authenticated publishers, per-subject publish permissions, credentials via the
ADR 0020 Secret mechanism, a NetworkPolicy); it doesn't specify the mechanism or where the
permissions come from. Those are the calls made here.

## What was decided

### 1. NATS JWT operator/account mode, with core as the credential authority

Every participant is a NATS *user* inside one account (`BOOTH`). Core holds the account's
signing key and mints a signed user credential per module, with the module's publish/subscribe
permissions embedded in the credential. The NATS server trusts only core's operator key.

**Why this over static users in `nats.conf`:** modules are installed and removed at runtime
(`POST /api/modules/{id}/install`, or a plain `helm install`). With static users every install
would mean rewriting the server config and reloading NATS. With signed credentials the server
config is written **once** at bootstrap and never changes when modules come and go — the same
"no live reconfiguration" property ADR 0020 wants for secrets generally.

Bootstrap (`internal/natsauth`, run at core start, before anything waits on NATS):
1. Generate operator / account / account-signing / system-account keys → Secret
   `booth-nats-keys` (only core reads it).
2. Render the server trust config → ConfigMap `booth-nats-auth` (public JWTs only, no seeds).
3. The bundled NATS chart mounts that ConfigMap and `$include`s it (see `values.yaml`); the
   config reloader sidecar watches it.

Idempotent and safe under several core replicas (create-then-adopt on `AlreadyExists`). Core
does **not** wait on NATS to do this — the NATS pod can't start until the ConfigMap exists, so
core connects to the bus later, in the background, with retry.

### 2. Permissions come from the module's manifest (**contract addition — flag**)

Core is "the one party that knows every module's legitimate subject scope" (ADR 0049), but that
knowledge has to come from somewhere. Rather than a central table in core's config (which would
make core know about every module, against ARCHITECTURE.md §3), each module declares it in its
own `BoothModule`:

```yaml
spec:
  events:
    publish:   ["dashboard.*"]      # -> may publish booth.*.dashboard.*
    subscribe: ["dashboard.*"]      # -> may use the JetStream consumer API on BOOTH_EVENTS
```

Optional; **a module that omits `events` gets no credential and cannot connect to the bus.**
Patterns are validated twice (CRD schema, and again in core): dotted lowercase tokens, first
token literal, `*` allowed after that — so no manifest can declare `>` or `*.*` and reach every
event type. Trust here is the same as for every other manifest field: installing a chart is an
owner-gated action; the boundary being defended is *pods forging events*, not *an owner
installing a hostile chart*.

Each pattern applies across **all** workspaces (`booth.*.<pattern>`). ADR 0049 says "ideally
only for workspaces it's actually installed into", but modules are platform-wide and serve
every workspace, so a narrower NATS permission isn't meaningful.

### 3. Credential Secret convention (**contract addition — flag**)

Per ADR 0020, core writes, ahead of time, into the module's namespace (the `BoothModule`'s
`serviceRef.namespace`, else its own):

| | |
|---|---|
| Secret name | `booth-event-bus-credentials` |
| `nats.creds` | a standard NATS `.creds` file (`nats.UserCredentials(path)` reads it as-is) |
| `url` | in-cluster URL to connect to |

The module's chart mounts or reads it the ordinary Kubernetes way. It's owned by the
`BoothModule` (garbage-collected on uninstall) when in the same namespace. Credentials last 90
days and are re-minted 30 days before expiry, and immediately when `events` changes.

### 4. Default-deny allow-list; the stream itself is protected

Anything not granted is refused by the server. Notably, **no module gets JetStream
stream-management** (`STREAM.DELETE/PURGE/UPDATE`, `MSG.DELETE`, `CONSUMER.DELETE`): a module
can read and consume, but cannot destroy or rewrite the shared stream. Verified against a real
server in `internal/natsauth/authority_test.go`. Only core has full access (it creates the
stream).

### 5. NetworkPolicy (`templates/networkpolicy-nats.yaml`)

NATS ingress is admitted only from core, other NATS pods, and namespaces core has labelled
`booth.projectbooth.io/event-bus-client=true` — which it does for each namespace hosting a
module that declared `events`. Enforced only if the CNI implements NetworkPolicy (k3s does).
`nats-box` (an unauthenticated CLI pod) is disabled.

## Honest residual limits (not solved by this)

1. **`publishedBy` is still advisory.** NATS permissions are per *subject*, and the three
   dashboard modules share `dashboard.*`, so `booth-superset` can still publish an event
   claiming `publishedBy: "metabase"`. Closing that needs a publisher-scoped subject (a change
   to ADR 0026) or signed envelopes. Forging *event types a module doesn't own* — the
   demonstrated gap — is closed.
2. **Subscriber read-confinement is a capability gate, not per-subject.** A module declaring
   `subscribe` can create a consumer with any filter, and JetStream consumer-API subjects aren't
   scoped to a consumer name — so one subscriber could in principle interfere with another's
   durable consumer or read event types it didn't declare. Requires an already-credentialed,
   already-installed module; lower severity than forgery.
3. **No online revocation.** Uninstalling stops renewal and the Secret is garbage-collected,
   but a copied credential stays valid until it expires (≤ 90 days). Shorter TTLs trade
   against reload churn in modules; revisit if it matters.
4. **Losing `booth-nats-keys` regenerates every key**, invalidating all credentials and
   orphaning the JetStream data (it lives under the old account). Back the Secret up.
5. **Modules must migrate.** Any module that connects to NATS today without credentials stops
   working once this ships: it must add `events` to its `BoothModule` and read the Secret. This
   is the intended hardening, and it's why the chart doesn't offer an off switch (NATS would wait
   forever on a ConfigMap core no longer writes).

## Flagged back to booth-architecture

- `contracts/module-manifest.md`: add the optional `events` field (`publish` / `subscribe`
  lists of event-type patterns).
- `contracts/core-platform-api.md`: replace the "Not yet enforced" note in the event-bus
  section; document the credential Secret convention above.
- Every publishing/subscribing module's brief (`booth-catalog`, `booth-superset`,
  `booth-metabase`, `booth-streamlit`, later `booth-pipeline`, `booth-spark`): declare `events`,
  read the Secret.
- Decide whether residual limit 1 (publisher identity within a shared event type) is worth an
  ADR of its own.
