# Design note: core supplies the shared identity settings to module installs (ADR 0113)

Status: **design note — not built.** ADR 0113 decided the *direction* (core's install API
supplies the shared identity settings; explicit request values win; no secrets in the block;
Helm CLI installs are unchanged) and left the mechanism to this repo to propose. This note
answers the four things the ADR asked for, grounded in the real install-path code and the real
`values.yaml` of every chart named below (read directly, not recalled) — no chart, contract, or
Go code changes yet.

## a) Where each module's key mapping lives

Three candidates, per the ADR:

1. **The catalog entry the Store installs from** (`booth-module-store/internal/catalog/bundled.yaml`
   and `contracts/module-registry-protocol.md`'s registry-entry JSON). Would need a new field on
   every catalog/registry entry (e.g. `identityKeys: {issuerUrl: "oidc.issuerUrl", ...}`), and a
   new field on `installRequest` (`internal/api/server.go`) so the Store can pass it through —
   the same pattern `chartRef`/`chartVersion` already use. Changes a public contract
   (`module-registry-protocol.md`), so every third-party registry implementer would need to adopt
   it too, and a hand-typed install (an admin form using `chartRef` directly, bypassing the Store)
   would carry no mapping at all unless that caller also knew to supply one.

2. **A chart annotation**, read off the chart itself. `internal/lifecycle/manager.go`'s `Install`
   already calls `loadChart(m, ref)` — which returns a real `*chart.Chart` with a
   `.Metadata.Annotations map[string]string` — *before* either `install.Run` or `upgrade.Run`.
   Reading a `booth.dev/identity-keys` annotation there needs no new wire field anywhere: it
   applies to every install that goes through `Manager.Install`, regardless of whether the caller
   was the Store, an external registry, or a hand-typed `chartRef`/structured `Chart` body on the
   HTTP API directly.

3. **A new values-schema field** (`values.schema.json`'s `x-*` extension point). Not every chart
   in the fleet ships one today, and the schema's job is input validation, not describing where a
   *computed* value should land — it would need the same new-field plumbing as (1) without even
   the catalog's display-metadata precedent to lean on.

**Recommendation: (2), a chart annotation.** It changes no contract (`module-registry-protocol.md`
is untouched; a third-party registry needs to do nothing differently), it is versioned with the
exact chart being installed (it cannot go stale the way a hand-maintained catalog field already
has — this is the same class of bug item 59 found: a chart comment giving the wrong workload
issuer), and it is implementable at the one chokepoint every install already passes through
(`Manager.Install`) rather than needing new fields threaded from the HTTP handler down. Proposed
shape, one JSON object mapping a shared setting name to that chart's own dotted values path:

```yaml
# Chart.yaml
annotations:
  booth.dev/identity-keys: |
    {"issuerUrl":"oidc.issuerUrl","clientId":"oidc.clientId","jwksUrl":"oidc.jwksUrl","workloadIssuerUrl":"oidc.workloadIssuerUrl","groupsClaim":"oidc.groupsClaim"}
```

A module that doesn't verify any of a given setting (e.g. `booth-design`'s browser-only flow has
no JWKS/groups concept) simply omits that key — core supplies only the keys a chart's own
annotation names. A chart with no annotation at all gets no merge, which is exactly today's
behavior (an operator-typed `values` block, unchanged) — the rollout order the ADR's consequences
section already expects: core ships the merge mechanism first, each module adds its own
annotation in its own follow-up PR, with nothing breaking in between.

**Merge semantics worth getting right, not glossed over**: "explicit request values win" (ADR
point 2) has to mean per-leaf, not per-block. If a caller's `values` sets `oidc.clientId` but not
`oidc.issuerUrl`, the merge must still fill in the derived `oidc.issuerUrl` — treating "the caller
touched `oidc` at all" as "skip the whole block" would silently reintroduce the exact
character-for-character mismatch bug this ADR exists to fix, just one key at a time instead of
all of them. The merge walks each dotted path from the annotation independently and only sets it
when that exact leaf is absent from the caller's `values`.

## b) How core derives each value from its own configuration

Every value the ADR names is already computed once, at startup, into `booth-core`'s own
`internal/config.Config` — the install handler needs to *read* `cfg`, not derive anything new:

| Shared setting | Already-resolved source | How it's computed today |
|---|---|---|
| issuer URL (public) | `cfg.OIDC.IssuerURL` | `BOOTH_OIDC_ISSUER_URL`, set by the chart's `effectiveOidcIssuerUrl` helper: the operator's explicit `oidc.issuerUrl`, else `https://<ingress.host>/realms/<realm>` for the bundled Keycloak (`_helpers.tpl`'s `keycloakExternalIssuerUrl`) |
| client id | `cfg.OIDC.ClientID` | `BOOTH_OIDC_CLIENT_ID`, same explicit-or-bundled-default shape; the bundled default is the literal string `booth-design` (`_helpers.tpl`'s `keycloakClientId`) — this is a single platform-wide client, not a per-module one, confirmed by the realm having no audience mapper, so every verified token's `aud` is this one value |
| in-cluster JWKS URL | `cfg.OIDC.JWKSURL` | `BOOTH_OIDC_JWKS_URL`, already the *in-cluster* Keycloak Service URL for a bundled install (`effectiveOidcJwksUrl`), deliberately never the external one, so a module never needs to trust core's own self-signed Ingress certificate just to fetch keys |
| groups claim | `cfg.OIDC.GroupsClaim` | `BOOTH_OIDC_GROUPS_CLAIM`, default `"groups"` |
| workload issuer | `cfg.Workload.IssuerURL` | `BOOTH_WORKLOAD_ISSUER_URL`, only set when `workloadIdentity.enabled`; defaults to core's own in-cluster Service URL |
| iframe issuer | `cfg.IframeIdentity.IssuerURL` | `BOOTH_IFRAME_IDENTITY_ISSUER_URL`, only set when `iframeIdentity.enabled`; defaults to core's own in-cluster Service URL + `/iframe-identity` |

**External identity provider**: no new description is needed. `cfg.OIDC.*` already holds whatever
was configured on core's own chart — ADR 0106/0108 already established "an explicit `oidc.*`
value on core's own chart always wins over the bundled-Keycloak default," and that's the entire
answer: swap Keycloak for an external IdP by setting `oidc.issuerUrl`/`clientId`/`jwksUrl` on
*core's* install, and every downstream module install inherits the same already-resolved values
through this mechanism with zero special-casing. This proposal adds no new provider-description
format.

**No secrets**: all six values are URLs, an OIDC client id, and a claim name — none of them a
credential. Confirmed by reading every chart above: nothing in this set lines up with any chart's
own secret-delivery path (`oidc.clientSecret` doesn't exist anywhere in the fleet; the bundled
flow is public-client PKCE, per ADR 0032).

**Scope**: this merge happens once, inside `Manager.Install`, only for calls through
`POST /api/modules/{id}/install`. A Helm CLI install never calls this code path at all, so "Helm
CLI installs are unchanged" (ADR point 6) holds structurally, not by a flag.

## c) Exact key spellings in today's charts

Read directly from each chart's `values.yaml`, not from memory or `MODULE_REGISTRY.md`:

| Module | issuer | client id / audience | in-cluster JWKS | workload issuer | groups claim |
|---|---|---|---|---|---|
| storage | `oidc.issuerUrl` | `oidc.clientId` | `oidc.jwksUrl` | `oidc.workloadIssuerUrl` | `oidc.groupsClaim` |
| catalog | `oidc.issuerUrl` | `oidc.clientId` | `oidc.jwksUrl` | `workloadIdentity.issuerUrl` | `oidc.groupsClaim` |
| database | `oidc.issuerUrl` | `oidc.clientId` | `oidc.jwksUrl` | *(none — doesn't trust workload tokens)* | `oidc.groupsClaim` |
| lakehouse | `identity.oidcIssuerUrl` | `identity.oidcAudience` | `identity.oidcJwksUrl` | `identity.workloadIssuerUrl` | `identity.groupsClaim` |
| logging | `oidc.issuerUrl` | `oidc.clientId` | `oidc.jwksUrl` | `oidc.workloadIssuerUrl` | `oidc.groupsClaim` |
| pipeline | `oidc.issuerUrl` | `oidc.clientId` | `oidc.jwksUrl` | *(none — `workloadIdentity.enabled` is a flag for minting tokens for tasks, not for trusting them)* | `oidc.groupsClaim` |
| notebooks | `oidc.issuerUrl` | `oidc.clientId` | *(no key at all — gap)* | *(none)* | `oidc.groupsClaim` |
| api | `oidc.issuerUrl` | `oidc.clientId` | `oidc.jwksUrl` | *(none)* | `oidc.groupsClaim` |
| streamlit | *(none — verifies only core's iframe-identity assertion, see below)* | — | — | — | `identity.groupsClaim` |
| spark | `oidc.issuerUrl` | `oidc.clientId` | `oidc.jwksUrl` | `oidc.workloadIssuerUrl` | `oidc.groupsClaim` |
| module-store | `oidc.issuerUrl` | `oidc.clientId` | `oidc.jwksUrl` | *(none)* | `oidc.groupsClaim` |
| design | `oidc.issuerUrl` | `oidc.clientId` (default: the literal string `booth-design`) | — (browser PKCE flow fetches its own discovery/JWKS client-side) | — | — |

This confirms the ADR context's "three ways" claim for the workload-issuer setting exactly:
`oidc.workloadIssuerUrl` (storage, logging, spark), `workloadIdentity.issuerUrl` (catalog), and
`identity.workloadIssuerUrl` (lakehouse) — three real spellings for one concept, plus three
modules (database, pipeline, api, module-store) that simply don't have the setting at all.

Four modules (`booth-logging`, `booth-notebooks`, `booth-streamlit`, `booth-spark`) separately
carry an `identity.issuerUrl` field for core's **iframe-identity** assertion (ADR 0069) — a
different issuer than the OIDC one, already hardcoded to the correct in-cluster default
(`http://booth-core.booth-system.svc.cluster.local:8080/iframe-identity`) in every chart that has
it. This proposal's annotation would still name it (as `iframeIssuerUrl` → that dotted path) for
modules that choose to accept an override here, but since every current default is already
correct by construction, this is lower-priority than the five settings in the table above.

`booth-notebooks` has no `oidc.jwksUrl` key today — a real gap this mapping exposes rather than
causes; it's worth that module's own follow-up PR adding one, not something this proposal papers
over by inventing a path that doesn't exist in the chart.

## d) Integration check

Proposed, not yet built: extend `booth-core`'s own `integration.yml` (the `ingress-tls-check` job
already stands up a real bundled Keycloak and completes a real PKCE login via
`scripts/testdata/https_login_check.py`) with one more step, after that login succeeds:

1. Install the already-published `booth-storage` chart
   (`oci://ghcr.io/projectbooth/charts/booth-storage`) through core's own
   `POST /api/modules/{id}/install`, with a `values` body that sets **no** `oidc.*` key at all —
   only `namespace` and whatever non-identity values that chart requires to come up (its backend
   config). This is the literal "no identity values" case the ADR asks for.
2. Reuse the real access token the login flow already obtained (not a new credential) to call one
   of `booth-storage`'s own authenticated routes directly, and assert it answers as an
   authenticated caller (not a 401/403) — proving the module verified a real, bundled-Keycloak-
   issued token using settings it was never typed by hand, exactly the gap item 59 found. The
   literal route path isn't pinned here — confirming the right one is a build-time detail against
   `booth-storage`'s actual API, not guessed at in this design note.
3. As a cheap negative control already implied by (1): if the merge mechanism is somehow not
   applied, this step fails loudly (`booth-storage` would either answer `503` per its own
   documented empty-`oidc.issuerUrl` behavior, or reject the token outright) rather than passing
   for the wrong reason.

`booth-storage` is the right target: it's the smallest already-published chart with its own real
token-verifying endpoint, needs no bundled-Keycloak-incompatible setup, and is already named
first in the bundled catalog.

## Not decided here

- The exact annotation key name (`booth.dev/identity-keys` above is a proposal, not a ruling) and
  whether it's a single JSON blob (as shown) or several smaller annotations — either works with
  the chokepoint in (a); picking between them is a detail, not a design question, and can be
  settled in the build PR.
- Which modules' charts actually get the annotation added, and in what order — per the ADR, that's
  a follow-up PR per module repo once this mechanism is approved.
- `booth-notebooks`' missing `oidc.jwksUrl` key — flagged above, not fixed here.
