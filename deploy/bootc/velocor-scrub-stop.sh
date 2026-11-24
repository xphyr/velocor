#!/bin/bash
# Stop md repair scrubs as the appliance shuts down (velocor-scrub-stop.service, after velod).
#
# An array shut down during a `repair` comes back "not clean" and resyncs at its next start, even
# though every write was finished (`check`, an idle array and one under writes come back clean).
# Nothing is lost by stopping it: velod's record of the scrub says it was running, so it resumes
# from there after the boot. velod has stopped by now, or it would take the scrub for finished.
set -euo pipefail

for action in /sys/block/md*/md/sync_action; do
  [[ -e "$action" && "$(<"$action")" == repair ]] || continue
  array="$(cut -d/ -f4 <<<"$action")"
  echo "Stopping the repair scrub of ${array} for the shutdown"
  echo idle >"$action" || { echo "can't stop the scrub of ${array}" >&2; continue; }
  for _ in {1..50}; do
    [[ "$(<"$action")" == repair ]] || break
    sleep 0.1
  done
  [[ "$(<"$action")" != repair ]] || echo "the scrub of ${array} is still running" >&2
done
