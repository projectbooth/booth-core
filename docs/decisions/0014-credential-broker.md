# booth-core decision 0014: The credential broker's mechanism (ADR 0080)

Status: **implemented; several contract additions flagged back to booth-architecture.** ADR 0080
decides the outcome — a cross-cutting broker, distinct from `platform_access`/`grant()`, that
authenticates/authorizes a request using the same identity chain `grant()` already uses and routes
it to whichever module provides a credential *kind*, never minting anything itself — and fixes the
hard requirements (pre-authorized before reaching a provider, mandatory short TTL stricter than a
workload token's, never log a raw credential, an append-only audit trail, a provider refuses rather
than widens). The manifest field(s), API shape, routing/discovery mechanism, and scope vocabulary
are left to this session, mirroring ADR 0065/0078's pattern. This records those calls, verified
against `booth-lakehouse`'s real, MinIO/Lakekeeper-tested provisional design and `booth-storage`'s
own provider-side analysis rather than invented from scratch.

## 1. The API shape: adopted from booth-lakehouse, with one deliberate deviation

`booth-lakehouse`'s `HttpBroker` stand-in (`docs/decisions/0001`) is real prior art — built and
tested against real MinIO and Lakekeeper before this contract existed — so its shape is the
starting point, not a strawman:

```
POST /api/credentials
Authorization: Bearer <caller's own token>      X-Workspace: <slug>
{"kind": "s3", "ttlSeconds": 900, "access": "read"|"readwrite",
 "scope": {...kind-specific...}, "options": {...kind-specific...}}

201 {"leaseId", "kind", "expiresAt", "scope": <echo>, "credential": {...kind-specific...}}
422 {"error": "scope_not_supported", ...}   # ADR 0080's "refuse rather than widen"
```

**One deviation from the provisional shape**: `access` is a **shared, top-level** request field
(`credentialbroker.AccessRead` / `AccessReadWrite`), not nested inside `scope`'s otherwise fully
opaque, kind-specific object. The broker's own authorization decision (§3) needs to read it
regardless of kind — burying it inside a blob the broker doesn't otherwise interpret would mean
either interpreting `scope` after all (undermining "opaque per kind") or duplicating `access` as a
second, broker-level field anyway. `booth-storage`'s own `docs/decisions/0006` independently
flagged the identical tension ("whether 'no session token allowed' is a formal part of the shared
request vocabulary... §1 assumes the latter is acceptable but the former would be cleaner") for a
different field — this resolves the same class of question for `access` specifically, since unlike
"no session token," `access` is universal across every kind, not `s3`-specific. Everything else
(`scope`, `options`) stays fully opaque to the broker, passed through unread.

`credentialbroker.Request`/`Response` (`internal/credentialbroker/service.go`) are the canonical
Go types; `contracts/credential-broker.md` should be updated to this exact shape — see §7.

## 2. Manifest field: providers only, no requester-side declaration

`providesCredentials: {kinds: [...]}` (`api/v1alpha1`, `contracts/module-manifest.md`) — a module
declares which kind(s) it provides, exactly parallel to `events`/`database`/`workloadIdentity`.
Core provisions `booth-credential-broker-provider-credentials` (Secret, ADR 0020 mechanism) only
to a module that declares it, and only ever routes a `kind` to whichever module registered it.

**No requester-side manifest field.** ADR 0080's "not yet fixed" list asks about a field to
*request or* provide a kind. A requester needs none: unlike the workload-minting endpoint (whose
caller is a module with no per-request human/workload identity yet, hence a bootstrapping
credential), the broker's caller **already has** a real, already-verified identity by the time it
calls — its own OIDC session, or a workload token it already obtained via `grant()`. The broker's
authorization is exactly the ordinary identity/workspace/role gate every other authenticated route
already has (§3); nothing about *being allowed to ask* needs a manifest opt-in on top of that. A
module doesn't need permission to call an endpoint any authenticated caller can call — only a
provider needs to register that it can be routed to.

## 3. Authorization: role-gated on the one shared field

The broker resolves `(subject, workspace, role)` from `auth.Middleware` exactly like the gateway
route — no new identity derivation. Its one authorization rule, applied to the shared `access`
field: **`readwrite` requires editor or owner; `read` is open to any real role**, directly
reapplying ADR 0048's already-established catalog precedent (editor/owner write, viewer reads)
rather than inventing a new policy. A workload token's role is already capped at mint time
(ADR 0056/0058 — the lesser of `roleCeiling` and the owner's current role), so this reads that
capped value the same way the gateway does; a run never gets more than its owner (and its own
`roleCeiling`) allowed, with no separate enforcement needed here.

## 4. Routing: a live registry scan, exactly one provider per kind

`Service.findProvider` scans `Modules.List()` (the live registry, ADR 0019) for whoever declares
`kind`. Zero matches → 404 (`ErrNoProvider`); more than one → 500 (`ErrAmbiguousProvider`), a
fleet misconfiguration the broker refuses to guess through rather than picking one arbitrarily.
No static core-side config table — same reasoning ADR 0050's event-bus permissions already
established (core shouldn't hold a central list of what every module does; each module declares
its own piece on its own manifest).

## 5. Provider authentication: a derived credential, reusing ADR 0056/0058's exact shape

`contracts/credential-broker.md`'s "a provider never re-derives trust itself, it trusts the
broker's forwarded, already-authorized request" needs *some* way for a provider to know a call
really came from core. Reused, not reinvented: `credentialbroker.Keys` derives a per-provider
credential (`bcbp.<module-id>.<hmac>`) the exact same way `workload.Keys.Credential` does for
workload-token minting credentials — one core-only HMAC key (`booth-credential-broker-keys`
Secret), no per-module state to keep in sync, revoked automatically the instant a module drops
`providesCredentials` (the same live-entitlement-check pattern `workload.Service.AuthenticateModule`
already uses, though direction is reversed here: core is the one *presenting* the credential
outbound, a provider is the one that would verify it inbound). Delivered to the provider as
`booth-credential-broker-provider-credentials` (`credential` key), core presents it as
`Authorization: Bearer <credential>` on every outbound call to `POST <provider's BaseURL>/internal/credentials`
— `credentialbroker.ProviderPath`, a fixed contract path every provider implements, the same way
`workload.MintPath` is fixed for the minting endpoint. `Keys.ModuleForProviderCredential` is
exported specifically so a provider implementation (`booth-storage`, `booth-database`) can verify
an inbound call with the identical derivation, not re-invent HMAC parsing independently — though a
provider needs its **own** copy of the *shared secret* to do so, which it gets from the delivered
Secret, not by importing this Go package (a provider may not even be Go).

The provider request body carries the requester's already-authorized identity (`subject`,
`workspace`, `role`) purely for **the provider's own audit logging** — never for any further
authorization decision, per the ADR's own wording.

## 6. TTL ceiling: stricter than workload's, and how that squares with MinIO's floor

`DefaultMaxTTL = 5 minutes` (`BOOTH_CREDENTIAL_BROKER_MAX_TTL`, chart default `5m`) —
`workload.DefaultTokenTTL` is 10 minutes, so this is genuinely stricter, per the ADR's explicit
requirement (pinned by a test that fails if anyone ever loosens `DefaultMaxTTL` past 10 minutes
without noticing). A request asking for more is **clamped down**, not refused — "at most this
long," the same spirit as `roleCeiling` never erroring when it exceeds what's actually granted.

This creates a real, already-anticipated tension: `booth-storage`'s own `docs/decisions/0006`
records that MinIO's expiring service accounts have a **15-minute floor** — it cannot issue
anything shorter, and that same document explicitly punts the ceiling decision to core while
already deciding its own floor behavior ("a requested TTL below 15 minutes is clamped **up** to 15
minutes, not refused... The ceiling side... is a separate, stricter-than-`grant()` concern that's
core's to set"). So: core's ceiling bounds what a caller may **ask for**; a provider's own
documented capability floor may still clamp the **actual** grant back up past it — that's the
provider's capability limit, not a violation of this ceiling, and `Response.ExpiresAt` (not the
request's `ttlSeconds`) is always the caller's and the audit trail's source of truth for what was
actually issued.

## 7. Audit trail: Postgres-backed, memory fallback, genuinely append-only

`internal/credentialbroker/audit.go` mirrors `directory`'s exact Store/MemoryStore/PostgresStore/
Switchable shape: starts in-memory, upgrades to core's own Postgres database (ADR 0053, the same
`booth_core` database the user directory already lives in, a second table not a second database)
once available. `booth_credential_issuances` has no UPDATE/DELETE anywhere in the code — genuinely
append-only, not just append-only by convention. Recorded fields are exactly what ADR 0080 asks
for (lease id, requester subject/workspace/role, kind, access, scope, provider, issued/expires) —
`Entry` has **no field capable of holding a credential value at all** (a structural test asserts
this, not just a behavioral one), and `Response.Credential` is never touched by anything that
writes to the audit store.

**A genuine edge case, handled explicitly**: `Service.Issue` mints via a real provider call and
*then* records the audit entry. If the audit write fails, the credential has already been
irreversibly minted — refusing to return it to the caller would just cause a retry that mints a
second, redundant one, worse than the original problem. `ErrAuditRecordFailed` signals this
distinctly so the HTTP handler still returns the credential (loudly logging the audit gap in
ordinary logs, since the trail itself is meant to stay separate from those).

## 8. ADR 0084's two asks, resolved

**(a) The broker must accept workload tokens.** Given the exact same treatment as the gateway
route (ADR 0059): its own router group, using `auth.WithWorkloadVerifier(deps.Workload)` via a new
shared helper (`workloadTrustingOpts`) factored out of the gateway route's own wiring so the two
don't duplicate the "OIDC or workload token" option-building logic. Every other `/api/*` route —
install/uninstall in particular — keeps the single-issuer middleware, the same scoping rule
`docs/decisions/0011` already established for the gateway route and now reapplies here for the
identical reason (a workload token can carry the owner role; it must never open an
owner-gated administrative route, only ever be *forwarded/relayed* as a caller identity).

**(b) Whether a non-person identity is needed for unattended renewal.** **Ruling: not needed —
the existing mechanism already covers this without any new identity class.**
`workload.Service.Mint`'s `owner` field already accepts **any** user's `sub`, looked up live; there
is no code-level requirement that it be the warehouse's original creator specifically. If that
person hasn't signed in within the 7-day recency window, `booth-lakehouse`'s renewal logic can name
a **different, currently-active** owner-role member of the workspace instead — a small change on
`booth-lakehouse`'s own side (pick a fallback `owner`, or rotate through current owners), not a
core mechanism change. Building an actual ownerless "service identity" would mean widening ADR
0025's role model itself (where does its role come from, if not a person's token?) — a genuinely
larger architectural question than this build warrants opening unilaterally, especially with only
one real consumer having hit the edge of the existing design so far. **Deferred, not rejected**:
revisit if a second real consumer surfaces a shape the "any current owner" escape hatch doesn't
cover (e.g. a workspace with zero remaining active owners at all, which is a different and
arguably more fundamental problem than credential renewal specifically).

## Honest residual limits

1. **No real provider exists anywhere in the fleet yet** (`booth-storage`'s own docs/decisions/0006
   says so explicitly: "not building the provider endpoint itself... core's broker routing landing
   first is the correct order"). Tested against a real HTTP server built for this purpose
   (`internal/credentialbroker`'s `realProvider`, `internal/api`'s `credProvider`) — a genuine
   socket, genuine JSON marshaling, checking the real derived `Authorization` header the same way a
   real provider would — but not against `booth-storage`'s actual code, which doesn't exist. The
   kind-deploy CI check (`Verify credential broker`) proves the manifest-driven Secret delivery and
   401 rejection on the real deployed chart, but — for the same reason the workload-identity CI
   check can't mint a real token (a placeholder OIDC issuer, no real login flow) — doesn't exercise
   a full successful 201 issuance end to end.
2. **The audit-swap window.** Same honest limit `directory.Switchable` already has: an entry
   recorded before core's own database becomes available is not retroactively persisted.
3. **No read API for the audit trail.** An operator reads `booth_credential_issuances` directly
   (`psql`), the same accepted scope-cut ADR 0056's "no UI to see/revoke" residual limit already
   set as precedent for this project.
4. **A provider that mis-scopes is trusted.** The broker can't verify a provider actually honoured
   the requested `scope` — `booth-lakehouse`'s own client already treats this as its problem
   ("the client refuses any grant whose echoed scope differs from the request"), consistent with
   ADR 0080 making the *provider* responsible for correct scoping, not the broker.

## Flagged back to booth-architecture

- `contracts/credential-broker.md`: fill in the "not yet fixed" list with this document's answers
  — the exact request/response shape (§1, including the `access` field placement), the manifest
  field (`providesCredentials: {kinds: [...]}`, provider-only), the fixed provider path
  (`POST /internal/credentials`) and its own auth mechanism (§5), the one-provider-per-kind routing
  rule (§4), and the TTL ceiling/floor interaction (§6).
- `booth-storage`'s brief: the provider path and provider-credential-verification mechanism are now
  concrete (§5) — its own `docs/decisions/0006` can move from "design only" to implementation
  against a real contract.
- `booth-lakehouse`'s brief: ADR 0084's non-person-identity ask is ruled "not needed" (§8b) — the
  existing `owner` field already supports naming any current workspace owner as a renewal fallback.
