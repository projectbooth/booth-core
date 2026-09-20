# booth-core decision 0009: Backup for the bundled PostgreSQL, and the external-cluster warning (ADR 0054)

Status: **implemented; nothing flagged back.** ADR 0054 ratified decision 0008 and asked for two
things: a minimal scheduled `pg_dump` for the bundled server, and a startup warning when core is
connected to an external PostgreSQL with `restrictMaintenanceAccess` left off. This records the
choices made in building them.

## The backup CronJob (`templates/postgresql-backup.yaml`, `files/pg-backup.sh`)

- **A daily logical dump, and only that.** Per ADR 0054 this is a baseline, not point-in-time
  recovery. `pg_dumpall --globals-only` (roles) plus one custom-format `pg_dump` per database, so a
  single module's database can be restored without touching the others.
- **Runs in the server's own image**, so `pg_dump`'s version always matches the server's — a
  `pg_dump` older than the server refuses to run, and the two drift apart the moment someone bumps
  `postgresql.image.tag`.
- **A backup is atomic.** Built in a hidden directory and renamed as the last step, so a crashed or
  killed run can never be mistaken for a good backup. Refuses to overwrite an existing timestamp
  (a plain `mv` onto a directory would silently nest the new backup inside the old one).
- **Destination: PVC by default, S3-compatible bucket when `postgresql.backup.s3.bucket` is set**
  (the two options ADR 0054 named). The bucket path stages the dump in an `emptyDir` in an
  init container (same image as the server) and uploads it from a second container with the AWS
  CLI, which works against MinIO/Ceph/R2 via `endpoint`. Retention on a PVC is the script's
  (newest N); on a bucket it's the bucket's lifecycle policy — the chart can't tell a bucket to
  expire objects portably, and pretending to would be worse than saying so.
- **The PVC is `helm.sh/resource-policy: keep`.** `helm uninstall` shouldn't delete your backups.
  Cost: a later install under the same release name trips over the surviving PVC. Chosen because
  silently deleting backups is the worse failure.
- **`concurrencyPolicy: Forbid`**, one retry, a one-hour deadline, and three jobs of history kept
  for both outcomes so a failure is visible with `kubectl get jobs`.
- **The PostgreSQL NetworkPolicy admits the backup pods** (by label). Without that the backup
  would work in tests on a CNI that doesn't enforce policy and fail on k3s, which does.
- **The uploader is pinned** (`amazon/aws-cli:2.36.49`) and used only in bucket mode.

## Verified against the real thing

The kind CI job runs the real CronJob on the real bundled server and checks: the backup is
complete and contains the roles and every database; a module's database restores into a scratch
database with its data intact; the exact in-place restore command from the runbook rolls a
drifted database back and leaves objects owned by the module's role; retention prunes to N with
no incomplete runs left behind; and, after switching the chart to a MinIO in the cluster, the
same backup arrives in the bucket. The embedded PostgreSQL used by the Go tests ships no
`pg_dump`, so the script has no local unit test — CI is where it's proven.

**Not verified end to end:** the major-version upgrade path in the runbook (its pieces are each
covered — backup, restore, and core re-provisioning a wiped server — but the sequence isn't run).
The runbook says so and says to rehearse it.

## Limits (all documented in the runbook and `NOTES.txt`)

Up to a day of data can be lost; consistency is per database, not across them; the default PVC
destination shares fate with the cluster; nothing alerts on a failed job; backups hold every
module's data and the roles' password hashes and must be protected like the database; a bucket
upload needs node ephemeral storage for one full dump.

## The external-cluster warning

`config.PostgresConfig.StartupWarnings` returns the message; `cmd/core` logs it at startup, prefixed
`WARNING:`, when connected to an external server with `restrictMaintenanceAccess` off. The chart's
`NOTES.txt` prints the same warning at install/upgrade, since an operator may never read core's
logs. The default is unchanged (opt-in) — ADR 0054 ruled that, since core doesn't own that
cluster. The bundled server never warns (it's always restricted).
