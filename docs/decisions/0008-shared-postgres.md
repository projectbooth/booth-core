# booth-core decision 0008: The shared PostgreSQL and per-module provisioning (ADR 0053)

Status: **implemented; several calls flagged back to booth-architecture.** ADR 0053 decides
the outcome — bundle a default PostgreSQL, swappable for an external one, and have core
provision each module's database and role, delivered as a Secret, on a manifest-level signal.
It leaves the mechanism to core. Those are the calls made here.

## What was decided

### 1. The bundled server is a small StatefulSet on the official image, not a third-party subchart

ADR 0053 says "a standard subchart". I didn't use one. The obvious candidate, Bitnami's
PostgreSQL chart, is tied to Bitnami's image distribution, whose free-tier terms changed in
2025; making the platform's *default database* depend on that felt like the wrong risk for a
"batteries included" promise. A single-replica StatefulSet on `postgres:16-alpine`
(`templates/postgresql.yaml`) is ~80 lines, has no external dependency beyond the official
image, and is exactly as HA as a bundled single node can honestly be — which is *not* HA. If
you'd rather have a subchart (or CloudNativePG for real HA in-cluster), the seam is
unchanged: core only needs a host, an admin account, and its password.

The bundled server is therefore explicitly **single-node, for self-hosted and development
installs**; ADR 0014's "shared, HA cluster" is met by pointing at an external cluster
(`postgresql.enabled=false`, `postgres.external.*`), as ADR 0053 anticipates.

### 2. The manifest signal (**contract addition — flag**)

```yaml
database:
  enabled: true
```

Optional, same shape as `events`. An object rather than a bare boolean so it can grow without a
second manifest field. A module that omits it gets nothing. This is `module-manifest.md`'s new
field.

### 3. Credential Secret convention (**contract addition — flag**)

Written into the module's namespace (`serviceRef.namespace`, else the `BoothModule`'s own) ahead
of need, per ADR 0020:

| Secret `booth-database-credentials` | |
|---|---|
| `dsn` | `postgres://user:pass@host:port/db?sslmode=…` — what most modules want |
| `host`, `port`, `database`, `username`, `password` | the parts, for a module that builds its own connection |

Owned by the `BoothModule` (garbage-collected on uninstall) when in the same namespace. The
database and role are named `booth_mod_<id with - → _>`; core's own is `booth_core`, which a
module can never collide with.

### 4. Isolation is enforced by the server, and tested against a real one

Each module's role is a plain login role (no superuser / createdb / createrole / replication)
that owns its database, and `CONNECT` is revoked from `PUBLIC` on every database. Tests
(`internal/dbprov`, run against a real PostgreSQL) verify a module's credentials can't open any
other module's database and can't create databases or roles.

Testing found a gap in the obvious design: PostgreSQL grants `PUBLIC` `CONNECT` on the
maintenance databases (`postgres`, `template1`) by default, so a module credential can log in
there and read the catalogs — it can't read other modules' *data*, but it can list their
database and role names. Core now revokes that **on the bundled server, which it owns**. On an
external cluster it's opt-in (`postgres.external.restrictMaintenanceAccess`), because changing
privileges on a maintenance database of a cluster core doesn't own could disrupt other tenants.
That's a judgement call: default-off is the safe choice for infrastructure I don't own, at the
cost of a metadata leak until an operator turns it on.

### 5. Nothing is ever dropped automatically

Uninstalling a module removes its credential Secret (garbage collection) but **leaves its
database and role**. Reinstalling gets a fresh password for the same database, so its data
comes back. Disabling the signal does the same. Deleting a module's data is a deliberate
operator action, not a side effect of `helm uninstall`. Cost: orphaned databases accumulate
until someone cleans them up.

### 6. Provisioning behaviour

- The Secret is the source of truth for a module's password: an existing one is honoured, so a
  running module's credential never changes on a routine reconcile. Only a *missing* Secret
  triggers a new password.
- If the server lost its roles/databases (restored from an older backup, recreated), core
  rebuilds them using the password already in the Secret, so running modules keep working.
  This check is rate-limited (5 min) since reconciles run every few seconds.
- If the server's address changes (the "swappable" case), the Secret's DSN is rewritten with
  the same password.
- Concurrent core replicas take turns via a Postgres advisory lock.
- Passwords are set as pre-hashed SCRAM-SHA-256 verifiers, because PostgreSQL logs the text of
  a statement that errors and a plaintext `CREATE ROLE … PASSWORD` would put the password in
  the server log. Identifiers and the verifier are quoted by the server (`format(%I, %L)`), not
  by string concatenation here.

### 7. Core's own database uses the same mechanism

Core is provisioned like a module (`booth_core`, Secret `booth-core-database` in its own
namespace), and the user directory (ADR 0047) switches from its in-memory fallback to it as
soon as it's ready — so a default install now persists it. The switch is live (a swappable
store; users are re-recorded on their next request), and it retries with backoff, so the
bundled server still starting doesn't delay or crash core. `BOOTH_POSTGRES_DSN` remains as an
override that bypasses provisioning for core's own database.

### 8. Ordering

As with NATS: for the bundled server, core generates the admin password Secret
(`booth-postgres-admin`) before anything waits on Postgres, and the StatefulSet's pod waits in
`ContainerCreating` until it exists. Core never blocks on Postgres to start.

## Requirements and limits to know about

- **The admin account** (bundled: `postgres`) must be able to create roles and databases and to
  hand a database to the role it created — in practice a **superuser**, or your provider's
  equivalent (e.g. `rds_superuser`). I only tested with a superuser.
- **Single-node bundled server**: no HA, and **backup is entirely unaddressed** — nothing here
  backs up the bundled server's PVC, so back it up (or use an external cluster) if the data
  matters. A major-version upgrade of the bundled image is likewise a manual dump/restore.
- **Losing `booth-postgres-admin`** after the server has initialised its data directory leaves
  core unable to authenticate (the server keeps the original password); core only ever creates
  the Secret when absent.
- **Modules pick this up by adding `database.enabled: true` and reading the Secret**; nothing
  changes for a module that already gets its DSN some other way.

## Flagged back to booth-architecture

- `contracts/module-manifest.md`: add the optional `database` field.
- `contracts/core-platform-api.md`: document the `booth-database-credentials` Secret shape.
- ADR 0053 said "a standard subchart"; core built a small StatefulSet instead (decision 1). If
  a subchart or an operator is preferred, say so.
- The maintenance-database default for external clusters (decision 4).
- Who owns backup/restore and major-version upgrades of the bundled server (limits above).
- `booth-storage` and `booth-catalog` can drop any expectation of an operator-supplied DSN and
  read `booth-database-credentials` instead.
