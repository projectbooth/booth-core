# booth-core decision 0015: The credential sidecar's implementation (ADR 0095)

Status: **implemented.** ADR 0095 and `contracts/credential-sidecar.md` fix the binary's
interface (flags, two modes, renewal timing shape, failure behavior, health) and leave the
implementation to this session. This records the calls made building it, and the places where the
contract's own wording left something genuinely open.

## 1. Renewal failure classification: fatal only before the first lease

The task's own wording ("on a broker refusal... log and exit non-zero rather than retrying
forever; on a broker outage... retry with backoff without dropping an existing valid lease") reads,
taken alone, as if refusal-vs-outage is the dividing line for every failure everywhere. But
`contracts/credential-sidecar.md`'s own renewal section describes a *renewal* failure — of either
kind — as something that "retries on the next poll interval, logging the failure," with new
connections refused only once "the lease fully expires with no successful renewal," and carves out
no exception for a refusal specifically. Those two framings only agree if the real dividing line is
**when** the failure happens, not **what kind** it is:

- **Before any lease has ever been obtained**: a refusal (400/401/403/404/422 — a config error that
  will fail identically on restart) is fatal, exits non-zero, and is the visible crash-loop signal
  the contract wants. An outage (network, 5xx) retries with backoff — there's nothing to protect
  yet, but also no reason to give up on a provider that's merely still starting.
- **After at least one lease has been obtained**: *every* renewal failure, refusal or outage, is
  logged and retried on the next interval — full stop. A caller whose role was revoked mid-session,
  or whose module briefly vanished from the registry during a deploy, keeps its current, already-
  authorized work running exactly as the contract's own renewal section describes; it only loses
  *new* access once its lease actually expires. Pinned by
  `TestRenewer_RenewalFailureNeverDropsTheCurrentLeaseOrExitsRun`, which specifically exercises a
  403 (not a network error) during renewal to make sure this isn't accidentally scoped to outages
  only.

## 2. Provider response shapes this binary needs but the broker contract doesn't fix

`contracts/credential-broker.md` deliberately leaves `scope`/`credential` opaque per kind. This
binary documents (`PostgresCredential`, `S3Credential` — `internal/sidecar/postgres.go`,
`s3file.go`) the shape it expects, matching each provider module's own real response exactly:

- **`s3`**: `{accessKeyId, secretAccessKey, sessionToken?, endpoint, region?, bucket, keyPrefix?,
  pathStyle}` — not invented here; this matches exactly what `booth-storage`'s own provider
  actually returns (`internal/credentialbroker/provider.go`'s `s3CredentialBody`, cross-referenced
  via `docs/decisions/0014`, itself citing `booth-storage`'s `docs/decisions/0006`). `sessionToken`
  must be omitted entirely, not sent empty, for the bare-2-tuple shape Lakekeeper's static-key
  credential needs — the credentials file writer already reflects that (no empty
  `aws_session_token` line). ADR 0095's third amendment (2026-10-05) caught that `S3Credential`
  declared only the three key fields, dropping `endpoint`/`region`/`bucket`/`keyPrefix`/`pathStyle`
  before anything was written to disk — and, since `OnRenew` already used `decodeStrict`, the real
  provider's response (which always sends all of these) would have failed to decode at all in
  production. Fixed by extending `S3Credential` to declare the full field set and having
  `S3FileWriter` also write a second, standard AWS shared *config* file (`<--credentials-file>
  .config`: `endpoint_url`/`region`/`addressing_style` in a profile section) alongside the existing
  credentials file, atomically, on the same renewal loop — present only when the lease carries a
  non-empty `endpoint` (a self-hosted backend; omitted entirely for real AWS S3, which has none).
  `addressing_style` was added by ADR 0095's fourth amendment (2026-10-05, after `booth-notebooks`
  measured the re-published sidecar against a real MinIO and found DuckDB needs path-style
  addressing that it doesn't read from anywhere else) and **corrected twice since, by the sixth
  amendment (2026-10-06)**: the fourth amendment's spec was wrong in two ways, both found by
  `booth-pipeline`'s integration test and verified against real botocore 1.43 (confirmed again here
  directly against `botocore.configloader.raw_config_parse`/`load_config`, not just taken on
  report) rather than this repo's own guess. First, a top-level `addressing_style` key in the
  profile section is silently ignored by botocore — it must be nested under an `s3 =` key, with the
  value on its own indented line. Second, the value must be derived from the response's own
  `pathStyle` field (`path` when true, `virtual` when false), not hardcoded to `path` for every
  self-hosted lease: `pathStyle` is `booth-storage`'s own declaration of how that specific backend
  must be addressed (`internal/backend/s3/s3.go`'s "forces path-style addressing"), not a separate,
  ignorable question as the fourth amendment assumed. `bucket`/`keyPrefix` are decoded but not
  written anywhere by this mode; a consuming engine's own bucket/path is resolved separately via
  `booth-lakehouse`'s `GET /api/warehouse`.
- **`postgres`**: `{host, port, database, username, password, sslMode?}` — also not invented here.
  `booth-database` already shipped a real `postgres`-kind provider in its first pass
  (`booth-database@b84fc3b`, `internal/credentialbroker/provider.go`'s `pgCredential`), well before
  this ADR — it's that module's only access path (`credentialBroker.enabled: true` by default, not
  optional), and it has its own tests (`internal/credentialbroker/provider_test.go`). An earlier
  draft of this document wrongly claimed no such design existed yet and used a `user` field instead
  of `pgCredential`'s actual `username`; `PostgresCredential.User`'s tag was briefly `json:"user"` as
  a result, which decoded silently as an empty string against the real provider (caught before this
  shipped to any consumer, fixed by matching `pgCredential` field-for-field and switching to a
  strict decoder — `decodeStrict` in `internal/sidecar/broker.go` — so an unknown or renamed field
  errors instead of silently zeroing).

## 3. The `postgres` proxy speaks real wire protocol on both sides, including real SCRAM-SHA-256

ADR 0095 calls this out explicitly ("Postgres's auth handshake is embedded in the connection
itself"), and it's not optional to get right: core's own bundled Postgres already requires
SCRAM-SHA-256 (`internal/dbprov.scramVerifier` derives exactly the stored-verifier shape this
binary's SCRAM *client* implementation, `internal/sidecar/pgwire`, authenticates against) — a proxy
that only spoke trust/cleartext/MD5 would not actually work against this platform's own default
database. `internal/sidecar/pgwire` implements the frontend/backend protocol v3 just far enough for
a handshake (StartupMessage, the Authentication message family, SCRAM per RFC 5802/7677) and
deliberately no further: once both the client-facing and upstream handshakes complete, the proxy
splices raw bytes (`io.Copy` both directions) rather than parsing query traffic, since no credential
ever needs to be re-injected mid-stream.

- **Downstream (client-facing) auth is trust-based.** The pod's own network namespace is the trust
  boundary (never bound beyond loopback), so the proxy answers AuthenticationOK immediately rather
  than implementing a second real auth exchange the main container's own code would have to satisfy
  — exactly what makes `DATABASE_URL=postgresql://localhost:5432/<db>` work with no password at all
  in the consumer's own connection string.
- **Upstream (to the real database) auth is real**, supporting trust, cleartext, MD5, and
  SCRAM-SHA-256 — whatever the real server actually requires.
- **Health is served on the same listener** (`contracts/credential-sidecar.md`'s stated shape for
  `postgres` mode) by sniffing each new connection's first bytes: an HTTP request line (`GET `/`HEAD
  `) is handled as `/healthz`; anything else is treated as a Postgres client and proxied. A
  StartupMessage's first 8 bytes can never look like ASCII HTTP method text, so this sniff is
  unambiguous, not a heuristic that could misfire against a real client.
- **Verified against real PostgreSQL, not a stand-in**, for the thing that matters most: a genuine
  `pgx` client, through the proxy, to a real embedded Postgres server (`internal/testpg`), with a
  role actually created to require SCRAM — `TestPostgresProxy_RealClientThroughProxyToRealPostgres`
  and the renewal-failure test both run against it, end to end. The SCRAM math itself has its own
  independent unit-level verification too (`pgwire/scram_test.go`'s `fakeScramServer`, written
  separately from the client so a shared bug couldn't make both sides agree on something wrong).

## 4. `postgres` mode renews at half the lease's real lifetime, not a fixed margin (ADR 0095 fifth amendment, 2026-10-06)

`booth-pipeline` and `booth-notebooks` both independently reported that `booth-database`'s reaper
terminates an open session at its lease's expiry regardless of whether the sidecar has since
renewed. Tracing it end to end (ADR 0095's fifth amendment) found the actual defect was here, not
in the reaper (which is enforcing ADR 0080/0088's credential-dies-at-expiry invariant on purpose):
`DefaultRenewMarginSeconds` is a fixed 60 seconds, so a client connection opened just before a
renewal swap stayed bound to the *old* lease, which the reaper then killed roughly one margin
later — the real guaranteed minimum life of a `postgres` connection through this proxy was about a
minute, not the hour a lease nominally lasts.

The user chose option A (`decisions/0095`'s fifth amendment): keep the reaper strict, make the
sidecar's own guarantee real instead. Unless `--renew-margin-seconds`/`RENEW_MARGIN_SECONDS` is
given explicitly, `postgres` mode now renews once **half of the lease's own real lifetime**
(`ExpiresAt` minus this `Renewer`'s own local observation of when it obtained the lease — there is
no server-provided "issued at" on the wire; `credentialbroker.Response` carries only `ExpiresAt`)
has elapsed, instead of a fixed margin before expiry. With today's one-hour leases that guarantees
roughly 30 minutes instead of roughly one. `s3` mode is unaffected and keeps the fixed default:
access there is signed per request, with no long-lived connection for a margin to protect.

Implementation: `Renewer.HalfLifetime` (`internal/sidecar/renewal.go`) overrides `Margin` when set;
`cmd/credential-sidecar`'s `main.go` sets it for `--kind=postgres` only when neither the flag nor
the env var was explicitly given (`flag.Visit` plus a direct `os.Getenv` check — an operator's
explicit choice always wins). The lease and this Renewer's own issue-time observation are now
stored together in one `leaseState`, read atomically as a pair, so a concurrent reader (healthz, the
postgres proxy) can never observe one updated without the other.

Pinned by `TestRenewer_HalfLifetimeRenewsAtHalfTheLeasesRealLifetime` (a fake clock and a fake
20-minute lease — never a real hour — proving the renewal fires at half, not at a fixed margin) and
`TestPostgresProxy_HalfLifetimeRenewalGuaranteesHalfALeaseForAnExistingConnection` (the same, but
through the real proxy against a real embedded Postgres, with a connection opened immediately
before the swap shown to survive, still authenticated as the pre-swap role, well past the point a
fixed 60-second margin would already have swapped it away).
`TestRenewer_RenewalFailureNeverDropsTheCurrentLeaseOrExitsRun` and
`TestPostgresProxy_RenewalFailureDoesNotDropAnOpenConnection` were re-run unchanged and stay green.

Cost, stated honestly per the brief's own ask: roughly two lease issuances per hour per pod instead
of one, each a role create/drop in `booth-database`. Not found to be a problem worth reporting back
on — nothing about this binary's own load profile makes that cost material.

## Honest residual limits

1. **No TLS, upstream or downstream, in v0.** Downstream: a client's SSLRequest is answered 'N'
   (denied), matching core's own bundled-Postgres connections, which already run `sslmode=disable`
   fleet-wide. Upstream: `PostgresCredential.SSLMode` is accepted and parsed but not acted on — if
   `booth-database`'s own deployment ever needs `sslmode=require` upstream, this needs real work
   (an upstream SSLRequest negotiation, then wrapping the connection in `crypto/tls`), not assumed
   to already work.
2. **No real cancel-request routing.** `BackendKeyData` sent to the client is fabricated (a random
   pid/secret, not the real upstream's) — a client issuing a real `CancelRequest` reconnection
   won't reach the right backend. This only affects the ability to kill a running query from a
   second connection; it has no effect on ordinary query/result traffic.
3. **A channel-binding (`-PLUS`) SCRAM variant is not implemented** — not needed while upstream is
   plaintext (channel binding ties the SASL exchange to a specific TLS channel, which doesn't exist
   here); revisit together with upstream TLS if that's ever added.
4. **The sidecar's own identity is handed to it, not managed by it** — `BOOTH_TOKEN`/
   `BOOTH_TOKEN_FILE` are new, small conventions this binary introduces (no existing fleet-wide
   name existed beyond a mention in `booth-notebooks`' own brief of a `BOOTH_TOKEN` fallback its
   client already uses informally); `BOOTH_CORE_URL`/`BOOTH_WORKSPACE` reuse names already
   established elsewhere in the fleet (`booth-pipeline`'s own gateway-routed default,
   `X-Workspace`'s header name respectively) rather than inventing new ones.
5. **No manifest field, no new `booth-core` HTTP route.** This is purely a new build target in this
   same repo (`cmd/credential-sidecar`) plus its own supporting packages (`internal/sidecar`,
   `internal/sidecar/pgwire`) — nothing about `internal/api`, `internal/registry`, or the CRD
   changed, matching ADR 0095's own "no new `contracts/module-manifest.md` field" consequence.

## Flagged back to booth-architecture / booth-database

- `contracts/credential-sidecar.md` could usefully name the exact renewal-failure timing rule (§1)
  explicitly, since its own wording and the task's framing only agree once read together — worth
  stating outright rather than leaving it to be re-derived.
