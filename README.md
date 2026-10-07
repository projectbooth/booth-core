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
internal/natsauth/      Event-bus authentication: credentials + per-subject permissions (ADR 0049)
internal/directory/     Minimal user directory: sub -> display claims (ADR 0047)
internal/dbprov/        Shared PostgreSQL: per-module database + role provisioning (ADR 0053)
internal/workload/      Workload identity: core as a second token issuer for unattended runs (ADR 0056)
internal/iframeidentity/ Iframe-proxy identity assertion: core as a third token issuer, for the iframe-proxy path (ADR 0069)
internal/credentialbroker/ Credential broker: routes an authorized request for a native-protocol credential (ADR 0080)
internal/sidecar/       Credential sidecar's shared core: broker client, renewal loop, postgres proxy, s3 file writer (ADR 0095)
internal/sidecar/pgwire/ Just enough Postgres wire protocol (incl. a real SCRAM-SHA-256 client) for the sidecar's proxy mode
cmd/credential-sidecar/ The credential sidecar binary + its own Dockerfile (ghcr.io/projectbooth/credential-sidecar, ADR 0095)
internal/testpg/        Real PostgreSQL for tests (embedded, no Docker)
internal/secrets/       Secret/ConfigMap provisioning (ADR 0020)
internal/lifecycle/     Module install/uninstall via the Helm SDK (ADR 0003)
internal/api/           HTTP router tying the above together
internal/config/        Env-var configuration
config/crd/bases/       Generated BoothModule CRD YAML
charts/booth-core/      Helm chart (bundles NATS, the CRD, RBAC, the core Deployment)
docs/decisions/         Decisions this repo made on brief-flagged open questions (see below)
docs/runbooks/          Operator runbooks (PostgreSQL backup, restore, major-version upgrade)
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

Two suites run real servers in-process rather than mocks: `internal/natsauth` starts a real
NATS/JetStream server to prove event-bus permissions are enforced, and `internal/directory`
runs its store contract against a real PostgreSQL (an embedded build, downloaded on first
run — no Docker; set `BOOTH_SKIP_POSTGRES_TESTS=1` to skip offline, or
`BOOTH_TEST_POSTGRES_DSN` to use your own).

Layer 3 (`test/integration/`) needs `KUBEBUILDER_ASSETS` pointed at envtest's
kube-apiserver/etcd binaries (`go install
sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.19`, then `setup-envtest use
1.31.0 -p path`) — no Docker/kind required for this layer, since it only exercises real
API-server CRUD + the reconcile loop, not a full pod-scheduling cluster. CI additionally
runs a real `kind` deploy of the Helm chart (`.github/workflows/integration.yml`'s
`kind-deploy` job) to catch chart/RBAC/rollout issues envtest can't.

## Decisions made here, since resolved at the architecture level

Per this repo's brief, three of `agent-briefs/core.md`'s open questions needed a
concrete answer before v0 could be built. Each was documented with reasoning in
`docs/decisions/`, flagged back, and has since been reviewed and promoted to a
`booth-architecture` ADR:

- **0001 — workspace-role claim shape** → **ADR 0025**, accepted exactly as proposed. The
  `groups` claim → `/workspaces/<slug>/<owner|editor|viewer>` grammar, and how the active
  workspace is resolved per-request (`X-Workspace` header, validated against the token),
  are now settled architecture-wide, not just a booth-core convention.
- **0002 — NATS subject naming** → **ADR 0026**, accepted exactly as proposed.
  `booth.<workspace>.<event-type>` subjects, one shared `BOOTH_EVENTS` JetStream stream,
  and the event envelope are now the contract every publishing/subscribing module builds
  against.
- **0005 — module lifecycle desired state** → **ADR 0027**, resolved differently than any
  of the three candidates this repo's decision considered: a new mandatory repo,
  `booth-module-store`, owns the installable-module catalog and Module Store UI end to
  end, calling this repo's existing on-demand install/uninstall API. **No code changes
  were required here** — see `docs/decisions/0005` for detail.

Two more are recorded but never needed architecture-level sign-off (repo-internal
conventions, not cross-cutting contracts):

- **0003 — local-dev registry fallback** shape.
- **0004 — backend language** choice (Go) and why.

## Follow-up fixes from downstream consumers' first build passes (2026-09-18)

`booth-module-store` and `booth-design` both built against this repo's real API and
flagged concrete gaps, resolved as **ADR 0028** and two direct bug reports respectively:

- **`POST /api/modules/{id}/install` accepts a `chartRef` string** (e.g.
  `oci://host/path/chart-name`) paired with `chartVersion`, as an alternative to the
  structured `chart` object — resolved internally by `internal/lifecycle.ParseChartRef`,
  lifted from `booth-module-store`'s own interim parser (ADR 0028). `chartRef` takes
  precedence if a caller supplies both; at least one of `chartRef` or `chart` is now
  required (previously an empty structured `chart` object would fail later, inside Helm,
  with a less legible error).
- **`GET /api/modules` now includes `uiIntegrationMode`** in each entry — it was already
  on the underlying `BoothModule` spec but missing from the response `moduleView`,
  so `booth-design`'s shell couldn't distinguish native modules from iframe-proxy ones.
- **`GET /api/me`'s nested fields now serialize lowerCamelCase** (`workspace`/`role`
  inside `memberships[]`/`active`) — `auth.Membership` was missing `json` struct tags,
  so those two fields alone came back capitalized while everything else in the response
  was already lowerCamelCase.

`contracts/core-platform-api.md` already documented ADR 0028's accepted shape ahead of
this implementation; the only wording gap filled in was calling out the `chartVersion`
companion field explicitly, since the contract's prose only named `chartRef`.

## Event-bus authentication and the user directory (2026-09-19)

Two action items from `booth-catalog`'s first build pass; reasoning and the calls that need
architecture-level review are in `docs/decisions/0006` and `0007`.

**ADR 0049 — the event bus is authenticated.** Previously any pod that could reach NATS could
forge a `dashboard.*` event into any workspace as any publisher. Now NATS runs in JWT
operator/account mode with core as the credential authority: it mints a signed, expiring
credential per module whose publish permissions come from the module's `BoothModule`
`spec.events` (`publish`/`subscribe` lists of event-type patterns). No `events` means no
credential. Credentials are written to the module's namespace as the Secret
`booth-event-bus-credentials` (`nats.creds`, `url`) via the ADR 0020 mechanism, and a
NetworkPolicy restricts who can reach NATS at all. No module can delete or purge the shared
stream. **Modules that connect to NATS today must declare `events` and read that Secret** —
see decision 0006 for the residual limits (notably: `publishedBy` is still advisory).

**ADR 0047 — user directory.** `GET /api/users/{sub}` and `GET /api/users?q=&limit=`,
populated from every verified token. Reads are scoped to the caller's active workspace (a
call beyond ADR 0047's text, flagged in decision 0007). Backed by PostgreSQL when
`BOOTH_POSTGRES_DSN` is set, otherwise an in-memory fallback that repopulates itself.

## Shared PostgreSQL and per-module databases (2026-09-19)

**ADR 0053.** The chart bundles a default single-node PostgreSQL (a small StatefulSet on the
official image), swappable for an external HA cluster via `postgresql.enabled=false` and
`postgres.external.*`. Core provisions a database and login role for itself and for every module
whose `BoothModule` declares `database: {enabled: true}`, and delivers the connection details as
the Secret `booth-database-credentials` (`dsn` plus `host`/`port`/`database`/`username`/
`password`) in the module's namespace. Roles are unprivileged and each database is closed to every
other module's credentials. Uninstalling a module **never drops its data**. Core's own user
directory uses the same mechanism, so a default install now persists it. See
`docs/decisions/0008`.

**Backup (ADR 0054).** The bundled server is single-node, but it is backed up: a daily `pg_dump`
CronJob writes to a PVC (default) or an S3-compatible bucket (`postgresql.backup.s3.*`) — a
baseline, **not** point-in-time recovery, and the default PVC doesn't survive losing the cluster.
Restore and major-version upgrade are manual: `docs/runbooks/postgres-backup-restore.md`
(`docs/decisions/0009`). Connecting to an **external** PostgreSQL with
`postgres.external.restrictMaintenanceAccess` left off logs a startup warning (and prints one on
`helm install`): PostgreSQL lets any role connect to the maintenance databases by default, so a
module's credentials can list other databases' and roles' names.

**Node pinning (ADR 0083).** The bundled server's `local-path-provisioner` volume is bound to
whichever node first created it; core now pins the StatefulSet to that same node
(`nodeSelector: kubernetes.io/hostname`) once it's known, so a reschedule on a multi-node cluster
(a rollout restart, a `helm upgrade`, a node drain) can't strand the pod away from its data. Applied
after the fact by a backgrounded, self-healing bootstrap step — it can't be static in the chart,
since the node isn't known until the pod has been scheduled once. See `docs/decisions/0013`.

## Workload identity for unattended runs (2026-09-21)

**ADR 0056.** A scheduled run has no human token, so core is a second trusted issuer for one narrow
purpose: a module whose `BoothModule` declares `workloadIdentity: {mint: true}` gets the Secret
`booth-workload-minting-credentials` (`credential`, `url`, `issuer`) in its namespace, and calls
`POST /api/internal/workload-tokens` with `{workspace, subject, roleCeiling, owner}` to get a
10-minute RS256 JWT. Its `groups` claim is `["/workspaces/<workspace>/<role>"]` (ADR 0025's grammar),
so a module's existing role derivation reads it unchanged; the only thing a module adds is trusting
`<issuer>/.well-known/jwks.json` (or OIDC discovery at the issuer URL) as a second issuer. The role is
the lesser of `roleCeiling` and the owner's role **as of that mint** - nothing is cached.

Two things to know, both flagged back in `docs/decisions/0010`: the request needs an `owner` field
ADR 0056 doesn't list, and "live" means *the owner's most recent verified token*, bounded by
`workloadIdentity.ownerMaxAge` (7 days), because core has no other source for a role.

**Gateway (ADR 0059).** The `/modules/{id}/*` route also accepts these workload tokens, verified against
core's own key and dispatched by `iss`, so a job's call to another module is routed and identity-stamped
like a person's. Only that route: core's own `/api/*` routes (install/uninstall, ...) still reject them.
See `docs/decisions/0011`.

## Iframe-proxy identity assertion (2026-09-23)

**ADR 0069.** The iframe-proxy path (`uiIntegrationMode: iframe-proxy`) has no bearer token at
all — a plain iframe navigation can't carry one, and the browser's own OIDC token never leaves
memory (ADR 0032) — so core mints a third, distinct token class: on every proxied request, both
`IframeEntryHandler` and `IframeFallbackHandler` attach a signed `X-Booth-Identity` JWT (`aud` the
target module id, `sub` the person's own subject, `groups` in ADR 0025's grammar, `exp` ≤ 2
minutes). A dedicated header, not `Authorization`, since JupyterHub/jupyter-server, Superset, and
Metabase each parse `Authorization` as their own API token. Published via its own discovery
document + JWKS at `<core's base URL>/iframe-identity`, deliberately a separate issuer (and key)
from the workload-token issuer (ADR 0056/0058): a workload token is never a person, and a module
trusting one issuer class must not implicitly accept the other. A client-supplied
`X-Booth-Identity` is always stripped before core's own is attached (or omitted, if minting
fails) — never forwarded. See `docs/decisions/0012` for implementation calls ADR 0069 left open
(the route-prefix scheme, the dev-mode ephemeral key, minting-failure behavior).

**Two real bugs found by `booth-notebooks`' end-to-end verification, fixed as corrections to this
same design**: `IframeFallbackHandler` now refuses a top-level document navigation
(`Sec-Fetch-Dest: document`) rather than silently proxying it into whatever module has a live
session cookie; and `IframeURLIssuer.URLFor` mints a relative `/iframe/<id>/...` URL instead of one
built from `BOOTH_PUBLIC_BASE_URL`, which no chart value ever set (every real deployment's iframe
URL pointed at `localhost:8080`). See `docs/decisions/0012`'s "Implementation notes".

## Credential broker (2026-09-29)

**ADR 0080.** For a raw native-protocol credential (a scoped S3 credential, a Postgres
connection) that can't go through the gateway's HTTP path — deliberately kept apart from
`platform_access`/`grant()`, since a checked gateway token can be revoked mid-flight and a raw
credential handed to a process can't be. `POST /api/credentials` (always on) authorizes an
already-resolved caller (human or workload token — the same `WithWorkloadVerifier` exception the
gateway route already has, ADR 0059/ADR 0084) against `{kind, ttlSeconds, access, scope,
options}`, routes to whichever module declares `providesCredentials: {kinds: [...]}` for that
`kind`, and relays exactly what it mints — never inspecting the credential itself. `access`
(`read`/`readwrite`) gates on role (editor/owner for write, ADR 0048's precedent); `ttlSeconds` is
clamped to a 5-minute ceiling, deliberately stricter than a workload token's 10 minutes. Every
issuance is recorded in a genuinely append-only, Postgres-backed audit trail
(`booth_credential_issuances`) that structurally cannot hold a credential value. A provider is
authenticated the same derived-credential way workload minting already works
(`booth-credential-broker-provider-credentials`), calling a fixed `POST /internal/credentials` on
its own `BaseURL()`. See `docs/decisions/0014` for the full design, including the ADR 0084 ruling
on non-person identity for unattended renewal (not needed — the existing `owner` field already
supports naming any current workspace owner).

## Platform-operator claim (2026-09-30, iframe-proxy path 2026-10-07)

**ADR 0094, amending ADR 0025.** A second, workspace-independent claim shape: the literal string
`/platform/operator` in a token's `groups` claim marks the caller as a platform operator —
cross-tenant admin views (`booth-logging`'s log viewer, `booth-lakehouse`'s and `booth-database`'s
admin views) aren't a per-workspace concern, and this replaces the per-module operator-allowlist
stopgap (ADR 0067) three modules had independently reinvented. **No code changed for a module that
reads its own caller's bearer token directly** — every such module already re-verifies its own
token and reads `groups` directly (ADR 0041); checking for this one extra literal string is the
same mechanism, not a new one, and doesn't go through `X-Booth-Role` (workspace-scoped by
construction) or any new endpoint.

**Correction (2026-09-30, real code shipped 2026-10-07): this did not hold for a module mounted
`uiIntegrationMode: iframe-proxy`.** An iframe-proxied module never sees the caller's real bearer
token at all — it only ever receives the short-lived `X-Booth-Identity` assertion core mints per
request (ADR 0069), whose `groups` claim was synthesized from a bare `(workspace, role)` pair with
no path for the operator claim to survive onto it. Fixed: `auth.IsOperator` (`internal/auth/
workspace.go`) checks a token's groups for the literal claim; `IframeClaims` (`internal/gateway/
iframetoken.go`) carries an `IsOperator` field set at `IframeURLIssuer.URLFor` time from the
caller's real `identity.Claims.Groups`; `iframeidentity.Service.Mint` (`internal/iframeidentity/
service.go`) appends `/platform/operator` to the minted assertion's groups when set. `booth-logging`
was blocked on exactly this for its Grafana view.

Documented in `contracts/core-platform-api.md`. The local-dev Keycloak realm
(`booth-architecture/local-dev/keycloak/realm-export.json`) gained a `platform` → `operator` group,
granted to `alice` (this realm's bootstrap admin — there's no separate literal "admin" user in the
`booth-local` realm; Keycloak's own `admin`/`admin` is the master-realm console login, a different
realm entirely) so a fresh install can exercise an operator-gated view with no extra IdP setup —
verified against a real running Keycloak container that an issued token for `alice` genuinely
carries `/platform/operator` alongside her workspace role, and that `bob` (never granted it) doesn't.

## Credential sidecar (2026-10-01)

**ADR 0095, `contracts/credential-sidecar.md`.** A new build target, `cmd/credential-sidecar`,
published as `ghcr.io/projectbooth/credential-sidecar` — a small, statically-built binary a
consuming module's own chart (`booth-notebooks`, `booth-pipeline`) adds as a sidecar container so
native database/lakehouse access needs no credential-broker-aware code in the main container at
all. Two modes off one shared renewal core (`internal/sidecar`), selected by `--kind`:

- **`postgres`**: a stateful TCP proxy that terminates a client's connection by trust (the pod's own
  network namespace is the boundary) and separately authenticates upstream with the broker-issued
  credential — including real SCRAM-SHA-256 (`internal/sidecar/pgwire`), since core's own bundled
  Postgres already requires it. `DATABASE_URL=postgresql://localhost:5432/<db>`, no password, just
  works. Verified against a real embedded PostgreSQL server (`internal/testpg`), not a stand-in.
- **`s3`**: refreshes a standard AWS shared-credentials file via atomic temp-file-then-rename —
  every mainstream S3 SDK already knows how to read one.

Both renew before the lease's real `expiresAt` (never the requested `ttlSeconds` — a provider's own
floor, e.g. MinIO's, may clamp the actual grant longer). A renewal failure — of any kind, including
an outright refusal — is logged and retried next interval, never drops an already-open connection or
exits the process; only a config error (bad scope/kind/role) at the very first lease attempt is
fatal. See `docs/decisions/0015` for the full reasoning, including the two provider response shapes
(`postgres`/`s3`-kind credentials) this binary documents and needs `booth-storage`/`booth-database`
to actually return.

## What's built vs. what's left, against the v0 definition of done

Built and tested (unit/contract tests in-repo; the CRD reconcile loop additionally
verified against a real API server via `test/integration/`):

- OIDC auth (JWKS/issuer/expiry/audience verification) + workspace-role derivation.
- Module registry: `BoothModule` CRD watcher/controller with health polling and status
  subresource updates; static-file fallback for local dev.
- Gateway: sync module-to-module/UI routing with identity attachment; iframe-proxy
  (token-in-query-param + scoped cookie, plus the root-relative-follow-up-call fallback
  ui-integration.md calls out as a known failure mode to design around).
- Event bus: NATS/JetStream wiring, `BOOTH_EVENTS` stream, publish/subscribe helpers —
  authenticated, with per-module scoped credentials (ADR 0049).
- User directory (ADR 0047), persistent on a default install via the bundled PostgreSQL.
- Shared PostgreSQL provisioning (ADR 0053): bundled default, per-module databases and roles.
- Bundled Postgres node pinning (ADR 0083): survives a reschedule on a multi-node cluster.
- Workload identity (ADR 0056): minting endpoint, signing key + JWKS, per-module minting credentials.
- Iframe-proxy identity assertion (ADR 0069): a third issuer, signed `X-Booth-Identity` on every iframe-proxied request.
- Credential broker (ADR 0080): authorized routing to a native-protocol credential provider, append-only audit trail.
- Credential sidecar (ADR 0095): `ghcr.io/projectbooth/credential-sidecar`, postgres/s3 renewal-and-proxy modes.
- Secrets/ConfigMap provisioning primitive (ADR 0020).
- Module install/uninstall via the Helm SDK, admin-role-gated (scoped per decision 0005).
- Helm chart: bundles NATS (subchart), the CRD, RBAC, the core Deployment/Service.
- CI: unit+contract on push/PR, layer-3 (envtest + a real `kind` chart deploy) on
  merge-to-main/nightly, tagged-release image+chart publishing.

Not yet built:

- Hosting `booth-design`'s shell — blocked on that repo existing (currently "not
  started" in `MODULE_REGISTRY.md`); nothing to host yet.
- Workspace *metadata* persistence (display name, settings) — membership/role is
  token-derived per decision 0001 and needs no database, but workspace creation/metadata does.
  The database it would live in now exists (`booth_core`); no workspace-metadata schema yet.
- Drift detection/reconciliation for module lifecycle (decision 0005's flagged gap).
- Least-privilege RBAC for the lifecycle manager (currently broad by necessity/default —
  see `charts/booth-core/templates/rbac.yaml`'s comment).
