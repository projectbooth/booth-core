# Design note: the `booth_owner` claim (ADR 0119) and service accounts (ADR 0120)

No code in this note — every claim below is grounded in the real files named, read on
`master` (`a562691`). Sent for review before building, per both ADRs' "do not implement
until the user has accepted" gate (both are now **accepted**, 2026-10-16).

## Part A — `booth_owner` claim on workload tokens (ADR 0119 item 1)

This is a small, contained change. `workload.Service.Mint` (`internal/workload/service.go:201-268`)
already receives and verifies `req.Owner` before it ever reaches the signing step — `ownerRole`
(line 275) resolves it to a live role and `Mint` refuses (`ErrOwnerNoAccess`) before any token is
built if that resolution fails. So "add a claim carrying the owner" is purely additive at the
point the `extra` claims map is built (line 254):

```
extra := map[string]any{
    s.opts.GroupsClaim: []string{...},
    ModuleClaim:        moduleID,
    OwnerClaim:         req.Owner,   // new: "booth_owner"
}
```

Three things make this fit cleanly, confirmed rather than assumed:

- **`sub` is untouched.** `claims.Subject = req.Subject` (line 242) already names only the run;
  nothing here changes that.
- **Human tokens never carry it, with no extra guard needed.** `booth_owner` is written only
  inside `Mint`, which is the workload-token minting path exclusively — a human's token comes
  from the deployment's own OIDC provider and never passes through this code at all. "Absent from
  human tokens" is already a structural fact, not something to test defensively, though I'll still
  assert it (a token round-tripped through `auth.Verifier`, the human-token path, parses with no
  `booth_owner` key present).
- **No gateway change needed.** I checked `gateway.proxyTo` (`internal/gateway/proxy.go:66-87`):
  the exact raw token the caller presented is forwarded unchanged as the `Authorization` header to
  the backing module (`r.Header.Set("Authorization", "Bearer "+rawToken)`, line 83). Core's own
  `Service.Verify` (used only to derive `X-Booth-Role` for core's own routing) never needs to know
  about `booth_owner` — Spark (or any consumer) reads the claim straight off the forwarded JWT via
  its own OIDC-discovery trust of core as a second issuer, exactly the mechanism the
  `/platform/operator` claim already proved out (ADR 0094, `core.md`'s 2026-10-07 entry) and the
  one `TestGateway_AppWorkloadTokenReachesBoothApiUnchanged` already pins for the ADR 0117 round.
  Nothing in `internal/gateway` needs to change for this ADR.

**What I'd build:** a new `OwnerClaim = "booth_owner"` const next to `ModuleClaim`
(`internal/workload/service.go:42`), one line added to `extra` in `Mint`, and tests: claim present
and equal to `req.Owner` on a workload token; absent on a human token verified through
`auth.Verifier`; absent on an iframe-identity assertion (`iframeidentity` package, a third,
unrelated issuer); mint still refused (403) for a stale/absent owner exactly as today (this path
is unchanged — the claim is only added *after* `ownerRole` already succeeds).

**Contract text to flag to booth-architecture** (same pattern as decision 0010's existing flags):
`contracts/core-platform-api.md`'s "Workload identity" paragraph should gain one sentence — the
minted token's claims now include `booth_owner` (the same value as the request's `owner`), for
audit and for the one documented consumer (ADR 0119); every other module must continue to ignore
it. I'll put this in the PR description as before, for the coordinator to apply.

## Part B — Service accounts (ADR 0120)

### B1. Storage and migration

New table, same pattern as `booth_users` (`internal/directory/postgres.go:14-26`): one idempotent
`CREATE TABLE IF NOT EXISTS`, no separate migration tool, created lazily on first use via a
`ready()` guard identical in shape to `PostgresStore.ready`.

```
CREATE TABLE IF NOT EXISTS booth_service_accounts (
    workspace    TEXT        NOT NULL,
    name         TEXT        NOT NULL,
    role         TEXT        NOT NULL,          -- "viewer" or "editor" only (ADR 0120 #6: no owner role)
    description  TEXT        NOT NULL DEFAULT '',
    created_by   TEXT        NOT NULL,          -- the creating owner's sub
    created_at   TIMESTAMPTZ NOT NULL,
    disabled     BOOLEAN     NOT NULL DEFAULT FALSE,
    PRIMARY KEY (workspace, name)
);
```

Id shape is `svc:<workspace>:<name>`, built, never stored as a single column — storing
`(workspace, name)` separately avoids ever having to parse the composite id back apart on a read,
and the mint path (below) already needs `workspace` split out to check it against the request's
own workspace.

**New package, not folded into `internal/directory`.** `directory.Store` is deliberately
opportunistic and lossy by design (see B-residual below); a service account is an explicit,
owner-authored record that must never be silently dropped. I'd add `internal/serviceaccount` with
its own `Store` interface (`Create`, `Get`, `List`, `Update(role/description/disabled)`,
`Delete`) and a `PostgresStore`, mirroring `directory`'s file layout but with real CRUD semantics
instead of upsert-merge — this is the same reasoning that already gave the credential-broker audit
trail its own package (`internal/credentialbroker`'s store, not a reuse of `directory`'s).

**Residual I'd flag rather than silently copy:** `directory.Switchable` (`internal/directory/switchable.go`)
starts on an in-memory store and swaps to Postgres once core's own database is provisioned
(`cmd/core/main.go:301`, `persistCoreDatabase` at line 581), and `Swap` **discards** whatever the
previous store held (`switchable.go:28`'s doc comment says so explicitly) — acceptable for the
directory because its entries are "opportunistic... repopulate on each user's next request." A
service account created during that same in-memory window has nothing that repopulates it: it
would just be gone, silently, the moment the real database comes up. That is a real bug class for
a resource whose whole point is a durable, owner-authored role grant (disabling one and having it
silently un-disable after a DB swap would be worse than merely inconvenient).

So I'm proposing service accounts do **not** follow the `Switchable`/in-memory-fallback shape at
all:
- `cfg.PostgresDSN != ""` (explicit DSN): build the real `PostgresStore` eagerly and `Ping`-retry
  before the server starts serving, exactly like the directory/audit stores already do in that
  branch (`cmd/core/main.go:312-325`).
- `dbProv != nil` (core provisions its own database in the background): register the
  service-account routes against a store that returns a 503 ("service accounts are temporarily
  unavailable while core's database is provisioning") until provisioning finishes, then swap once,
  permanently, into the real `PostgresStore` — never swapping away from it again. This needs a
  small `serviceaccount.Switchable`-alike, but one whose *initial* backing implementation refuses
  writes and reads with `ErrUnavailable` rather than silently accepting them into memory.
- No database configured at all (dev/test only): the routes 503, with the same kind of startup log
  line core already prints for "no database configured" (`cmd/core/main.go:342-343`). This matches
  ADR 0120's own stated caution ("no credentials in v0, so the feature cannot become a new way
  in") — better for this one admin feature to be unavailable on an ephemeral deployment than to be
  silently volatile.

### B2. Mint path for `owner = svc:<workspace>:<name>`

`Service.ownerRole` (`internal/workload/service.go:275-291`) is the one place to branch:

```
func (s *Service) ownerRole(ctx context.Context, owner, workspace string) (auth.Role, error) {
    if rest, ok := strings.CutPrefix(owner, "svc:"); ok {
        return s.serviceAccountRole(ctx, rest, workspace)
    }
    // ...existing directory lookup, unchanged...
}

func (s *Service) serviceAccountRole(ctx context.Context, rest, workspace string) (auth.Role, error) {
    ws, name, ok := strings.Cut(rest, ":")
    if !ok || ws != workspace {
        return "", ErrOwnerNoAccess // malformed id or cross-workspace use — one refusal, no probing
    }
    acc, found, err := s.serviceAccounts.Get(ctx, workspace, name)
    if err != nil {
        return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
    }
    if !found || acc.Disabled {
        return "", ErrOwnerNoAccess
    }
    return acc.Role, nil // live from the stored record; no MaxOwnerAge window (ADR 0120 #2)
}
```

This satisfies ADR 0120 item 2 exactly as worded: role comes from the stored record, read live,
no `ownerMaxAge`; a disabled or deleted account fails closed with the same `ErrOwnerNoAccess` a
person's demotion already produces, so `Mint`'s caller-facing behaviour doesn't gain a new error
shape. The cross-workspace check (`ws != workspace`) is new and necessary — nothing in ADR 0120
calls it out, but without it a service account minted in workspace A could be named as the
`owner` of a mint request for workspace B, which is a real privilege leak a workspace-scoped
resource must not allow. I'd flag this explicitly as the one correctness gap in the ADR's text.

`workload.NewService`'s constructor gains a `serviceaccount.Store` parameter, mirroring the
existing `users directory.Store` parameter. This is an internal Go signature, not a wire contract
— its only callers are `cmd/core/main.go` and this package's own tests, both inside this repo, so
it's a safe, non-breaking-to-anyone-else change.

`validate(req)` needs no new case: `req.Owner` is already required and length-bounded
(`service.go:301-302`); the `svc:` grammar check happens inside `ownerRole` above, where the
workspace-equality check also lives, rather than duplicating a second regex in `validate`.

### B3. Owner-only create/edit/disable/delete

`requireAdmin` (`internal/api/server.go:276-289`) already exists and does precisely this — it 403s
any caller whose `identity.Active.Role` isn't `auth.RoleOwner`. No new authorization primitive
needed; the new routes wrap their mutating handlers in it exactly as `handleInstallModule`/
`handleUninstallModule` already do.

**"Only workspace owners may assign one as a pipeline's owner" (ADR 0120 #3) is not something core
can enforce.** Assignment happens inside Pipeline's own data model (setting a `Pipeline.owner`
field to `svc:...`), not through any core API — core only ever sees the resulting `owner` string
at mint time, by which point it's too late to ask "was the person who set this an owner?". This
has to be Pipeline's own authorization check on its "set schedule owner" action, and today
Pipelines are editable by editors (ADR 0071), so Pipeline needs a **new**, stricter check
specifically for assigning a service-account owner — not a core gap, but a real gap I'd flag back
so it doesn't get missed in Pipeline's own ADR 0120 gate-2 note.

### B4. REST routes

Following the shape every existing core-owned resource already uses — workspace comes from the
already-validated `X-Workspace` header / `identity.Active.Workspace`, never a `{ws}` path segment
(see `/api/modules`, `/api/users`, `/api/users/{sub}`) — rather than the ADR's literal
`/api/workspaces/{ws}/service-accounts` text, which no existing core route follows. I'd flag this
wording mismatch and use core's own convention instead, for consistency with every other resource
on this surface:

- `GET /api/service-accounts` — any authenticated workspace member (viewer+); a Pipeline owner
  picker needs to list available accounts without being a workspace owner itself.
- `GET /api/service-accounts/{name}` — same, any member.
- `POST /api/service-accounts` — `requireAdmin`. Body `{name, role, description}`.
- `PATCH /api/service-accounts/{name}` — `requireAdmin`. Body `{role?, description?, disabled?}`.
- `DELETE /api/service-accounts/{name}` — `requireAdmin`. Refused (409) while any usage record
  says the account is in use — see B6.

None of these ever return `createdBy` or any field a consuming module shouldn't see beyond what
ADR 0120 lists; `createdBy` is stored for audit, not necessarily returned in the list/get response
(happy to include it — it's not sensitive — but it's a detail I'd confirm rather than assume).

### B5. Shell UI under workspace settings

Core has no frontend code at all (confirmed: no `.tsx`/`.vue` files in this repo) — "the shell" is
`booth-design`'s UI (ARCHITECTURE.md's module-hosting description: nav rail, Module Store and
Settings sitting outside the three module groups). So this item is **not core's to build**: core's
job ends at the REST routes in B4. I'd flag to the coordinator that `booth-design` needs its own
brief once the API shape above is confirmed — a page under workspace settings listing accounts
(name, role, disabled, created-by/at, and whatever usage summary B6 produces), with create/edit/
disable/delete forms gated the same way Module Store's install/uninstall already are (owner-only,
same 403 surfaced as a plain error state). I can describe the page's data needs precisely once the
API is settled, but I don't own or build that repo.

### B6. How "account in use by a pipeline" is known to core

This is the one item ADR 0120 states as an outcome ("a record of which pipelines use each
account... deleting an account in use is refused") without saying how core learns it, and core
genuinely has no existing mechanism for this — it's never been the *receiver* of a cross-module
"this resource of yours is referenced by my data" notice before. Two real options, not a silent
pick:

1. **A small usage-registry endpoint on core, pushed to by consumers.** New route,
   `PUT /api/service-accounts/{name}/usage/{consumerModule}/{resourceId}` with body
   `{inUse: bool, label: string}`, authenticated the normal way (any member of the workspace, no
   special credential — this is bookkeeping, not a privileged action). Pipeline calls it whenever
   a schedule's owner is set to or cleared from a service account. `DELETE` on the account 409s
   while any `{consumerModule, resourceId}` row for it has `inUse: true`. This fits core's
   existing shape well: modules already call core's own API directly and not just through the
   gateway's human-request proxy (the credential broker and the workload-mint endpoint are both
   exactly this pattern), so this isn't a new kind of traffic, just a new route.

   **Real residual:** this view can go stale. If Pipeline fails to call back when it deletes a
   pipeline or clears an owner (a bug, a crash mid-request, a direct database edit bypassing its
   own API), the usage row is never cleared and a legitimate delete stays refused forever with no
   automatic reconciliation. That fails in the safe direction (over-blocking, not under-blocking),
   but it's a real support burden with no v0 answer here. I'd ask the coordinator whether a
   force-delete override (owner-only, logged loudly) is wanted as an escape hatch, or whether
   Pipeline is expected to offer its own periodic reconciliation call.

2. **Don't track usage in core at all.** Skip the "refused" behaviour; rely entirely on `Mint`'s
   existing fail-closed behaviour (B2: a deleted/disabled account just stops minting, exactly like
   a demoted or long-absent human owner already does per ADR 0119's own accepted precedent for
   stale owners). The cost is no friendly guardrail — a workspace owner can delete an account out
   from under a running pipeline, discoverable only via the next run's failure status, not at
   delete time. Simpler, no new API, and consistent with decision 0005's existing stance that core
   holds no durable cross-module relationship state — but it doesn't match ADR 0120's literal
   wording ("deleting an account in use is refused"), so I'm not picking it unilaterally.

I'd default to option 1 if the coordinator doesn't have a preference, since it matches the ADR's
stated behaviour, but wanted both put in front of you rather than deciding silently given the
staleness residual.

### B7. Audit fields in the mint log

Already mostly true with no code change: `Mint`'s existing log line
(`internal/workload/service.go:264-265`) is `module=%s subject=%s workspace=%s owner=%s
ceiling=%s granted=%s`, and `owner` is already a plain string — for a service-account owner it
already logs the full `svc:<workspace>:<name>` id verbatim, satisfying ADR 0120 gate 1's "mint
logs show owner id" today, unchanged.

**Optional addition, not required by the ADR text:** an explicit `ownerKind=person|service-account`
field, so an operator grepping the log doesn't have to pattern-match the `owner` value's prefix
themselves. Cheap to add alongside the `OwnerClaim` work in Part A. I'd add it unless told not to
bother.

## Chart value: `workloadIdentity.ownerMaxAge`

Confirmed path: `charts/booth-core/values.yaml:58` (`workloadIdentity.ownerMaxAge: 168h`) →
`charts/booth-core/templates/deployment.yaml:65` (`value: {{ .Values.workloadIdentity.ownerMaxAge |
quote }}`) → the container env var read once by `internal/config/config.go:277-280`
(`BOOTH_WORKLOAD_OWNER_MAX_AGE`) into `cfg.Workload.OwnerMaxAge` → passed once into
`workload.NewService`'s `Options.MaxOwnerAge` at process startup (`cmd/core/main.go:364`).

**Confirmed: a restart is the only effect of changing it.** There is no watch, no reload, no
re-read anywhere in this path — it's parsed exactly once at boot. Raising the value via
`helm upgrade` changes the pod template's env var, which changes the template hash, which is what
actually triggers the rolling restart that picks the new value up; editing the Helm value alone,
without a `helm upgrade`, does nothing. This only bounds *person* owners (ADR 0119's amendment:
"the 7-day owner window... applies to person owners only") — raising it is the interim step named
in ADR 0120's build order and has no effect on service-account mints either way, since those never
consult `MaxOwnerAge` at all (B2).

## Items flagged, summarized

1. Cross-workspace check on `svc:` owners (B2) — a real correctness gap in the ADR's text, not
   optional.
2. The in-memory-fallback/`Switchable` pattern is wrong for this resource (B1) — proposing a
   different store lifecycle than the directory's.
3. "Only workspace owners may assign" (B3) is enforceable only by Pipeline, not by core — flagging
   so it isn't assumed to be covered here.
4. REST path convention: core doesn't use `{ws}` path segments anywhere today; proposing
   `/api/service-accounts` (X-Workspace-scoped) instead of the ADR's literal
   `/api/workspaces/{ws}/service-accounts` (B4).
5. The shell UI (B5) is booth-design's to build, not core's — needs its own brief once this API is
   settled.
6. "In use by a pipeline" (B6) has no existing mechanism in core and two real options with
   different failure modes — asking rather than picking silently.
7. `ownerKind` log field (B7) — optional, offered, not required.

## Proposed build order

Part A (`booth_owner` claim) is small and independent — could ship first, alone, as its own PR.
Part B's storage/mint-path pieces (B1/B2) are the next layer; REST routes (B4) and the usage
registry (B6) depend on B1 being settled first since `DELETE`'s refusal needs the usage table to
exist. The shell UI (B5) is out of this repo's scope and can proceed in parallel once B4's shapes
are confirmed.

Not building anything until you say which of the B1/B6 options to take.
