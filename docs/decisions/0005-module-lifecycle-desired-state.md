# booth-core decision 0005: Where module install/uninstall "desired state" lives

Status: **resolved** — `../../../booth-architecture/decisions/0027-module-store-as-dedicated-module.md`
(2026-09-17). Not accepted as-proposed: the catalog question below is answered by a new,
separate mandatory repo, `booth-module-store`, rather than any of the three candidates
this file considered. **Zero code changes required in booth-core** — the on-demand
install/uninstall API this decision shipped already accepts a chart reference from any
caller, which is exactly how `booth-module-store` calls it. Kept below for the reasoning
that led to the (correctly) narrower v0 scope cut this repo shipped.

## Context

`agent-briefs/core.md`'s v0 definition of done says booth-core's install/uninstall
lifecycle "reconciles a module's Helm chart against the registry's declared state
(ADR 0003)." ADR 0003 itself says this reconciles against `MODULE_REGISTRY.md`'s state —
but that file lives in the `booth-architecture` repo and tracks fleet-wide repo/build
status for the coordinator session, not a live deployment's "which modules does this
specific installation want running" intent. Something has to hold that real, per-deployment
desired state, and neither document says what.

## Decision (v0 scope cut, not a full answer)

For v0, booth-core does **not** introduce its own desired-state store (no new CRD, no
Postgres-backed installation-intent table). `internal/lifecycle.Manager` is built as a
direct, on-demand wrapper over Helm's install/upgrade/uninstall actions — an admin-gated
API call (`POST /api/modules/{id}/install`, `DELETE /api/modules/{id}`) performs the
action immediately, given a chart reference and values in the request body. Helm's own
release state (its Secret-backed storage in the module's namespace) is the only source of
truth for "is this module installed" — booth-core does not duplicate it.

This deliberately does not implement a continuous reconcile loop that would notice drift
(e.g. someone `helm uninstall`s a module out-of-band) and re-converge it — that's the gap
this decision leaves open, not something already solved.

## What was unresolved (now answered by ADR 0027)

Where does the *catalog* of installable modules — their chart references, versions,
available-vs-installed status the Module Store UI shows — actually live? Three
candidates were weighed here: a static/config-driven catalog inside booth-core, a
`BoothModuleCatalog`-style CRD mirroring `BoothModule`, or `booth-design` owning it as
part of its Module Store feature. ADR 0027 rejected all three in favor of a fourth: a new
mandatory repo, `booth-module-store`, owning the catalog (bundled + optional external
registry connections) and the browsing/install UI end to end, calling booth-core's
existing install/uninstall API. See that ADR for the full reasoning on why the three
candidates considered here didn't hold up.

## Consequences

- `booth-module-store` builds against the on-demand install/uninstall API described
  above, supplying the chart reference itself from its own two-tier catalog. No API
  change needed on booth-core's side.
- No drift detection/reconciliation in v0 — flagged as a real gap, not silently accepted
  as permanent, and still not addressed by ADR 0027 either.

## Flagged back

Resolved via `booth-architecture` ADR 0027. `booth-design`'s brief was corrected
separately (per that ADR) to stop planning any catalog/browsing logic of its own.
