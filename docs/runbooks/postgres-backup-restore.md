# Runbook: backing up, restoring, and upgrading the bundled PostgreSQL

Applies to the **bundled** single-node PostgreSQL only (`postgresql.enabled=true`, the chart
default). An external cluster's backup, restore, and upgrade are yours (ADR 0054).

What this is, and isn't: a **daily logical backup** (`pg_dump`) — a v0 baseline. It is **not**
point-in-time recovery: you can lose up to a day of writes, and restoring means replaying a dump.
The bundled server is single-node and not highly available.

## What gets backed up, and where

A `CronJob` (`<release>-postgresql-backup`, daily at 03:00 by default) runs `pg_dump` in the same
image as the server, so the versions always match. Each run produces one directory:

```
20260919T030000Z/
  MANIFEST            timestamp, server version, file list
  globals.sql         roles, including their password hashes  (pg_dumpall --globals-only)
  booth_core.dump     one pg_dump custom-format archive per database
  booth_mod_<id>.dump ...
```

A backup only appears once it is complete (it's built in a hidden directory and renamed last), so
any timestamp-named directory is a whole backup.

**Destination** — set in chart values:

- **PVC** (default): `<release>-postgresql-backup`. The newest `postgresql.backup.retention`
  (default 7) are kept. The PVC is annotated `helm.sh/resource-policy: keep`, so `helm uninstall`
  doesn't delete your backups — which also means a later `helm install` under the same release
  name will complain the PVC already exists; delete it deliberately or adopt it.
- **S3-compatible bucket**: set `postgresql.backup.s3.bucket` (plus `endpoint` for MinIO/Ceph/R2,
  and `existingSecret` holding `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`). Objects land under
  `<prefix><timestamp>/`. **Retention is your bucket's lifecycle policy**, not the chart's.

> **A backup on the same cluster and storage as the database does not survive losing that
> cluster.** For real disaster recovery use a bucket, or copy the PVC's contents off-site on a
> schedule. Backups contain all module data and password hashes: protect them like the database.

**Checking that backups are happening.** Nothing alerts you if the job fails. Look:

```sh
kubectl -n <ns> get cronjob,jobs -l app.kubernetes.io/component=postgresql-backup
kubectl -n <ns> logs job/<a-recent-job>
```

Wire `kube_job_status_failed` (or your platform's equivalent) to an alert if you have monitoring.

## Reading the backups from the PVC

```sh
kubectl -n <ns> run backup-shell --rm -it --restart=Never --image=postgres:16-alpine \
  --overrides='{"spec":{"securityContext":{"runAsUser":70,"fsGroup":70},
    "containers":[{"name":"backup-shell","image":"postgres:16-alpine","command":["sh"],"stdin":true,"tty":true,
      "volumeMounts":[{"name":"b","mountPath":"/backups"}]}],
    "volumes":[{"name":"b","persistentVolumeClaim":{"claimName":"<release>-postgresql-backup"}}]}}' \
  --labels=app.kubernetes.io/name=postgresql-backup,app.kubernetes.io/instance=<release>
```

The labels matter when the NetworkPolicy is enforced: they're what lets the pod reach PostgreSQL.
Inside, `ls /backups` lists complete backups; use `kubectl cp` from a pod like this to copy one off.

## Restoring a module's database (the server is fine, the data is wrong)

The database and its role already exist, so restore over the top. Do this as the `postgres`
superuser, with the module **stopped** so nothing writes while you do it.

```sh
# PGHOST / PGUSER=postgres / PGPASSWORD (Secret booth-postgres-admin) set as usual.
pg_restore --clean --if-exists --no-owner --role=booth_mod_storage \
  --dbname=booth_mod_storage /backups/20260919T030000Z/booth_mod_storage.dump
```

`--clean --if-exists` drops and recreates the archived objects; `--role` makes the restored
objects owned by the module's own role. (This exact command is exercised in CI against the real
bundled server: after the database drifted, restoring rolled it back and left the tables owned by
the module's role.) Objects that exist in the database but **not** in the backup are left alone —
for a full reset, drop and recreate the database first.

To inspect a backup without touching anything, restore into a scratch database:

```sh
createdb restore_check
pg_restore --no-owner --dbname=restore_check /backups/<ts>/booth_mod_storage.dump
```

## Restoring after losing the server (empty server, PVC lost)

1. Install the chart as before. Core generates a new admin password only if the
   `booth-postgres-admin` Secret is gone; if the Secret survived it reuses it, which is what
   you want.
2. Core re-provisions on its own: for every module whose credential Secret still exists it
   recreates the role and an **empty** database using the password *already in that Secret*, so
   running modules keep their credentials. Wait for core's log line `user directory is now
   persistent` and for modules' Secrets to be present.
3. Restore each database with the command above.
4. If a module's credential Secret was also lost, core issues a new password for it when the
   module reconciles; the module picks it up the way it always does (the Secret).

`globals.sql` is only needed to restore the roles with their **original** password hashes when
the credential Secrets are gone too; normally step 2 makes it unnecessary. Review it before
applying — it contains the `postgres` superuser's hash as well.

## Major-version upgrade of the bundled server

Not automated (ADR 0054). PostgreSQL will not start a new major version on an old data
directory, so an upgrade is dump → new empty server → restore. **Rehearse this on a copy first;
the end-to-end upgrade path is not exercised in CI** (its pieces are: backup, restore, and core's
re-provisioning of a wiped server).

1. Take a fresh backup (`kubectl create job --from=cronjob/<release>-postgresql-backup upgrade-backup`),
   confirm it completed, and **copy it off-cluster**.
2. Stop the modules that use databases, so nothing writes after the backup.
3. `kubectl scale statefulset/<release>-postgresql --replicas=0`, then delete the data volume
   claim `data-<release>-postgresql-0` (this is the point of no return — the copy from step 1 is
   your only data).
4. `helm upgrade` with the new `postgresql.image.tag` (for example `17-alpine`). The server
   initialises a fresh cluster with the same admin password (still in `booth-postgres-admin`).
5. Let core re-provision (see "Restoring after losing the server", step 2), then restore each
   database from the backup, and restart the modules.
6. Take a new backup with the new version and confirm it.

## Limits worth knowing

- Up to 24 hours of data can be lost between the last backup and a failure; shorten
  `postgresql.backup.schedule` to reduce it (each run dumps everything).
- Dumps hold a consistent snapshot per database, not across databases.
- Backups run on the bundled server's own resources; a very large database will lengthen the run
  (a job is stopped after one hour: `activeDeadlineSeconds`).
- For an object-storage destination the dump is staged on the node's ephemeral storage
  (`emptyDir`) before upload, so the node needs room for one full dump.
