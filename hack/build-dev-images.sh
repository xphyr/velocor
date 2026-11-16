#!/usr/bin/env bash
# Build all Velocor images from the current commit and push them as dev-<sha> to a dev registry.
#
# Usage: IMAGE_PREFIX=<registry>/<org> hack/build-dev-images.sh <authfile>
#   <authfile>  podman authfile with push rights (a robot account, not a personal login)
#
# A thin wrapper around `make push` (which refuses dirty trees and release tags). Dev builds
# carry quay.expires-after so Quay removes them after DEV_EXPIRY as a backstop; remove superseded
# builds right away with hack/prune-dev-tags.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
AUTH=${1:?usage: IMAGE_PREFIX=<registry>/<org> $0 <authfile>}
: "${IMAGE_PREFIX:?set IMAGE_PREFIX to the dev registry and org, e.g. quay.example.com/velocor}"
TAG=dev-$(git rev-parse --short=7 HEAD)
export REGISTRY_AUTH_FILE=$AUTH

make push IMAGE_PREFIX="$IMAGE_PREFIX" TAG="$TAG" BUILD_LABELS="quay.expires-after=${DEV_EXPIRY:-30d}"

echo "=== quadlets in $IMAGE_PREFIX/velocor-os:$TAG"
podman run --rm --entrypoint grep "$IMAGE_PREFIX/velocor-os:$TAG" -h '^Image=' \
  /usr/share/containers/systemd/velocor-spdk.container /usr/share/containers/systemd/velocor-velod.container |
  tee /dev/stderr | grep -q "^Image=$IMAGE_PREFIX/velocor-velod:$TAG@sha256:" ||
  { echo "OS image is not pinned to velod $TAG by digest" >&2; exit 1; }
echo "=== DONE $TAG"
