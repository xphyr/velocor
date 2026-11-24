#!/usr/bin/env bash
# Decide which CI jobs a pull request needs from the files it changes (one path per line on
# stdin). Prints key=value lines for $GITHUB_OUTPUT:
#   go=true      Go code changed: unit, CSI sanity and data plane tests, veloctl, images
#   images=true  something that goes into an image changed: images (build)
# Lint always runs (it also lints and renders the Helm chart), so chart, docs and lint-only
# changes need nothing else. A path not listed below turns everything on: a gap in this list
# must cost CI time, never skip a test. main and tags don't use this; they run everything.
#
# Usage: git diff --name-only BASE...HEAD | hack/ci-changes.sh
set -euo pipefail

go=false images=false all=false
while IFS= read -r f; do
  [[ -z "$f" ]] && continue
  case "$f" in
    # Documentation: nothing to build or test.
    *.md | docs/* | LICENSE) ;;
    # Helm chart and static manifests: covered by lint (helm lint, render, check-pins).
    deploy/charts/* | deploy/k8s/*) ;;
    # Run by the lint job itself.
    hack/check-pins.sh | hack/check-alerts.sh | .golangci.yml | .golangci.yaml) ;;
    # Go code, and the alert rules velod embeds.
    cmd/* | internal/* | go.mod | go.sum | deploy/monitoring/*) go=true ;;
    # Everything else that goes into an image.
    deploy/containers/* | deploy/bootc/* | deploy/quadlets/* | hack/build-qcow2.sh | hack/pin-image.sh) images=true ;;
    # Anything else (the workflow, the Makefile, renovate.json, a new directory): run everything.
    *) all=true ;;
  esac
done

if [[ "$all" == true ]]; then go=true images=true; fi
if [[ "$go" == true ]]; then images=true; fi
echo "go=$go"
echo "images=$images"
