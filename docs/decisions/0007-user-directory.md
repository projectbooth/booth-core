# booth-core decision 0007: The user directory (ADR 0047)

Status: **implemented; one scoping decision flagged back to booth-architecture.** ADR 0047
decides *that* core keeps a `sub` → display-claims mapping, populated opportunistically, with
`GET /api/users/{sub}` and `GET /api/users?q=`. It's silent on tenancy and on a few shapes.
Those are the calls made here.

## What was built as specified

- Upserted from every token core's auth path verifies (`auth.WithClaimsObserver`, attached to
  every authenticated route — including `GET /api/me`), from `preferred_username`, `name`,
  and `email`, with a `lastSeenAt`. No admin action, no sync, no IdP admin API.
- `GET /api/users/{sub}` and `GET /api/users?q=&limit=` through the normal auth path.
- Scoped to identities core has actually verified. Third-party identities are out of scope.

## Decision 1 (**flag back**): reads are scoped to the caller's active workspace

ADR 0047 says the directory is "display data, not a permissions concept" and doesn't mention
tenancy. But ADR 0008 makes multi-workspace real from v0, and a directory that any
authenticated user can search would let a member of workspace A enumerate every user of
workspace B on the same deployment — names and emails included.

So each entry remembers the workspace slugs the user belonged to **when last seen** (from the
same token), and:

- `GET /api/users?q=` returns only users who share the caller's **active** workspace;
- `GET /api/users/{sub}` returns 404 unless the user shares it — the same 404 as for an
  unknown `sub`, so the endpoint can't be used to probe which identities exist;
- the workspace list is never returned (it would leak a user's other memberships).

These routes therefore require `X-Workspace` like every other authenticated route (only
`GET /api/me` is exempt, ADR 0034).

**Consequences to know about:**

- A module resolving a stored `owner` reference works when the owner is (still) a member of
  the workspace the caller is acting in — the normal case, since the owner registered the item
  *in* that workspace. If they've since left, the lookup 404s and the module should fall back
  to the raw `sub`, which is what a free-form-owner module does today anyway.
- Visibility follows membership as of the user's most recent token, so it lags a revocation
  until that user's next request. Same staleness class as ADR 0025's token-derived roles.
- If you'd rather the directory be deployment-wide (the literal reading of ADR 0047), it's a
  one-line change in each store's read query — but I'd want that to be a deliberate decision
  about cross-tenant visibility, not a default.

## Decision 2: empty claims never erase stored ones

"Most recent display claims" is applied per field: a token from a client scope that omits
`email` doesn't blank an email an earlier token supplied. (`preferred_username` and `name`
likewise.) Membership lists, by contrast, are *replaced* each time — leaving a workspace must
remove visibility.

## Decision 3: storage

`booth_users` in core's own database on the shared PostgreSQL cluster (ADR 0014) when
`BOOTH_POSTGRES_DSN` is set; otherwise an in-memory store. The in-memory fallback loses entries
on restart but **repopulates itself** as people make authenticated requests, so it degrades
gracefully rather than breaking anything. The schema is one idempotent `CREATE ... IF NOT
EXISTS`, applied lazily on first successful use and retried on failure, so a database that isn't
up at boot doesn't crash core — directory calls just fail (503) until it is. A migration tool
becomes worth adding when core has a second table.

**Unresolved (not core's to decide alone): who provisions the PostgreSQL cluster and core's
database/role on it (ADR 0014).** The chart passes a DSN through if you supply one but deploys no
Postgres. Until something does, a default install runs on the in-memory fallback.

## Smaller shape decisions

- Write cost: every authenticated request passes the observer, so unchanged identities are
  debounced in-process (one write per user per 5 minutes; immediately on any change to claims
  or memberships). A failed write is logged, never fails the request, and is retried on the
  next one.
- Wire shape: `GET /api/users/{sub}` returns an object, `GET /api/users` a bare array (matching
  `/api/modules`); fields `sub`, `preferredUsername`, `name`, `email`, `displayName`,
  `lastSeenAt`. `displayName` (name, else username, else email, else sub) is a convenience so
  callers don't each reimplement the fallback. `limit` defaults to 25, max 100; `q` max 100
  characters; search is case-insensitive substring over name, username, and email, and `%`/`_`
  in `q` match literally.
- A `sub` with reserved characters (some IdPs issue `auth0|abc`) works: the path parameter is
  percent-decoded.

## Flagged back to booth-architecture

- ADR 0047 / `contracts/core-platform-api.md` (User directory bullet): state that reads are
  workspace-scoped (or overrule that), and document the response shapes above.
- The Postgres-provisioning question under decision 3.
