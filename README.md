# booth-core

Project Booth's mandatory control plane: pluggable-OIDC auth, multi-workspace
management, module registry/lifecycle, gateway, and event bus. The only repo every
deployment of Project Booth must run. See `../booth-architecture/agent-briefs/core.md`
for the full brief this repo builds against.

## Stack

Go (`docs/decisions/0004-backend-language-go.md` explains why), using:

- `sigs.k8s.io/controller-runtime` — the `BoothModule` CRD watch/reconcile loop
  (ADR 0019) and the Secret/ConfigMap provisioning reconciler (ADR 0020).
- `github.com/coreos/go-oidc` — OIDC discovery/JWKS-based JWT verification (ADR 0004).
- `github.com/nats-io/nats.go` (`jetstream` package) — the event bus (ADR 0021).
- `helm.sh/helm/v3` — module install/uninstall (ADR 0003).
- `github.com/go-chi/chi` — HTTP routing.

## Repo layout

```
api/v1alpha1/        BoothModule CRD Go types (contracts/module-manifest.md's transport, ADR 0019)
cmd/core/             Entrypoint: wires every subsystem together
internal/auth/        OIDC verification + workspace/role derivation (ADR 0004, 0008)
internal/gateway/      Sync request routing + iframe-proxy (ADR 0007, ui-integration.md)
internal/registry/     BoothModule CRD controller + in-memory module registry (ADR 0019)
internal/devregistry/  Static-file registry fallback for local dev without a cluster
internal/eventbus/      NATS/JetStream pub-sub (ADR 0007, 0021)
internal/secrets/       Secret/ConfigMap provisioning (ADR 0020)
internal/lifecycle/     Module install/uninstall via the Helm SDK (ADR 0003)
internal/api/           HTTP router tying the above together
internal/config/        Env-var configuration
config/crd/bases/       Generated BoothModule CRD YAML
charts/booth-core/      Helm chart (bundles NATS, the CRD, RBAC, the core Deployment)
docs/decisions/         Decisions this repo made on brief-flagged open questions (see below)
test/contract/          Layer-2 tests: contract fixtures vs. Go types, no cluster needed
test/integration/       Layer-3 tests: real (envtest) kube-apiserver, no Docker needed
hack/                    Local-dev convenience files (example dev-registry.yaml, etc.)
```

## Running locally

**Without a cluster** (fast iteration on the gateway/API surface):

```sh
go build -o bin/booth-core ./cmd/core
BOOTH_DEV_REGISTRY_PATH=hack/dev-registry.example.yaml BOOTH_HTTP_ADDR=:8080 ./bin/booth-core
```

This skips the real `BoothModule` CRD watch and OIDC verification setup — see
`docs/decisions/0003-local-dev-registry-fallback.md`. `/healthz` will respond; `/api/*`
routes need `BOOTH_OIDC_ISSUER_URL`/`BOOTH_OIDC_CLIENT_ID` pointed at a real (or
locally-run) OIDC provider to actually authenticate requests.

**Against a real cluster**: `helm install booth-core charts/booth-core --namespace
booth-system --create-namespace --set oidc.issuerUrl=... --set oidc.clientId=... --set
iframeSigningKey=$(openssl rand -hex 32)`.

## Testing

Per `contracts/testing-strategy.md` (ADR 0024):

```sh
go build ./...
go vet ./...
gofmt -l .                 # should print nothing
go test $(go list ./... | grep -v /test/integration)   # layers 1-2, no cluster
```

Layer 3 (`test/integration/`) needs `KUBEBUILDER_ASSETS` pointed at envtest's
kube-apiserver/etcd binaries (`go install
sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.19`, then `setup-envtest use
1.31.0 -p path`) — no Docker/kind required for this layer, since it only exercises real
API-server CRUD + the reconcile loop, not a full pod-scheduling cluster. CI additionally
runs a real `kind` deploy of the Helm chart (`.github/workflows/integration.yml`'s
`kind-deploy` job) to catch chart/RBAC/rollout issues envtest can't.

## Decisions made here, flagged back to booth-architecture

Per this repo's brief, three of `agent-briefs/core.md`'s open questions needed a
concrete answer before v0 could be built, and are documented (with reasoning) in
`docs/decisions/`:

- **0001 — workspace-role claim shape**: the `groups` claim →
  `/workspaces/<slug>/<owner|editor|viewer>` grammar, and how the active workspace is
  resolved per-request (`X-Workspace` header, validated against the token).
- **0002 — NATS subject naming**: `booth.<workspace>.<event-type>` subjects, one shared
  `BOOTH_EVENTS` JetStream stream, and a minimal event envelope.
- **0005 — module lifecycle desired state**: v0 ships on-demand install/uninstall only
  (no drift-reconciling desired-state store); where a Module Store catalog of installable
  modules actually lives is unresolved and needs `booth-design`'s Module Store design.

Two more are recorded but don't need architecture-level sign-off (repo-internal
conventions, not cross-cutting contracts):

- **0003 — local-dev registry fallback** shape.
- **0004 — backend language** choice (Go) and why.

**These need review and promotion to `booth-architecture` ADRs (0001, 0002, 0005) before
another module bakes in an assumption about them** — per the ground rule in
`booth-architecture`'s README, this repo doesn't get to unilaterally settle cross-cutting
contract questions.

## What's built vs. what's left, against the v0 definition of done

Built and tested (unit/contract tests in-repo; the CRD reconcile loop additionally
verified against a real API server via `test/integration/`):

- OIDC auth (JWKS/issuer/expiry/audience verification) + workspace-role derivation.
- Module registry: `BoothModule` CRD watcher/controller with health polling and status
  subresource updates; static-file fallback for local dev.
- Gateway: sync module-to-module/UI routing with identity attachment; iframe-proxy
  (token-in-query-param + scoped cookie, plus the root-relative-follow-up-call fallback
  ui-integration.md calls out as a known failure mode to design around).
- Event bus: NATS/JetStream wiring, `BOOTH_EVENTS` stream, publish/subscribe helpers.
- Secrets/ConfigMap provisioning primitive (ADR 0020).
- Module install/uninstall via the Helm SDK, admin-role-gated (scoped per decision 0005).
- Helm chart: bundles NATS (subchart), the CRD, RBAC, the core Deployment/Service.
- CI: unit+contract on push/PR, layer-3 (envtest + a real `kind` chart deploy) on
  merge-to-main/nightly, tagged-release image+chart publishing.

Not yet built:

- Hosting `booth-design`'s shell — blocked on that repo existing (currently "not
  started" in `MODULE_REGISTRY.md`); nothing to host yet.
- Workspace *metadata* persistence (display name, settings) on the shared PostgreSQL
  cluster (ADR 0014) — membership/role itself is token-derived per decision 0001 and
  needs no database, but workspace creation/metadata does. `internal/config` already
  reads a Postgres DSN; no schema/queries exist yet.
- Drift detection/reconciliation for module lifecycle (decision 0005's flagged gap).
- Least-privilege RBAC for the lifecycle manager (currently broad by necessity/default —
  see `charts/booth-core/templates/rbac.yaml`'s comment).
