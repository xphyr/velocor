#!/bin/bash
# Load the container images embedded in this OS image into podman's storage, so the quadlets
# (which reference them by digest) start without a registry. Runs at every boot: after a
# `bootc upgrade` it loads the new images and removes the previous Velocor images that are no
# longer referenced (never by force, so an image still in use is kept).
set -euo pipefail

# Image IDs appear with and without the "sha256:" prefix depending on the podman command.
norm() { echo "${1#sha256:}"; }

refs=()
for name in velocor-spdk velocor-velod velocor-prometheus velocor-alertmanager; do
  ref="$(awk -F= '/^Image=/{print $2; exit}' "/usr/share/containers/systemd/${name}.container")"
  archive="/usr/share/velocor/images/${name}.oci.tar"
  [[ -n "$ref" && -f "$archive" ]] || { echo "no embedded image for ${name}" >&2; continue; }
  if ! podman image exists "$ref"; then
    echo "Loading ${ref} from ${archive}"
    podman load -q -i "$archive"
    podman image exists "$ref" || { echo "loaded ${archive} but ${ref} is still missing" >&2; exit 1; }
  fi
  refs+=("$ref")
done

# Remove superseded images of the same repositories (left behind by earlier OS versions).
for ref in "${refs[@]}"; do
  keep="$(norm "$(podman image inspect --format '{{.Id}}' "$ref")")"
  repo="${ref%@*}"
  while read -r id; do
    [[ -n "$id" && "$(norm "$id")" != "$keep" ]] || continue
    if podman image rm "$id" >/dev/null 2>&1; then echo "Removed superseded ${repo} image $(norm "$id" | cut -c1-12)"; fi
  done < <(podman images --no-trunc --filter "reference=${repo}" --format '{{.ID}}' | sort -u)
done

# Never leave the appliance without the images its quadlets need.
for ref in "${refs[@]}"; do
  podman image exists "$ref" || { echo "BUG: ${ref} missing after cleanup" >&2; exit 1; }
done
