#!/usr/bin/env bash
# Print the reference of one platform's image in a multi-arch image (manifest list or OCI index):
#   hack/platform-image.sh registry/org/repo@sha256:<list> linux/arm64  ->  registry/org/repo@sha256:<instance>
# A reference that is a single image is printed unchanged.
#
# Multi-platform builds pin their base to this rather than to the list: CI's podman 4.9 resolves
# a FROM naming a list by digest for the build host, whatever --platform says (it built v0.2.0's
# arm64 CSI image on the amd64 fedora-minimal, even with --pull=always).
# INSECURE_REGISTRY=true allows a plain-HTTP registry (CI builds of pull requests only).
set -euo pipefail
ref=${1:?usage: $0 <image ref> <os/arch[/variant]>}
platform=${2:?usage: $0 <image ref> <os/arch[/variant]>}
repo=${ref%@*}
[[ "$repo" == "$ref" && "${ref##*/}" == *:* ]] && repo=${ref%:*}   # a tag, not a registry port
tls=()
[[ "${INSECURE_REGISTRY:-false}" == true ]] && tls=(--tls-verify=false)
raw=$(skopeo inspect "${tls[@]}" --raw "docker://$ref")
digest=$(python3 -c '
import json, sys
m = json.loads(sys.argv[1])
want = sys.argv[2].split("/")
if "manifests" not in m:
    print("-")
    sys.exit(0)
for d in m["manifests"]:
    p = d.get("platform", {})
    have = [p.get("os"), p.get("architecture")] + ([p["variant"]] if len(want) > 2 and "variant" in p else [])
    if have == want:
        print(d["digest"])
        sys.exit(0)
sys.exit(f"{sys.argv[3]} has no image for {sys.argv[2]}")
' "$raw" "$platform" "$ref")
[[ "$digest" == - ]] && { echo "$ref"; exit 0; }
echo "$repo@$digest"
