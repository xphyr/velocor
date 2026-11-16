#!/bin/bash
# Load the container images embedded in this OS image into podman's storage, so the quadlets
# (which reference them by digest) start without a registry. Runs at every boot: after a
# `bootc upgrade` it loads the new images and removes the previous Velocor images that are no
# longer referenced (never by force, so an image still in use is kept).
set -euo pipefail

declare -A wanted=()
for name in velocor-spdk velocor-velod; do
  ref="$(awk -F= '/^Image=/{print $2; exit}' "/usr/share/containers/systemd/${name}.container")"
  archive="/usr/share/velocor/images/${name}.oci.tar"
  [[ -n "$ref" && -f "$archive" ]] || { echo "no embedded image for ${name}" >&2; continue; }
  if ! podman image exists "$ref"; then
    echo "Loading ${ref} from ${archive}"
    podman load -q -i "$archive"
    podman image exists "$ref" || { echo "loaded ${archive} but ${ref} is still missing" >&2; exit 1; }
  fi
  wanted["$(podman image inspect --format '{{.Id}}' "$ref")"]=1
  repo="${ref%@*}"; repo="${repo%:*}"
  while read -r id; do
    [[ -n "$id" && -z "${wanted[$id]:-}" ]] || continue
    podman image rm "$id" >/dev/null 2>&1 && echo "Removed superseded ${repo} image ${id:0:12}" || true
  done < <(podman images --no-trunc --filter "reference=${repo}" --format '{{.ID}}' | sort -u)
done
