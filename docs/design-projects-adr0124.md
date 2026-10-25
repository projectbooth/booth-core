# Design note: booth-core's part of ADR 0124 (projects as a grouping label)

Status 2026-10-25: ADR 0124 is accepted. No code — this is the note itself, per
`agent-briefs/ux-feedback-2026-10-24.md`'s booth-core prompt.

## 1. The project registry: table and API shape

`booth_projects`, workspace-scoped (not stated outright in the ADR, but implied by every other
core registry — directory, service accounts — and by "project" being deliberately *not*
`workspace`; flagged under Disagreements below so the coordinator can confirm rather than let it
be assumed):

```
slug         TEXT PRIMARY KEY (with workspace)   -- immutable once created
workspace    TEXT
name         TEXT
parent_slug  TEXT NULL  -- references another slug in the same workspace
description  TEXT NULL
color        TEXT NULL  -- opaque to core, shell's to interpret
icon         TEXT NULL  -- opaque to core, shell's to interpret
archived     BOOLEAN DEFAULT FALSE
created_by   TEXT
created_at   TIMESTAMPTZ
```

Slug grammar: `^[a-z0-9-]{1,63}$`, matching `serviceaccount.namePattern` (ADR 0120) rather than
inventing a third convention — lowercase, hyphenated, short. Same durable-store posture as
service accounts (ADR 0120 ruling 2): no in-memory fallback, 503 until Postgres is real, because
a project assignment is an owner-authored grant-shaped record a module's switchable fallback
would lose or invent across a Postgres swap, not a cache.

Routes (unversioned, see Disagreements — the ADR writes `/api/v1/projects`):

```
GET    /api/projects                 any member — list (name, slug, parent, archived, ...)
GET    /api/projects/{slug}          any member — single project, for a cheap existence check
POST   /api/projects                 create {slug, name, parent?, description?, color?, icon?}
PATCH  /api/projects/{slug}          edit {name?, parent?, description?, color?, icon?, archived?}
GET    /api/projects/{slug}/bindings any member — list that project's bindings
POST   /api/projects/{slug}/bindings create a binding
DELETE /api/projects/{slug}/bindings/{id}  remove one
```

No `DELETE /api/projects/{slug}`. The ADR's own point 2 only ever says "create, rename, archive" —
delete isn't listed, and for good reason: unlike a service account (ADR 0120's "disable before
delete" works because core itself is the sole place that *uses* the record, at mint time), core
has no visibility into whether any module still has resources tagged with a slug — "each module
stores the field itself (no cross-module join)" (point 3) means core structurally cannot check
"is this slug still referenced anywhere" before deleting it. Archiving (hide, don't delete,
reversible) is the only safe lifecycle operation core can offer on its own; I'd keep it that way
rather than add a delete that can orphan a module's stored field with no way to detect it.

**Who may write.** The ADR doesn't actually say (compare ADR 0120's explicit "owner role
required" for service accounts) — flagged as an open question below. My default: owner-only for
create/rename/archive/bindings (it's workspace-structural, same posture as service accounts and
module install/uninstall), open-read for every role (every role needs the list for the picker and
for "No project" grouping, including viewers).

**Depth and re-parenting.** `parent` must reference an existing, non-archived project in the same
workspace whose *own* `parent` is null (a child can't have children — point 5's "two levels to
start"). Re-parenting is allowed (`PATCH .../{slug} {parent: "..."}`) but re-validated the same
way each time, and refused if the project being re-parented already has children of its own
(that would make three levels). Archiving a parent does not cascade to its children — independent
flags; how the shell renders an archived parent with live children is a UI call, not core's.

## 2. The shared convention for modules

A nullable `project` string field on a module's own resource rows — the module's column, the
module's migration, never a core table. A `?project=<slug>` query parameter on that module's own
list endpoints.

**"Selecting a parent includes its children" needs no new core endpoint.** Every module that
groups by project already has to fetch the full project list from `GET /api/projects` anyway (for
the picker and for rendering "No project" / parent headings per point 6), so expanding a selected
parent slug to "that slug or any of its children" is pure client-side filtering over a list the
module already holds — `children := [p for p in allProjects if p.parent == selected]`. I
deliberately did not propose a `/descendants` endpoint for this; it would duplicate data the
module already has in hand.

**Validating a slug.** Point 3 of the task asks how a module checks a slug against core.
Recommendation: a module validates against its own periodically-refreshed cache of `GET
/api/projects` (the same fetch it needs for grouping/the picker), not a synchronous call to core
on every write. This is cheap to justify precisely because of point 6: a project carries no
authorization meaning, so a brief staleness window — accepting a slug that was archived a minute
ago, or momentarily rejecting one created a minute ago — is a UX rough edge, not a security or
correctness bug. A module that wants stricter freshness instead is free to call `GET
/api/projects/{slug}` synchronously (included in the route list above for exactly that); both are
valid under this contract, core doesn't need to pick one for every module.

## 3. Bindings

```
booth_project_bindings
id            (generated)
workspace     TEXT
project_slug  TEXT
kind          TEXT  -- "postgres_schema" | "iceberg_namespace" | "storage_prefix"
value         TEXT  -- schema name / namespace string / storage path
created_by    TEXT
created_at    TIMESTAMPTZ
UNIQUE (workspace, kind, value)
```

The uniqueness constraint is deliberate: one external name binds to at most one project — an
ambiguous "this schema groups under two projects" is refused at write time, not resolved by
some precedence rule later.

**Matching semantics.** `postgres_schema` and `iceberg_namespace`: exact string equality (a schema
or namespace name is a whole unit). `storage_prefix`: containment, same rule ADR 0046 already
established for `dashboard.*`'s `location` sources — same `backendId`, and the bound prefix equals
the resource's path or is a `/`-boundary ancestor of it. Reusing an existing, already-accepted
containment rule rather than inventing a second one.

**Who resolves a binding (point 3 of the task): the owning module, not core.** Core cannot resolve
anything itself — it has no idea what a Postgres schema, an Iceberg namespace, or a Storage prefix
actually contains; only `booth-database`, `booth-lakehouse`, and `booth-storage` know their own
resource lists. The ADR's own point 4 supports this directly: "the Database module groups tables
by schema regardless; bindings only add the project heading above the schemas" — that's a module
applying a mapping it fetched, at render time, to resources it already enumerates itself. So: core
is the dumb, authoritative store for the `(kind, value) -> project` mapping; `GET
/api/projects/{slug}/bindings` (or a workspace-wide listing, if a module would rather fetch once
and index locally — I'd add `GET /api/projects/bindings?kind=storage_prefix` as a convenience, open
question below) is all core offers; every module applies the mapping against its own resource
names on its own side.

## 4. The "suggest from names" helper

No new core endpoint beyond the list already needed for the picker. The matching logic has to
live in each module, because only the module has its own resource names to match — core has
neither the pipeline list nor the catalog dataset list. What I'd flag back to the ADR: this
bullet conflates two different outcomes depending on resource kind, worth separating explicitly
in each module's own note:

- For a resource kind that *is* one of the three bindable external-name kinds (a Postgres schema,
  an Iceberg namespace, a Storage prefix) — e.g. `booth-database`'s schemas — "suggest" should
  propose creating a **binding**, since one binding covers every current and future table in that
  schema for free, rather than tagging each table.
- For a resource kind with *no* bindable external name (pipelines, Streamlit apps, notebooks, API
  deployments, Catalog datasets by their own name) — the helper has nothing to bind to, so
  "suggest" means proposing a direct **per-resource `project` field assignment**, one-by-one or in
  bulk, confirmed by the user in that module's own UI.

Core's only involvement in either case is being the source of the project-slug list the module's
prefix-matcher compares names against — already covered by `GET /api/projects`.

## 5. Shell header picker reaching modules

**Native-mode modules** (loaded directly in the shell's own page, e.g. `booth-pipeline`): zero
core change. `?project=<slug>` is an ordinary query parameter on the shell's own address bar; the
module's frontend bundle reads `window.location.search` directly. Core's gateway isn't on this
path at all for the navigation itself (only for the module's own API calls, which already forward
query strings untouched — see below).

**Iframe-proxied modules**: this is where I have a concrete proposal and a concrete gap, read from
`internal/gateway/proxy.go` and `iframeproxy.go` directly rather than assumed:

- Every proxy path in `booth-core` today (`Gateway.proxyTo`, used by `/modules/{id}/*`, and
  `proxyIframeRequest`, used by `/iframe/{id}/*` and the root-relative fallback) only ever
  overwrites `req.URL.Path`; neither touches `RawQuery`. Whatever query string arrives on the
  actual HTTP request is forwarded to the module byte-for-byte, already, with no code change.
  Likewise, `IframeEntryHandler`'s one-time "clean redirect" (stripping the single-use
  `booth_iframe_token` query param after the cookie is set) only deletes that one key —
  `q.Del(iframeQueryParam)` — and keeps every other query parameter, so anything else present on
  the initial src survives that redirect intact.
- The gap: `IframeURLIssuer.URLFor`, which mints the one src URL the shell points an iframe at
  (`/iframe/{id}/?booth_iframe_token=<token>`), takes no project parameter today and has no path
  for the shell to add one — `handleIframeURL` (`GET /api/modules/{id}/iframe-url`) is a
  fixed-shape call with no query passthrough.

**Proposed contract for booth-design**, to resolve the ADR's own open point ("query string on the
proxied URL vs. a message from the shell"): extend `GET /api/modules/{id}/iframe-url` to accept an
optional `?project=<slug>`, threaded into `URLFor` so the minted src becomes
`/iframe/{id}/?booth_iframe_token=<token>&project=<slug>` — confirmed above to survive the
token-stripping redirect and to forward on every subsequent proxied request untouched. The shell
re-requests a fresh iframe URL (and re-points the iframe's `src`, forcing a reload of that module)
whenever the picker changes. This is the one, small, genuinely new core change this ADR implies —
worth being explicit that it exists, since the ADR's "no new backend state, no module-to-module
calls" framing is true of the registry itself but doesn't quite cover this one wire-up. It is not
new *state*, just a parameter passthrough on an existing mint call.

A live update with no iframe reload needs `postMessage` instead, which is pure frontend work (the
module's own JS adding a listener) and doesn't touch core at all either way. I'd ship the
query-string change regardless of whether `postMessage` is also wanted later — it's cheap, gives
every iframe-proxied module project-filtering on first load for free, and doesn't foreclose adding
`postMessage` on top for a nicer no-reload experience per module.

## 6. Confirmed: project carries no authorization meaning

Checked directly, not assumed: `grep -rni project internal/auth internal/natsauth` finds nothing
but the `projectbooth.io` domain string (an unrelated name collision, not the concept). The
groups-claim grammar (`internal/auth/workspace.go`) is a closed regex,
`^/workspaces/([a-z0-9-]+)/(owner|editor|viewer)$`, plus the separate exact-match
`/platform/operator` string for ADR 0094 — neither has, nor would silently accept, anything
`/projects/...`-shaped; `DeriveMemberships` ignores non-matching entries outright, it doesn't
error, so even a mistaken future claim shaped like a project grant would be silently dropped, not
honoured. `internal/natsauth`'s manifest-derived NATS grants are keyed purely on event-type
strings, with no notion of project at all. Nothing to change; this is a true statement about the
code as it stands today, and I'd keep the registry and bindings tables exactly as described above
— no role or grant ever derived from a project slug.

## 7. Migration

Trivially true, not something to build: `project`/`parent_slug` are new nullable columns with no
backfill: every existing row in every module (and the registry itself, which starts with zero
rows) reads as unassigned, which is exactly "No project." Bindings and the suggest-from-names
helper are the opt-in path to bulk-assign retroactively; nothing is migrated automatically.

## Proposed `contracts/` diff

New file, `contracts/projects.md`, parallel to `contracts/credential-broker.md`'s shape:

```diff
+# Contract: Projects (ADR 0124)
+
+## Roles
+core owns the registry and bindings; every other module owns its own `project` field and
+`?project=` filter.
+
+## Registry API (booth-core)
+GET/POST/PATCH /api/projects, GET /api/projects/{slug}, GET/POST/DELETE on
+/api/projects/{slug}/bindings — [exact shapes as drafted in booth-core's design note]
+
+## The shared module convention
+A nullable `project` field on a resource; a `?project=<slug>` list filter; a selected parent
+slug's children are included by the consuming module, computed from the full project list it
+already holds — never a new core endpoint per module.
+
+## Binding kinds and matching
+postgres_schema / iceberg_namespace (exact match), storage_prefix (containment, same rule as
+ADR 0046's `location` dashboard sources). Resolution happens in the owning module, not core.
+
+## No authorization meaning
+`project` must never appear in any authorization decision, in any module. See ADR 0117/0120's
+checklists, amended to say so explicitly.
```

`contracts/core-platform-api.md` gains a short pointer to the new file next to the existing
event-bus and module-manifest bullets, the same way the event-bus section already points at ADR
0026/0046 rather than inlining them.

`contracts/ui-integration.md` gains the iframe-proxy query-passthrough contract from section 5
above, under "The iframe-proxy auth mechanism" or a new sibling heading — this is the piece
booth-design's note needs to build against.

## Disagreements and open questions

1. **`/api/v1/projects` vs. `/api/projects`.** Every existing route in `booth-core` today is
   unversioned (`/api/me`, `/api/service-accounts`, `/api/credentials`, ...) — nothing in this
   repo has ever used an `/api/v1/` prefix. I'd keep that consistency and drop the `v1` segment
   unless the coordinator actually wants to start versioning core's API now, which is a bigger,
   separate decision than this ADR.
2. **Who may write to the registry.** The ADR says projects are "managed from the shell" but
   never states a role requirement (contrast ADR 0120's explicit "owner role required" for service
   accounts). I've defaulted to owner-only writes / open reads above; needs a yes.
3. **A workspace-wide bindings listing.** I added `GET /api/projects/bindings?kind=...` as a
   convenience beyond the ADR's literal `GET /api/projects/{slug}/bindings`, so a module that
   wants "every binding of kind X across the workspace" (e.g. `booth-database` wanting every
   `postgres_schema` binding at once) doesn't have to fetch bindings project-by-project. Confirm
   it's wanted, or it's a two-line addition to drop.
4. **No binding-create/delete route is in the ADR's own API bullet** (point 7 lists only the `GET`
   for bindings). Bindings can't exist if nothing creates them, so I've added `POST`/`DELETE` — a
   near-certain omission in the ADR text, not a real design choice, but flagging it rather than
   silently assuming.
5. **Re-parenting and archiving edge cases** (a project with children being re-parented or
   archived) are specified above as "refuse"/"independent flags" by my own default, not stated in
   the ADR. Low-stakes, but worth a nod before booth-design builds the picker against it.

Not implementing anything until this is reviewed, per the brief.
