#!/usr/bin/env bash
# Fail if anything the build depends on is referenced by a moving name:
#  - every FROM in a Containerfile is pinned by digest (or names a build stage / build arg);
#  - the Go image used to build release binaries matches go.mod's toolchain line;
#  - every GitHub Action is pinned to a full commit SHA.
set -euo pipefail
cd "$(dirname "$0")/.."
fail=0
bad() { echo "check-pins: $*" >&2; fail=1; }

for f in $(git ls-files 'deploy/**/Containerfile*'); do
  stages=$(awk 'toupper($1)=="FROM" && toupper($(NF-1))=="AS" {print tolower($NF)}' "$f")
  while read -r ref; do
    [[ -z "$ref" || "$ref" == *'${'* || "$ref" == *@sha256:* ]] && continue
    grep -qx "$(tr '[:upper:]' '[:lower:]' <<<"$ref")" <<<"$stages" && continue
    bad "$f: FROM $ref is not pinned by digest"
  done < <(awk 'toupper($1)=="FROM" {for (i=2;i<=NF;i++) if ($i !~ /^--/) {print $i; break}}' "$f")
done

toolchain=$(awk '$1=="toolchain" {sub(/^go/, "", $2); print $2}' go.mod)
[[ -n "$toolchain" ]] || bad "go.mod has no toolchain line"
for f in $(git ls-files 'deploy/**/Containerfile*'); do
  while read -r v; do
    [[ "$v" == "$toolchain" ]] || bad "$f builds with golang:$v but go.mod's toolchain is go$toolchain"
  done < <(grep -oE 'library/golang:[0-9.]+' "$f" | cut -d: -f2)
done

for f in $(git ls-files '.github/workflows/*.yml' '.github/workflows/*.yaml'); do
  while read -r line; do
    bad "$f: action not pinned to a commit SHA: $line"
  done < <(grep -E '^\s*(-\s+)?uses:' "$f" | grep -vE 'uses:\s*\./' | grep -vE '@[0-9a-f]{40}(\s|$)' || true)
done

exit $fail
