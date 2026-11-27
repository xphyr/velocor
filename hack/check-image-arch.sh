#!/usr/bin/env bash
# Check that each image of a local multi-arch manifest list holds programs for its own platform.
#
# Usage: hack/check-image-arch.sh <local manifest list> [program in the image ...]
#   default program: /usr/bin/ls
#
# A cross-compiled Go binary comes out right for every platform even when the base under it is
# wrong: podman 4.9 (CI's) built the v0.2.0 CSI image's arm64 instance on the amd64 fedora-minimal
# that the velod build had just pulled (same reference, by digest), so nvme, iscsiadm, mkfs and
# mount were x86-64 and every node operation on arm64 failed. Reads the ELF header (e_machine) of
# each program, so it needs no emulation.
set -euo pipefail
LIST=${1:?usage: $0 <local manifest list> [program ...]}
shift
PROGS=("${@:-/usr/bin/ls}")
PODMAN=${PODMAN:-podman}

declare -A MACHINE=([amd64]=62 [arm64]=183 [ppc64le]=21 [s390x]=22)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

list=$($PODMAN manifest inspect "$LIST") || { echo "check-image-arch: $LIST is not a local manifest list" >&2; exit 1; }
# Each local image's ID, digest and repo digests (a pulled image is listed by the digest it was
# pulled by, which can be the list's; its own is in RepoDigests).
images=$($PODMAN images --all --no-trunc --format '{{.ID}} {{.Digest}} {{.RepoDigests}}' | tr -d '[]')
fail=0
n=0
while read -r arch digest; do
  n=$((n + 1))
  want=${MACHINE[$arch]:-}
  [[ -n "$want" ]] || { echo "check-image-arch: $LIST: no ELF machine known for $arch" >&2; fail=1; continue; }
  # Instances are local images whose digest is the one the list names.
  id=$(awk -v d="$digest" '{for (i = 2; i <= NF; i++) if ($i == d || $i ~ ("@" d "$")) {print $1; exit}}' <<<"$images")
  [[ -n "$id" ]] || { echo "check-image-arch: $LIST: $arch instance $digest is not in local storage" >&2; fail=1; continue; }
  ctr=$($PODMAN create -q "$id")
  for prog in "${PROGS[@]}"; do
    out="$tmp/$arch-$(basename "$prog")"
    if ! $PODMAN cp "$ctr:$prog" "$out" >/dev/null 2>&1; then
      echo "check-image-arch: $LIST ($arch): $prog not found" >&2; fail=1; continue
    fi
    # Follow a symlink copied as such (podman cp keeps links).
    [[ -L "$out" ]] && { $PODMAN cp "$ctr:$(dirname "$prog")/$(readlink "$out")" "$out.t" >/dev/null 2>&1 && mv -f "$out.t" "$out"; }
    got=$(od -An -tu2 -j18 -N2 "$out" | tr -d ' ')
    if [[ "$(head -c4 "$out" | od -An -c | tr -d ' ')" != "177ELF" || "$got" != "$want" ]]; then
      echo "check-image-arch: $LIST: the $arch image's $prog is ELF machine ${got:-?}, not $want ($arch)" >&2
      fail=1
    fi
  done
  $PODMAN rm "$ctr" >/dev/null
done < <(python3 -c 'import json,sys; [print(m["platform"]["architecture"], m["digest"]) for m in json.load(sys.stdin)["manifests"]]' <<<"$list")

[[ $n -gt 0 ]] || { echo "check-image-arch: $LIST has no images" >&2; exit 1; }
[[ $fail -eq 0 ]] && echo "check-image-arch: $LIST: $n platform(s), programs match"
exit $fail
