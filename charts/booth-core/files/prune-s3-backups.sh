#!/bin/sh
# Count-based retention for the S3-compatible backup destination (ADR 0109 amendment 2,
# 2026-10-12): keep the newest RETENTION complete backups, delete nothing else, and never use
# an age-based rule. A bucket lifecycle rule based on age was rejected -- if backups stop
# arriving, every copy ages out and the last good backup is deleted with it. This script is run
# by the upload container in templates/postgresql-backup.yaml, and only after its own upload has
# completed successfully (see that container's "set -e" ordering): a prune failure must fail the
# job loudly, but it must never undo or affect the backup that was just uploaded.
#
# Lists the immediate entries under s3://$BACKUP_BUCKET/$BACKUP_PREFIX (one level, not
# --recursive). Only entries matching the backup timestamp form exactly, ^[0-9]{8}T[0-9]{6}Z/$,
# are ever candidates; anything else under that prefix (a stray object, a differently-named
# subdirectory) is logged and left alone -- this script only ever deletes something it can prove
# is one of pg-backup.sh's own timestamped backups.
#
# Environment:
#   BACKUP_BUCKET    required
#   BACKUP_PREFIX    optional, may be empty
#   BACKUP_ENDPOINT  optional; passed to `aws s3` as --endpoint-url when set (MinIO/Ceph/R2/etc.)
#   RETENTION        required; must be an integer >= 1 -- refused otherwise, loudly
set -eu

: "${BACKUP_BUCKET:?BACKUP_BUCKET is required}"
BACKUP_PREFIX="${BACKUP_PREFIX:-}"

case "${RETENTION:-}" in
  '' | *[!0-9]*)
    echo "prune: RETENTION must be an integer >= 1, got '${RETENTION:-}'" >&2
    exit 1
    ;;
esac
if [ "$RETENTION" -lt 1 ]; then
  echo "prune: RETENTION must be an integer >= 1, got '$RETENTION'" >&2
  exit 1
fi

endpoint_opt=""
if [ -n "${BACKUP_ENDPOINT:-}" ]; then
  endpoint_opt="--endpoint-url $BACKUP_ENDPOINT"
fi

uri="s3://${BACKUP_BUCKET}/${BACKUP_PREFIX}"

# `aws s3 ls` on a non-recursive URI marks a subdirectory with a "PRE" column instead of a
# date/size, the same shape `ls -F` uses for directories; files are left behind entirely here,
# since nothing but a timestamped subdirectory is ever a backup.
all_prefixes=$(aws s3 ls $endpoint_opt "$uri" | awk '/^ *PRE / {print $NF}')

candidates=""
for p in $all_prefixes; do
  case "$p" in
    [0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z/)
      candidates=$(printf '%s\n%s' "$candidates" "$p")
      ;;
    *)
      echo "prune: ignoring non-backup entry under ${uri} (never deleted): $p"
      ;;
  esac
done
candidates=$(printf '%s\n' "$candidates" | sed '/^$/d' | sort)

count=0
if [ -n "$candidates" ]; then
  count=$(printf '%s\n' "$candidates" | wc -l | tr -d ' ')
fi

if [ "$count" -le "$RETENTION" ]; then
  echo "prune: $count backup(s) under ${uri}, retention $RETENTION -- deleting nothing"
  printf '%s\n' "$candidates" | sed '/^$/d' | while IFS= read -r p; do
    echo "prune: keeping $p"
  done
  exit 0
fi

to_delete_count=$((count - RETENTION))
to_delete=$(printf '%s\n' "$candidates" | head -n "$to_delete_count")
to_keep=$(printf '%s\n' "$candidates" | tail -n "$RETENTION")

printf '%s\n' "$to_keep" | sed '/^$/d' | while IFS= read -r p; do
  echo "prune: keeping $p"
done

printf '%s\n' "$to_delete" | sed '/^$/d' | while IFS= read -r p; do
  echo "prune: deleting ${uri}${p}"
  aws s3 rm $endpoint_opt --recursive "${uri}${p}"
done
