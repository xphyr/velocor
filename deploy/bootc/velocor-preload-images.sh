#!/bin/bash
set -euo pipefail

load_if_missing() {
  local image_ref="$1"
  local archive="$2"
  if [ -n "$image_ref" ] && [ -f "$archive" ] && ! podman image exists "$image_ref" 2>/dev/null; then
    echo "Preloading ${image_ref} from ${archive}..."
    podman load -i "$archive"
  fi
}

SPDK_REF="$(awk -F= '/^Image=/{print $2; exit}' /usr/share/containers/systemd/velocor-spdk.container 2>/dev/null || true)"
VELOD_REF="$(awk -F= '/^Image=/{print $2; exit}' /usr/share/containers/systemd/velocor-velod.container 2>/dev/null || true)"

load_if_missing "$SPDK_REF" /usr/share/velocor/images/velocor-spdk.tar
load_if_missing "$VELOD_REF" /usr/share/velocor/images/velocor-velod.tar
