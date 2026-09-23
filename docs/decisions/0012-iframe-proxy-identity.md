# booth-core decision 0012: The iframe-proxy identity assertion, implementation details (ADR 0069)

Status: **implemented.** ADR 0069 decides the outcome in full detail — a signed `X-Booth-Identity`
JWT on every iframe-proxied request, `aud` the module id, `sub` the person's own subject, `groups`
in ADR 0025's grammar, `exp` ≤ 2 minutes, a distinct issuer from the workload-token issuer, published
via its own discovery document + JWKS — and `contracts/core-platform-api.md` already carries the
wire shape. What was left to core is where exactly the issuer lives, how it's keyed, and what
happens when minting can't complete. Those are the calls made here; no contract gaps to flag back
this time.

## What was built as specified

- `internal/iframeidentity`: `Keys` (an RSA key, bootstrapped the same create-then-adopt way as
  `workload.Keys` — see `docs/decisions/0010`) and `Service` (`Mint`, `JWKS`, `Discovery`,
  `Issuer`), deliberately a sibling package to `workload`, not a variant of it: no shared key
  material, no shared Secret, no shared code path a future change to one could accidentally leak
  into the other.
- `internal/gateway`: `Gateway.IframeIdentity` (an `IframeIdentityMinter` interface, so gateway
  doesn't import the JOSE/JWT libraries directly), consulted in `proxyIframeRequest` — the one
  function both `IframeEntryHandler` and `IframeFallbackHandler` funnel through, so both entry
  points get the assertion from a single call site.
- `internal/api`: `registerIframeIdentity` mounts the two well-known routes outside
  `auth.Middleware`, mirroring `registerWorkload`.

## Calls made

### 1. The issuer lives at `<core>/iframe-identity`, a route prefix, not a second hostname

ADR 0069's own example already suggested this shape. Concretely: `iframeidentity.RoutePrefix =
"/iframe-identity"`, and `JWKSPath`/`DiscoveryPath` are relative to `Options.Issuer` exactly like
`workload.Service`'s — so `Issuer = "http://core:8080/iframe-identity"` yields `jwks_uri =
"http://core:8080/iframe-identity/.well-known/jwks.json"`, and the chi route is registered at
precisely that path. The workload issuer stays at core's root. This is what makes "distinct issuer
URL" true even when a deployment's chart renders both from the same Service address (the default):
the two are still never reachable at the same URL, so a module's OIDC-discovery-based trust config
for one can't be pointed at the other by accident.

### 2. No minting credential, no per-module Secret

Unlike workload tokens, an iframe-identity assertion is minted only by core itself, in response to
its own gateway proxying a request — never on a module's request. So there's nothing analogous to
`booth-workload-minting-credentials` to provision, and no manifest field gates it: every
iframe-proxy module gets assertions once the issuer is configured, with nothing to opt into beyond
pointing its own OIDC verification at the discovery URL (exactly what `booth-notebooks`'
`identity.py` already does, per the ADR).

### 3. Dev mode uses an ephemeral key; a real cluster persists one

`internal/iframeidentity.LoadOrCreateKeys` needs a Kubernetes client (it reads/writes the Secret
`booth-iframe-identity-keys`). Dev mode (`BOOTH_DEV_REGISTRY_PATH` set) has none, so
`cmd/core/main.go` calls `iframeidentity.NewKeys()` directly there when the issuer is configured —
the same trade-off `iframeSigningSecret`'s dev-mode fallback already makes: losing the key on
restart is a non-issue locally (every outstanding assertion lives at most 2 minutes anyway, and a
fresh one is minted on the very next proxied request).

### 4. A minting failure degrades to "no header", never a stale or forged one

`proxyIframeRequest` always calls `req.Header.Del(auth.HeaderBoothIdentity)` first — stripping
whatever the embedded page's own JS may have set — then sets a fresh one only if `Mint` succeeds.
If it fails (only plausible if the key material itself is broken), the request still proceeds
without the header rather than failing outright; the module's own required re-verification
(`core-platform-api.md`'s "Auth enforcement" item, ADR 0041) is the backstop that then rejects
whatever it can't verify. This mirrors how a missing `Authorization` header is handled on the
ordinary gateway path — the module decides what to do with an unauthenticated request, core
doesn't guess.

### 5. A real implementation pitfall worth naming: typed-nil interfaces

`Gateway.IframeIdentity` is typed as the `IframeIdentityMinter` *interface*, but
`cmd/core/main.go` only ever has a concrete `*iframeidentity.Service` (nil when the issuer is
off). Assigning a nil `*Service` to an interface-typed field unconditionally produces a **non-nil**
interface value wrapping a nil pointer — `g.IframeIdentity != nil` would then be true, and calling
`Mint` on it panics. `main.go` guards the assignment (`if iframeIdentityKeys != nil { gw.IframeIdentity
= iframeIdentitySvc }`) specifically to avoid this. `internal/gateway`'s own tests pass a literal
`nil` (an untyped nil, which *does* produce a nil interface) and so never hit this, so it's called
out here rather than left to be rediscovered by a future refactor.

## Honest residual limits

1. **No revocation, same as every short-lived-token design in this repo.** A 2-minute lifetime
   bounds the exposure; nothing invalidates an assertion early.
2. **Minting happens on the request path of every single iframe-proxied request** — an RSA
   signature per hit, not amortized like a workload token's one-per-run. Acceptable at today's
   scale; worth revisiting only if iframe-proxy traffic volume ever makes RSA signing a bottleneck
   (an EC key would be the first lever, not a redesign).
3. **Items B (shell nginx routing) and C (session renewal)** from ADR 0069 are `booth-design`'s
   side, not built here.
