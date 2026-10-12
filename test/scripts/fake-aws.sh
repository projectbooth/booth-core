#!/bin/sh
# A fake `aws` CLI for test/scripts/prune-s3-backups_test.sh. Understands only the subset of
# `aws s3` that charts/booth-core/files/prune-s3-backups.sh (and the upload container's own
# `aws s3 cp`) actually calls, against a local directory fixture named by FAKE_S3_ROOT --
# directories under it are "prefixes", regular files are objects. No network, no real aws-cli,
# no real bucket: the point is to test the prune logic's decisions, not the AWS SDK.
set -eu

: "${FAKE_S3_ROOT:?FAKE_S3_ROOT must point at the local fixture root}"

[ "$1" = "s3" ] && shift
sub=$1
shift

uri=""
recursive=0
for a in "$@"; do
  case "$a" in
    s3://*) uri="$a" ;;
    --recursive) recursive=1 ;;
  esac
done

path=$(printf '%s' "$uri" | sed 's#^s3://[^/]*/##')
dir="$FAKE_S3_ROOT/$path"

if [ -n "${FAKE_AWS_LOG:-}" ]; then
  printf 's3 %s %s\n' "$sub" "$uri" >>"$FAKE_AWS_LOG"
fi

case "$sub" in
  ls)
    [ -d "$dir" ] || exit 0
    for entry in "$dir"/*; do
      [ -e "$entry" ] || continue
      name=$(basename "$entry")
      if [ -d "$entry" ]; then
        printf '                           PRE %s/\n' "$name"
      else
        size=$(wc -c <"$entry" | tr -d ' ')
        printf '2026-01-01 00:00:00 %10s %s\n' "$size" "$name"
      fi
    done
    ;;
  rm)
    if [ "${FAKE_AWS_RM_FAIL:-0}" = "1" ]; then
      echo "fake-aws: simulated rm failure" >&2
      exit 1
    fi
    [ "$recursive" = 1 ] || { echo "fake-aws: rm without --recursive not supported by this fixture" >&2; exit 2; }
    rm -rf "${dir%/}"
    ;;
  cp)
    if [ "${FAKE_AWS_CP_FAIL:-0}" = "1" ]; then
      echo "fake-aws: simulated cp failure" >&2
      exit 1
    fi
    mkdir -p "$dir"
    touch "$dir/uploaded"
    ;;
  *)
    echo "fake-aws: unsupported subcommand '$sub'" >&2
    exit 2
    ;;
esac
