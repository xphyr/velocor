# Monitoring and alerts

velod exports Prometheus metrics at `https://<appliance>:8443/metrics`, and Velocor ships a set
of alert rules for them in [`deploy/monitoring/`](../deploy/monitoring/). Point an existing
Prometheus and Alertmanager at the appliance as described below.

Or turn on the appliance's **built-in Prometheus and Alertmanager** (below), which run the same
rules.

**Monitor from outside as well.** Alerts sent by the appliance itself can't report that the
appliance is down, hung or cut off from the network. Route the always-firing `VelocorWatchdog`
alert to a dead man's switch (a heartbeat service that raises an alarm when the heartbeat stops),
and alert on `up{job="velocor"} == 0` from a Prometheus that runs elsewhere.

## Built-in monitoring

Off by default. Turn it on from the System page, or:

```bash
veloctl system monitoring --on      # --off stops it; with neither, shows its state
```

velod then writes the configuration under `/var/lib/velocor-monitoring/config` and starts two
containers that ship with the OS image (pinned by digest, updated with OS upgrades):

- **Prometheus** on `127.0.0.1:19090`: scrapes velod every 30 seconds with its own READONLY
  token (`builtin-prometheus`, replaced each time velod applies the setting), evaluates the
  shipped rules, and keeps 15 days of history, at most 2 GB, in
  `/var/lib/velocor-monitoring/prometheus`.
- **Alertmanager** on `127.0.0.1:19093`, without clustering.

Both listen on localhost only, are limited in memory (768 MB and 256 MB) and CPU weight, and
velod keeps them off the cores SPDK polls on. Their units have no `[Install]` section: velod
starts them at boot when the setting is on. Turning monitoring off stops them and keeps their
data.

The **Alerts** page lists firing alerts (silence them for a while, e.g. during a disk swap),
sets where alerts go (email by SMTP, a webhook in Alertmanager's format, and a heartbeat URL
that receives the watchdog every 5 minutes for a dead man's switch), sends a test alert, and
tunes the rules: each can be turned off or given another delay, and the disk temperature and
wear rules another threshold. Receiver secrets (the SMTP password, webhook and heartbeat URLs)
are kept in velod's database and in files only the containers read; the API never returns them,
and they are never part of the pools' metadata copies.

The overview, pool and volume pages show **history graphs** (IOPS, throughput, average latency,
space) over 1 hour to 15 days, from the built-in Prometheus.

Until monitoring is on, or something else scrapes `/metrics`, the status page warns that no
alerts reach anyone (after velod has been up for 15 minutes).

## Health endpoints

`GET /healthz` (velod is serving) and `GET /readyz` (velod's database and the data plane answer) need
no token and answer only `ok` (200) or `not ready` (503), for load balancers, uptime checks and
container health. The velod quadlet runs `velod healthcheck` every 30 seconds and restarts a
velod that stops answering.

## Scraping

`/metrics` needs a READONLY API token (unless velod runs with `--metrics-public`, which status
reports as a warning):

```bash
veloctl token create --name prometheus --role READONLY
```

Save the token where Prometheus can read it, and the appliance's certificate if it is the
self-signed one (`/var/lib/velocor/tls.crt` on the appliance). Then merge
[`prometheus-scrape.example.yml`](../deploy/monitoring/prometheus-scrape.example.yml) into
`prometheus.yml` and copy [`velocor-alerts.rules.yml`](../deploy/monitoring/velocor-alerts.rules.yml)
next to it. The rules expect the scrape job to be named `velocor`.

A 30-second scrape interval is enough; each scrape reads the database and asks SPDK for its I/O
counters once.

### OpenShift and the Prometheus Operator

Wrap the rules file in a `PrometheusRule` (for OpenShift's user-workload monitoring, in a
namespace where it is enabled):

```bash
yq '{"apiVersion": "monitoring.coreos.com/v1", "kind": "PrometheusRule",
     "metadata": {"name": "velocor-alerts", "labels": {"role": "alert-rules"}},
     "spec": {"groups": .groups}}' deploy/monitoring/velocor-alerts.rules.yml > velocor-prometheusrule.yaml
```

Scraping an appliance outside the cluster takes a `ScrapeConfig` (or a `Service` without a
selector plus an `Endpoints` and a `ServiceMonitor`) with `job: velocor`, the token as a
`Secret`, and the appliance's CA.

## Alerts

| Alert | Severity | Fires when |
|---|---|---|
| `VelocorDown` | critical | The appliance's `/metrics` can't be scraped for 2 minutes |
| `VelocorDataPlaneDown` | critical | The data plane doesn't answer velod for 2 minutes |
| `VelocorPoolFaulted` | critical | A pool is FAULTED, OFFLINE or UNAVAILABLE (not served) |
| `VelocorPoolDegraded` | critical | A pool has lost redundancy for 5 minutes |
| `VelocorPoolSpaceCritical` | critical | A pool is at CRITICAL: new volumes and growth are refused |
| `VelocorPoolFull` | critical | A pool is FULL: writes to new space fail on hosts |
| `VelocorDiskFailed` | critical | A disk in a pool is FAULTED, OFFLINE or UNAVAILABLE |
| `VelocorDiskSmartCritical` | critical | A disk reports a SMART critical warning |
| `VelocorDiskSpareLow` | critical | An NVMe drive's spare capacity is at or below its own threshold |
| `VelocorScrubFoundLostData` | critical | A scrub found blocks no copy could supply (data lost there) |
| `VelocorScrubRepairFailed` | critical | A scrub couldn't rewrite a damaged copy (the member was failed) |
| `VelocorPoolRebuilding` | warning | A pool is rebuilding (no redundancy until it finishes) |
| `VelocorRebuildStalled` | warning | A rebuild has made no progress for two hours |
| `VelocorPoolDurabilityAtRisk` | warning | A pool may lose acknowledged writes on power loss (P0-4) |
| `VelocorPoolMetadataCopyFailing` | warning | The on-pool copy of the database can't be written for 30 minutes |
| `VelocorScrubRepaired` | warning | The current or last scrub rewrote unreadable copies or found differing ones |
| `VelocorScrubOverdue` | warning | A pool hasn't completed a scheduled scrub for twice the interval |
| `VelocorPoolSpaceWarning` | warning | A pool is at WARNING for 10 minutes |
| `VelocorPoolFillsWithin24h` | warning | A pool over 50% used will be full within a day at the rate of the last 6 hours |
| `VelocorPoolThinMetadataFilling` | warning | A pool's thin metadata (kernel data plane) is over 80% full for 10 minutes: velod grows it from half full and couldn't |
| `VelocorPoolThinMetadataFull` | critical | A pool's thin metadata is over 95% full: writes to new space are about to fail |
| `VelocorPoolDataUnreadable` | critical | Part of a pool can be read from no disk: bad blocks are recorded on every copy, the data there is lost |
| `VelocorPoolMemberBadBlocks` | warning | A pool member has bad blocks recorded for 10 minutes: the pool has a copy fewer there until the disk is replaced |
| `VelocorDiskMediaErrors` | warning | A disk reported new media errors in the last hour |
| `VelocorDiskWornOut` | warning | An NVMe drive is 90% through its rated endurance |
| `VelocorDiskHot` | warning | A disk is above 70 °C for 15 minutes |
| `VelocorExportUnhealthy` | warning | An enabled export isn't ONLINE for 5 minutes |
| `VelocorVolumeUnhealthy` | warning | A volume isn't ONLINE for 5 minutes |
| `VelocorReconcilerStalled` | warning | The reconciler hasn't completed a pass for 10 minutes |
| `VelocorReconcilerFailing` | warning | Reconciler passes keep failing for 15 minutes |
| `VelocorUnmanagedSPDKObjects` | warning | SPDK holds objects no Velocor record accounts for (P1-5) |
| `VelocorBackupMissing` | warning | No hourly database backup for 3 hours, or no daily one for 2 days |
| `VelocorWatchdog` | none | Always: route it to a dead man's switch |

The rules' unit tests are in [`velocor-alerts.test.yml`](../deploy/monitoring/velocor-alerts.test.yml)
(`make check-alerts` runs them with promtool).

## Metrics

Health metrics carry the current state as a label with the value 1, for example
`velocor_pool_health{pool="p1",status="DEGRADED"} 1`. When the state changes, the old series
ends and a new one starts.

| Metric | Labels | Meaning |
|---|---|---|
| `velocor_up` | | velod answers (always 1 when scraped) |
| `velocor_build_info` | `version` | velod's version |
| `velocor_dataplane_up` | | The data plane answers velod |
| `velocor_status_warnings` | | Warnings on the status page |
| `velocor_reconciler_last_run_timestamp_seconds` | | Last completed reconciler pass |
| `velocor_reconciler_last_run_failed` | | The last pass ended with an error |
| `velocor_reconciler_degraded_resources` | | Resources reported as degraded |
| `velocor_reconciler_drift_repaired_total` | | Resources repaired so far |
| `velocor_unmanaged_objects` | `kind` | Objects in the data plane no record accounts for |
| `velocor_backup_last_timestamp_seconds` | `kind` | Newest database backup of a kind |
| `velocor_pool_health` | `pool`, `status` | Pool health |
| `velocor_pool_members` | `pool`, `state` | RAID members by state (ACTIVE, REBUILDING, FAILED, MISSING) |
| `velocor_pool_member_rebuild_ratio` | `pool`, `slot`, `disk` | Rebuild progress, 0–1 (while rebuilding) |
| `velocor_pool_member_bad_block_bytes` | `pool`, `slot`, `disk` | Bad blocks recorded on an active member (kernel data plane: md's bad-block log) |
| `velocor_pool_unreadable_bytes` | `pool` | How much of the pool no disk can supply (bad blocks on every copy): lost data |
| `velocor_pool_durability` | `pool`, `durability` | DURABLE, AT_RISK or UNKNOWN |
| `velocor_pool_space_state` | `pool`, `state` | 0 OK, 1 WARNING, 2 CRITICAL, 3 FULL |
| `velocor_pool_used_ratio` | `pool` | Allocated fraction of the volume store |
| `velocor_pool_thin_metadata_used_ratio` | `pool` | How full the thin pool's metadata is (kernel data plane only) |
| `velocor_pool_total_bytes`, `velocor_pool_free_bytes` | `pool`, `raid` | Capacity and free space |
| `velocor_pool_committed_bytes`, `velocor_pool_available_bytes` | `pool` | Space promised, and how much more the space policy accepts |
| `velocor_pool_metadata_copy_failed` | `pool` | The on-pool metadata copy failed on the last write |
| `velocor_pool_metadata_copy_timestamp_seconds` | `pool` | When it was last written |
| `velocor_pool_deleted_volumes`, `velocor_pool_deleted_volume_bytes` | `pool` | Deleted volumes kept until purged, and the space they hold |
| `velocor_disk_health` | `disk`, `type`, `pool`, `status` | Disk health |
| `velocor_disk_smart_critical_warning` | `disk`, `type`, `pool` | SMART critical warning |
| `velocor_disk_media_errors_total` | `disk`, `type`, `pool` | Media and data-integrity errors |
| `velocor_disk_unsafe_shutdowns_total` | `disk`, `type`, `pool` | Unsafe shutdowns the drive counted |
| `velocor_disk_available_spare_ratio`, `velocor_disk_available_spare_threshold_ratio` | `disk`, `type`, `pool` | NVMe spare capacity and its threshold |
| `velocor_disk_wear_ratio` | `disk`, `type`, `pool` | NVMe wear (percentage used) |
| `velocor_disk_temperature_celsius` | `disk`, `type` | Temperature (0 when unknown) |
| `velocor_disk_write_cache_error` | `disk`, `type`, `pool` | The write-cache policy couldn't be applied |
| `velocor_volume_health` | `volume`, `pool`, `status` | Volume health |
| `velocor_volume_size_bytes`, `velocor_volume_allocated_bytes` | `volume`, `pool` | Provisioned size and space held |
| `velocor_snapshot_allocated_bytes` | `snapshot`, `pool` | Space a snapshot holds |
| `velocor_export_health` | `export`, `volume`, `protocol`, `status` | Export health |
| `velocor_export_disabled` | `export`, `volume`, `protocol` | Export disabled |
| `velocor_{pool,volume,disk}_{read,write}_ops_total` | `pool` / `volume`, `pool` / `disk`, `pool` | Completed I/Os |
| `velocor_{pool,volume,disk}_{read,written,unmapped}_bytes_total` | as above | Bytes read, written, unmapped |
| `velocor_{pool,volume,disk}_{read,write}_seconds_total` | as above | Time spent on I/O; `rate(..._read_seconds_total) / rate(..._read_ops_total)` is the average read latency |
| `velocor_device_{read,write}_ops_total` | `device` | Raw per-device counters for every data plane device |
