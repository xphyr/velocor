#!/usr/bin/env bash
# Print an image reference pinned to the digest its tag points to in the registry now:
#   hack/pin-image.sh registry/org/repo:tag  ->  registry/org/repo:tag@sha256:...
# The digest is that of the exact manifest (or index) the tag resolves to.
# INSECURE_REGISTRY=true allows a plain-HTTP registry (CI builds of pull requests only).
set -euo pipefail
ref="${1:?usage: $0 registry/org/repo:tag}"
[[ "$ref" != *@* ]] || { echo "$ref"; exit 0; }
tls=()
[[ "${INSECURE_REGISTRY:-false}" == true ]] && tls=(--tls-verify=false)
digest="sha256:$(skopeo inspect "${tls[@]}" --raw "docker://$ref" | sha256sum | cut -d' ' -f1)"
echo "$ref@$digest"
