# booth-core decision 0013: How the bundled Postgres pod gets pinned to its node (ADR 0083)

Status: **implemented.** ADR 0083 decides the outcome — the bundled Postgres `StatefulSet`'s pod
must stay on the node its `local-path-provisioner` volume actually lives on, via a `nodeAffinity`
or `nodeSelector` "keyed on a label set at first schedule" — and leaves the mechanism to core.
This is the mechanism, and why it can't be a static value in the chart.

## The chicken-and-egg problem

The pod's node isn't known until it has been scheduled at least once. A *required* nodeAffinity
baked statically into the chart from day one, referencing a label no node has yet, would leave the
very first install's pod permanently unschedulable (0 matching nodes) — breaking every fresh
single-node install, which is the common case this platform optimizes for. The pin has to be
applied **after** the fact, against the live cluster, once the node is actually known.

## What was decided

### 1. `kubernetes.io/hostname`, not a custom label

ADR 0083 names this label directly as an option, and it's the right one: every node in every
distribution (k3s included) carries it by default, so pinning needs no separate step that labels
`Node` objects themselves — it only ever reads `Pod.Spec.NodeName` (which, in every standard
cluster, equals the node's `kubernetes.io/hostname` value) and writes that string into the
StatefulSet's own `spec.template.spec.nodeSelector`. No new RBAC verbs beyond what core's chart
already grants on `StatefulSet`/`Pod` (its ClusterRole is already `apiGroups: ["*"], resources:
["*"], verbs: ["*"]` — see `charts/booth-core/templates/rbac.yaml`'s own comment on why).

### 2. `internal/dbprov.EnsureNodeAffinity`: read-then-patch, idempotent, self-healing

Mirrors `EnsureAdminPassword`/`EnsureBackupClaim`'s existing pattern in the same package — core
bootstrapping its own bundled-Postgres infra directly against the live cluster, not through Helm:

1. Get the ordinal-0 pod (`<statefulSetName>-0` — the chart names the Service, StatefulSet, and
   pod all `<release>-postgresql`, so the caller only has to know one name,
   `cfg.Postgres.Host`, which is already that string). Not found, or found with no
   `Spec.NodeName` yet: return `pinned=false, err=nil` — not a failure, just "too early". The
   caller decides what "too early" means (see below).
2. Get the StatefulSet. If its `nodeSelector[kubernetes.io/hostname]` already equals the pod's
   node, return `pinned=true` and touch nothing — no patch call at all, so re-running this on
   every tick costs nothing once steady-state is reached.
3. Otherwise, `client.MergeFrom` + `Patch` **only** `spec.template.spec.nodeSelector` — a merge
   patch touches exactly the field it sets, so this can run against a real Helm-managed object
   without fighting Helm's own reconciliation of every other field (verified directly:
   `TestEnsureNodeAffinity_TouchesOnlyTheNodeSelector`, and Helm's own upgrade diffing never
   reverts a field it didn't render either way).

Because step 2 recomputes from the pod's *current* node every time rather than remembering "already
pinned once", this is self-healing for free: if the pin is ever removed, or a volume is manually
migrated to a different node (an accepted, low-priority edge case the ADR doesn't require handling),
the next check simply re-pins to wherever the pod actually is now.

### 3. A backgrounded, retrying caller — not a blocking boot step

Unlike `EnsureAdminPassword` (which must complete *before* the Postgres pod can even start, since
the pod waits on the Secret it creates), pinning inherently needs the pod to already exist and be
scheduled — the opposite ordering. So it can't block `cmd/core`'s startup the way the other bundled-
Postgres bootstrap steps do. `pinBundledPostgresNode` runs as a background goroutine (gated on
`cfg.Postgres.Bundled`, alongside the other bundled-only bootstrap steps): retries with backoff
(1s → 30s cap) while there's nothing to pin to yet, then — once pinned — settles into a slow,
5-minute steady-state recheck for the rest of the process's life, the same "cheap ongoing
self-heal" shape `dbprov`'s own `recheckInterval` already uses for database existence checks.

## Honest residual limits

1. **A pod that's never successfully scheduled anywhere never gets pinned** — but it also never
   needs to be, since there's nothing to protect yet.
2. **The window between "pod first lands on node A" and "core successfully patches the pin"** is
   real but small (one backoff cycle, ≤30s once caught up): a reschedule that happens to land in
   exactly that window could still, in principle, strand the pod. Closing it fully would need a
   synchronous wait tied to pod-Running status rather than a background retry loop — not done here,
   since `helm install --wait` already keeps an operator from treating the release as "up" before
   core (and shortly after, the pin) has had time to settle, and a reschedule racing that exact
   narrow window on a brand-new install is a very different risk profile from the steady-state
   protection this exists for.
3. **Doesn't touch `ARCHITECTURE.md`/`MODULE_REGISTRY.md`** — per ADR 0083's own consequences,
   those are `booth-architecture`'s edits, not core's.
