#!/usr/bin/env bash
# Build all Velocor images from the current commit and push them as dev-<sha>.
#
# Usage: hack/build-dev-images.sh <authfile>
#   <authfile>  podman authfile with push rights (a robot account, not a personal login)
#
# Dev builds carry quay.expires-after so Quay removes them on its own after DEV_EXPIRY (a
# backstop); remove superseded builds right away with hack/prune-dev-tags.sh. Refuses to run on
# a dirty tree, so a dev-<sha> tag always matches the commit it names, and never pushes latest
# or release tags.
set -euo pipefail
cd "$(dirname "$0")/.."
AUTH=${1:?usage: $0 <authfile>}
[[ -z "$(git status --porcelain --untracked-files=no)" ]] || { echo "working tree has uncommitted changes; commit first" >&2; exit 1; }
SHA=$(git rev-parse --short HEAD)
TAG=dev-$SHA
P=${IMAGE_PREFIX:-registry.xphyrlab.net/velocor}
LABELS="quay.expires-after=${DEV_EXPIRY:-30d}"
log() { echo "=== $(date +%T) $*"; }

log "build spdk";  make container-spdk  BUILD_LABELS="$LABELS" SPDK_IMAGE="$P/velocor-spdk:$TAG"
log "build velod"; make container-velod BUILD_LABELS="$LABELS" VELOD_IMAGE="$P/velocor-velod:$TAG"
log "build csi";   make container-csi   BUILD_LABELS="$LABELS" CSI_IMAGE="$P/velocor-csi:$TAG"
for img in velocor-spdk velocor-velod velocor-csi; do
  log "push $img:$TAG"; podman push --authfile "$AUTH" "$P/$img:$TAG"
done
log "build os (bootc), pinned to $TAG"
make container-bootc BUILD_LABELS="$LABELS" SPDK_IMAGE="$P/velocor-spdk:$TAG" \
  VELOD_IMAGE="$P/velocor-velod:$TAG" BOOTC_IMAGE="$P/velocor-os:$TAG"
log "push velocor-os:$TAG"; podman push --authfile "$AUTH" "$P/velocor-os:$TAG"
log "verify the OS image's quadlets are pinned to $TAG"
podman run --rm --entrypoint grep "$P/velocor-os:$TAG" -h '^Image=' \
  /usr/share/containers/systemd/velocor-spdk.container /usr/share/containers/systemd/velocor-velod.container
log "DONE $TAG"
