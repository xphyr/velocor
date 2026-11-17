#!/usr/bin/env bash
# Run SPDK's unit tests for every library or module our local patches touch. Runs inside the
# `unit-tests` stage of deploy/containers/Containerfile.spdk (SPDK configured with unit tests,
# patches applied), so a carried patch can't ship without its own and SPDK's tests passing:
#   hack/spdk-patch-unit-tests.sh <spdk source dir> <patch dir>
# Paths map to SPDK's unit test layout:
#   lib/<component>/...        -> test/unit/lib/<component>/
#   module/<kind>/<name>/...   -> test/unit/lib/<kind>/<name>/   (e.g. module/bdev/raid)
set -euo pipefail
src="${1:?usage: $0 <spdk source dir> <patch dir>}"
patches="${2:?usage: $0 <spdk source dir> <patch dir>}"
cd "$src"

mapfile -t dirs < <(grep -h '^+++ b/' "$patches"/*.patch 2>/dev/null | sed -E \
  -e 's#^\+\+\+ b/lib/([^/]+)/.*#test/unit/lib/\1#' \
  -e 's#^\+\+\+ b/module/([^/]+)/([^/]+)/.*#test/unit/lib/\1/\2#' \
  -e '/^\+\+\+ /d' | sort -u)
if [[ ${#dirs[@]} -eq 0 ]]; then
  echo "no SPDK library or module is patched; nothing to test"
  exit 0
fi

ran=0 fail=0
for d in "${dirs[@]}"; do
  mapfile -t tests < <(find "$d" -type f -name '*_ut' -perm -u+x 2>/dev/null | sort)
  # A patched component without unit tests would pass silently: make that visible.
  if [[ ${#tests[@]} -eq 0 ]]; then
    echo "no unit tests found in $d for a patched component" >&2
    fail=1
    continue
  fi
  for t in "${tests[@]}"; do
    ran=$((ran + 1))
    echo "== $t"
    "$t" || { echo "FAILED: $t" >&2; fail=1; }
  done
done
echo "ran $ran SPDK unit test binaries in: ${dirs[*]}"
exit $fail
