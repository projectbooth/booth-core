# Accounts and workspaces

`booth-core` has no user, workspace, or membership management of its own, by design
(`booth-architecture/decisions/0025-workspace-role-claim-shape.md`). A caller's workspace
membership and role come entirely from the `groups` claim on their verified token — the grammar
is `/workspaces/<slug>/(owner|editor|viewer)`, plus the workspace-independent
`/platform/operator` claim (ADR 0094) — read directly off the token by core and by every module
independently (`contracts/core-platform-api.md`'s "Auth enforcement"). Keycloak (or whichever
OIDC provider a deployment runs) is the actual source of truth for who exists and what they
belong to.

`scripts/booth-admin` is a small operator script that maintains that structure in Keycloak: it
creates and nests the groups the grammar above expects, creates users, and manages their group
memberships. It never talks to `booth-core` itself.

## Prerequisite: this script does not provision Keycloak itself

`scripts/booth-admin` assumes a realm already exists with:

- a client it can use (direct-access-grants enabled, if you intend to fetch tokens non-
  interactively — the browser PKCE client a real user signs in through doesn't need this), and
- a `groups` claim mapper of type `oidc-group-membership-mapper` with `full.path: true`, so a
  nested group like `/workspaces/acme/owner` actually emits that exact string on the token,
  rather than just the group's bare name.

**As of this writing, a fresh `booth-core` Helm install does not provision any of this.**
ADR 0004 states "Keycloak ships as the default, bundled identity provider for self-hosted
installs," but `charts/booth-core` has no Keycloak dependency, template, or realm-import
mechanism at all — `oidc.issuerUrl`/`oidc.clientId` are empty by default and every real-install
instruction in this repo's own `README.md` requires the operator to supply them, pointing at
infrastructure they bring themselves. This is a real gap between that ADR's decision and what's
actually shipped, not something this script works around — it's flagged back to the architecture
coordinator separately, not fixed here. Until it's resolved, a fresh homelab install needs its
own Keycloak (or other OIDC provider), realm, client, and groups mapper set up by hand (or via
the local-dev realm export below as a starting template) before this script has anything to
manage.

The one realm this script is actually verified against today is the local-dev one described
below.

## Reaching Keycloak from your laptop

**Local development** (`booth-architecture/local-dev/docker-compose.yml`): Keycloak is already
reachable directly at `http://localhost:8081`, realm `booth-local`. No port-forward needed.

```sh
export KEYCLOAK_URL=http://localhost:8081
export KEYCLOAK_REALM=booth-local
export KEYCLOAK_ADMIN_USER=admin
export KEYCLOAK_ADMIN_PASSWORD=admin   # local-dev only; never do this for a real deployment
```

**A real cluster**: port-forward to wherever your Keycloak Service lives, then point
`KEYCLOAK_URL` at `localhost` — this script refuses a plain `http://` URL against anything that
isn't `localhost`/`127.0.0.1`, specifically so a port-forward is the normal way to reach a real
instance without the admin password or any token crossing the network in the clear:

```sh
kubectl -n <keycloak-namespace> port-forward svc/<keycloak-service> 8081:8080
export KEYCLOAK_URL=http://localhost:8081
export KEYCLOAK_REALM=<your realm>
export KEYCLOAK_ADMIN_USER=<your admin username>
# KEYCLOAK_ADMIN_PASSWORD left unset -- the script prompts for it silently.
```

## Worked example: a new workspace with an owner and an editor

```sh
$ python3 scripts/booth-admin create-workspace home
workspace 'home': created

$ python3 scripts/booth-admin create-user alice --email alice@example.com --name "Alice Smith"
user 'alice': created
Temporary password (shown once -- it is not logged or saved anywhere): <random>

$ python3 scripts/booth-admin add-member alice home owner
'alice' added as owner in 'home'

$ python3 scripts/booth-admin create-user bob --email bob@example.com --name "Bob Jones"
user 'bob': created
Temporary password (shown once -- it is not logged or saved anywhere): <random>

$ python3 scripts/booth-admin add-member bob home editor
'bob' added as editor in 'home'

$ python3 scripts/booth-admin list-members home
alice   owner
bob     editor
```

Give alice and bob their printed temporary passwords through whatever channel you'd use for any
other credential handoff (this script never stores or logs them anywhere) — they'll be forced
through Keycloak's own "update your password" screen on first sign-in.

Moving someone to a different role in the same workspace is just `add-member` again with the new
role — it removes the old role group membership first, so nobody ever ends up holding two roles
in one workspace at once:

```sh
$ python3 scripts/booth-admin add-member bob home viewer
'bob' moved from editor to viewer in 'home'
```

`remove-member` only removes the group membership — it does not delete the user, and this script
has no subcommand that deletes a user or a workspace at all.

## Facts worth knowing, stated plainly

- **A membership or role change takes effect when the user's token next refreshes** — role is
  derived from the token on every request, never cached in a database (ADR 0025). If someone is
  already signed in when you run `add-member`/`remove-member`, they won't see the change until
  their session token refreshes on its own, or they sign out and back in to force it immediately.
- **A new user appears in core's own user directory only after their first sign-in.** The
  directory (ADR 0047) is populated from verified tokens core itself sees, not from Keycloak
  directly — `create-user` makes the account exist in Keycloak, but core has no record of them
  (e.g. for `GET /api/users` lookups another module might rely on) until they actually log in at
  least once.
- **A `booth-api` key stops working a week after its creator last signed in.** `booth-api` mints
  a workload token per workspace owned by the person who created the key, and minting refuses
  once that owner hasn't been seen in a verified token for longer than `BOOTH_WORKLOAD_OWNER_MAX_AGE`
  (default 7 days) — ADR 0103. If a key you depend on has gone quiet, check whether its creator
  has signed in recently before assuming anything else is wrong.
- **Creating a user without `--name` is fine, but may surface a profile-completion prompt on
  first sign-in.** Keycloak's default user-profile validation can require a first/last name
  before considering an account "fully set up." A real person hitting this in the normal browser
  sign-in flow just sees an ordinary "complete your profile" form — harmless — but any
  non-interactive flow (e.g. testing with a direct password grant) will get a generic
  "Account is not fully set up" error until a name is set. Passing `--name` up front avoids it
  entirely.
- **Membership changes are an IdP-side operation, not an instant, core-visible one.** There is no
  notification, webhook, or event when you run this script — the change exists in Keycloak the
  moment the command succeeds, but nothing *tells* core or any module about it; they simply see
  it the next time they verify that user's token.

## Automated verification

`.github/workflows/integration.yml`'s `booth-admin-check` job runs this script's full sequence
against a real, disposable Keycloak container on every push to `master` and nightly: creates a
workspace, creates a user, adds them as editor then viewer (asserting exactly one role survives),
removes them (twice, asserting idempotency), re-creates the workspace and re-adds the membership
(also asserting idempotency), fetches a real token, decodes it, and asserts the `groups` claim
reads exactly `["/workspaces/<slug>/<role>"]` — then builds `booth-core` itself, points it at that
same Keycloak, and asserts `GET /api/me` reports the matching workspace and role. This is the same
sequence the manual steps above walk through by hand; the CI job exists so a future change to
this script or to core's own claim handling can't silently break either half without a test
noticing.

The realm it runs against (`scripts/testdata/ci-realm.json`) is a minimal fixture owned by this
repo for exactly this check — not the local-dev realm shown above (which lives in a sibling repo
this CI job doesn't check out), and not a stand-in for the still-open "what does a real install's
Keycloak actually ship with" question noted above.
