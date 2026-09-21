# booth-core decision 0011: The gateway accepts workload tokens (ADR 0059)

Status: **implemented; one scoping call and two implementation details flagged back to
booth-architecture.** ADR 0059 decides *that* core's own gateway route trusts workload tokens
(ADR 0056/0058) as a second issuer, dispatched by `iss`, producing the same `Claims` as a human's.
It leaves the placement and a few mechanics to core. Those are the calls made here.

## What was built as specified

- A workload token presented at `/modules/{id}/*` is verified against core's own signing key and
  yields `auth.Claims{Subject, Groups}` — the same shape `auth.Verifier` returns — so
  `DeriveMemberships`, `ResolveActiveWorkspace`, and the `X-Booth-Workspace`/`X-Booth-Role`
  forwarding are unchanged and unaware which issuer vouched for the caller. The module receives the
  original token and re-verifies it itself, as with a human's.
- `auth.Middleware` gains `WithWorkloadVerifier(v)`; `workload.Service` is the verifier
  (`Issuer()`, `Verify()`), checking against the in-process key rather than fetching its own JWKS.

## Calls made (flag)

### 1. Trusted on the gateway route only, not on `auth.Middleware` everywhere (**scoping call — flag**)

ADR 0059 allows either extending `auth.Middleware` or a wrapper on the gateway route. The route's
shape decides it: `auth.Middleware` also guards `POST /api/modules/{id}/install` and `DELETE
/api/modules/{id}`, which are owner-gated, and a workload token can legitimately carry the `owner`
role. If the option were global, a job's token could install or uninstall modules on core itself.
So the gateway route now sits in its own router group with the option; every other core route keeps
the single-issuer middleware and answers a workload token with 401. The gateway is where a token is
*forwarded* as a caller identity; it isn't a credential for acting on core. Pinned by
`TestWorkloadTokensAreRejectedByEveryCoreRouteExceptTheGateway`, which fails if the option is widened.

### 2. Dispatch is exclusive, on the unverified `iss`

The middleware reads the token's `iss` *without verifying it* solely to pick a verifier. A token whose
issuer is core's goes to the workload verifier and only it; everything else goes to the OIDC verifier
and only it. There is no "try one, then the other". A forged `iss` can at worst steer a token to the
wrong verifier, which rejects it on signature. Both verifiers also check `iss` themselves, so the
separation doesn't rest on dispatch alone (go-oidc rejects a relabelled token even if dispatch were
wrong). The workload verifier accepts: RS256 only; core's key; `iss` = core's issuer; `aud` including
the configured audience; unexpired and not-before (5 s leeway); a non-empty `sub`; and the
`booth_module` claim only `Mint` writes.

### 3. Workload tokens don't depend on the IdP, and never enter the user directory

- A run's token verifies against core's own key, so with the OIDC provider unreachable, workload
  traffic through the gateway still works while human requests still get 503. (Consistent with the
  point of ADR 0056: unattended runs shouldn't be at the mercy of a human login path.)
- The `ClaimsObserver` that feeds the user directory (ADR 0047) is **not** called for workload tokens:
  `sub` is `job:42`, a run, not a person, and would otherwise be recorded as a user.

## Not changed

ADR 0057's boundary is untouched: the minting credential stays out of the runner pod; this only
concerns how the already-minted token is verified when used. Nothing about minting changed.

## Honest residual limits

1. **An issued token stays valid until it expires (≤10 minutes)**, and the gateway checks only the
   token, not the owner's live role, exactly like every module. A demotion shows up at the next mint.
2. **The gateway now trusts one more key.** Anyone who obtained `booth-workload-keys` could mint
   arbitrary run tokens accepted at the gateway (and by every module trusting core's issuer). That's
   the same exposure ADR 0056 already created for modules; the gateway adds no new secret.
3. **`booth-pipeline`'s `runner.networkPolicy.egress.extra` entries** (its direct-Service workaround)
   are its own to remove; nothing in core changes for that.

## Flagged back to booth-architecture

- ADR 0059: state that the gateway trusts workload tokens on the `/modules/{id}/*` route **only** (the
  scoping call above), and that core's own `/api/*` routes deliberately do not.
- `contracts/core-platform-api.md`: the gateway item can say workload tokens don't populate the user
  directory and are verified independently of the OIDC provider's availability.
- `booth-pipeline`: its default can flip back to gateway-routed (`BOOTH_CORE_URL/modules/{storage,
  catalog}`) — see the commit that lands this.
