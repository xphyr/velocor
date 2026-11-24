#!/bin/bash
# Load the container images embedded in this OS image into podman's storage, so the quadlets
# (which reference them by digest) start without a registry. Runs at every boot: after a
# `bootc upgrade` it loads the new images and removes the previous Velocor images that are no
# longer referenced (never by force, so an image still in use is kept).
set -euo pipefail

# A power cut can leave podman's storage unreadable (its record of --rm containers isn't synced by
# design: on a lab appliance it came back as zero bytes and nothing could start), and velod runs
# in a container. Everything in it can be had again: containers are made anew at every start, and
# the images are embedded here. Repaired in steps, the least first.
storage="$(podman info --format '{{.Store.GraphRoot}}' 2>/dev/null || true)"
root="${storage:-/var/lib/containers/storage}"
# With a container running it isn't boot: leave the storage alone.
if [[ -z "$storage" ]] && ! pgrep -x conmon >/dev/null; then
  echo "podman's storage can't be read: $(podman info 2>&1 | tail -1)" >&2
  for f in "$root/overlay-containers/volatile-containers.json" "$root/overlay-containers/containers.json" "$root/overlay-layers/volatile-layers.json"; do
    [[ -e "$f" ]] || continue
    jq -e 'type == "array"' "$f" >/dev/null 2>&1 && continue
    echo "Removing the damaged $f" >&2
    rm -f "$f"
  done
  if ! podman info >/dev/null 2>&1; then
    echo "Starting over with empty storage (the old one is kept in $root.damaged)" >&2
    rm -rf "$root.damaged"
    mv "$root" "$root.damaged"
    podman info >/dev/null
  fi
fi

# Image IDs appear with and without the "sha256:" prefix depending on the podman command.
norm() { echo "${1#sha256:}"; }

refs=()
for name in velocor-velod velocor-prometheus velocor-alertmanager; do
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

# Remove superseded images of the same repositories (left behind by earlier OS versions), and the
# SPDK image of releases that still had it.
while read -r id; do
  [[ -n "$id" ]] || continue
  if podman image rm "$id" >/dev/null 2>&1; then echo "Removed the old velocor-spdk image $(norm "$id" | cut -c1-12)"; fi
done < <(podman images --no-trunc --filter "reference=*/*/velocor-spdk" --format '{{.ID}}' | sort -u)
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
