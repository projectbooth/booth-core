# booth-core decision 0016: Public route prefixes (ADR 0101)

Status: **implemented.** ADR 0101 decides the shape — an optional `publicRoutes` manifest field,
a distinct `/modules/{id}/public/<prefix>` gateway URL, no authentication at all on that path, and
the stripped trust-header set. This records the calls left to this session.

## 1. Route precedence: a literal segment, not a flag

`/modules/{id}/public/*` is registered as its own plain `r.Handle` (no `auth.Middleware` group at
all), separately from the existing authenticated `/modules/{id}/*` group. chi's router matches the
more specific literal `/public/` segment ahead of the wildcard `*` in the ordinary route regardless
of registration order — verified directly (`TestRouter_PublicRouteRequiresNoPlatformLogin`,
`TestRouter_OrdinaryGatewayRouteStillRequiresAuth`) rather than assumed, since getting this wrong
either way is a real risk: public traffic accidentally requiring auth would be a availability bug,
and ordinary traffic accidentally skipping auth would be the opposite kind of bug entirely.

## 2. Prefix matching is a plain `strings.HasPrefix` on the module-relative forwarded path

A request to `/modules/api/public/v1/x` forwards `/v1/x` to the module — the same path shape the
ordinary route already forwards, prefix included. `matchesPublicRoute`
(`internal/gateway/proxy.go`) checks the module's declared `publicRoutes.pathPrefixes` with a
plain prefix check; a module whose `PublicRoutes` field is nil (the overwhelming common case)
matches nothing, and anything under `/public/` that doesn't match a declared prefix is a 404 from
core, never reaching the module (`TestPublicHandler_UndeclaredPathUnderPublicIs404`,
`TestRouter_UndeclaredPathUnderPublicIs404`).

## 3. Header handling: strip and never set, let everything else through unmodified

`PublicHandler`'s proxy `Director` deletes `X-Booth-Workspace`/`X-Booth-Role`/`X-Booth-Identity`
from the inbound request (covering both a caller trying to forge one and a stale value that
somehow arrived) and never sets any of them — this route has no verified identity to attach, by
design. `Authorization` and every other header pass through completely untouched: unlike the
ordinary route's `proxyTo` (which overwrites `Authorization` with the token `auth.Middleware` just
verified), there is no verified token here to substitute, so an API key (`Authorization: Bearer
booth_ak_...`) reaches the module exactly as the caller sent it. Pinned by
`TestPublicHandler_StripsTrustHeadersAndLeavesOthersUntouched` and
`TestRouter_PublicRouteStripsTrustHeaders` (the same property through the real chi router, not
just the handler in isolation).

## 4. Manifest/CRD: a struct, not a bare list

`PublicRoutesSpec{PathPrefixes []string}` (`api/v1alpha1/boothmodule_types.go`) rather than a bare
`[]string` field directly on `BoothModuleSpec`, matching this file's existing convention
(`WorkloadIdentityRequirement`, `DatabaseRequirement`) so the field can grow (e.g. a future
per-prefix rate limit) without a new top-level manifest key. `PathPrefixes` requires at least one
entry and each must match `^/.*/$` (starts and ends with `/`), mirroring the contract's own
grammar. CRD regenerated with `controller-gen` (`config/crd/bases/`, copied to
`charts/booth-core/crds/` — the two have always been kept as plain copies of each other, not
independently maintained).

## 5. Not done here: `devregistry`

The local-dev static-file registry (`internal/devregistry`) has its own hand-rolled `fileEntry`
struct that already omits several other optional manifest fields (`Events`, `Database`,
`WorkloadIdentity`, `ProvidesCredentials`) — it was never meant to mirror every manifest field, only
enough for local development without a cluster. `publicRoutes` follows that same precedent and
isn't added there; nothing stops it being added later if local-dev testing of this feature turns
out to need it.

## Honest residual limits

1. **No rate limiting**, per ADR 0101's own explicit deferral — this is the first route in core
   that proxies traffic nobody has authenticated. `contracts/module-manifest.md` already states
   not to expose core to the public internet while any module declares `publicRoutes` until that's
   decided; nothing in this implementation changes that posture.
2. **No Ingress/TLS story** (`ARCHITECTURE.md` item 37a) — unaffected by this change, stays open.
