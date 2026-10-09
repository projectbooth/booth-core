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

**First time setting up a bundled install?** `docs/operations/first-login-runbook.md` walks
through install → retrieve the generated admin password/certificate → create the first
workspace and user → sign in, start to finish. This doc is the reference underneath it:
`booth-admin`'s own subcommands, the shared values every module needs, and facts worth knowing
once you're running.

## Prerequisite: what realm this script expects

`scripts/booth-admin` assumes a realm already exists with:

- a client it can use (direct-access-grants enabled, if you intend to fetch tokens non-
  interactively — the browser PKCE client a real user signs in through doesn't need this), and
- a `groups` claim mapper of type `oidc-group-membership-mapper` with `full.path: true`, so a
  nested group like `/workspaces/acme/owner` actually emits that exact string on the token,
  rather than just the group's bare name.

**A default `booth-core` Helm install now provisions exactly this**, via the bundled Keycloak
(ADR 0106/0108): a starter realm named `booth`, imported on first boot, with the `workspaces`/
`platform` group tree already present and a `groups` client-scope mapper already attached — see
`docs/operations/first-login-runbook.md` for the full install-to-first-sign-in walkthrough. This
script still never talks to `booth-core` or provisions Keycloak itself; it only manages the
group/user structure inside whichever realm you point it at, bundled or external.

Running an **external** identity provider instead (`keycloak.enabled=false`)? You bring your own
realm, client, and groups mapper matching the shape above — see "Shared values across module
installs" below for the `oidc.*` values every module (including core) then needs set by hand.

The realm this script's own CI (`booth-admin-check`, below) runs against is a disposable fixture
it owns (`scripts/testdata/ci-realm.json`), not the bundled starter realm or the local-dev one
shown next.

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

## Shared values across module installs

Every module verifies the deployment's identity provider independently
(`contracts/core-platform-api.md`'s "Auth enforcement") — there is **no core-delivered
ConfigMap or Secret carrying the OIDC issuer, the ADR 0108 key-fetch override, or the
workload-issuer-trust setting**. The only configuration core actually provisions
automatically into a module's namespace is the small set of declarative, per-module
Secrets ADR 0020 already covers (`booth-database-credentials`,
`booth-event-bus-credentials`, `booth-workload-minting-credentials`) — each gated behind
that module's own manifest declaring `database`/`events`/`workloadIdentity.mint`, and
none of them carry the primary IdP's issuer or key-fetch settings. Nothing reads these
values from anywhere but its own chart's `--set`/`values.yaml` — an operator sets them
on every module install, by hand, every time. This was checked directly against every
module's chart, not assumed from its absence.

**Updated as each module's own `oidc.jwksUrl` PR landed** (checked directly against every
module's chart again, not left at the earlier snapshot): `oidc.issuerUrl` and `oidc.jwksUrl` on
9 modules, plus the workload-issuer setting on the 3 named for Streamlit apps (`booth-storage`,
`booth-catalog`, `booth-lakehouse`) — **7 different key-path spellings for 3 underlying
concepts**, not 3:

| Concept | Modules | Key path(s) actually used |
|---|---|---|
| OIDC issuer | `booth-storage`, `booth-catalog`, `booth-module-store`, `booth-api`, `booth-database`, `booth-logging`, `booth-pipeline` (7), `booth-notebooks` (optional, see below) | `oidc.issuerUrl` — consistent across all 8 |
| OIDC issuer | `booth-lakehouse` (1) | `identity.oidcIssuerUrl` — **the one outlier**, called out explicitly in `booth-e2e/bringup/bringup.sh`'s own comments ("not the `oidc.*` key every other module here uses") |
| Key-fetch override (ADR 0108) | The same 7 modules as the `oidc.issuerUrl` row above | `oidc.jwksUrl` — landed in every one of them. `booth-notebooks` has no equivalent (its default path never touches the primary IdP at all — see below) |
| Key-fetch override (ADR 0108) | `booth-lakehouse` (1) | `identity.oidcJwksUrl` — the same outlier naming as its issuer URL |
| Workload-issuer trust | `booth-storage` | `oidc.workloadIssuerUrl` |
| Workload-issuer trust | `booth-catalog` | `workloadIdentity.issuerUrl` (a separate top-level block, not under `oidc`) |
| Workload-issuer trust | `booth-lakehouse` | `identity.workloadIssuerUrl` |

**`booth-notebooks` is the one module that may need none of this at all.** Its default,
actually-configured path trusts core's own iframe-identity issuer (`identity.issuerUrl`,
pre-filled to a sensible default), not the primary IdP directly — so `oidc.jwksUrl` was never
added there, and `oidc.issuerUrl` stays optional, needed only if you deliberately reconfigure it
to trust the primary IdP directly instead.

The values an operator pastes into each module's `helm install`/`values.yaml`, using each
chart's own real key names, with the bundled Keycloak's in-cluster Service as the example
`jwksUrl`/workload-issuer target (adjust for your actual release/namespace if different from
`booth-core`/`booth-system`):

```yaml
# booth-storage, booth-module-store, booth-api, booth-database, booth-logging, booth-pipeline:
oidc:
  issuerUrl: "https://<your-idp-issuer>"
  jwksUrl: "http://booth-core-keycloak.booth-system.svc.cluster.local:8080/realms/booth/protocol/openid-connect/certs"

# booth-catalog: the same oidc.issuerUrl/jwksUrl, plus a separate block for workload-issuer trust:
oidc:
  issuerUrl: "https://<your-idp-issuer>"
  jwksUrl: "http://booth-core-keycloak.booth-system.svc.cluster.local:8080/realms/booth/protocol/openid-connect/certs"
workloadIdentity:
  issuerUrl: "http://booth-core.booth-system.svc.cluster.local:8080"

# booth-storage also needs workload-issuer trust, under oidc (not a separate block):
oidc:
  issuerUrl: "https://<your-idp-issuer>"
  jwksUrl: "http://booth-core-keycloak.booth-system.svc.cluster.local:8080/realms/booth/protocol/openid-connect/certs"
  workloadIssuerUrl: "http://booth-core.booth-system.svc.cluster.local:8080"

# booth-notebooks: both oidc.* values are optional here, and left unset by default (see above):
oidc:
  issuerUrl: "https://<your-idp-issuer>"   # only if reconfiguring away from the iframe-identity default

# booth-lakehouse: the one outlier key path, for every concept:
identity:
  oidcIssuerUrl: "https://<your-idp-issuer>"
  oidcAudience: "booth-design"
  oidcJwksUrl: "http://booth-core-keycloak.booth-system.svc.cluster.local:8080/realms/booth/protocol/openid-connect/certs"
  workloadIssuerUrl: "http://booth-core.booth-system.svc.cluster.local:8080"
```

For the **bundled** Keycloak specifically, `oidc.issuerUrl` is `https://<ingress.host>/realms/booth`
(the same value every token-verifying module must agree on), and the `jwksUrl` shown above is
the exact in-cluster value the bundled install needs — deliberately plain `http://` and
in-cluster-only, so no module needs to trust the Ingress's certificate just to fetch signing
keys (ADR 0108's whole point for this value).

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
this CI job doesn't check out), and not the bundled starter realm.

**The bundled starter realm itself** is covered separately, by `integration.yml`'s
`bundled-keycloak-check` job: installs the real chart with no overrides, runs `booth-admin`
(including `harden-master`, asserted idempotent by running it twice) against the real realm the
chart actually produced, and asserts a real token and `/api/me` agree — the thing that would
catch a starter-realm mistake (a missing mapper, a wrong client setting) that a fixture built
independently of the real chart never could. `ingress-tls-check` covers the same realm again,
this time through a real Ingress with a real browser, and additionally asserts the admin console
and master realm are never reachable through it (item (g) in the first-login runbook).
