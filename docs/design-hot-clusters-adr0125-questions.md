# booth-core's answers to ADR 0125's three open questions (hot Spark clusters)

Status 2026-10-25: ADR 0125 is proposed, not accepted. Note only, per
`agent-briefs`'s relay — no code, no deploy. Every claim below about core's current behavior is
read from `internal/workload/service.go` directly (line references are to that file as it
stands on `master`), not from ADR prose, specifically because question 1 and 2 turn on whether
core's *existing* mechanism already covers this or needs new code.

## 1. May booth-spark mint a `svc:` owner for a service-account-owned hot cluster?

**Yes, already, with zero core change and no ADR 0120 amendment needed.** This isn't new
behavior ADR 0125 would be asking for — it's the same generic mechanism ADR 0119's amendment
("owner may be a service account... Spark treats it as an opaque owner id and sends it to
core's mint, which resolves its role; nothing else here changes") already built, and it is not
module-specific in the code: `Service.Mint` calls `s.ownerRole(ctx, req.Owner, req.Workspace)`
(service.go:278) for *every* mint regardless of which module's credential is presented, and
`ownerRole` branches purely on whether `owner` has a `svc:` prefix (service.go:331) — there is no
`if moduleID == "pipeline"` anywhere near it. Any module holding a minting credential — spark
already does, for its own `spark:<ws>:<runId>` data-access tokens — gets the identical treatment
the moment it sends `owner: "svc:<ws>:<name>"` on `POST /api/internal/workload-tokens`.

**What core checks, precisely (service.go:330-375):**
1. `owner`'s embedded workspace (`svc:<ws>:<name>`) must equal the mint request's own
   `workspace` — refused otherwise (ADR 0120 ruling 1, a required check the ADR's original text
   omitted).
2. The named service account must exist and be enabled — `ErrServiceAccountNotFound` /
   `ErrServiceAccountDisabled` otherwise, surfaced as the two machine-readable 403 codes ADR 0119
   amendment 3 point 5 added.
3. Its stored role is read live, with **no `ownerMaxAge` recency window** — a service account has
   no "last seen" (ADR 0120 #2), so this never goes stale the way a person owner's directory
   entry can.
4. The granted role is `LesserRole(roleCeiling, ownerRole)` (service.go:282) — the same
   lesser-of-two-roles cap for a `svc:` owner as for a person. A hot cluster asking for more than
   the service account actually holds gets capped down, never up.

**What core does *not* check: who is asking.** The mint request shape
(`{workspace, subject, roleCeiling, owner}`) carries no "requested by" field — core only ever
sees booth-spark's own trusted minting credential, never the live person clicking "create" in
Spark's UI. **"Only a workspace owner may create a service-account-owned hot cluster" has to be
booth-spark's own check, before it calls core's mint endpoint**, exactly mirroring ADR 0120
ruling 3's existing precedent for pipeline ("owner-only assignment is booth-pipeline's check, not
core's... that is the same trust core already places in a module that names a person owner").
This residual is already named and accepted in ADR 0120's own text; nothing new to rule on.

**ADR 0120 doesn't need an amendment**, but its point 3's literal wording ("only workspace owners
may assign one as a pipeline's owner") only names pipelines. I'd suggest a one-line editorial
addition — "...or as the owner of any other module resource that can be owned by a service
account (e.g. a hot Spark cluster)" — so a future reader doesn't mistake the specific example for
the whole rule. Cosmetic, not behavioral; the mechanism already covers this today.

## 2. booth-notebooks: configuration-only, or does core need code?

**Configuration only, and it's already done — predates ADR 0125 entirely.** `booth-notebooks`
already declares `workloadIdentity` on its manifest and already holds a minting credential (ADR
0070 item 1, ratified 2026-09-23): its kernel pulls a workload token per call, capped at the
person's live role. Confirmed directly in the sibling repo:
`booth-notebooks/charts/booth-notebooks/templates/boothmodule.yaml` declares `id: notebooks` and
a `workloadIdentity` block already. Nothing about ADR 0125 changes this; there's no new manifest
field or chart value to add.

**The `booth_owner` claim is already on every notebook-server token too, automatically — also
zero new core code.** `owner` is a required field on every mint request (ADR 0058 #1), and
`OwnerClaim: req.Owner` / `ModuleClaim: moduleID` are set unconditionally in the same `extra`
claims map (service.go:308-312) for *every* mint, not added per-module. Since the notebook
kernel's existing mint call already supplies `owner = <the person's sub>` (it has to — that's
what "capped at the person's live role" means), its tokens have carried `booth_owner` since the
moment core's build added the claim (this session's ADR 0119 work), with no notebooks-side or
core-side change needed to make that true.

**What core actually mints for a notebook server today is `notebook:<hub-username>`**
(`"<workspace>.<sha256(sub)[:16]>"`, ADR 0070 item 4) — **not** the `notebook:<ws>:<serverId>`
shape ADR 0125 point 8 proposes. This is the one real conflict to flag: ADR 0125's text describes
a subject shape that doesn't match what's actually deployed. I'd recommend *not* changing the
subject (it's an already-running, tested system — `booth-notebooks`' own `tests/hub` covers the
exact hashed-username construction) and instead resolving this the way ADR 0119 amendment 3
already resolved the identical question for pipelines: **booth-spark's new acceptance rule should
key off `booth_module == "notebooks"`, never off parsing `sub`'s shape or prefix.**

That amendment-3 lesson applies in full here, and checking the code confirms it's load-bearing,
not just stylistic: `reservedSubjectKinds` (service.go:126) — the one place core *does* bind a
subject kind to a specific module — only reserves `apiwrite` to module `api` today. Nothing
reserves `notebook:` or `spark:` to anyone. **Any module holding a minting credential could
mint a `notebook:`-shaped subject right now**, so a Spark rule that trusted the subject prefix
alone would have exactly the forgeable-prefix problem amendment 3 already named and fixed for
`pipeline:`. `booth_module` is signed by core from the verified minting credential and cannot be
forged by the subject string; that's the only safe thing to check. Since that claim is already on
every token unconditionally, booth-spark's new rule needs no core change at all — it's a Spark-
side code change reading a claim core already provides.

## 3. Lease renewal: role change or disable while a hot cluster is running

Checked against the actual constant, not the ADR's rounded description:
`DefaultTokenTTL = 10 * time.Minute` (service.go:25) — every workload token, including a hot
cluster's, is valid for exactly 10 minutes from the mint that issued it, renewed by minting
again. `ownerRole` is re-evaluated live on *every* mint, full stop (service.go:278) — there is no
cached or partial trust carried from the previous mint.

**At the next re-mint:** if the owner (person or service account) no longer resolves to a usable
role — demoted below the cluster's ceiling, disabled (service account), removed from the
workspace, or (person owner only) not seen within `ownerMaxAge` — `ownerRole` returns
`ErrOwnerNoAccess` (or the two service-account-specific errors), `LesserRole` can't produce a
real role, and the mint is refused outright. ADR 0125 point 3's "a refused mint ends the
cluster" means booth-spark is expected to tear the cluster down the moment that refusal is
observed, not merely stop renewing and let the old token quietly expire later.

**How fast that's observable — two bounds, not one:**
- **Typical case, bounded by booth-spark's own re-mint cadence.** ADR 0125/0110 describe
  re-minting "at two-thirds of [the token's] life" — roughly every 6 minutes 40 seconds against
  the 10-minute `DefaultTokenTTL`. If revocation happens the instant *after* a re-mint just
  succeeded, the next attempt (and so the next point core can even notice) is up to ~6m40s away.
  This part is booth-spark's own scheduling choice, not something core enforces or can shorten.
- **Hard ceiling, bounded by core regardless of booth-spark's behavior.** Even if booth-spark's
  renewal loop stalled or its "end the cluster on refusal" step had a bug, the *already-issued*
  token is cryptographically bound to expire 10 minutes after the mint that produced it
  (`Expiry: jwt.NewNumericDate(exp)`, service.go:299) — every downstream module independently
  verifying that JWT (storage, catalog) rejects it on `exp` with no dependency on Spark noticing
  anything. **Worst case, full stop: 10 minutes from the last successful mint before
  revocation**, never indefinite, regardless of what booth-spark does or fails to do.

So: **typical observed revocation-to-teardown time is on the order of minutes (bounded by
Spark's own ~6m40s re-mint cadence); the absolute worst case, assuming Spark's own teardown logic
never runs at all, is 10 minutes** — the same number and the same mechanism ADR 0110/0119 already
rely on for ordinary Spark runs and pipeline tasks. Nothing about a hot cluster changes this bound
in either direction; it's the same token, the same TTL, the same live re-check.

## Summary: what, if anything, core needs to build for ADR 0125

**Nothing.** All three questions resolve to "the existing mechanism already covers this" —
zero new manifest fields, zero new core endpoints, zero new claims, zero chart changes. The one
real finding is the subject-shape conflict in §2, and the fix for that lives entirely in
booth-spark's acceptance rule (key off `booth_module`, not `sub`), not in core.
