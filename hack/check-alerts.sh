#!/usr/bin/env bash
# Check the shipped alert rules (deploy/monitoring) with promtool: syntax and the unit tests in
# velocor-alerts.test.yml. Runs promtool from the Prometheus image, so nothing needs installing.
#
# Usage: hack/check-alerts.sh            (CONTAINER=docker on hosts without podman)
set -euo pipefail
cd "$(dirname "$0")/.."
CONTAINER=${CONTAINER:-podman}
IMAGE=docker.io/prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996
vol="$PWD/deploy/monitoring:/m:ro"
[[ "$CONTAINER" == podman ]] && vol+=",Z"

run() { "$CONTAINER" run --rm -v "$vol" -w /m --entrypoint promtool "$IMAGE" "$@"; }
run check rules velocor-alerts.rules.yml
run test rules velocor-alerts.test.yml
