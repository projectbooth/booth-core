# booth-core decision 0001: Workspace-role claim shape

Status: proposed by booth-core, needs to return to booth-architecture as an ADR before
any other module treats it as settled (per `agent-briefs/core.md`'s open questions and
`ARCHITECTURE.md`'s ground rule).

## Context

ADR 0004 (pluggable OIDC) and ADR 0008 (multi-workspace from v0) both point at
`OpenDataPlatform`'s `groups` claim -> `/workspaces/<name>/<role>` pattern as the
reference shape, but neither nails down the exact grammar. `contracts/core-platform-api.md`
lists this as "explicitly not decided yet." booth-core's OIDC integration needs a concrete
answer to build against.

## Decision

1. **Claim name**: configurable per deployment (`BOOTH_OIDC_GROUPS_CLAIM`, default
   `groups`), since not every OIDC provider names this claim the same way. Keycloak's
   default is `groups` when a group-membership mapper is attached to the client scope.

2. **Entry grammar**: each string in the claim's array is matched against
   `^/workspaces/([a-z0-9-]+)/(owner|editor|viewer)$`. Anything not matching this shape is
   ignored (a user may belong to unrelated IdP groups for other purposes). Capture group 1
   is the workspace slug, capture group 2 is the role.

   Example `groups` claim value:
   ```json
   ["/workspaces/acme-analytics/owner", "/workspaces/acme-marketing/viewer"]
   ```

3. **Workspace identity**: the slug (`acme-analytics` above) is the workspace's stable
   identifier everywhere in core's API and every module's data model — not a separate
   UUID. It must match `^[a-z0-9-]+$`, mirrors the module `id` pattern already fixed in
   `contracts/module-manifest.md` for consistency, and is what a workspace is created
   with in Keycloak (as a top-level group, e.g. `/workspaces/acme-analytics`, with
   `owner`/`editor`/`viewer` subgroups) or in whatever equivalent structure another OIDC
   provider uses.

4. **Role set**: exactly `owner`, `editor`, `viewer`, per ADR 0008. `owner` is the role
   ADR 0023's `adminNavPath` gating checks for. No finer-grained roles in v0 — a module
   needing more than this should handle it with its own logic against these three, not by
   inventing a fourth role unilaterally.

5. **A user with zero matching entries** is authenticated but has no workspace to act in —
   the shell shows an empty/onboarding state rather than an error. A user is not required
   to have a "default" workspace; the frontend persists last-selected workspace (e.g. in
   local storage) and re-validates it against the token's current claim set on each
   session.

6. **Active-workspace selection on each request**: the client sends the active workspace
   slug as an `X-Workspace` HTTP header on every API call (extending
   `OpenDataPlatform`'s pattern, ADR 0004's consequences). Core's auth middleware
   validates the caller actually holds a role entry for that slug — reading it from the
   JWT is authoritative, not from any separately-stored membership table — and rejects
   with 403 if not. The gateway forwards this validated `(workspace, role)` pair to the
   backing module as two headers, `X-Booth-Workspace` and `X-Booth-Role`, alongside the
   already-forwarded/re-verifiable JWT, so a module doesn't have to re-parse the groups
   claim itself to get the resolved role for the request's workspace.

## Why derive from the token, not a database, on every request

Keeps core statelessly consistent with whatever the IdP currently says a user's
memberships are — a revoked membership takes effect on the user's next token refresh, not
on some separate cache-invalidation path. Workspace *metadata* (display name, settings)
still lives in core's own Postgres database; only the *membership/role* fact is
token-derived. If IdP-side group management proves too slow/heavy an operational path for
managing membership day-to-day, that's a real v1 reconsideration — not resolved here.

## Consequences

- Every module that wants role-gated behavior (ADR 0023's `adminNavPath` being the
  concrete v0 case) reads `X-Booth-Role` from the gateway-forwarded request rather than
  parsing tokens itself.
- Keycloak realm setup needs one top-level `workspaces` group with per-workspace
  subgroups, each with `owner`/`editor`/`viewer` sub-subgroups, and a group-membership
  protocol mapper configured to emit full group paths into the `groups` claim. This is
  the concrete Keycloak-side shape `booth-core`'s bundled realm config/import will ship.
- `booth-design` needs this claim shape to build the workspace switcher against — this is
  the "agree on session/identity shape... early" dependency the brief calls out.

## Flagged back

This is a booth-core proposal, not a unilaterally-settled cross-cutting decision. It
should be reviewed and, if accepted, promoted to a `booth-architecture` ADR (filling in
`ARCHITECTURE.md` §7's "exact workspace-role claim shape" item) before `booth-design` or
any other module bakes in an assumption about it.
