#!/bin/sh
# Script-level tests for charts/booth-core/files/prune-s3-backups.sh (ADR 0109 amendment 2):
# count-based retention for the S3 backup destination. Runs the real script against
# test/scripts/fake-aws.sh and a local directory fixture standing in for a bucket -- no network,
# no real aws-cli, no real bucket required, so this runs in the fast CI layer (ci.yml), not
# integration.yml's kind cluster.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
repo_root=$(cd "$here/../.." && pwd)
prune_script="$repo_root/charts/booth-core/files/prune-s3-backups.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

bindir="$work/bin"
mkdir -p "$bindir"
ln -s "$here/fake-aws.sh" "$bindir/aws"
PATH="$bindir:$PATH"
export PATH

failures=0

fail() {
  echo "FAIL: $1" >&2
  failures=$((failures + 1))
}

# fixture_root <name> sets up (and echoes) a fresh, empty local-directory "bucket" for one test.
fixture_root() {
  r="$work/fixtures/$1"
  mkdir -p "$r"
  printf '%s' "$r"
}

# backup <root> <prefix> <timestamp> creates one fake timestamped backup "object".
backup() {
  mkdir -p "$1/$2$3/"
  echo dummy >"$1/$2$3/MANIFEST"
}

# run_prune <root> <prefix> <retention> [endpoint] runs the real script against the fixture.
run_prune() {
  r="$1" prefix="$2" retention="$3" endpoint="${4:-}"
  FAKE_S3_ROOT="$r" BACKUP_BUCKET=test-bucket BACKUP_PREFIX="$prefix" RETENTION="$retention" \
    BACKUP_ENDPOINT="$endpoint" sh "$prune_script"
}

# entries <root> <prefix> lists what's left under the fixture's prefix, one name per line.
entries() {
  d="$1/$2"
  [ -d "$d" ] || return 0
  (cd "$d" && for e in *; do [ -e "$e" ] && echo "$e"; done)
}

test_fewer_than_retention() {
  r=$(fixture_root fewer)
  backup "$r" "p/" 20260101T000000Z
  backup "$r" "p/" 20260102T000000Z
  backup "$r" "p/" 20260103T000000Z
  out=$(run_prune "$r" "p/" 7)
  got=$(entries "$r" "p" | sort)
  want=$(printf '20260101T000000Z\n20260102T000000Z\n20260103T000000Z')
  [ "$got" = "$want" ] || fail "fewer-than-retention: kept = [$got], want [$want] (output: $out)"
}

test_exactly_retention() {
  r=$(fixture_root exact)
  for ts in 20260101T000000Z 20260102T000000Z 20260103T000000Z; do backup "$r" "p/" "$ts"; done
  run_prune "$r" "p/" 3 >/dev/null
  got=$(entries "$r" "p" | wc -l | tr -d ' ')
  [ "$got" = 3 ] || fail "exactly-retention: kept $got entries, want 3"
}

test_more_than_retention() {
  r=$(fixture_root more)
  for ts in 20260101T000000Z 20260102T000000Z 20260103T000000Z 20260104T000000Z 20260105T000000Z; do
    backup "$r" "p/" "$ts"
  done
  run_prune "$r" "p/" 3 >/dev/null
  got=$(entries "$r" "p" | sort)
  want=$(printf '20260103T000000Z\n20260104T000000Z\n20260105T000000Z')
  [ "$got" = "$want" ] || fail "more-than-retention: kept = [$got], want the newest 3 [$want]"
}

test_non_timestamp_prefix_survives() {
  r=$(fixture_root mixed)
  for ts in 20260101T000000Z 20260102T000000Z 20260103T000000Z 20260104T000000Z; do
    backup "$r" "p/" "$ts"
  done
  mkdir -p "$r/p/README/"
  echo notes >"$r/p/README/notes.txt"
  run_prune "$r" "p/" 2 >/dev/null
  got=$(entries "$r" "p" | sort)
  want=$(printf '20260103T000000Z\n20260104T000000Z\nREADME')
  [ "$got" = "$want" ] || fail "non-timestamp-prefix: kept = [$got], want [$want] (README must survive)"
  [ -f "$r/p/README/notes.txt" ] || fail "non-timestamp-prefix: README/notes.txt was deleted, it never should have been touched"
}

test_empty_bucket() {
  r=$(fixture_root empty)
  out=$(run_prune "$r" "p/" 7)
  got=$(entries "$r" "p")
  [ -z "$got" ] || fail "empty-bucket: expected nothing under the prefix, got [$got]"
  echo "$out" | grep -q "deleting nothing" || fail "empty-bucket: expected a 'deleting nothing' log line, got: $out"
}

test_retention_must_be_a_positive_integer() {
  r=$(fixture_root bad-retention)
  backup "$r" "p/" 20260101T000000Z
  badlog="$work/bad-retention-out.log"
  for bad in 0 -1 abc "" "3.5" " 3"; do
    if run_prune "$r" "p/" "$bad" >"$badlog" 2>&1; then
      fail "retention='$bad': expected a non-zero exit, got success"
    fi
  done
  # And the one backup present must survive every rejected call above.
  got=$(entries "$r" "p")
  [ "$got" = "20260101T000000Z" ] || fail "retention validation: the existing backup was touched: [$got]"
}

test_upload_failed_so_nothing_is_pruned() {
  r=$(fixture_root upload-failed)
  backup "$r" "p/" 20260101T000000Z
  backup "$r" "p/" 20260102T000000Z
  log="$work/aws-calls-upload-failed.log"
  : >"$log"

  # Mirrors templates/postgresql-backup.yaml's upload container exactly: `sh -ec` so a failed
  # `aws s3 cp` (simulated via FAKE_AWS_CP_FAIL) must stop the script before "upload complete"
  # is ever printed and before the prune script is ever invoked.
  set +e
  out=$(FAKE_S3_ROOT="$r" FAKE_AWS_CP_FAIL=1 FAKE_AWS_LOG="$log" \
    BACKUP_BUCKET=test-bucket BACKUP_PREFIX="p/" RETENTION=1 \
    sh -ec '
      aws s3 cp --recursive /nonexistent "s3://${BACKUP_BUCKET}/${BACKUP_PREFIX}20260103T000000Z/"
      echo "upload complete"
      sh "'"$prune_script"'"
    ' 2>&1)
  rc=$?
  set -e

  [ "$rc" -ne 0 ] || fail "upload-failed: the combined script succeeded, want it to fail"
  echo "$out" | grep -q "upload complete" && fail "upload-failed: 'upload complete' was printed despite the failed upload"
  grep -q "s3 rm" "$log" 2>/dev/null && fail "upload-failed: the prune script ran an 's3 rm' despite the failed upload"
  got=$(entries "$r" "p" | sort)
  want=$(printf '20260101T000000Z\n20260102T000000Z')
  [ "$got" = "$want" ] || fail "upload-failed: the pre-existing backups were touched: [$got]"
}

test_prune_failure_fails_loudly_without_touching_the_newest() {
  r=$(fixture_root rm-fails)
  for ts in 20260101T000000Z 20260102T000000Z 20260103T000000Z; do backup "$r" "p/" "$ts"; done

  set +e
  FAKE_AWS_RM_FAIL=1 run_prune "$r" "p/" 1 >/dev/null 2>&1
  rc=$?
  set -e

  [ "$rc" -ne 0 ] || fail "prune-rm-failure: expected a non-zero exit when 'aws s3 rm' fails"
  [ -d "$r/p/20260103T000000Z" ] || fail "prune-rm-failure: the newest backup must survive a failed prune"
}

test_fewer_than_retention
test_exactly_retention
test_more_than_retention
test_non_timestamp_prefix_survives
test_empty_bucket
test_retention_must_be_a_positive_integer
test_upload_failed_so_nothing_is_pruned
test_prune_failure_fails_loudly_without_touching_the_newest

if [ "$failures" -gt 0 ]; then
  echo "$failures test(s) failed"
  exit 1
fi
echo "all prune-s3-backups.sh tests passed"
