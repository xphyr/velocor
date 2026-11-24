# Design: metadata copies, backups and pool import (P0-3)

> Written for the SPDK data plane, which was removed on 2026-11-24: kept for the reasoning. What runs
> now is in [kernel-dataplane.md](kernel-dataplane.md) and [../ARCHITECTURE.md](../ARCHITECTURE.md).

Status: implemented and lab-tested 2026-11-17. Tracks TODO P0-3.

## What happens today

velod's bbolt database (`/var/lib/velocor/velocor.db`, on the boot disk) is the only record of:

- volume and snapshot names, labels, QoS and filter policies;
- exports, hosts, initiator groups and portals;
- which disks form which pool, and each pool's space policy;
- users and API tokens.

If it's lost (boot disk failure, a reinstall, corruption):

- **The data is intact.** The reconciler never deletes SPDK objects it doesn't know about.
  Exports even keep serving until spdk_tgt restarts.
- After a restart nothing comes back. No disk is claimed, so no RAID assembles and nothing is
  exported. Velocor has **no recovery path**: neither import, nor restore, nor backup.

What the pool disks already carry (written by SPDK), and so survive a lost database:

| Item | Where |
|---|---|
| RAID superblock: name `raid_<pool-id>`, UUID, members' bdev UUIDs | start of every member (`SPDKRAID`) |
| Volume store name `lvs_<pool-id>` | blobstore super block |
| Every volume, snapshot and clone under its velod ID, with size, thin/thick and snapshot/clone parentage | blobstore metadata |

That is a recoverable skeleton, but without names, labels, exports or access control.

Measured on the lab appliance (dev-b083815), for writing into a pool:

- SPDK v26.09 has no RPC to write data into a bdev, and no lvol xattr RPC.
- **ublk is the chosen path** (decided 2026-11-17; no NBD fallback). SPDK's ublk target
  exposes an lvol as `/dev/ublkbN`. Our SPDK is now built `--with-ublk`, and the Fedora kernel
  ships `ublk_drv`. Tested on the lab appliance with a ublk-enabled spdk_tgt:
  - direct-I/O writes read back identically through a new ublk device;
  - the data survives an SPDK restart (after `ublk_create_target` again);
  - **if spdk_tgt dies while a ublk device is open, I/O on it hangs** in the kernel (D state:
    20 minutes in the test, unkillable) until a restarted SPDK takes the device over with
    `ublk_recover_disk`. Recovery worked: the stuck I/O completed, the device stopped cleanly,
    and its ID could be reused.
- **Use direct I/O:** NBD, also tested, works too, but buffered reads across sessions
  returned stale page-cache data. The same applies to any kernel block device, so the writer
  always uses `O_DIRECT` plus a flush.

## Proposal

### 1. A metadata copy on every pool (highest value)

- Each pool gets a small **thick** lvol, `velocor-meta` (8 MiB). It counts as committed
  space, and is hidden from volume lists and the CSI driver.
- velod writes the pool's **manifest** into it. The manifest is everything needed to bring that
  pool back on any appliance:
  - the pool record, including its space policy;
  - member disk identities (serial, model, size, slot);
  - volumes, snapshots, group snapshots and clones, with names, labels, QoS and filter policies;
  - the exports of those volumes, plus the hosts, initiator groups and portals they reference;
  - the node ID, a generation number, a timestamp and the velod version.
- **No users, tokens or other node secrets** go into the manifest.
- **Format:** two slots (A/B). Each slot is a header (magic `VELOMETA`, format version,
  generation, length, SHA-256) followed by the JSON. A write goes to the older slot, so a torn
  write leaves the previous generation valid. A read takes the newest slot with a valid
  checksum.
- **When:** after any change that affects the pool's manifest, debounced (at most every 10 s),
  and confirmed by the reconciler: if the manifest's hash differs from the last one written, it
  is written again.
- **How:** `ublk_create_target` (once per spdk_tgt start; "busy" means it exists), then
  `ublk_start_disk` on a free ID from a dedicated range (serialized), an `O_DIRECT` write and
  flush, and `ublk_stop_disk`.
- **Protecting velod from the hang.** The device I/O runs in a short-lived helper process
  (`velod meta-io`), never in velod itself, so a hang can't wedge the control plane.
  - The ublk ID in use is recorded in `/run/velocor` before the device is started.
  - After an spdk_tgt restart, and at velod start, any recorded or left-over device
    (`/dev/ublkc*`) is taken over with `ublk_recover_disk` and stopped. That releases a stuck
    helper.
  - The interrupted write is retried whole; the A/B slots keep the previous generation valid
    meanwhile.
- **If writing fails**, the pool shows a "metadata copy is stale" warning. It never blocks I/O.

### 2. Database backups

- **Local snapshots:** bbolt `Tx.WriteTo` copies kept on the boot disk in
  `/var/lib/velocor/backups/`. Hourly for 24 hours, daily for 7 days, and one before every OS
  upgrade and every restore. These protect against corruption and mistakes, not against losing
  the boot disk (item 1 covers that).
- **Download:** `GET /api/v1/system/backup` (admin only; the file contains password hashes and
  the JWT secret), `veloctl system backup -o file`.
- **Restore:** `POST /api/v1/system/restore` (upload, validated, the current database backed up
  first, swapped, velod restarts), `veloctl system restore file`.
- **Later:** automatic off-box shipping (S3-compatible / HTTP).

### 3. Pool import (recovery)

**Detection.** The hardware scan reads the `SPDKRAID` superblock of unclaimed disks through the
kernel, before SPDK owns them. It then shows "member of Velocor pool `<id>` (RAID1, slot 1 of
2) — importable", and groups the disks of each pool.

**`POST /api/v1/pools:import {pool_id}`** (`veloctl pool import`, and an Import button in the
UI) does the following:

1. Claims the member disks without wiping them. They carry Velocor signatures, so this is an
   explicit force.
2. Waits for SPDK to assemble the RAID and load the volume store.
3. Reads the manifest.
4. Recreates the records with their **original IDs**, refusing on ID or name conflicts.
5. Restores exports. Those whose portals' IP addresses don't exist on this appliance (moved
   disks) are restored disabled, with a message to fix their portals.

If the manifest is missing or corrupt, it falls back to the **skeleton** from SPDK: pool,
volumes and snapshots named by their IDs, with sizes and parentage, and no exports. It says
clearly which parts were recovered.

**A pool missing a member** can still be imported if its RAID can run degraded, through the
existing start-degraded path (P1-8).

### What this covers

| Lost | Recovered by |
|---|---|
| boot disk / reinstall | pool import from the on-pool manifest (no users or tokens: set up again) |
| corrupted database | restore from a local backup, or pool import |
| a bad change | restore from a local backup |
| a pool moved to another appliance | pool import (exports restored disabled if its portals don't fit) |

## Tests

- **Unit:**
  - manifest encode/decode and A/B slots, including a torn write (the older generation is kept)
    and a corrupt checksum;
  - deciding when to write, through a test double for the ublk writer;
  - import from a manifest, from the skeleton, and with conflicts;
  - backup and restore round trips;
  - signature detection from a fake superblock.
- **Integration (real SPDK):** the skeleton import, which needs no NBD. Rootless containers
  can't use ublk, so the manifest writer is behind an interface.
- **Lab (appliance):**
  - delete the database, then import: names, exports and access come back and a host reads its
    data;
  - move the disks to a fresh appliance VM, import, and check exports come back disabled with
    their portals flagged.

## Implementation notes (2026-11-17)

Where the implementation differs from the proposal above:

- **When copies are written:** on every reconciler pass (15 s by default) whose manifest
  digest differs from the last one written, not through a separate debounce. The first pass
  after velod starts reads the pool's slots, so a database restored from an older backup
  continues above the generation already on the pool.
- **The commit limit** leaves out the 8 MiB lvol (`LimitBytes` subtracts it from the volume
  store's size) rather than counting it as committed.
- **CHAP secrets** are stripped from the copy too. An imported export whose initiator group
  uses CHAP comes back disabled until the secrets are set again. (CHAP isn't enforced yet:
  TODO P2-1.)
- **API:** `GET|POST /api/v1/pool-imports`, not `POST /pools:import`. Detection runs on demand
  (the list reads the superblocks of unclaimed disks), not as a disk attribute.
- **Conflicts and failures:** checked after the manifest is read, because the manifest needs
  the array assembled. A failed import undoes only the database records. The disks stay
  claimed and the array assembled, and the candidate list includes such arrays, so a retry
  (e.g. under another name) resumes. SPDK's state isn't undone: detaching RAID1 members one by
  one marks the first as failed in the others' superblocks, and `bdev_raid_delete` under a
  loaded volume store leaves the RAID half-deleted.
- **Skeleton parentage:** SPDK keeps no creation order, so a snapshot that has both its volume
  and clones as children can't be told apart. The import picks one (deterministically), records
  the others as clones, and says so in its warnings.
- **Disabled exports:** exports gained `disabled` / `disabled_reason`. The reconciler removes
  their SPDK objects and reports them as needing attention. `PATCH /exports/{id}
  {"enabled": true}` serves them again.
- **Moving an export:** an export restored disabled because its portal's address isn't on the
  new appliance is moved to a local portal with `PATCH /exports/{id} {"portal_ids": [...],
  "enabled": true}`. Portals themselves can't be readdressed. The NVMe listeners of dropped
  portals are removed.
- **Disk settings:** the member disks' power-loss-protected flag and write-cache policy are
  in the manifest, and an import restores them.
- **ublk hygiene (from the lab run):** the ublk target is created once per spdk_tgt. Before
  stopping the device, velod waits until it has been idle for 300 ms or more, because the
  kernel's and udev's own probe reads would otherwise fail and log I/O errors. A udev rule
  (`59-velocor-ublk.rules`) keeps udev from probing `ublkb100`. After `ublk_recover_disk`,
  velod waits for the recovery to finish before stopping the device.
