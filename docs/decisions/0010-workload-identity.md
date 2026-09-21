# booth-core decision 0010: Workload identity — how a run's role is derived (ADR 0056)

Status: **implemented; three contract gaps flagged back to booth-architecture.** ADR 0056 decides
the outcome (core mints short-lived, workspace-scoped tokens for unattended runs; `groups` shaped
like a human's; role re-derived at mint time; a minting credential only for modules that declare
`workloadIdentity`). It doesn't say where "whatever the run's owning user currently holds" comes
from, or how the request names that user. Those are the calls made here.

## The gap: core has no live source for a user's role

ADR 0056 says the token's role is "the lesser of `roleCeiling` and whatever the run's owning user
currently, actually holds in that workspace". Two things make that under-specified:

1. **The request has no owner.** `{workspace, subject, roleCeiling}` — and `subject` must name the
   run, never a person. Nothing tells core *whose* role to cap by.
2. **Roles exist only inside tokens.** ADR 0025/decision 0001 make the JWT authoritative ("not from
   any separately-stored membership table"), and ADR 0047 explicitly rejected an IdP admin API. The
   directory (ADR 0047) recorded workspace *slugs* per user, not roles. So there is nothing for core
   to read "live" *from*, other than tokens it has already seen.

## What was decided

### 1. `owner` is a required request field (**contract addition — flag**)

`POST /api/internal/workload-tokens` takes `{workspace, subject, roleCeiling, owner}`; `owner` is the
`sub` of the user the run belongs to. The module already has it (it's the user who created the
schedule). It is used only to look up a role; it never appears in the token, so `sub` still names
only the run and downstream audit still separates "a human did this" from "job X did this".

### 2. The role comes from the directory, which now records roles (**semantic call — flag**)

The user directory records each user's role per workspace (`booth_users.roles`, added with
`ALTER TABLE ... ADD COLUMN IF NOT EXISTS`, so an existing table upgrades in place and pre-existing
rows read as "no role", never a default one) from every token core verifies, through the same
observer that already feeds it. So "live" here means **the role in the owner's most recent verified
token** — the same staleness class ADR 0025 already has for human sessions, not a real-time IdP query.

- A demotion is written immediately (the recorder's debounce fingerprint includes roles), so the next
  mint after the owner's next request is lower.
- A **removed** user's token lists no membership → the entry's workspaces are replaced → mints refused.
- If a token lists two roles for one workspace (ADR 0025 doesn't say which wins), the **lesser** is
  recorded: this value only ever caps privilege, so it must not round up.

**The bound.** A user removed from the IdP who never signs in again is never re-recorded, so without
more their last role would stand forever and their scheduled jobs would stay privileged — exactly what
the ADR exists to prevent. So a role only counts if the owner was seen within
`workloadIdentity.ownerMaxAge` (default **7 days**; `BOOTH_WORKLOAD_OWNER_MAX_AGE`). Past that, mints
are refused (403) until they next use the platform. The trade-off is real: a run owned by someone who
never opens the UI stops minting after a week. Raise the value for such deployments, or — if that's
the norm — decide that core should query the IdP instead (see below).

**Not chosen: an IdP admin lookup** (Keycloak first). It would be genuinely live, but it ties core to
one provider's admin API — the thing ADR 0047 and ADR 0004 avoided — and needs a configured
service account. The seam is small (`workload.Service.ownerRole`) if that's decided later.

**Consequence:** the directory must be persistent for this to survive a core restart. On the bundled
Postgres it is; with the in-memory fallback, core logs a warning at startup, and no run can mint until
its owner makes an authenticated request again.

### 3. Minting credentials are derived, not stored

`bwmc.<module-id>.<HMAC-SHA256(key, module-id)>`, with the key in the core-only Secret
`booth-workload-keys` next to the RSA signing key. Any core replica verifies with no shared state.
Verification is *also* a live registry check that the module **currently** declares
`workloadIdentity.mint`, so removing the field (or uninstalling) revokes a copy of the credential
immediately — no revocation list. The credential binds to its module: relabelling it fails the MAC.
It shares the JWT's three-dot shape but has a fixed `bwmc` prefix and a MAC, so no token from any
issuer — the IdP's, or core's own workload tokens — can authenticate to the minting endpoint; it is a
separate path from `auth.Middleware`, which only ever trusts the OIDC provider.

The credential is delivered as the Secret `booth-workload-minting-credentials` (**convention — flag**):
`credential`, `url` (the full minting endpoint), `issuer` (what to trust). Owned by the `BoothModule`
when in the same namespace, removed if the module drops the field, re-issued if the keys change.

### 4. Token and issuer details

- **RS256**, `kid` = the key's thumbprint. RS256 because it's the algorithm every JWT library's
  default accepts (go-oidc's default set is RS256 only), so adding core as a second issuer needs no
  algorithm configuration.
- **`iss`** = `BOOTH_WORKLOAD_ISSUER_URL` (chart default: core's in-cluster Service URL). **`aud`** =
  the OIDC client ID, i.e. what modules already expect of a human token. **`groups`** claim uses the
  deployment's configured claim name, with exactly one entry for one workspace. **`jti`** is random.
  `booth_module` names the requesting module, for audit only. No email/name claims.
- **Well-known paths** (**flag**): `/.well-known/jwks.json` and, additionally,
  `/.well-known/openid-configuration` — ADR 0056 asks only for the JWKS, but modules using go-oidc
  need a discovery document to add an issuer, and serving one makes "trust this issuer" the same
  one-liner as for the IdP. Both are public and unauthenticated (they hold no secrets).
- **`subject` must look like `<kind>:<id>`** (e.g. `job:42`). ADR 0056 says "never a person"; a
  person's `sub` (UUID, `auth0|…`, email) never has that shape, so it is structurally enforced.
- The response is `{token, tokenType, expiresAt, role}`; `role` is the granted role, so a caller can
  see when it got less than it asked for. `Cache-Control: no-store`.
- Refusals: 401 not a credential core issued; 403 module not (or no longer) entitled, or owner has no
  current access (deliberately one message, so it can't probe membership); 400 malformed; 503 directory
  down.
- Every mint is logged (module, subject, workspace, owner, ceiling, granted) — never the token.

## Honest residual limits

1. **Staleness is bounded, not zero** (see 2): up to `ownerMaxAge` for an owner who vanishes without
   a further token, and one owner-request for a demotion. A 10-minute token can outlive a demotion by
   up to 10 minutes; nothing revokes an already-issued token.
2. **A module holding a minting credential can pick any `owner` core has seen in that workspace**, and
   so mint up to that user's role for a run of its own choosing. That's inherent in "a module declares
   it runs unattended work on a workspace's behalf": the credential is trusted to name the right owner.
   The ceiling and the owner's real role still bound it, and it can never mint above what *some real
   member* holds. Installing a module chart remains an owner-gated action (as for every manifest field).
3. **Key rotation isn't built.** The JWKS format supports several keys, but there's one, from
   `booth-workload-keys`. Deleting that Secret regenerates the key and credentials (credentials
   self-heal on the next reconcile; tokens in flight, at most 10 minutes' worth, fail).
4. **The endpoint is on core's main listener.** It's credential-protected, but shouldn't be routed
   through an ingress; the chart values say so.
5. **Role recording only sees tokens that reach core.** A user who only ever talks to modules directly
   is never recorded — the same limitation the user directory already has.

## Flagged back to booth-architecture

- ADR 0056 / `contracts/core-platform-api.md`: add `owner` to the minting request, and state where
  the role's "live" source is (the directory, bounded by a max age) — or decide on an IdP lookup.
- `contracts/core-platform-api.md`: document the Secret keys (`credential`, `url`, `issuer`), the
  response shape, the 401/403/400/503 semantics, and the `<kind>:<id>` subject rule.
- ADR 0056: name the well-known paths (`/.well-known/jwks.json`, `/.well-known/openid-configuration`).
- `booth-storage`, `booth-catalog` briefs: trust `iss` = core's issuer URL via OIDC discovery; their
  existing role derivation needs nothing else. `booth-pipeline`: send `owner` (the schedule's creator's
  `sub`), keep the minting credential out of the task-execution pod, expect 403 when the owner has lapsed.
