#!/usr/bin/env bash
# Run SPDK's unit tests for every library our local patches touch. Runs inside the
# `unit-tests` stage of deploy/containers/Containerfile.spdk (SPDK configured with unit tests,
# patches applied), so a carried patch can't ship without its own and SPDK's tests passing:
#   hack/spdk-patch-unit-tests.sh <spdk source dir> <patch dir>
set -euo pipefail
src="${1:?usage: $0 <spdk source dir> <patch dir>}"
patches="${2:?usage: $0 <spdk source dir> <patch dir>}"
cd "$src"

# lib/<component>/... paths changed by the patches -> test/unit/lib/<component>/*/*_ut
mapfile -t comps < <(grep -h '^+++ b/lib/' "$patches"/*.patch 2>/dev/null |
  sed -E 's#^\+\+\+ b/lib/([^/]+)/.*#\1#' | sort -u)
if [[ ${#comps[@]} -eq 0 ]]; then
  echo "no SPDK library is patched; nothing to test"
  exit 0
fi

ran=0 fail=0
for c in "${comps[@]}"; do
  found=0
  for t in test/unit/lib/"$c"/*/*_ut; do
    [[ -x "$t" ]] || continue
    found=1 ran=$((ran + 1))
    echo "== $t"
    "$t" || { echo "FAILED: $t" >&2; fail=1; }
  done
  # A patched component without unit tests would pass silently: make that visible.
  [[ $found -eq 1 ]] || { echo "no unit tests found for patched lib/$c" >&2; fail=1; }
done
echo "ran $ran SPDK unit test binaries for: ${comps[*]}"
exit $fail
