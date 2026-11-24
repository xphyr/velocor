# Design: pool capacity, overcommit and space safety (P0-2)

> Written for the SPDK data plane, which was removed on 2026-11-24: kept for the reasoning. What runs
> now is in [kernel-dataplane.md](kernel-dataplane.md) and [../ARCHITECTURE.md](../ARCHITECTURE.md).

Status: proposal, 2026-11-17. Tracks TODO P0-2.

## What happens today (measured)

- **Lab appliance** (dev-fe45c4d): an 8 GiB RAID1 SSD pool, two 6 GiB thin volumes, written
  through the Linux NVMe/TCP initiator.
- **Integration tests:** a 126 MiB pool, two 256 MiB thin volumes.

What happened:

- Creating 12 GiB of volumes on an 8 GiB pool succeeds. **Nothing checks capacity**, for
  volumes, clones, expansion or snapshots.
- Once the pool's last cluster is allocated, every write that needs a new cluster fails, in
  every thin volume of the pool at once.
  - The host sees a generic **NVMe Internal Error** (status 0x6), which Linux reports as EIO.
  - Filesystems treat EIO as fatal: ext4 and XFS shut down or go read-only, and databases on
    them crash.
- Data already written is intact. Overwriting already-allocated clusters still works, and once
  space is freed new writes succeed again.
- `pool.free_bytes` is updated by the reconciler. Per-volume and per-snapshot allocated space is
  recorded at creation and never updated, so nothing shows who uses the space or how much the
  snapshots hold.

So the damage happens at the moment the pool fills. It affects every host on the pool and looks
like a disk failure. Velocor can't make writes succeed in a full pool. What it can do:

1. not let a pool get there by accepting more than it can hold;
2. see it coming, and say so early;
3. make the end state less destructive (see Later).

## Accounting (always on)

| Quantity | Definition |
|---|---|
| **capacity** C | the volume store's data capacity (`total_data_clusters × cluster_size`) |
| **used** U | `C - free`, from the volume store |
| **allocated** (per volume, clone, snapshot) | `num_allocated_clusters × cluster_size` from `bdev_lvol_get_lvols`, refreshed by the reconciler each pass (one RPC) |
| **committed** K | `Σ size of every volume and clone (thin or thick) + Σ allocated of every snapshot` |

Committed is the worst case: every volume fully written, while every snapshot keeps the
clusters it holds now. Thick volumes are fully allocated already.

Exposed in the API, `veloctl`, the UI and metrics: per pool C, U, K, K/C and the space state;
per volume and snapshot, size and allocated.

## Admission control

Every operation that commits space is checked against the pool before SPDK is asked. That covers
creating a volume, cloning, expanding, taking a snapshot (of a volume or a group) and
CSI-provisioned volumes.

| Operation | ΔK |
|---|---|
| create or clone | new size |
| expand | size increase |
| snapshot | the source volume's current allocated bytes (the clusters the snapshot will hold) |

The operation is refused when:

1. `K + ΔK > ratio × C × (1 - reserve)` (the overcommit limit), or
2. the pool's space state is CRITICAL or FULL, for **any** growth, whatever the ratio.

Deletes are always allowed; they free space. The error states the numbers ("pool fast: 7.2 of
7.6 GiB committed, 1.0x allowed; this volume needs 2 GiB"). CSI returns `RESOURCE_EXHAUSTED`, and
its pool selection skips pools that can't take the volume.

## Per-pool settings

Settable at creation and with `PATCH /pools/{id}`:

| Setting | Default | Meaning |
|---|---|---|
| `max_overcommit` | **1.0** (proposed) | 1.0 = everything committed fits: the pool can't fill from volume growth, only from snapshots holding more than their admission estimate. Raise it (e.g. 2.0) to allow thin overcommit on purpose. |
| `reserve_percent` | 5 | capacity never committed, so snapshots and metadata have room |
| `warning_percent` | 80 | used ≥ this: space state WARNING |
| `critical_percent` | 90 | used ≥ this: CRITICAL, all growth refused |

## Space state

The space state is separate from RAID health, so a full pool doesn't look like a disk failure:

| State | When |
|---|---|
| `OK` | used below the warning threshold |
| `WARNING` | used ≥ warning |
| `CRITICAL` | used ≥ critical; growth refused |
| `FULL` | no free clusters: writes to new space are failing now |

- The pool's overall health shows DEGRADED with a message when space is WARNING, CRITICAL or
  FULL, so `system status` and the UI flag it.
- The reconciler logs every state change. Alerting proper is P1-1.

## Existing pools at upgrade

A pool that's already beyond its limits keeps working, and nothing is changed or deleted. New
growth is refused and the state says why. The operator raises `max_overcommit` deliberately, or
frees space.

## Later (not in this change)

- **ENOSPC instead of EIO.** An SPDK patch could complete out-of-space writes with NVMe
  *Capacity Exceeded* (status 0x81), which Linux reports as ENOSPC. Applications and filesystems
  handle ENOSPC far better than an I/O error. This would be another carried patch (for upstream)
  and would need host testing.
- Per-volume space guarantees (reservations) for thin volumes; SPDK has no such mechanism.
- Automatic snapshot deletion policies.
- Alerts (P1-1).

## Tests

- **Unit:**
  - accounting from a fake lvstore with known cluster counts;
  - every growth path refused or allowed at the right numbers, including CSI;
  - state transitions and health;
  - settings validation;
  - an upgrade with an already overcommitted pool.
- **Integration:** with default settings, creating more than fits is refused.
- **Lab (appliance):** with `max_overcommit` 2.0, fill the pool and watch WARNING → CRITICAL →
  FULL, with growth refused at CRITICAL and messages and metrics shown; free space and it
  recovers.
- **Integration harness:** make `initiator.write` fail when writes fail (`spdk_dd` reports
  success even for failed writes, found while measuring the above).
