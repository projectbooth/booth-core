#!/bin/sh
# Logical backup of the bundled PostgreSQL (ADR 0054): a v0 baseline, not point-in-time
# recovery. Run by the CronJob in templates/postgresql-backup.yaml inside the same
# postgres image as the server, so pg_dump's version always matches.
#
# One backup is a directory:
#   globals.sql      roles (including their password hashes) — pg_dumpall --globals-only
#   <database>.dump  one pg_dump custom-format archive per database (selective pg_restore)
#   MANIFEST         timestamp, server version, and the files above
# Treat it as sensitive: it contains every module's data and the roles' password hashes.
#
# Environment (standard libpq variables do the connecting):
#   PGHOST PGUSER PGPASSWORD [PGPORT] [PGSSLMODE]
#   BACKUP_MODE   pvc   write $BACKUP_DIR/<timestamp>/ and keep the newest $RETENTION
#                 stage write $STAGE_DIR/dump/ and $STAGE_DIR/ts for an uploader to ship;
#                       retention is then the bucket's lifecycle policy, not this script's
#   BACKUP_DIR    (pvc)   directory backups accumulate in
#   STAGE_DIR     (stage) scratch directory shared with the uploader
#   RETENTION     (pvc)   how many complete backups to keep (default 7)
#
# A backup only becomes visible once it is complete: it is built in a hidden directory and
# renamed as the last step, so a crashed run can never be mistaken for a good one.
set -eu

: "${PGHOST:?PGHOST is required}"
: "${PGUSER:?PGUSER is required}"
: "${BACKUP_MODE:?BACKUP_MODE must be pvc or stage}"
RETENTION="${RETENTION:-7}"

ts=$(date -u +%Y%m%dT%H%M%SZ)

case "$BACKUP_MODE" in
  pvc)
    : "${BACKUP_DIR:?BACKUP_DIR is required in pvc mode}"
    root="$BACKUP_DIR"
    work="$root/.incomplete-$ts"
    ;;
  stage)
    : "${STAGE_DIR:?STAGE_DIR is required in stage mode}"
    root="$STAGE_DIR"
    work="$STAGE_DIR/dump"
    ;;
  *)
    echo "unknown BACKUP_MODE '$BACKUP_MODE' (want pvc or stage)" >&2
    exit 2
    ;;
esac

ok=0
trap 'if [ "$ok" != 1 ]; then rm -rf "$work"; fi' EXIT

rm -rf "$work"
mkdir -p "$work"

echo "backup $ts: starting (server $PGHOST, mode $BACKUP_MODE)"

version=$(psql -X -Atq -d postgres -c 'show server_version')

pg_dumpall --globals-only > "$work/globals.sql"
[ -s "$work/globals.sql" ] || { echo "globals dump is empty" >&2; exit 1; }
files="globals.sql"

# Every database that accepts connections and isn't a template. One archive each, so a single
# module's database can be restored without touching the others.
psql -X -Atq -d postgres -c \
  "select datname from pg_database where not datistemplate and datallowconn order by 1" > "$work/.databases"

while IFS= read -r db; do
  [ -n "$db" ] || continue
  file=$(printf '%s' "$db" | tr '/ ' '__').dump
  echo "backup $ts: dumping $db"
  pg_dump --format=custom --dbname="$db" --file="$work/$file"
  [ -s "$work/$file" ] || { echo "dump of $db is empty" >&2; exit 1; }
  files="$files $file"
done < "$work/.databases"
rm -f "$work/.databases"

{
  echo "timestamp: $ts"
  echo "server_version: $version"
  echo "files: $files"
} > "$work/MANIFEST"

case "$BACKUP_MODE" in
  pvc)
    # Timestamps have one-second resolution. `mv` onto an existing directory would silently
    # nest the new backup inside the old one, so refuse instead.
    if [ -e "$root/$ts" ]; then
      echo "backup $ts already exists; not overwriting" >&2
      exit 1
    fi
    mv "$work" "$root/$ts"
    ok=1
    echo "backup $ts: complete -> $root/$ts"

    # Retention: keep the newest $RETENTION complete backups. Leftover incomplete runs (a
    # previous crash) are removed too; the CronJob forbids overlapping runs, so none is live.
    for d in "$root"/.incomplete-*; do
      [ -e "$d" ] && rm -rf "$d"
    done
    i=0
    for d in $(ls -1d "$root"/[0-9]*T*Z 2>/dev/null | sort -r); do
      i=$((i + 1))
      if [ "$i" -gt "$RETENTION" ]; then
        echo "backup $ts: pruning $d (keeping newest $RETENTION)"
        rm -rf "$d"
      fi
    done
    ;;
  stage)
    printf '%s' "$ts" > "$STAGE_DIR/ts"
    ok=1
    echo "backup $ts: staged for upload"
    ;;
esac
