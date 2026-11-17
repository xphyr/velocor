# Design: disk replacement, rebuild and degraded start (P0-1 + P1-8)

Status: proposal, 2026-11-17. Tracks TODO P0-1 and P1-8.

## Problem

- A RAID1 pool that loses a disk runs without redundancy indefinitely. Velocor has no way to
  add a replacement and rebuild it, so the next failure destroys the pool (P0-1).
- A disk that fails while the appliance is down leaves the pool unavailable after boot. SPDK
  waits for every member recorded in the superblock and never starts degraded (P1-8).

## What SPDK v26.09 provides

All references are to `module/bdev/raid/`.

- **Online failure.** When a member fails or is hot-removed, `raid_bdev_fail_base_bdev()` removes
  it, the array stays ONLINE as long as `num_base_bdevs_operational >= min_base_bdevs_operational`
  (1 for a 2-way raid1), and the superblock marks the slot `FAILED`/`MISSING` with a new
  `seq_number` (`raid_bdev_remove_base_bdev_on_quiesced`). The slot is now empty.
- **Starting after an online failure.** At startup the operational count is the number of
  `CONFIGURED` slots in the superblock (`raid_bdev_create_from_sb`). An array that lost a member
  online therefore starts degraded on the next boot.
- **Missing at boot.** A member that vanished while SPDK was down is still `CONFIGURED` in the
  superblock, so the array waits in `configuring` until it appears. There is no RPC to start
  it without the member, in v26.09 or on master (2026-11-17). This is P1-8.
- **Replacement.** `bdev_raid_add_base_bdev {raid_bdev, base_bdev}` on an ONLINE array:
  - fills the first empty slot and starts a **rebuild** process (`raid_bdev_start_rebuild`);
  - is refused while another process runs (`-EPERM`);
  - reports progress in `bdev_raid_get_bdevs` as
    `process: {type: "rebuild", target, progress: {blocks, percent}}`.

  The superblock marks the new member `CONFIGURED` only **after** the rebuild completes
  (`raid_bdev_process_finish_write_sb`).
- **Restart during a rebuild.** The array comes back degraded on the surviving members. The
  half-rebuilt disk isn't a member, so it has to be added again, and the rebuild starts from 0.
- **Proactive replacement.** `bdev_raid_remove_base_bdev {name}` removes a healthy member
  (degrading the array). raid1 has a fixed number of slots and no spare, so replacing a
  still-working disk means remove, then add: there is a window without redundancy.
- **A member returns.** SPDK checks a disk with an older superblock against the array's
  current one (`raid_bdev_examine_sb`). If its UUID still names an empty FAILED/MISSING slot of a
  running array, SPDK **re-adds it and rebuilds it** ("Re-adding bdev ... to raid bdev"; found
  by the integration tests). If another disk has taken the slot, the returning disk is refused.
  It never resumes as an up-to-date member, so stale data is never served.
- **Rebuild throttling.** `bdev_raid_set_options {process_max_bandwidth_mb_sec,
  process_window_size_kb}` is global for the whole target, and a rebuild takes the limit when it
  starts: changing it doesn't affect one already running (found by the integration tests).

## Velocor model

**Pool members.** Replace `Pool.DiskIDs` with members that have a lifecycle. `DiskIDs` stays,
derived from the members, for API compatibility.

```
Member{DiskID, Slot, State}
State: ACTIVE | REBUILDING | FAILED | MISSING | REMOVED
```

**Pool health, derived from SPDK on each reconciler pass:**

| Health | When |
|---|---|
| `ONLINE` | all slots `ACTIVE` |
| `DEGRADED` | the array is online but with fewer operational members than slots and no rebuild running; message names the failed or missing disk |
| `REBUILDING` | a rebuild is running; `rebuild: {disk_id, percent, started_at, rate}` |
| `UNAVAILABLE` | the array is in `configuring` with a member missing (P1-8); needs an operator decision |
| `FAULTED` | fewer members than `min_operational`; data is not accessible |

The new states `REBUILDING` and `UNAVAILABLE` are explicit, because operators need to tell
them apart.

## Operations (API, CLI and UI)

### 1. Replace a disk: `POST /api/v1/pools/{id}/replace {new_disk_id, old_disk_id?}`

Preconditions:

- The pool is ONLINE or DEGRADED, with no rebuild running.
- The new disk is unclaimed, not the boot disk, and at least as large as the member data area
  (`data_offset + data_size`).

Steps:

1. **Wipe the new disk's RAID superblock area and any lvstore signature first.** A disk that
   was once in another Velocor pool would otherwise be matched by SPDK's examine as a member of
   *that* array.
2. If `old_disk_id` names a member that is still present and working (proactive replacement),
   require `allow_degraded: true`, then `bdev_raid_remove_base_bdev` it. Omitted or already
   failed: skip this step.
3. Claim the new disk (create its bdev), then `bdev_raid_add_base_bdev`.
4. Record the member as `REBUILDING`. The old disk becomes `REMOVED`, and is released or wiped
   on request.

The call returns at once with the rebuild state; progress is polled from the pool.

### 2. Start a degraded pool: `POST /api/v1/pools/{id}/start-degraded {confirm: "<pool name>"}`

- Only for an UNAVAILABLE pool whose present members can serve the data (raid1: at least one).
- Needs **SPDK patch 0002** (see below).
- Never automatic. A member that is only slow to appear at boot would otherwise be dropped.
  The appliance shows the pool as UNAVAILABLE with the missing disk's serial number, and the
  operator decides.

### 3. Rebuild speed: `PUT /api/v1/system/rebuild {max_mb_per_sec}`

This maps to `bdev_raid_set_options`. The default is unlimited (SPDK's default). It's a global
setting, which the UI states.

### 4. Release a removed disk: `DELETE /api/v1/pools/{id}/members/{disk_id}`

Wipes the disk's superblock so it can't be confused with a member later, and returns it to the
unclaimed list.

## Reconciler

- Derives member state from `base_bdevs_list` (`is_configured`), the superblock-backed slot
  count, and `process`.
- **Rebuild interrupted by a restart:** the member is `REBUILDING` in the store, but SPDK's array
  doesn't contain it and no process is running. If the disk is present and the pool is online,
  re-add it (the rebuild restarts from 0), and log/notify that it restarted.
- **Rebuild target fails:** SPDK removes it and the process stops. Mark the member `FAILED`;
  the pool goes back to `DEGRADED`.
- **Source fails during rebuild (2-way raid1):** the pool is `FAULTED`. This is the case the
  feature exists to make rare.
- **A removed or replaced disk reappears:** SPDK ignores it (lower `seq_number`). Velocor shows
  it as a "stale pool member" and offers to release it (operation 4). It never re-adds it
  automatically.
- **Never adds a disk on its own.** There is no automatic hot spare in this version (see Later).
- **Never re-attaches a disk that left its pool.** Because SPDK re-adds a returning member and
  rebuilds onto it, a disk that failed would otherwise be trusted again as soon as it reappears
  (a flapping disk over and over). The reconciler doesn't re-create the bdev of a disk whose slot
  failed, that was replaced, removed or dropped by a degraded start, or that the running array
  no longer contains. A disk an UNAVAILABLE pool waits for, and a rebuild target, are still
  attached. To reuse such a disk, release it (which wipes it), then replace with it.

## SPDK patch 0002: start a superblock RAID degraded

New RPC `bdev_raid_start_degraded {name}`. It applies to a raid with a superblock in
`configuring` state where `num_base_bdevs_discovered >= min_base_bdevs_operational`. It:

1. marks every `CONFIGURED` slot that has no discovered bdev as `MISSING`, sets
   `num_base_bdevs_operational = num_base_bdevs_discovered` and increments `seq_number`;
2. writes the superblock to the present members, then configures the array through the
   existing path, which then starts as on any degraded boot.

The missing disk, if it ever returns, carries a lower `seq_number`: like a member removed while
online, it never resumes as an up-to-date member (examine re-adds it to a still-empty slot and
rebuilds it; velod doesn't re-attach it in the first place). The patch carries unit tests, like 0001, and is a candidate for
upstream (Linux md has the same concept in `mdadm --run`).

## Tests

**Fake SPDK** (`internal/spdk/spdktest`) models:

- add/remove of base bdevs;
- a rebuild process with stepped progress;
- superblock slot states and `seq_number`;
- configuring with a missing member;
- `start_degraded`;
- restarts at any point.

**Unit tests** (velod against the fake):

- replace a failed member;
- proactive replace (with and without `allow_degraded`);
- restart mid-rebuild (re-added);
- target failure mid-rebuild;
- refusals: rebuild already running, new disk too small, boot disk, disk in another pool;
- a stale member returning;
- start-degraded (confirmation required; refused when not enough members).

**Integration tests** (real SPDK, aio file disks; hot-removing a member = `bdev_aio_delete`):

- fail a member under write load, then replace it: rebuild completes, and data read back from
  each member alone matches;
- `kill -9` during a throttled rebuild: comes back degraded, re-adds, completes;
- missing member at boot (needs patch 0002): start degraded, then replace;
- the failed disk's drive is still there: velod doesn't re-attach it and SPDK doesn't re-add it;
- a disk dropped by a degraded start returns after its slot was filled: refused by SPDK.

**Lab** (serenity VM, hot-unplug of virtio disks through QEMU's monitor): the same scenarios on
the appliance image, plus a whole-appliance reboot with a disk removed.

## Later (not in this change)

- Hot spares: a pool-level spare disk that the reconciler adds automatically on failure.
- Rebuild scheduling and priority, per-pool throttling (needs SPDK support).
- Scrub before or after rebuild (P2-2), and SMART-driven proactive replacement.
- RAID10 (L-4) and parity RAID (L-3) reuse the same member model.
