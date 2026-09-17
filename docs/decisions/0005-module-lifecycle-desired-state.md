# booth-core decision 0005: Where module install/uninstall "desired state" lives

Status: proposed by booth-core, needs to return to booth-architecture as an ADR — this
one touches `booth-design`'s Module Store feature too, not just booth-core internals.

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

## What's still unresolved (the actual open question)

Where does the *catalog* of installable modules — their chart references, versions,
available-vs-installed status the Module Store UI shows — actually live? Candidates:

1. **A static/config-driven catalog booth-core loads at startup** (e.g. a ConfigMap
   listing known modules' chart repos) — simple, but means adding a new installable
   module to a running deployment needs a core config change/restart.
2. **A `BoothModuleCatalog`-style CRD**, mirroring the `BoothModule` pattern — installable
   but not-yet-installed modules get a lighter-weight resource than a full `BoothModule`
   (which per ADR 0019 is only created once a module's *own* chart is actually installed).
3. **Owned by `booth-design`'s Module Store feature as its own backing store**, with
   booth-core only exposing the install/uninstall action, not owning "what's available to
   install" at all.

This needs `booth-design`'s Module Store design (mentioned in
`contracts/core-inventory-for-architect.md` §2 as a real `OpenDataPlatform` feature worth
carrying forward) to be far enough along to agree on before booth-core builds more than
the on-demand action this decision ships for v0.

## Consequences

- `booth-design` can build a Module Store UI against the on-demand install/uninstall API
  now, supplying the chart reference itself (e.g. from its own hardcoded/config-driven
  catalog for early development) without waiting on the catalog-storage question above.
- No drift detection/reconciliation in v0 — flagged as a real gap, not silently accepted
  as permanent.

## Flagged back

Needs review with whoever ends up building `booth-design`'s Module Store, and promotion
to a `booth-architecture` ADR once the catalog-storage question (three options above) is
settled — not something booth-core should decide unilaterally, since it's genuinely a
joint core/design concern.
