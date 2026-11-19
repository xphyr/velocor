#!/usr/bin/env bash
# Fail if anything the build depends on is referenced by a moving name:
#  - every FROM in a Containerfile is pinned by digest, names a build stage, or uses a build arg;
#    a build arg's default must itself be pinned, except Fedora's rolling base images (B17: they
#    follow the release tag for daily fixes; `make` resolves the tag to a digest per build and
#    records it in the image's org.opencontainers.image.base.* labels);
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

# Base images allowed to follow a rolling tag (registry/repo:<Fedora release>).
rolling='^quay\.io/fedora/(fedora|fedora-minimal|fedora-bootc):[0-9]+$'
for f in $(git ls-files 'deploy/**/Containerfile*'); do
  while read -r name value; do
    [[ "$value" == */* ]] || continue                      # not an image reference
    [[ "$value" == *@sha256:* || "$value" =~ $rolling ]] && continue
    bad "$f: ARG $name defaults to $value, which is neither pinned by digest nor an allowed rolling base"
  done < <(awk 'toupper($1)=="ARG" && index($2, "=") {split($2, kv, "="); print kv[1], kv[2]}' "$f")
done

# One Fedora release for every base image (a bump must move them all), and the Makefile takes
# them from the Containerfiles rather than keeping its own copy (it once overrode a bump).
releases=$(git ls-files 'deploy/**/Containerfile*' | xargs grep -hoE 'quay\.io/fedora/[a-z-]+:[0-9]+' | sed 's/.*://' | sort -u)
[[ $(wc -l <<<"$releases") -eq 1 ]] || bad "Containerfiles use several Fedora releases: $(tr '\n' ' ' <<<"$releases")"
grep -nE 'quay\.io/fedora/[a-z-]+:[0-9]+' Makefile && bad "Makefile names a Fedora release; it must take the base images from the Containerfiles"

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

# The static manifest (shipped with releases) must run the same CSI sidecars as the Helm chart,
# whose values Renovate updates.
chart_sidecars=$(grep -oE 'registry\.k8s\.io/sig-storage/[^[:space:]"]+' deploy/charts/velocor-csi/values.yaml | sort)
manifest_sidecars=$(grep -oE 'registry\.k8s\.io/sig-storage/[^[:space:]"]+' deploy/k8s/csi/velocor-csi.yaml | sort)
[[ "$chart_sidecars" == "$manifest_sidecars" ]] ||
  bad "deploy/k8s/csi/velocor-csi.yaml's CSI sidecars differ from the chart's values.yaml: $(diff <(echo "$manifest_sidecars") <(echo "$chart_sidecars") | grep '^[<>]' | tr '\n' ' ')"
while read -r ref; do
  [[ "$ref" == *@sha256:* ]] || bad "chart sidecar $ref is not pinned by digest"
done <<<"$chart_sidecars"

exit $fail
