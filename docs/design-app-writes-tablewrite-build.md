# Build note: the `tablewrite` broker change (ADR 0117 "Reconciled design" item 4)

Status: **superseded as a to-do, kept as a record.** The coordinator has already applied this
diff to `booth-architecture`'s real `contracts/credential-broker.md`, with one correction this
note got wrong below: the `tablewrite` 201 echo nests under `scope.write` (`scope.write: {grant,
tables, revoked?}`), matching `booth-database` PR #15 (`docs/decisions/0008-table-scoped-
write-leases.md`) — **not** the flat `{grant, tables}` siblings-of-`workspace` shape this note
originally showed. That flat form is corrected below; the real contract text in
`booth-architecture` is authoritative over this file for any future disagreement.

Supersedes this repo's earlier `docs/design-app-writes.md` (kept, unedited, on its own branch
`design-app-writes-broker-rule-adr0117`, per the ADR's own instruction) wherever the two
disagree — ADR 0117's "Reconciled design" overruled that note's "no broker change" conclusion:
without a distinct `access` value, `booth-database`'s provider cannot tell a table-scoped
request from a whole-database `readwrite`, and an editor-ceiling `apiwrite:` app token could ask
for `_rw`.

## Diff

```diff
 ## Request/response shape (ADR 0088)

 POST /api/credentials
 Authorization: Bearer <caller's own token>      X-Workspace: <slug>
 {"kind": "s3", "ttlSeconds": 900, "access": "read" | "readwrite",
  "scope": {...kind-specific, opaque to the broker...},
- "options": {...kind-specific, opaque to the broker...}}
+ "options": {...kind-specific, opaque to the broker...},
+ "onBehalfOf": "<opaque identity string, optional>"}

 201 {"leaseId", "kind", "expiresAt", "scope": <echo>, "credential": {...kind-specific...}}
 422 {"error": "scope_not_supported", ...}   # "refuse rather than widen"
```

`access` gains a third value (ADR 0117): **`tablewrite`**, admitted by the broker's own rule
exactly like `readwrite` (editor or owner) and forwarded to the provider verbatim — never
translated to or from `readwrite`. `readwrite` keeps meaning a lease in the whole-database `_rw`
group; `tablewrite` means a lease in a per-grant write group scoped to the tables named in
`scope.write` (below), and the two are never interchangeable at any layer. `read` and `readwrite`
issuance are otherwise byte-for-byte unchanged by this addition.

`onBehalfOf` is a new, optional, **audit-only** field on the request. It names the identity a
requester is acting for (e.g. `booth-api`, authenticated as its own `apiwrite:<api-id>` workload
token, naming the app subject whose declared write triggered the call) and is passed through to
the provider's `requester.onBehalfOf` verbatim. The broker's authorization decision never reads
it — it is informational, forwarded for the *provider's* own audit record, exactly as
`requester.subject`/`workspace`/`role` already are (ADR 0080: "uses only for its own audit
logging, never for any further authorization decision").

```diff
 ## Authorization (ADR 0088)

 The broker resolves `(subject, workspace, role)` exactly as `auth.Middleware` already does for
 the gateway route — no new identity derivation. One rule, applied to the shared `access` field:
-`readwrite` requires editor or owner; `read` is open to any real role — the same editor/owner-
-write, viewer-read precedent ADR 0048 already established for `booth-catalog`. A workload
+`readwrite` and `tablewrite` both require editor or owner; `read` is open to any real role — the
+same editor/owner-write, viewer-read precedent ADR 0048 already established for `booth-catalog`,
+now applied identically to both write-shaped access values. A workload
 token's role is already capped at mint time (ADR 0056/0058); the broker reads that capped value
 the same way the gateway does.
+
+**One further restriction, specific to one subject shape (ADR 0117 "Reconciled design" item 3).**
+A workload token whose subject has the prefix `apiwrite:` (booth-api's own per-(API, granting
+owner) write sidecar) may request `tablewrite` only — a `read` or `readwrite` request from such a
+token is refused regardless of its role. This token exists for exactly one purpose; nothing about
+its editor-capped role should let it ask for anything broader. The subject's own shape is the
+signal (ADR 0056's subject grammar already makes a `<kind>:<id>` prefix structurally meaningful);
+no new stored state, config, or manifest field was needed.
```

```diff
 ## Provider path and authentication (ADR 0088)

 Every provider implements a fixed path, `POST /internal/credentials` (`credentialbroker.
 ProviderPath`) ... The provider request body carries the requester's already-authorized
 `subject`/`workspace`/`role` purely for the **provider's own audit logging** — never for any
 further authorization decision.
+
+It now also carries `onBehalfOf`, verbatim from the request, under the same rule: audit only,
+never authorization. A provider that ignores it loses nothing — it was never required.
```

```diff
 ## Scope vocabulary

 Free per kind — `scope`/`options` are fully opaque to the broker beyond the shared `access`
 field above. No shared vocabulary is imposed across kinds; `s3` and `postgres` are free to
 define their own scope shapes independently, and a future kind does the same.
+
+**`postgres`'s `tablewrite` scope shape** (from `booth-database` PR #15 / `docs/decisions/
+0008-table-scoped-write-leases.md` §1 — the authoritative, as-built shape; this corrects an
+earlier draft of this note that nested `scope.write` in the request but wrongly echoed it back
+flat, as siblings of `workspace`/`database`/`access`, in the response):
+
+```
+access: "tablewrite"
+scope: {"workspace": "<slug>",                    # optional, as for every other kind
+        "write": {"grant": "<id, 1-128 chars, no control characters, e.g. api:<api uuid>>",
+                  "tables": [{"schema": "finance_input", "name": "transactions",
+                              "operations": ["insert", "update", "delete"],
+                              "columns": ["amount", "booked_on", "entered_by"]}]}}
+options: none
+201 scope echo: {workspace, database, access,
+                  "write": {grant, tables: [{schema, name, operations, columns, sequences}]}}
+```
+
+`tablewrite` without `scope.write` is refused by the provider (422); so is a `read` or
+`readwrite` request that carries one. The broker never reads `scope.write` itself — table
+existence, column validity, and the excluded-schema/ignore-list checks are entirely the
+provider's job (ADR 0088: "a provider never re-derives trust itself... it only refuses what it
+cannot scope narrowly").
+
+**Revocation shape, and its `credential: null` response.** Turning writes off is one more
+`tablewrite` request with `scope.write.tables: []`, allowed only together with
+`options: {"revoke": true}`. The provider answers **201** with:
+- `credential: null`;
+- `scope.write.revoked: true` (added to the echo above, so a caller can tell a revoke
+  acknowledgement apart from an ordinary grant echo by shape, not only by having sent the
+  request);
+- a fresh `leaseId`;
+- `expiresAt` set to now, since nothing was actually issued.
+
+This is a genuine success (every live lease for that grant is ended, its privileges revoked),
+not an incomplete response. The broker relays this exact shape to the caller rather than
+rejecting it: `callProvider` accepts a null/absent credential only when the request matches this
+documented revoke shape; every other request (including a non-revoke `tablewrite`) still
+requires a real credential and still fails loudly (`503`) if a provider omits one — this is a
+narrow, named exception, not a general relaxation.
+
+**What a caller sees for a `credential: null` 201:** a one-off caller (e.g. `booth-api` turning
+writes off) simply stops using the lease; there is nothing further to tear down on its side
+beyond whatever local resource depended on it. A caller holding a long-running proxy process
+built against `contracts/credential-sidecar.md`'s renewal shape — this is defense-in-depth,
+not a path `booth-api`'s own design is expected to exercise, since its revoke call is a one-off
+HTTP request, not something sent through a renewal loop — must stop serving new connections on
+the next renewal that comes back null, without dropping a connection already relayed (the
+provider has already ended the underlying database sessions server-side, so an already-open
+connection ends naturally rather than needing to be forcibly closed by the sidecar itself).
```

## What changed in `booth-core`, concretely

- `internal/credentialbroker/service.go`: `AccessTableWrite` constant; `validate()` accepts it;
  `authorize()` takes `subject` and applies the `apiwrite:` restriction; `Request`/
  `providerRequester` gain `OnBehalfOf`; `callProvider` accepts a null/absent credential only for
  the documented revoke shape (`isRevokeRequest`), everything else is exactly as strict as before
  (`isNullOrAbsent`).
- `internal/sidecar/broker.go` + `internal/sidecar/postgres.go`: the shared `credential-sidecar`
  binary's client no longer treats a null credential as malformed, and `PostgresProxy` now
  explicitly distinguishes "never obtained a lease" from "revoked," refusing new connections with
  a clean message instead of silently proxying with an empty, zero-valued credential. Defense-in-
  depth for this one shared, published binary, not currently exercised by `booth-api`'s planned
  usage (see above).
- `cmd/credential-sidecar/main.go`: `--access` accepts `tablewrite`, restricted to `--kind=postgres`.
- `read`/`readwrite` issuance: unchanged. Full existing broker/sidecar/workload/integration test
  suites pass unmodified; new tests cover tablewrite authorization (editor 201 / viewer 403), the
  `apiwrite:` restriction, `onBehalfOf` reaching the provider request without affecting
  authorization, the null-credential relay (both the revoke and non-revoke cases), and a gateway
  regression pinning that an `app:<id>` token reaches `/modules/api/app/v1/...` unchanged.

## What this build could not exercise

No real `booth-database` provider implementing `tablewrite` exists yet (its own design note is
design-only, per its own status line) — every test above is against this repo's own fake/real-
HTTP-server provider fixtures, the same accepted scope-cut precedent ADR 0088 itself used for `s3`/
`postgres` `read`/`readwrite` before a real provider existed. The gateway test confirms the
mechanism generically; it cannot confirm `booth-api`'s own realstack suite passes, since that code
doesn't exist in this repo.
