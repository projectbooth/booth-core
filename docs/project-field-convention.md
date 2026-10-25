# The `?project=` convention for module authors (ADR 0124)

A project is a navigation label, not a boundary. It grants nothing, denies nothing, isolates
nothing — **never use it in an authorization decision.** Everything below is purely about
grouping and filtering a list.

## What core owns

Core stores the registry (`GET/POST/PATCH /api/projects`, `GET /api/projects/{slug}`) and
bindings (`GET/POST /api/projects/{slug}/bindings`, `GET /api/projects/bindings?kind=...`,
`DELETE /api/projects/{slug}/bindings/{id}`). Reads are open to any workspace member; create/
edit/archive and binding create/delete are owner-only. Full shapes: `contracts/projects.md`.

There is no delete on a project itself — only archive. Core has no visibility into which modules
still reference a slug (each module stores its own `project` field, not a core-side join), so it
can't safely offer a "delete if unused" check the way service accounts can.

## What your module owns

1. **A nullable `project` field** on your own resource rows (pipelines, apps, datasets,
   whatever you have). Your own column, your own migration. No existing resource is migrated —
   everything starts unassigned, shown as "No project".
2. **A `?project=<slug>` filter** on your list endpoint(s). Fetch the full project list from
   `GET /api/projects` (you need it anyway for the picker and for "No project"/parent grouping
   headings) and cache it — don't call core synchronously on every list request. Filtering is
   equality against your stored field, **expanded to include children** when the selected slug
   has any: compute `{selected} ∪ {p.slug for p in allProjects if p.parent == selected}` from the
   list you already hold. There is no `/descendants` endpoint — you already have everything
   needed to compute this yourself.
3. **Validating a slug** you're about to store: check it against your own cached project list,
   not a synchronous call to core. A brief staleness window (accepting a slug archived a minute
   ago, or not yet seeing one created a minute ago) is a UX rough edge, not a bug — a project
   carries no authorization meaning, so there's nothing to get wrong security-wise. If you want
   stricter freshness anyway, `GET /api/projects/{slug}` is there for a direct check.

## Bindings: bulk assignment without touching every row

If your resources are identified by an external name core already has a binding kind for — a
Postgres schema, an Iceberg namespace, a Storage prefix — you don't need the `project` field at
all for grouping. Fetch your relevant bindings (`GET /api/projects/{slug}/bindings` per project,
or `GET /api/projects/bindings?kind=postgres_schema` once for every binding of your kind across
the workspace) and apply the mapping yourself at render time: group by schema/namespace/prefix
as you already do, and add the project heading above it from the binding. Core never resolves
anything — it doesn't know your resource list. Matching is exact for `postgres_schema`/
`iceberg_namespace`; for `storage_prefix` it's containment (same `backendId`, and the bound
prefix is a `/`-boundary ancestor of — or equal to — the resource's path), the same rule ADR
0046 already uses for dashboard lineage.

## The "suggest from names" helper

A one-time, your-own-UI helper: prefix-match your resource names against the project slug list,
let the user confirm. Two different outcomes depending on what you're grouping:

- If it's one of the three bindable kinds above, suggest **creating a binding** — one binding
  covers every current and future resource under that name, cheaper than tagging each row.
- Otherwise (pipelines, apps, anything with no external-name kind to bind), suggest a direct
  **per-resource `project` field assignment**, confirmed one-by-one or in bulk.

Either way, the only thing you need from core is the project list you already have.

## The shell picker and your UI

The shell stores the selected project as `?project=<slug>` in its own URL and remembers it per
user in the browser; "All projects" is the default. How it reaches you depends on your
integration mode:

- **Native mode** (your UI mounts directly in the shell's page): it's an ordinary query
  parameter on the browser's address bar. Read `window.location.search` directly — core isn't
  involved.
- **Iframe-proxied mode**: the shell re-requests `GET /api/modules/{id}/iframe-url?project=<slug>`
  whenever the picker changes and re-points your iframe's `src` at the result (a reload). Your
  frontend reads the `project` query parameter the same way at load. Core validates the slug
  grammar and URL-encodes it before appending — an invalid value is refused with a 400 at that
  call, never silently passed through malformed.

Either way: group your own list by project with collapsible headings when "All projects" is
selected, and show "No project" for anything unassigned. A module that never adopts any of this
shows everything, unfiltered — ADR 0124 is opt-in per module.
