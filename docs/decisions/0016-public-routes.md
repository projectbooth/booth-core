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

## 2. Prefix matching is `strings.HasPrefix` — against a decoded, validated, cleaned path, not the raw one chi hands over

A request to `/modules/api/public/v1/x` forwards `/v1/x` to the module — the same path shape the
ordinary route already forwards, prefix included. `matchesPublicRoute`
(`internal/gateway/proxy.go`) checks the module's declared `publicRoutes.pathPrefixes` with a
plain prefix check; a module whose `PublicRoutes` field is nil (the overwhelming common case)
matches nothing, and anything under `/public/` that doesn't match a declared prefix is a 404 from
core, never reaching the module (`TestPublicHandler_UndeclaredPathUnderPublicIs404`,
`TestRouter_UndeclaredPathUnderPublicIs404`).

**This was a real path-traversal bug, not a theoretical one, caught in review before merge.** The
prefix check originally ran directly against whatever `remainderPath(r)` returned, and that same
value was then forwarded unmodified. `GET /modules/api/public/v1/../admin` forwards
`/v1/../admin`, which passes `strings.HasPrefix(path, "/v1/")` textually and would have reached
the module exactly as sent — the declared prefix bounded nothing. Tracing it found something
worse than plain `".."` handling: chi's router (`go-chi/chi/v5`'s `mux.go`, `routeHTTP`) matches
against `r.URL.RawPath` whenever it's non-empty, falling back to `r.URL.Path` only otherwise, and
`net/url` sets `RawPath` to the caller's **original, undecoded** text whenever `Path`'s own
canonical re-escaping wouldn't reproduce it — which covers any percent-encoded `.`, `/`, or `\`
(none of which strictly need encoding). So `chi.URLParam(r, "*")` can hand back either raw wire
text or an already-decoded-once string depending on exactly what encoding the caller used,
inconsistently — a bare substring check for `%2e`/`%2f` (an earlier version of this fix) caught
some encoded variants only by accident of which branch chi happened to take, and missed others
(an encoded backslash, verified to still reach the module backend in a now-passing regression
test before the real fix landed).

**Fix**: `PublicHandler` now calls `url.PathUnescape` on `remainderPath(r)` exactly once,
explicitly, before anything else — this is correct regardless of how many times (zero or one)
net/url/chi already decoded the string, because the two paths this fix actually has to handle are
"chi handed back raw wire text, needs one decode" and "chi handed back already-decoded text that
itself still contains a literal encoded sequence because the caller double-encoded it, needs one
more decode" — both resolved by exactly one additional explicit pass. `publicPathIsSafe` then
rejects (404) a `.` or `..` segment, a backslash, or a NUL byte in that decoded string — a hard
refusal, not an attempt to clean these specific shapes up, since a public route is unauthenticated
and the conservative answer to "does this look like an escape attempt" is to refuse outright.
`cleanPublicPath` (`path.Clean`, with a trailing slash restored if the input had one — `path.Clean`
drops it, which would otherwise break matching a request for exactly a bare declared prefix)
canonicalizes what's left, and matching + forwarding both happen against that same cleaned string,
never the original. `req.URL.RawPath` is explicitly cleared before forwarding too, so the proxy's
own request serialization can't fall back to the caller's stale, unvalidated raw encoding instead
of the rewritten `Path`.

Tests at both the `Gateway`-unit level (`internal/gateway/public_test.go`) and the real chi router
(`internal/api/public_routes_test.go`): a literal `..`, singly- and double-percent-encoded `..`
(both cases), an encoded slash splitting a `..` out of what looked like one segment, a literal
`.`, a backslash, an embedded NUL, a doubled slash (benign — still matches and forwards in its
cleaned form), a request for exactly a declared prefix with its trailing slash, and an ordinary
multi-segment path. Mutation-tested: reverted to the original naive forwarding and confirmed every
one of these fails, then restored and reconfirmed green. The `Gateway`-unit test harness's own
`chiStarParam` helper was itself a source of false confidence on the first pass — it read
`r.URL.Path` directly, which doesn't reproduce chi's `RawPath`-preference, so a backslash test
passed at that level while still reaching the module through the real router. Fixed to mirror
chi's actual preference explicitly, rather than trust it implicitly.

**The existing authenticated `/modules/{id}/*` route (`Handler`/`proxyTo`) has the identical
mechanism — same unclean, unvalidated `r.URL.Path = forwardPath` — but not the same weakness.**
Checked directly, not assumed: `Handler` (`internal/gateway/proxy.go`) imposes no declared-prefix
or any other path-based restriction at all — an authenticated caller for a module can already
reach any path on it, with or without a traversal trick, since there is no allowlist for one to
bypass. A `..` there can only ever change which path gets requested on the *same* module's *same*
backend (the target host:port comes from `mod.BaseURL()`, fixed independently of the forwarded
path), never grant access to a path that caller's identity/workspace/role didn't already allow.
Left unchanged in this PR, per instruction — flagged here, not fixed, in case a future prefix-style
restriction is ever added to that route too.

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
