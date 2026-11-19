# Velocor TODO and review status

This file tracks everything found by the two code reviews (2026-11-15 and 2026-11-16): the
work still open, ranked by risk to data, how fixes are verified, and what has been done.

- **P0**: can lose acknowledged data, or a pool, in plausible conditions. Fix before anyone stores real data.
- **P1**: outage, lock-out, or silent degradation an operator can't see. Fix before production.
- **P2**: hardening and operability.
- "Verified" means demonstrated against a real `spdk_tgt` by the integration harness (see
  [Verification](#verification)). An item is done when it is fixed **and** covered by a test.

---

## Open work

### P0: data loss

- [ ] **P0-1. Disks can never be replaced, so a degraded mirror stays degraded until the next failure kills it.**
  There is no API, reconciler path or UI to add a replacement member to a RAID and rebuild it.
  A RAID1/RAID10 pool that loses a disk runs without redundancy indefinitely, and the second
  failure destroys the pool. For a storage product this is the most important missing feature.
  SPDK provides `bdev_raid_add_base_bdev` / `bdev_raid_remove_base_bdev`.
  *Needed:* "replace disk" (claim a new disk, add it to the array, track rebuild progress),
  surfaced as a pool state (`REBUILDING n%`) and as an alert. See also P1-8.
  **In progress (2026-11-17):** design in `docs/design/p0-1-disk-replacement.md` (SPDK v26.09
  raid facts, member model, replace / start-degraded / throttle APIs, reconciler cases, tests).
  Needs SPDK patch 0002 (`bdev_raid_start_degraded`) for P1-8.
  - [x] Design approved (manual-only degraded start, `allow_degraded` for proactive
    replacement, no hot spares yet, unlimited rebuild speed by default, carry patch 0002).
  - [x] SPDK patch `0002-raid-start-degraded.patch`: `raid_bdev_start_degraded()` + RPC +
    schema/CLI/doc entries + `test_raid_start_degraded`; all raid and scsi unit tests pass
    (`make spdk-unit-tests`, which now also maps `module/<kind>/<name>` to its tests).
  - [x] Go SPDK client (`BdevRaidAddBaseBdev`/`RemoveBaseBdev`/`StartDegraded`/`SetOptions`,
    `num_base_bdevs_operational`, rebuild `process`) + fake SPDK RAID model rewritten
    (`spdktest/raid.go`: per-member superblock copies with slot states and sequence numbers,
    online failure, rebuild with progress, start-degraded, stale members, restarts; tests in
    `raid_test.go`). Found on the way: the reconciler reports a pool ONLINE when its RAID is
    online but degraded (it never read the operational count), so a mirror that loses a disk
    while running still shows ONLINE; fixed by the member model below.
  - [x] Store: `Pool.Members` (slot states ACTIVE/REBUILDING/FAILED/MISSING, rebuild %),
    `DetachedDiskIDs` (left the RAID, bound until released), `ReplacePoolMember`,
    `ReleasePoolDisk`, node `Settings` (rebuild speed); new pool health REBUILDING and
    UNAVAILABLE (no new volumes on UNAVAILABLE).
  - [x] Reconciler: member states and health from SPDK (fixes "degraded mirror shows ONLINE"),
    re-adds a rebuild target after a restart (rebuild restarts from 0), re-applies the rebuild
    speed limit when SPDK lost it (read-only check, no steady-state writes).
  - [x] API: `POST /pools/{id}/replace` (wipes and claims an unclaimed new disk; refuses RAID0,
    a running rebuild, a disk of another pool or one SPDK assembled into a stale array;
    `allow_degraded` for a working disk), `POST /pools/{id}/start-degraded` (confirm = pool
    name), `DELETE /pools/{id}/disks/{disk_id}` (release + wipe), `GET|PUT /system/rebuild`.
    Disk claim now also refuses disks with SPDK RAID/volume-store signatures unless forced;
    `hardware.WipeMetadata` zeroes the first 32 MiB and last 8 MiB (exclusive open).
  - [x] CLI: `veloctl pool get|replace|start-degraded|release-disk`, `system rebuild`
    (tested against the API). UI: pool page by slot (state, rebuild %), Replace per slot, Start
    degraded (typed confirmation), detached disks with Release; System > Rebuild speed (checked
    in headless Chrome).
  - [x] Real-SPDK integration tests (`internal/integration/rebuild_test.go`): replace a member
    that failed online, rebuilt copy alone serves data written before and during the degraded
    period; missing at boot -> UNAVAILABLE -> start degraded (patch 0002) -> replace -> restart,
    old disk returning stays out; `kill -9` mid-rebuild -> rebuild restarted and completed.
    Found on real SPDK: (1) **SPDK re-adds a returning member** to its still-empty slot and
    rebuilds onto it (`raid_bdev_examine_sb` "Re-adding bdev"), so velod must not re-attach a
    disk that left its pool, or a failed disk would silently be trusted again: the reconciler
    now leaves such disks detached (also one the running array lost since the last pass); to
    reuse one, release (wipe) then replace. (2) The rebuild speed limit applies only to
    rebuilds that start after it is set.
  - [x] Serenity lab, appliance image dev-7309a18 (QEMU VM: AHCI SATA, virtio-SCSI, NVMe on
    hot-plug PCIe ports, vIOMMU; data checked through a loopback NVMe/TCP export), 2026-11-17:
    - virtio-SCSI (io_uring, as SATA SSDs): hot-unplug under a write load (0 write errors) ->
      DEGRADED -> replace with an unclaimed disk (wiped, claimed) -> rebuilt (8 GiB, ~55 s)
      -> original unplugged, data read back from the rebuilt disk alone.
    - SATA (AHCI): rebooted without one member -> UNAVAILABLE -> start-degraded (patch 0002)
      -> data intact -> release a detached SSD (wiped) and replace with it -> rebuilt, data intact.
    - NVMe (vfio-pci, SPDK's NVMe driver; the first time this path ever ran): claim, mirror,
      hot-unplug under writes (0 errors; vfio relays the removal to SPDK) -> DEGRADED -> replace
      -> rebuilt (~20 s) -> original unplugged, data read back from the rebuilt drive alone.
    - Reboot: SSD and SATA pools reassemble with data intact; disks that left their pool and
      came back (ssd0/ssd1, nvme0/nvme1) stay detached. **The NVMe pool did not come back**,
      which found the next two bugs.
  - [x] **NVMe drives weren't re-bound after a reboot** (all NVMe pools unavailable after every
    reboot): bindings don't survive a reboot, but `AttachDisk` trusted the stored VFIO_PCI state
    and skipped the bind. It now checks the actual driver (`hardware.PCIDriver`).
  - [x] **NVMe namespaces without NGUID/UUID got a random bdev UUID at every start**, so RAID
    superblocks (which match members by bdev UUID) never matched again: `spdk.json` now sets
    `bdev_nvme_set_options generate_uuids=true` (stable UUID from serial + NSID). The lab's QEMU
    namespaces now have fixed UUIDs, like real drives.
  - [x] **Intel IOMMU off by default on Fedora**: velod refused every NVMe claim on Intel
    servers ("no IOMMU is available"). The OS image's kargs.d now adds `intel_iommu=on iommu=pt`.
  - [x] NVMe re-run on dev-fe45c4d (fresh VM; IOMMU now from the image's kargs.d): clean reboot
    -> pool ONLINE, data intact; member hot-unplugged under writes (0 errors) -> replace ->
    rebuilt -> reboot -> ONLINE with the new member, the returning failed drive stays on the
    kernel driver (detached), data intact; rebooted without a member -> UNAVAILABLE ->
    start-degraded -> data intact.
  - [ ] Real hardware before release: hot-swap bays (SATA/SAS and NVMe surprise removal),
    consumer NVMe without NGUID/UUID (generate_uuids path), a drive failing with I/O errors
    rather than disappearing.

- [x] **P0-2. Thin pools can be overcommitted without limit or warning.**
  Volume and clone creation never check pool capacity. When the pool fills, every thin and
  snapshot-backed volume in it starts failing writes at once, which typically corrupts host
  filesystems and databases. Snapshots keep old clusters alive, and nothing shows how much
  space they hold.
  *Needed:* an overcommit ratio per pool (refuse creation beyond it), a reserve so snapshots
  and metadata can't take the last free space, warning/critical thresholds as pool health and
  metrics, and per-volume/snapshot allocated space.
  **In progress (2026-11-17):** measured on the lab appliance and real SPDK: 12 GiB of thin
  volumes accepted on an 8 GiB pool; once full, every write needing a new cluster fails in all
  thin volumes at once with NVMe Internal Error (EIO on Linux: filesystems shut down); data
  already written stays intact and writes resume once space is freed. Also found: the
  integration harness's `spdk_dd` reports success for failed writes. Design:
  `docs/design/p0-2-capacity.md` (accounting incl. snapshots, admission control on every growth
  path, per-pool max_overcommit/reserve/thresholds, space state separate from RAID health).
  Decisions (2026-11-17): default max_overcommit 1.0, reserve 5%, warning 80%, critical 90%;
  the ENOSPC SPDK patch is a follow-up (L-6).
  - [x] Store: `SpacePolicy` per pool, committed/available bytes, space state OK / WARNING /
    CRITICAL / FULL (pure functions in `store/capacity.go`); `ErrInsufficientCapacity` = 507.
  - [x] Reconciler: per-volume/snapshot allocated bytes refreshed each pass (one read-only
    `bdev_lvol_get_lvols`), pool committed/available/state, state changes logged, non-OK
    pools flagged in system status; RAID health untouched.
  - [x] API admission on create, clone, expand, snapshot and group snapshot (per-object
    reservations, so concurrent requests can't overcommit together; race-tested), space
    policy at pool creation and `PATCH /pools/{id}` (merges), metrics
    (`velocor_pool_committed_bytes`, `_available_bytes`, `_used_ratio`, `_space_state`,
    `velocor_volume_allocated_bytes`, `velocor_snapshot_allocated_bytes`).
  - [x] CSI picks pools by available bytes (falls back to free bytes from a velod without
    accounting) and skips pools that don't serve I/O; 507 -> RESOURCE_EXHAUSTED.
  - [x] CLI (`pool list/get` space columns, `pool create`/`pool set-space` policy flags,
    `volume list` allocated) and UI (space pill, committed/available, policy dialog).
  - [x] Integration harness: writes are verified by reading back (spdk_dd reports success
    for failed writes).
  - [x] Real-SPDK integration test (`TestPoolSpaceLifecycle`): a volume over the default
    limit refused (507); with 3x overcommit, filled through exports: OK 6% -> WARNING 83% ->
    CRITICAL 95% (new volume and snapshot refused) -> the write to new space fails (FULL);
    everything written before is intact; after deleting a volume the pool recovers and writes
    work again. Harness: spdk_dd now runs with queue depth 32, no I/O retries and a 120 s
    limit.
  - [x] Lab test on the appliance (dev-b083815, 8 GiB SSD mirror, kernel NVMe/TCP host,
    2026-11-17): at the default policy a 6 GiB volume is accepted and a further 2 GiB refused
    (507, "6.0 GiB committed of 7.6 GiB allowed"); with 2x overcommit and 12 GiB of thin
    volumes filled through the kernel host: OK -> WARNING at 82% -> CRITICAL at 91% (new
    volume and snapshot refused) -> FULL at 100% (the next write gets EIO); every transition
    logged, system status DEGRADED naming the pool, metrics and UI correct (full pill, Create
    volume disabled); deleting a volume brings the pool back to OK, all 6 GiB of the other
    volume intact and writable.
  P0-2 is done, apart from L-6 (ENOSPC instead of EIO).

- [x] **P0-3. The metadata database is a single point of failure, with no backup or recovery.**
  `/var/lib/velocor/velocor.db` is the only record of which volume is which, who may access it,
  snapshot lineage and CBT order. If it is lost, the data is still on the pool disks but
  nothing can name or export it.
  *Needed, in order of value:*
  1. A copy of the metadata **on the pool disks themselves** (a small reserved lvol per pool,
     rewritten on every change), so a pool's disks carry what's needed to bring it back.
  2. Scheduled `bbolt` backups (`Tx.WriteTo`) to a second location, plus `veloctl` backup/restore.
  3. A recovery command that rebuilds pools/volumes/snapshots from SPDK (lvol names are
     resource IDs and SSD bdev UUIDs are derived from disk IDs, so the skeleton is
     recoverable) and from the on-pool copy.
  **In progress (2026-11-17):** today a lost database leaves the data intact (the reconciler
  never deletes unknown SPDK objects; exports even serve until spdk_tgt restarts) but nothing
  comes back after a restart and there is no recovery path. SPDK has no RPC to write into a
  bdev. Decisions: accounts stay out of pool copies (DB backups carry them); **ublk only** (no NBD
  fallback); exports restored enabled when their portals exist on the appliance; backups hourly
  x24, daily x7, before upgrade/restore. ublk tested on the lab appliance with a
  `--with-ublk` spdk_tgt: direct-I/O data persists across devices and SPDK restarts; if spdk_tgt
  dies with a ublk device open, its I/O hangs (D state) until a restarted SPDK runs
  `ublk_recover_disk`, which worked; so the device I/O runs in a helper process and velod
  recovers stale devices. Design: `docs/design/p0-3-metadata-recovery.md` (per-pool
  `velocor-meta` lvol with an A/B-slot manifest written through ublk, local bbolt backups +
  download/restore, pool import from the manifest or the SPDK skeleton, importable-pool
  detection from RAID superblocks).
  - [x] SPDK built `--with-ublk` (Containerfile.spdk).
  - [x] Manifest format (`internal/meta`): A/B slots with header + SHA-256, generation, digest
    that ignores live figures; no accounts, tokens or CHAP secrets (CHAP secrets stripped: an
    import disables exports whose groups use CHAP until they are set again).
  - [x] ublk writer (`meta.UblkDevice`): I/O in the `velod meta-io` helper (O_DIRECT + fsync),
    ublk ID 100 reserved, in-flight record in `/run/velocor/meta-ublk.json`, stale devices
    recovered (`ublk_recover_disk`) and stopped before each use and every reconcile pass.
    `--meta-copies=false` / `VELOCOR_META_COPIES=false` turns it off.
  - [x] Reconciler sync: thick 8 MiB `velocor-meta` lvol per pool (created by the reconciler,
    excluded from the commit limit), a new generation written whenever the manifest's digest
    changes; never below the generation already on the pool (a restored older database
    carries on above it); a failed write flags the pool ("out of date") and is retried.
  - [x] Local backups (`internal/backup`): hourly x24, daily x7, before upgrade (`POST
    /system/upgrade`) and restore, manual x5, in `/var/lib/velocor/backups/`; `GET
    /system/backup[?name=]`, `GET|POST /system/backups`, `POST /system/restore` (validated,
    current DB backed up, staged as `velocor.db.restore`, applied at the next velod start;
    velod exits to be restarted). veloctl `system backup|backups|restore`; UI section.
  - [x] Pool import: `GET /pool-imports` reads SPDK RAID superblocks of unclaimed disks
    through the kernel (parser checked against real SPDK superblocks, CRC-32C included) and
    lists arrays not in the database; `POST /pool-imports {pool_id, name?, allow_degraded?}`
    claims without wiping, waits for assembly (starts a RAID1 degraded if asked), reads the
    manifest (else the SPDK skeleton: volumes/snapshots by ID with sizes and parentage, no
    exports; a snapshot shared by a volume and its clones is assigned with a warning, since
    SPDK keeps no creation order), checks name/ID conflicts, records everything with the
    original IDs in dependency order, restores exports disabled when a portal address isn't
    on the appliance. A failure undoes only the database records: the disks stay claimed and
    the array assembled (detaching RAID1 members one by one would mark the first failed in
    the others' superblocks), and a retry resumes. Exports gained `disabled` /
    `disabled_reason` (`PATCH {enabled}`; `veloctl export enable|disable`). veloctl `pool
    importable|import`; UI Import dialog, configuration-copy status on the pool page.
  - [x] Tests: unit (manifest, helper I/O, reconciler sync incl. torn write, failing device,
    generation floor; backups schedule/retention/restore; import from manifest, skeleton,
    conflict + resume; CLI), integration with real SPDK (skeleton import after losing the
    database, superblocks read from the disk files, data read back).
  - [x] Lab test on the appliance (dev-497a397, serenity VM, 2026-11-17), RAID1 pools on
    virtio-SCSI SSDs (io_uring) and on NVMe:
    - copies written through real ublk (gen 3 / 2 after setup), no errors;
    - spdk_tgt killed with the metadata device open and I/O waiting on it (D state): on velod's
      first pass after spdk_tgt came back (~10 s) the device was recovered
      ("Recovering ublk 100 ... recover done successfully") and stopped, the waiting I/O
      ended (EIO; the next pass rewrites), the in-flight record was cleared;
    - database, backups and bootstrap file deleted + reboot: both pools listed as importable
      from their superblocks (SSD slots identified), imported from the manifests (latest
      generations), names / labels / snapshots / host / group / portals back, exports served,
      the export on a portal address gone after the reboot restored disabled; 64 MiB on
      each volume read back identical through NVMe/TCP;
    - restore of a manual backup: change undone, pre-restore backup taken, velod restarted;
      pools held newer generations (6 / 4 vs 5 / 3): logged, and writes went on at 7 / 5;
    - disks moved to a fresh install (new node ID): imported through the UI, data identical;
      enabling the disabled export failed cleanly until its address existed, then ONLINE;
    - UI: import dialog and result, configuration-copy fact, backups section.
  - [x] Lab follow-ups (fixed after the run): SPDK logged an error on every write
    (`ublk_create_target` on an existing target: now created once per spdk_tgt); the kernel
    logged "I/O error, dev ublkb100" per write (its probe of the new device was still running
    when velod stopped it: velod now waits until the device is idle, >= 300 ms, and a udev
    rule keeps udev from probing it); stopping right after `ublk_recover_disk` made SPDK fail
    END_USER_RECOVERY (now waits for the recovery); the disabled-export message told
    operators to change a portal's address, which no API allows: exports' portals can now be
    changed (`PATCH /exports/{id} {portal_ids}`, `veloctl export access --portals`, UI) and NVMe
    listeners of dropped portals are removed; an import now restores the member disks'
    power-loss-protected / write-cache-policy settings; UI header showed "false" for token
    sessions.
  - [x] Recheck on dev-085d5ed (2026-11-18, same lab VM upgraded with `POST /system/upgrade`:
    pre-upgrade backup taken, pools kept): ublk target created once at boot; 12 metadata writes
    over both pools (snapshot create/delete) with no kernel messages and no SPDK ublk errors;
    spdk_tgt killed with the metadata device open and a read waiting: recovered on velod's first
    pass ("recover done successfully", no END_USER_RECOVERY error), and the waiting read now
    **completes** (exit 0) instead of failing; record cleared; 64 MiB on both volumes identical.
  P0-3 is done. Found on the way: P2-12.

- [ ] **P0-4. Hosts are told writes are durable when they may only be in a disk's volatile cache. (Verified)**
  In SPDK v26.09 the lvol module does not support FLUSH (`vbdev_lvol_io_type_supported`), and
  the RAID module always reports `write_cache = 0`. NVMe-oF advertises a volatile write cache
  only when the namespace's bdev reports one, so **every Velocor volume tells hosts there is no
  volatile write cache**. Hosts then never send FLUSH and treat a completed write as durable,
  but the member drives (NVMe without power-loss protection, nearly all SATA SSDs) do cache
  writes, so **a power cut can lose acknowledged writes**. Reproduced by
  `TestWriteCacheAndFlushAdvertised` (reported as a known issue until this is decided).
  **Decision (2026-11-16): interim handling, no SPDK patch for now.** The proper fix (patching
  SPDK so FLUSH reaches the drives) is deferred to a later phase: see
  [L-1](#long-term). Until then, Velocor makes the risk visible and safe by default per disk:
  1. **Detect the volatile write cache at claim.** Read it before the disk goes to SPDK: NVMe
     Identify Controller `VWC` (and Get Features 06h for whether it's on), the ATA IDENTIFY
     write-cache bits for SATA, the caching mode page `WCE` / Extended INQUIRY `NV_SUP` for SAS;
     the kernel's `/sys/block/*/queue/write_cache` reflects the same signal. Record it on the
     disk and show it in the UI, CLI and API.
  2. **`VWC = 0`: durable, nothing to do.** The drive has no volatile cache, so a completed
     write is on durable media.
  3. **`VWC = 1`: treated as unprotected** unless an operator marks the disk "power-loss
     protected" (or its model is on a known-PLP list). There is no standard field for power-loss
     protection, and many PLP drives still report `VWC = 1`, so this has to be declared.
  4. **For unprotected disks, offer to turn the write cache off** (NVMe Set Features VWC=0
     through SPDK's admin passthrough; `hdparm -W0` for SATA; `WCE=0` for SAS) and keep it off
     across restarts. This is safe but can cut write speed and endurance sharply on consumer
     drives, so the trade-off is shown per disk; pools containing unprotected disks with the
     cache on are flagged.
  5. **Validate with a power-pull test** on real hardware (write with fsync under load, cut
     power, verify) before calling this done.

  This item stays open until steps 1–5 are implemented and tested, and P0-4 as a whole is not
  closed until L-1 is done.

  **Progress (2026-11-16):**
  - [x] Step 1 (detection) via the kernel (`queue/write_cache`) in every hardware scan; the last
    known state is kept while SPDK owns a device. Shown per disk (`write_cache`, derived
    `durability` = DURABLE / AT_RISK / UNKNOWN) in the API, `veloctl disk list` and the UI;
    `/system/status` warns about at-risk pool disks.
  - [x] Step 3: `PATCH /disks/{id}` `power_loss_protected` (operator declaration; scans never
    change it). No known-PLP model list yet.
  - [x] Step 4 for SATA/SAS/virtio SSDs: `write_cache_policy: DISABLED` switches the drive's
    own cache off through `scsi_disk/*/cache_type` (MODE SELECT; never the "temporary" values,
    which only change the kernel's view), at claim and on every reconciler pass (drives can
    re-enable it after a power cycle). Verified on velocor-1's emulated SATA disks (kernel logs
    "Write cache: disabled" from the drive).
  - [x] Step 1 + 4 for NVMe owned by SPDK (2026-11-18): admin commands through
    `bdev_nvme_send_cmd` (`spdk.BdevNvmeAdminCmd`): Identify Controller for VWC and ONCS save
    support, Get / Set Features 06h for WCE (`dataplane.NVMeWriteCache`, `SetNVMeWriteCache`;
    saved across power cycles when the drive supports it, falling back to unsaved when the
    feature isn't saveable). The reconciler reads every claimed NVMe drive's cache each pass,
    switches it off where the policy says so (again after a drive re-enables it), and handles a
    refusal like an SSD's. `PATCH /disks/{id}` applies the policy through SPDK for a claimed
    drive (refusal: 400 with the drive's status); for an unclaimed drive it is recorded and
    applied at claim, before anything is written. `TestNVMeWriteCacheThroughSPDK`,
    `TestNVMeWriteCacheKeptOffAndPoolDurability`, `TestNVMeWriteCachePolicyAPI` (fake SPDK
    modelling the admin commands). Lab (P0-1 VM, QEMU NVMe, dev-dfa39a5, 2026-11-18): the first
    lab run found every admin command refused ("Invalid parameters": SPDK's base64 decoder wants
    padding; fixed in dfa39a5, the fake now decodes as strictly). Then: the drives report VWC 1,
    saveable, WCE 1; `PATCH` DISABLED on both `nvmepool` members switched them off (saved value
    0, read back from the drives) and the pool went from AT_RISK to DURABLE; a cache switched
    back on behind velod's back was off again after one pass; a policy set on an unclaimed drive
    was applied by its claim; the drive was then restored (policy left, cache on, released).
  - [x] Pool-level flag (2026-11-18): pools carry `durability` (the weakest member's: DURABLE,
    AT_RISK, UNKNOWN), maintained by the reconciler; shown in the pool list and detail of the UI
    with what to do, and in `veloctl pool list`.
  - [ ] Step 5: power-pull test on real hardware. *Deferred (2026-11-18):* no suitable hardware
    for now.
  - [x] **Disks whose cache can't be turned off.** Lab, 2026-11-18: virtio-scsi disks of a
    Synology VM ("SYNOLOGY Storage", LIO behind them) report "write back" and refuse the change
    (`cache_type` write: EINVAL). velod retried every pass and listed all six disks under the
    reconciler's degraded resources and last error, half the dashboard. *Fixed:* a refusal is
    recorded on the disk (`write_cache_error`, shown on the disk), logged once, and retried only
    after velod restarts or the operator sets the policy again; the pass stays clean. The
    status warnings carry one line naming such disks and what to do (declare PLP if the host
    protects the cache, or leave the policy). Setting the policy through the API was already
    refused when the drive refuses. `TestRefusedWriteCacheIsRecordedNotRetried`,
    `TestRefusedWriteCacheWarning`.

- [x] **P0-6. A failed io_uring setup crashes `spdk_tgt`. (Verified)**
  Claiming an SSD with the io_uring backend where io_uring can't be set up (locked-memory
  limits, seccomp, `kernel.io_uring_disabled`) makes `spdk_tgt` segfault instead of returning an
  error, taking every export down. The production quadlet sets `memlock=-1`, which avoids the
  known cause, but nothing checks it. *Done:* an `aio` backend (`--ssd-bdev aio` /
  `VELOCOR_SSD_BDEV=aio`). *Needed:* velod should probe io_uring before using it and fall back
  to AIO; report the SPDK crash upstream.
  *Done (2026-11-18):*
  - **Cause, reproduced** with the SPDK test image (v26.09) under rootless Podman on Fedora:
    `bdev_uring_create` → "uring I/O context setup failure" (EPERM) → exit 139. Not memlock (8 MiB
    is fine on current kernels, which no longer charge rings to it) and not seccomp (unconfined):
    **SELinux** (`container_t` may not use io_uring). The same container as `spc_t`
    (`label=disable`, what the appliance quadlets use) works. In SPDK, `bdev_uring_create_cb`
    uses `spdk_get_io_channel(&uring_if)` without a NULL check; report and one-line fix in
    [docs/upstream/spdk-uring-channel-crash.md](docs/upstream/spdk-uring-channel-crash.md).
  - **Probe.** `--ssd-bdev auto` is the new default: before every SSD attach (claims, and
    re-attaches after an `spdk_tgt` restart, where a crash would repeat on every restart) velod
    checks `kernel.io_uring_disabled`, sets up a ring itself, and checks each visible `spdk_tgt`
    process (host PID namespace): SELinux type when enforcing (spc_t/unconfined only), no
    seccomp filter, and an unlimited memlock on kernels before 5.12. Unsure means AIO, logged
    with the reason when the choice changes. `uring` still forces io_uring (with a warning at
    start if the probe disagrees), `aio` forces AIO. `TestProbeIOURing`, `TestKernelBefore`;
    checked live against the confined and unconfined containers; the integration suite now runs
    with `auto` (it used to force AIO), so a wrong verdict would crash its `spdk_tgt`.
  - *Open:* send the report upstream (user), and carry the fix as an SPDK patch once
    `spdk-patch-unit-tests.sh` can accept a module without unit tests (bdev_uring has none).

### P1: outages, lock-outs, things operators can't see

- [x] **P1-1. Nothing alerts anyone.** Degraded or faulted pools, SMART critical warnings, a
  filling pool or a stalled reconciler are visible only in the UI. `/metrics` exports capacity
  but no health. *Needed:* health gauges per disk/pool/volume/export (so Prometheus /
  Alertmanager can page), and optionally a webhook or e-mail notifier.
  **Design (agreed 2026-11-19):** alerts are Prometheus rules on velod's metrics, written once:
  shipped for existing monitoring (rules file, OpenShift `PrometheusRule`) and run by an
  optional built-in Prometheus + Alertmanager, off by default.
  1. Metrics the rules need (health gauges per disk/pool/volume/export, rebuild progress, SMART,
     write-cache refusals, data plane up, unmanaged objects), the rule set, and docs/files for
     external Prometheus.
  2. Built-in stack: Prometheus and Alertmanager images pinned by digest in the OS image;
     quadlets on localhost only, on cores SPDK doesn't use, memory-limited; Prometheus data on
     `/var` with 15 days' retention, capped at 2 GB; scrapes `/metrics` with a dedicated
     read-only token. velod generates both configs and reloads them; on/off in the API, UI and
     `veloctl`. While off, status warns that no alerting is configured.
  3. UI: receivers (email and generic webhook first, then Slack and PagerDuty); each shipped
     rule on/off with its threshold and delay (no free-form PromQL); active alerts and
     silences through Alertmanager's API; an always-firing watchdog to an external heartbeat
     service, plus docs that production sites should also monitor from outside.
  4. **History graphs in the UI** (in scope, agreed 2026-11-19): pool space over time, IOPS
     and throughput per pool/volume, from the built-in Prometheus.
  Receiver secrets (SMTP password, webhook URLs) never go into the pools' metadata copies.
  **Step 1 done (2026-11-19):** exporter rewritten (`internal/server/metrics.go`: grouped
  families, Prometheus escaping; health, SMART, rebuild, durability, metadata copy, backups,
  data plane, reconciler, per pool/volume/disk I/O); 25 rules in
  `deploy/monitoring/velocor-alerts.rules.yml` with promtool unit tests (`make check-alerts`,
  in lint and CI); a Go test ties rule metrics to the exporter; `docs/monitoring.md`.
  **Step 2 code (2026-11-19, not yet lab-tested):** `internal/monitoring` (config, scrape-token
  rotation, systemctl start/stop/restart, AllowedCPUs off SPDK's reactors, status);
  `MonitoringEnabled` setting, `GET|PUT /system/monitoring`, `veloctl system monitoring`,
  System page section; status warns when nothing scrapes /metrics and built-in is off;
  quadlets `velocor-prometheus` / `velocor-alertmanager` (no [Install], loopback ports
  19090/19093, memory limits); images quay.io/prometheus v3.15.1 / v0.34.2 embedded in the OS
  image; Renovate regex manager for those ARGs.
  **Step 2 lab check (velocor-1, dev-7ffea94, 2026-11-19):** turned on via the API; target up
  (19 ms scrapes), 25 rules healthy, only the watchdog firing; loopback-only listeners;
  AllowedCPUs=1-3 (SPDK on 0); 34 MB / 12 MB memory.
  **Steps 3 and 4 code (2026-11-19):** receivers (email, webhook, heartbeat; secrets in files,
  never returned by the API), per-rule on/off, delay and thresholds (disk temperature, wear),
  test alert, silences, Alerts page; named history graphs (`/monitoring/graphs/{name}`, no
  PromQL from browsers) as SVG charts on the overview, pool and volume pages.
  **Done (2026-11-19), lab-checked on velocor-1 (dev-99eece0):** monitoring restarts with velod
  after a reboot and keeps its history; test alert, silence round trip and a rule change
  (reloaded into Prometheus) work through the API; graphs checked by the user with load from
  snoarm. Found there: latency graphs are 0/0 = NaN while idle, which encoding/json refused
  after a 200 (fixed in 99eece0: NaN dropped, writeJSON encodes before writing).
  *Left:* delivery to a real email/webhook receiver isn't tested (no receiver in the lab);
  Slack/PagerDuty-native receivers (webhook covers them through relays).

- [x] **P1-3. Network changes can lock the appliance out.** `PUT /system/network/interfaces/{name}`
  applies at once; a wrong static address or disabling the management interface cuts the
  appliance off, and changing an interface that carries portals breaks the SAN silently.
  *Needed:* apply, then confirm from a new connection within ~120 s or roll back automatically;
  refuse or warn about interfaces that host portals.
  **Done (2026-11-19):** velod takes a NetworkManager checkpoint of the interface (rollback
  after 120 s, 60-600 s), applies the change with `nmcli --temporary`, and answers 202 with the
  pending change; `POST /system/network/confirm` saves it and commits the checkpoint, `/rollback`
  restores it now. Without a confirmation NetworkManager itself restores the checkpoint, so the
  appliance comes back even if velod can't be reached. Interfaces carrying portals need
  `"force": true`; one change at a time. Network page: confirm banner with countdown and a link
  to the new address; `veloctl system network [confirm|rollback <id>]` for the console.
  Lab-checked on the p01-lab VM (dev-5a3608e): a wrong static address cut the API off and
  NetworkManager restored DHCP at the 60 s deadline (reachable again after 55 s, velod reported
  EXPIRED); a confirmed MTU change survived a reboot; an unconfirmed one was gone after a
  reboot during its window. *Left:* a pending change is forgotten if velod restarts during the
  window (NetworkManager still rolls it back; it just can't be confirmed any more).

- [x] **P1-4. Deletion is immediate and irreversible.** With CSI automation and API tokens, one
  mistake is unrecoverable. *Needed:* per-volume deletion protection and/or a recycle period in
  which a deleted volume is unexported and hidden but kept for N hours.
  *Done (2026-11-18), both:*
  - **Retention.** `DELETE /volumes/{id}` (UI, veloctl, CSI) marks the volume deleted
    (`deleted_at`, `purge_at`, `deleted_by`) and leaves its lvol alone. It is then hidden from
    the volume API (404, not listed, CSI sees it gone), can't be exported, attached, snapshotted
    or resized, and its name is free again. The reconciler purges it (lvol, then record) once
    `purge_at` has passed, only while its pool is online. Default 24 h;
    `GET|PUT /system/volume-retention {hours}` (0 = delete at once, at most 720), on the System
    page and `veloctl system volume-retention`. Admin only: `GET /deleted-volumes`,
    `POST /deleted-volumes/{id}/restore {name?}` (a new name if the old one was reused),
    `DELETE /deleted-volumes/{id}` (purge now); veloctl `volume deleted | restore | purge --yes`,
    and a Deleted volumes section on the Volumes page. A purge marks the volume `purging` in the
    same transaction that checks it is due, so a concurrent restore can't bring back a volume
    whose lvol is being deleted.
  - **Space.** A deleted volume counts as committed by its allocated bytes (it can't grow), and
    a capacity refusal says how much deleted volumes still hold. A pool holding deleted volumes
    can't be deleted (the refusal says so).
  - **Deletion protection.** `deletion_protection` on create, `PUT /volumes/{id}/protection`
    (admin; veloctl `volume protect [--off]`, UI Protect/Unprotect). Deletes are refused (409;
    CSI `FailedPrecondition`, so the provisioner retries) until an administrator turns it off.
  - **Snapshots with clones.** Deleting a snapshot that volumes were cloned from used to be
    refused, which with retention would have kept a deleted PVC's VolumeSnapshot stuck for a
    day (and was already wrong for Kubernetes, which allows deleting a VolumeSnapshot after
    restoring from it). It is now hidden at once and the reconciler deletes it when no volume,
    live or deleted, depends on it. Snapshots with no clones are still deleted at once.
  - Tests: `TestDeletedVolumeIsKeptAndRestorable`, `TestDeletedVolumeIsPurgedWhenRetentionEnds`,
    `TestDeletionProtection`, `TestVolumeRetentionSetting`, `TestSnapshotDeleteWaitsForItsClones`,
    `TestDeleteVolumeProtectionAndRetention` (CSI); real SPDK: `TestSnapshotsAndClones` (purge,
    deferred snapshot delete, lvols gone afterwards), `TestPoolDeleteThenReuseDisks`,
    `TestPoolSpaceLifecycle`.
  - *Not done:* a `deletionProtection` StorageClass parameter (clones made from snapshots take
    another API path, so both would need it); a retention period for snapshots without clones.

- [x] **P1-5. Orphaned SPDK objects are invisible.** Lvols, RAIDs or lvstores in SPDK but not
  in the store (failed rollback, crash between SPDK and store writes, manual changes) consume
  space silently, and after metadata loss they're the only copy of the data. *Needed:* the
  reconciler reports unmanaged objects (never deletes them).
  *Done (2026-11-18):* every pass (after SPDK has examined its bdevs) compares lvstores, lvols in
  managed lvstores (volumes, snapshots, deleted ones, the metadata lvol), RAIDs, other bdevs
  (claimed disks', pools', volumes'), NVMe-oF subsystems and iSCSI targets (exports') with the
  store. Anything unaccounted for a minute (the API creates in SPDK first and records right
  after) is reported: reconciler state `unmanaged_objects` (kind, name, UUID, detail, first
  seen) in `/system/status`, one status warning with counts and what to do, a list on the
  System page and in `veloctl system status`, `velocor_spdk_unmanaged_objects{kind}` for
  alerting, and a log line when first reported. Lvols of an unmanaged lvstore are covered by
  its line (a lost database reports each pool once). Unmanaged subsystems and targets are
  called out as access no export controls. Nothing is deleted or degraded; a failed listing
  keeps the previous report. `TestUnmanagedSPDKObjectsAreReportedNotDeleted`,
  `TestUnmanagedLvstoreAfterDatabaseLoss`; real SPDK: the integration suite asserts nothing
  managed is reported after crash/clean restarts and the snapshot/clone/purge scenario, and
  that a stray lvol is reported and kept.

- [ ] **P1-6. Single node, no high availability.** One appliance, one copy of each pool; ANA
  states are modelled but unused. A legitimate first-version scope, but it should be stated in
  the docs together with what hosts see during an appliance reboot (I/O stalls until
  `ctrl_loss_tmo`, then errors).

- [ ] **P1-7. OS lifecycle.** `bootc upgrade` runs inside the HTTP request (minutes) and should
  be a tracked background job. Reboot doesn't stop exports or SPDK cleanly first, and doesn't
  warn about connected hosts. **A failed upgrade is silent:** if the staged deployment isn't
  finalized at shutdown, the appliance simply boots the old version. velod should remember the
  image it staged and, after the reboot, warn when the booted image differs (seen on velocor-1,
  see the UEFI `/boot` fix below).

- [ ] **P1-8. A RAID1 missing a member at startup is not served. (Verified)** SPDK keeps a
  superblock RAID1 in `configuring` until every member is present; it does not start degraded.
  A disk that fails while the appliance is down makes the pool unavailable after boot (data
  intact; Velocor reports DEGRADED and re-creates nothing). Online failures degrade gracefully.
  *Needed:* a supported degraded start (SPDK investigation; possibly removing the absent member
  from the superblock with operator confirmation). Together with P0-1 this is "redundancy".

- [x] **P1-10. iSCSI WRITE SAME onto freshly discarded thin-volume space deadlocks SPDK
  (root cause found 2026-11-17).** Symptom (NVW + lab): `mkfs.ext4` on a volume that already
  holds data hangs; the initiator aborts ("abort task not supported"), resets and offlines the
  LUN; SPDK can't stop (SIGKILL after 120 s). "Still wedged after a restart" was the initiator
  resending the same commands to the restarted target, not damage: after a restart with the
  initiator logged out, the volume reads end to end.
  **Mechanism (SPDK v26.09, reproduced with `sg_write_same` from the lab initiator):**
  1. SPDK advertises MAXIMUM WRITE SAME LENGTH = 4 MiB / block size (0x2000 blocks at 512 B)
     and executes WRITE SAME as one single-block bdev write per block, submitting as many as the
     bdev_io pool allows (`bdev_scsi_split`, lib/scsi/scsi_bdev.c): one 4 MiB command = 8192
     bdev I/Os. Linux zeroing (BLKZEROOUT, mkfs) sends up to the queue depth (32) of them.
  2. After a whole-device discard the thin lvol's clusters are freed, so every write needs a
     cluster allocation, whose metadata update runs on the blobstore metadata thread (app_thread).
  3. The iSCSI poll group's flood empties the global bdev_io pool (65535). The metadata thread's
     I/O then waits in its own `io_wait_queue`, which is only woken by a bdev_io freed **on that
     thread** (`spdk_bdev_free_io`, lib/bdev/bdev.c); nothing is in flight there, so it never
     wakes, and the user writes wait on the allocation forever. The iSCSI thread spins at ~85%
     CPU with zero writes completing; nothing is logged.
  Evidence: after a discard, 16 parallel 4 MiB WRITE SAMEs (131k bdev writes) complete in 1 s,
  24 hang; on **allocated** clusters 24 and 32 (262k writes) complete in 1.4 / 2 s.
  NVMe/TCP is not affected the same way (Write Zeroes is one bdev I/O).
  Our side: thin provisioning (the CSI default) makes it reachable, and 512-byte blocks make the
  fan-out 8x larger than with 4 KiB blocks. A bigger bdev_io pool only moves the threshold.
  **Fix (a) implemented:** `deploy/containers/spdk-patches/0001-scsi-bound-WRITE-SAME-fan-out.patch`
  (applied by Containerfile.spdk; SPDK's scsi unit tests pass, and the 3 new WRITE SAME tests
  fail on unpatched SPDK). **Verified on velocor-1 (dev-559ea38, 2026-11-17):** after a whole-device
  discard, 16/24/32/64 parallel 4 MiB WRITE SAMEs complete in 0.2/0.3/0.3/0.6 s (24 hung before);
  zero WRITE SAME reads back zero with neighbouring blocks intact; patterned WRITE SAME (40 and
  3000 blocks) exact; whole-device BLKZEROOUT reads zero; `mkfs.ext4` over a filesystem holding
  data (twice) takes <1 s, fsck clean, write/remount/verify OK; no SPDK errors logged. NVW (RHCOS,
  `velocor-iscsi`): 300 MB written on one node, verified and rewritten after re-attach on
  another. *Remaining:* send the patch upstream (SPDK GitHub/Gerrit) and drop it once merged.
  *Fix options considered:* (a) carry an SPDK patch in Containerfile.spdk: WRITE SAME with an all-zero block
  and no UNMAP/ANCHOR bits -> one `spdk_bdev_write_zeroes_blocks`; other WRITE SAMEs split with a
  small cap on outstanding children per task; send upstream with the io_wait cross-thread
  starvation report. (b) interim: CSI node sets `max_write_same_blocks=0` on Velocor iSCSI LUNs
  (protects CSI hosts only, not VMware/Windows initiators). (c) consider 4 KiB logical blocks for
  new pools. Lab: Fedora initiator VM on serenity (`~/.cache/iscsi-lab`); on velocor-1 volume
  `iscsi-lab` (mirror250a), portals `lab-iscsi-mgmt` / `lab-nvme-mgmt`, host `lab-initiator`,
  group `lab-initiators`.
- [x] **P1-13. The CSI driver picks a volume's portal by chance.** `ControllerPublishVolume`
  exports a volume on the first portal of its protocol in velod's list, which is in portal-ID
  order, and IDs are random. On velocor-1 (2026-11-18) the SAN portals happened to sort before
  the lab's management-network portals; with other IDs every CSI volume would silently have
  gone over the 1500-MTU management network. Adding a portal can change where new volumes go.
  `portalOverride` (`VELOCOR_PORTAL_OVERRIDE`) can't fix it: it only changes the address the
  node connects to (the export still listens on the chosen portal only), and it is one
  `host:port` for both protocols (4420 / 3260). Each export also gets exactly one portal, so no
  multipath across SAN NICs, and a wildcard (0.0.0.0) portal isn't connectable as it is.
  *Fix:* a StorageClass parameter `portals` (names or IDs), validated at provisioning (exists,
  right protocol); without it, the protocol's only portal, else refuse with a message naming
  the parameter; the export gets every listed portal and the node connects to each (NVMe
  native multipath, dm-multipath for iSCSI; the node side can follow later); deprecate
  `portalOverride`.
  **Done (2026-11-19):** StorageClass parameter `portals` (names or IDs) and the chart's
  `defaultPortals` (`--default-portals`, both protocols, applies at each attach so volumes from
  before the parameter can be given portals). Checked at provisioning (exists, right
  protocol); several portals and no choice is refused with a message listing them; a wildcard
  portal is refused unless the deprecated `portalOverride` is set. Exports get every listed
  portal; the publish context adds `portals` (`portal_ip`/`portal_port` stay, for older node
  plugins). Nodes: NVMe/TCP connects every path when native multipath is on (else the first
  that answers); iSCSI logs in to one portal (an existing session first, else the first that
  answers) and logs out of all. Unit tests: `internal/csi/portals_test.go`.
  *Left:* iSCSI multipath (dm-multipath on the node) and a lab check with two SAN paths.
- [x] **P1-14. A pool whose disks come back under new IDs stays UNAVAILABLE, with no way to
  recover it. (Verified)** Lab, 2026-11-18: velocor-1's six pool disks moved from emulated SATA
  to virtio-scsi (same disks, same data). Their IDs changed (`ssd-t10.ATA_QEMU_HARDDISK_QM00003`
  became `ssd-naa.6001405…`), because velod records members by disk ID, and all three pools came
  up UNAVAILABLE ("0/2 member bdevs present"). velod knows where the data is: `GET
  /pool-imports` lists each pool's RAID superblock on the new disks with the right pool ID, but
  refuses to import ("a pool with this ID already exists"), and replacing members would mean a
  rebuild with no source. The same happens after moving drives to another HBA or changing the
  controller mode. Also seen:
  - the old disk records stay listed, DEGRADED and `URING_CLAIMED`, under kernel names (`sdb`…)
    that now belong to the new disks;
  - the import candidates mark every disk `stale` although all carry the newest sequence number;
  - the new disks' serials end in a NUL byte (`…-e2ca277e5875\u0000`): `readSysfsString` trims
    whitespace only.
  *Fixed:* a member's SSD bdev UUID can now be kept on its disk record (`Disk.BdevUUID`,
  used by `dataplane.DiskBdevUUID` instead of the ID-derived one). Each reconcile pass looks
  at pools whose RAID isn't online and members whose disk the scan no longer finds, reads the
  superblocks on the unclaimed SSDs, and `dataplane.MatchMovedMembers` assigns the missing
  members' UUIDs to the disks carrying the array's current superblock. The new record takes
  the old one's slot, UUID and disk settings (write-cache policy, PLP) in one transaction
  (`RelinkPoolMember`), the old record is deleted, and SPDK examine then assembles the array.
  It matches only when certain: every member the current superblock lists as configured must
  be back with that superblock's sequence number (a single returning disk could be an
  out-of-date copy whose current counterpart is still away), and several members only for a
  RAID1 whose members share one data layout (interchangeable); a stripe is never guessed and
  the pool says why. Pool import uses the same matching, so disks that changed ID on the way to
  a new appliance import too. sysfs strings lose NUL padding; released disks drop a kept UUID.
  `TestMirrorUnderNewDiskIDsComesBack`, `TestOneMovedMirrorMemberComesBack`,
  `TestMovedStripeIsNotGuessed`, `TestMovedMirrorWaitsForEveryMember`,
  `TestImportMirrorUnderNewDiskIDs`, `TestMatchMovedMembers`.
  *Still open:* NVMe drives moved to another PCIe slot (their ID is the PCI address) aren't
  relinked yet; no event is raised (velod logs a warning per relinked disk).
  Lab (dev-a548637, 2026-11-18): velocor-1's three RAID1 pools, UNAVAILABLE after its six disks
  moved from emulated SATA to virtio-scsi, came back ONLINE on the first boot of the upgrade
  without any operator step; the members now have the new IDs and keep their old bdev UUIDs,
  the old records are gone, serials no longer end in NUL, and the volume on `mirror250a` is
  ONLINE. The new disks can't have their write cache switched off (see P0-4).
- [ ] **P2-11. velod reports ONLINE before a restarted data plane is restored.** After an SPDK
  restart, status said ONLINE within ~10 s while volume stores, the iSCSI portal and target came
  back about a minute later.
- [x] **P2-12. An enabled export on a portal whose address is gone retries forever with SPDK's
  error.** Lab, 2026-11-18: after a reboot removed 10.77.0.1, the export on that portal was
  DEGRADED with "add listener 10.77.0.1:4420 ...: spdk rpc error (code -32602): Invalid
  parameters", and each reconcile pass (15 s) logged four SPDK `*ERROR*` lines. *Fixed:* the
  data plane checks a portal's address against the appliance's interfaces (wildcards count;
  `dataplane.AddressPresent`, also used by the pool import) before adding an NVMe listener or
  creating an iSCSI portal group. A missing one is skipped without calling SPDK, the export is
  served on its other portals and reported DEGRADED with "portal NAME (ID) has address A,
  which isn't on this appliance; add the address or give the export another portal", and the
  next pass after the address appears serves it. Existing listeners are kept (a brief flap
  doesn't tear them down). Creating an export, adding such a portal or enabling the export is
  refused with that message; other changes to an export that already had the portal apply.
  `TestPortalWithoutItsAddress`. Lab (dev-91f9415, 2026-11-18): after the reboot the export
  was DEGRADED with that message and SPDK logged no errors; adding 10.77.0.1 made it ONLINE on
  the next pass (data read over it); removing the address again kept the listener, no errors.

### P2: hardening

- [ ] **P2-1. CHAP and real host identities** (was S10). Any host that can spoof an allowed NQN
  or IQN gets access. The API rejects `chap_user` rather than silently ignoring it. *Needed:*
  iSCSI CHAP auth groups and NVMe DH-HMAC-CHAP with encrypted secrets; the CSI node plugin
  should register the host's real hostnqn/IQN instead of names derived from the node name.
- [ ] **P2-2. No background scrub.** Mirrors are never read and compared, so latent sector
  errors surface only during a rebuild, when they turn a degraded pool into a lost one.
- [ ] **P2-3. Reconciler cost.** `EnsureExport` lists subsystems/target nodes per export
  (O(n²) RPCs); prefetch once per pass (was F5).
- [ ] **P2-4. SPDK sizing.** One core and 1 GiB of memory; sizing isn't documented or
  validated against the number of exports and connections. Lab, 2026-11-18
  ([docs/performance.md](docs/performance.md)): on a VM with emulated SATA disks the disks were
  the limit (92–97% busy) and a second core (`-m 0x3`) changed nothing. With 2 cores the
  NVMe/TCP queues spread over both reactors, but an iSCSI volume (one connection) stays on one,
  and `app_thread` was 40–56% busy during deep NVMe/TCP I/O (find out what runs there). Measure
  again on NVMe drives, where one reactor will be the limit.
  **Plan (agreed 2026-11-19; after the release gate, tuned on real hardware):** SPDK fixes its
  core mask at start and reactors poll at 100%, so "auto" means sizing at boot, not all cores.
  On virtio-scsi a second core gained nothing; on emulated SATA, writes were about 20% slower.
  1. A boot step picks the count and writes it to a file the SPDK unit reads (replacing the
     hardcoded `-m 0x1`): 1 core on a VM; on hardware about a quarter of the cores (at least 1,
     capped), never CPU 0, preferring the NUMA node of the NICs and NVMe drives. The quarter
     and the cap are guesses until measured on NVMe hardware.
  2. An operator override (API, UI, `veloctl`) for a fixed count, applied at the next boot or
     an explicit SPDK restart, with a warning that the restart interrupts I/O.
  3. Later, maybe: SPDK's dynamic scheduler (idle cores sleep); test it with NVMe/TCP, iSCSI
     and io_uring first.
- [ ] **P2-13. virtio-blk disks are invisible.** Hardware discovery keeps a `vdX` device only
  if it reports `rotational=0` or a virtual model name. virtio-blk reports `rotational=1` and
  has no `device/model` (checked on the lab VM), so it never shows up and can't be put in a
  pool. Without a QEMU `serial=` it also has no `/dev/disk/by-id` link and no serial, so its
  disk ID would be the kernel name (`ssd-vda`), which can change between boots. *Needed:* treat
  virtio-blk (`device/driver` is `virtio_blk`) as an SSD candidate; require a serial for a pool
  member and tell the user why a disk without one can't be used. Also check that a pool still
  assembles when its disks move to another controller (SATA to virtio-scsi), which changes
  their IDs.
- [ ] **P2-14. iSCSI writes are split into 64K disk writes.** 128K sequential writes reach
  177–209 MB/s over iSCSI against 300 MB/s over NVMe/TCP on the same pool
  ([docs/performance.md](docs/performance.md)), and the appliance sampler showed two 64K disk
  writes for each 128K request. Find where SPDK's iSCSI target splits them (data segment or
  buffer size) and whether a larger `MaxRecvDataSegmentLength` or iobuf size avoids it.
- [ ] **P2-5. Slow test suites.** Auth/server tests take ~50–100 s under `-race` because every
  login runs 600k PBKDF2 iterations; consider a test-only iteration override (was F7).
- [ ] **P2-7. The CSI driver logs nothing per request.** Stage/publish failures are only
  visible as Kubernetes events; the node plugin's log shows just its start line.
- [ ] **P2-10. Recovery time after an unclean SPDK stop** is now ~1 min per 500 GB on emulated
  disks (was 16 min). If it matters on real hardware, lower the lvstore metadata reservation
  (`num_md_pages_per_cluster_ratio`) and report a "recovering" state instead of DEGRADED.
- [ ] **P2-8. CSI sidecar images are pinned by tag** (`csi-provisioner:v5.1.0`, ...), not digest.
- [ ] **P2-6. No health endpoint.** velod has no unauthenticated liveness/readiness route
  (found in the 2026-11-16 qcow2 boot test), so quadlets can't use `HealthCmd=` and monitoring
  can't tell "velod up but SPDK unreachable" from "all good". *Needed:* `/healthz` (process up)
  and `/readyz` (SPDK RPC answers, DB open), no auth and no detail leakage, plus `HealthCmd=` in
  the velod quadlet.

### Build, packaging and CI (review of 2026-11-16)

Everything here was written by Gemini and is in scope to rewrite.

- [x] **B8 (rest). Publish a qcow2 with releases?** `make qcow2` now runs `hack/build-qcow2.sh`
  (rootless, KVM only; bootc-image-builder and its sudo are gone). **Decision (2026-11-19): no
  qcow2 with releases for now.** The README's install Option 2 explains how to build one from
  the published `velocor-os` image with `hack/build-qcow2.sh`.
- [x] **B17. Pinned Fedora base digests disappear from quay.io.** `quay.io/fedora/fedora`
  deletes superseded `:45` builds: the pinned `sha256:e203b417…` now returns "manifest unknown"
  (found 2026-11-17 when a test pod couldn't pull it; local builds only work from cache). Clean CI
  runners and rebuilds of older commits will fail. Options: mirror the pinned base images into
  our own registry and pin those; or keep Renovate bumping the pin (reproducibility of older
  commits is then lost). Checked 2026-11-17: the pins of `fedora`, `fedora-minimal` and
  `fedora-bootc` (all set 2026-11-16) are already gone; `golang` (Docker Hub) still resolves.
  **Decision (2026-11-17): follow Fedora's rolling `:45` tags** (daily security fixes). Done:
  the Containerfiles take the base as a build arg defaulting to the tag; `make` resolves it to a
  digest per build, builds from that digest and labels the image with
  `org.opencontainers.image.base.name/.digest`; `hack/check-pins.sh` allows only those three
  Fedora repos to roll (any other unpinned base, including build-arg defaults, still fails);
  Renovate no longer pins them. Accepted trade-offs: rebuilding an old commit gets today's base,
  and a broken Fedora build reaches the next build (CI integration tests and lab upgrades of
  dev builds are the guard).
- [ ] **B14. RPM packages in image builds are not pinned.** Base images are pinned by digest, but
  `dnf install` takes whatever the Fedora mirrors have, so rebuilding a commit can produce
  different packages. *Option:* RPM lockfiles (e.g. rpm-lockfile-prototype) for the SPDK builder
  and the OS image.
- [ ] **B15. CI compiles SPDK on every run** (one compile per run, shared by the integration
  tests and the image). *Option:* reuse the previous `velocor-spdk` image when
  `Containerfile.spdk` hasn't changed.
- [x] **B18. CI review after the SPDK patch and rolling bases (2026-11-17).** Nothing we
  changed broke the workflow (all builds go through `make`; the static CSI manifest still
  matches the chart), but: (1) the integration tests and the push resolved the rolling Fedora
  base separately, so the tested and the shipped SPDK image could differ (and compile SPDK
  twice): the images job now sets `BASE_PIN_DIR`, resolving each base once per run, and lists the
  bases in `images.txt`; (2) the unit tests that come with our SPDK patches never ran: new
  `unit-tests` stage in Containerfile.spdk (`make spdk-unit-tests`, runs SPDK's unit tests for
  every library a patch touches) and a CI job that gates image publication; (3) the lint job now
  runs actionlint (pinned image) and renders the CDI StorageProfile hook, which plain
  `helm lint` skips.
- [x] **B19. veloctl didn't build for macOS/Windows** (found by the first CI run, PR #1,
  2026-11-17): password prompt used Linux-only termios ioctls; now `golang.org/x/term`. All five
  release targets cross-compile.
- [x] **B20. Pushing extra tags failed on `main`** (first `main` run, 44111134347, after
  merging PR #1): `push-one` and `push-csi` copied the extra tag from `repo:tag@digest`, which
  skopeo rejects; PRs set no extra tags, so the path never ran. Now copied from `repo@digest`.
  The `sha-39e803b` spdk image was pushed; the `main` tags, signing, attestations and the chart
  push did not run.
- [x] **B21. The OS image build couldn't pull private images** (second `main` run,
  44116125345): the build embeds the spdk/velod images with skopeo inside the build container,
  which had no registry credentials; ghcr.io packages of a private repository need them (the lab
  registry allows anonymous pulls, so this never showed). Now `make image-os` passes
  REGISTRY_AUTH_FILE (or podman's default) as a build secret (`--mount=type=secret`, never in a
  layer; an empty file when there are none), and CI points REGISTRY_AUTH_FILE at one file for
  podman login, skopeo and the build.
- [x] **B22. Attestations fail on a private user-owned repository** (third `main` run,
  44123550774): GitHub artifact attestations aren't available there. Integration tests, the
  build and push of every image (incl. the OS image, so B21 works) and cosign signing passed.
  The attest steps now run only when the repository is public; until then images are only
  cosign-signed. The chart push and the release job are still unexercised.
- [x] **First fully green `main` run** (44132095413, 8d828a4, 2026-11-17, repository now
  public): integration tests, all images built, pushed (incl. the `main` tags), cosign-signed and
  attested, Helm chart pushed to ghcr.io. Only the release job (v* tags) is still unexercised.
- [x] **B16. The new CI workflow has not fully run yet.** First run (PR #1, 2026-11-17, run
  44102090198, aa8411d): lint (incl. actionlint and the CDI hook render), unit tests, SPDK
  patch unit tests, images (integration tests on the runner, rootless podman, arm64 CSI via
  QEMU, OS image build, push to the PR's throwaway registry; one base digest per run) and the
  Helm chart all pass; veloctl failed (B19, fixed in 9778130). Second run (44105853598,
  0d04dbe): **every job passes**, including the veloctl cross-builds for all five targets.
  *Still unproven:* the `main`/tag-only steps: ghcr.io login and push, cosign signing,
  attestations, chart push, and the release job. Also enable Renovate on the repository. It passes actionlint and every step uses
  `make` targets verified locally, but the GitHub-side pieces (ghcr login, cosign, attestations,
  rootless podman with arm64 emulation on the runner, the PR local registry) are only proven by
  a first run. Also enable Renovate on the repository (`renovate.json`).
  **2026-11-19:** PR #5 green after fixing the rebuild tests for io_uring on runners (`fefb779`);
  merged; the repository is now public, so the `main` run is the first with attestations.
  Pre-release tags (`v0.1.0-rc.1`) now push only their own image tag (not `0.1`/`latest`) and
  create a GitHub pre-release, so the release job can be proven with an rc tag first.
  **Done (2026-11-19):** `main` run 44548529213 (ghcr push, cosign, attestations, chart push)
  and the `v0.1.0-rc.1` release run 44553069247 both pass. Checked from outside: the release is
  a pre-release with all assets; checksums, `cosign verify-blob` and `cosign verify` of the
  images pass with cosign v3.1.4 (v2.4.1 can't read them: the release text now says v3);
  `gh attestation verify` passes for binaries and images; the chart installs from ghcr at
  0.1.0-rc.1 with the CSI image pinned by digest; packages are public. *Left:* enable
  Renovate; the old `latest` tags on ghcr (pre-review build 977d4ae, 2026-11-14) until the
  first final release replaces them.
- [x] **B1/B2.** The OS image now embeds the velod/spdk images built and pushed in the same run
  (`make push`; one CI job) as `oci-archive` and its quadlets reference them by digest, so an
  appliance runs exactly what it was built with, needs no registry at boot, and changes only via
  `bootc upgrade`. Repo quadlets carry placeholders; the build fails without digest-pinned refs.
  velod's separate container-update path (pull the tag again and restart) was removed with it;
  `GET /api/v1/system/containers` lists the containers read-only.
- [x] **B3.** All base images pinned by digest, SPDK pinned by commit (tag checked against it);
  `hack/check-pins.sh` (in `make lint` and CI) rejects unpinned FROMs and actions;
  `renovate.json` proposes bumps.
- [x] **B4.** Images build with the official golang image matching go.mod's new `toolchain`
  line (`GOTOOLCHAIN=local`, `-trimpath`, commit time as build time); check-pins enforces it.
- [x] **B5.** CI runs check-pins, go vet (incl. integration tag), golangci-lint v2.15.0 (clean;
  `.golangci.yml`), shellcheck, helm lint, and the real-SPDK integration tests.
- [x] **B6.** No workflow-level permissions; each job declares its own. Every action pinned to a
  commit SHA.
- [x] **B7.** Makefile rewritten: `TAG` defaults to `dev-<sha>`; `latest`/version tags need
  `RELEASE=true`, a clean tree and a commit tagged `VERSION`; pushes record digests.
- [x] **B9.** No internal registry names in the repo's artifacts: default prefix
  `ghcr.io/xphyr`; hack scripts take `IMAGE_PREFIX`; release manifests and the Helm chart pin the
  CSI image by digest (`image.digest`).
- [x] **B10.** spdk_tgt and velod images have no default arguments; the quadlets are the only
  source.
- [x] **B13.** Outdated dev builds piled up in the registry: `hack/build-dev-images.sh` builds
  and pushes `dev-<sha>` from a clean tree (labelled `quay.expires-after`), and
  `hack/prune-dev-tags.sh` removes superseded builds by tag (consecutive builds share digests,
  so deleting by digest would remove the newer build too).
- [x] **B11.** Secrets could enter build contexts (`COPY . .` with a name-based ignore list):
  `.containerignore` now excludes them by pattern.
- [x] **B12.** The lab disk builder copied the host's registry credentials into disk images:
  removed (images are pulled anonymously).

### Long-term

- [ ] **L-1. Patch SPDK so flushes reach the drives (the real fix for P0-4).** Make the lvol
  module support FLUSH and pass it through to its blobstore's device, and have the RAID module
  report `write_cache` when any member has one, so NVMe-oF advertises a volatile write cache and
  hosts send FLUSH/FUA. Every drive then behaves correctly with its cache on, and PLP drives pay
  almost nothing for it. Deferred by decision on 2026-11-16 (see P0-4); **still required in a
  future phase**. Aim to upstream it rather than carry it; nvweaver's SPDK durability patch is
  prior art. Once it ships, the interim handling in P0-4 can relax (caches left on, the PLP flag
  kept as information).
- [ ] **L-2. Changed-block tracking (CBT) for incremental backups.** Deferred by decision on
  2026-11-16 (was P1-9): SPDK v26.09 has no RPC that reports a snapshot's allocated clusters, so
  CBT is switched off rather than guessed. *Needed:*
  - a small **read-only** SPDK extension (no data-path change, much lower risk than L-1) that
    returns the clusters a snapshot/lvol owns itself, not inherited from its parent. Prior art:
    `bdev_lvol_get_fragmap` exists **only in Longhorn's SPDK fork** (github.com/longhorn/spdk,
    commit 3e764f556 "fragmap: implement bdev_lvol_get_fragmap", 2023, ~520 lines incl. test;
    still carried on `longhorn-v26.05`), never upstream (checked v23.09–v26.09 and master on
    2026-11-16). Porting it onto our v26.09 build is the likely route; verify its semantics
    (own-layer vs. inherited clusters) before relying on it. For two
    snapshots in the same chain, the clusters owned by the layers between them are exactly the
    changed blocks (granularity = pool cluster size, 64 KiB–1 MiB);
  - velod: rework `blocks:allocated` / `blocks:changed` (and `spdk.BdevLvolGetFragmap`, the fake
    SPDK's `bdev_lvol_get_fragmap`, `TestStorageEndToEndAndCBT`) to the real RPC; the current
    code was written against an RPC that doesn't exist, so its request/response shape is a guess;
  - CSI: reinstate the `SnapshotMetadata` service and `SNAPSHOT_METADATA_SERVICE` capability
    (removed; in git history), only advertised when velod reports CBT works, plus the
    `external-snapshot-metadata` sidecar, its TLS and the `SnapshotMetadataService` CR in the Helm
    chart; re-add veloctl `allocated-blocks` / `changed-blocks` and the UI "Changed blocks" view;
  - verification against real SPDK (integration harness) and a Kasten incremental
    backup/restore round trip on NVW, comparing restored data byte-for-byte.

- [ ] **L-3. Parity RAID (RAID5/6) for capacity efficiency.** Requested 2026-11-16. SPDK's only
  parity RAID, `raid5f`, accepts full-stripe writes only and the lvol blobstore can't even create
  a volume store on it (R3, `TestRAID5FRefused`), so pools today are RAID0/1/10. Options, all
  substantial:
  1. a write-gathering layer between lvols and raid5f that turns small writes into full stripes
     (SPDK's FTL bdev works this way; large component with its own metadata and durability story);
  2. a read-modify-write RAID5 in SPDK with a journal against the write hole (data-path change,
     highest risk; candidate for upstreaming);
  3. kernel md RAID5/6 with its partial-parity log underneath SPDK via AIO/io_uring (simplest,
     but every write crosses the kernel and recovery spans two RAID layers).
  Needs a design decision, then power-loss and rebuild testing on real hardware.

- [ ] **L-4. RAID10.** SPDK v26.09's `bdev_raid` has no raid10 (levels: raid0, raid1, raid5f,
  concat); velod sent "raid10" and SPDK rejected it, so RAID10 pools never worked (found on
  velocor-1, 2026-11-16; the fake accepted it). Refused by the API, UI and CLI for now. *Needed:*
  build it as raid0 over raid1 pairs, each with a superblock, and handle assembly after restart,
  a degraded pair, rebuild and deletion of the nested layers; real-SPDK tests for all of those.
- [ ] **L-6. Report a full pool as "capacity exceeded" (ENOSPC) instead of an I/O error.**
  When a thin pool is full SPDK fails writes with NVMe Internal Error, which Linux reports as
  EIO (filesystems shut down). An SPDK patch completing lvol ENOSPC with NVMe Capacity Exceeded
  (status 0x81) would give hosts ENOSPC, which filesystems and applications handle far better.
  Agreed follow-up to P0-2 (2026-11-17); test with ext4/XFS hosts.
- [ ] **L-7. Pools can't be expanded.** Requested 2026-11-19. A pool is one SPDK RAID (RAID0 or
  RAID1) with one lvstore on it; member count and per-member size are fixed in the RAID
  superblock. `bdev_raid_add_base_bdev` only fills an empty slot (disk replacement, P0-1), and
  replacing every disk with a bigger one leaves the array at its old size. Today the only way to
  add capacity is a second pool (volumes can't span pools).
  *Options to evaluate:* (a) build pools as a concatenation of RAID1 pairs and grow by adding a
  pair, then `bdev_lvol_grow_lvstore`; (b) after replacing every member with a larger disk, grow
  the array, then the lvstore; (c) spread volumes across pools (no SPDK change, but not real
  expansion). (a) and (b) depend on SPDK support that is unverified in v26.09: first read its
  raid (concat, superblock, resize) and lvstore-grow code, then decide on a design and whether
  it needs an SPDK patch (with unit tests, like 0002). Crash safety of an interrupted expansion
  and the pool metadata copies (P0-3) must be part of the design.

- [ ] **L-5. Delta upgrades for the OS image.** Requested 2026-11-17 (back burner). A
  `bootc upgrade` pulls every changed layer of `velocor-os` in full; with the rolling Fedora base
  (B17) that is most of the image on every release. Goal: publish much smaller upgrade packages,
  e.g. a delta between two released OS images that an appliance applies to the version it has
  (also useful for offline / air-gapped upgrades). To evaluate: bootc/ostree-native options
  (ostree static deltas, `rpm-ostree compose build-chunked-oci`-style rechunking so unchanged
  content keeps identical layers), zstd:chunked layers (partial pulls), and OCI layer-delta
  tools; check what `bootc upgrade` / `bootc switch` can consume natively before building our
  own format. Needs: size measurements between two real dev builds, signing/verification of the
  delta, and a fallback to a full pull.

### Suggested order of work

**Release gate (agreed 2026-11-17):** no first release until items 1–5 are done; work them in
this order and keep this file current as each moves.

1. **P0-1 + P1-8 redundancy** (done; real-hardware hot-swap tests open): replace a failed disk with rebuild progress
   (`REBUILDING n%`), and a supported degraded start. Starts with a design note on SPDK's raid
   rebuild API and its behaviour across restarts; tested with real SPDK (fail, replace,
   rebuild, restart during rebuild, second failure) on the serenity VM lab, where disks can be
   hot-unplugged.
2. **P0-2 capacity** (done): overcommit limit, reserve for snapshots/metadata, thresholds,
   per-volume/snapshot allocated space.
3. **P0-3 metadata** (done): on-pool metadata copy, scheduled backups + `veloctl` backup/restore,
   recovery from SPDK and the on-pool copy.
4. **P0-4 durability, remaining**: NVMe write cache through SPDK and the pool flag are done and
   checked on QEMU NVMe. The power-pull test on real hardware is deferred (no hardware for it
   yet, 2026-11-18). The SPDK FLUSH patch stays L-1.
5. **P1-4 deletion protection** (done: retention period, per-volume protection) and **P0-6**
   (done: io_uring probe, AIO fallback; upstream report drafted).

Before tagging the first release (agreed 2026-11-19): P1-13 portal choice (done), then a test
tag to prove the release pipeline (B16). B8: no qcow2 with releases (the README explains how to
build one). The real-hardware tests of P0-1 and P0-4 stay open.

After the release gate: P1-1 alerting (done 2026-11-19), P1-3 network rollback (done 2026-11-19), P2-2 scrub, P2-11 (P1-5 orphan
reporting done 2026-11-18).

---

## Verification

**Unit tests** (`go test ./...`) run against `internal/spdk/spdktest`, a stateful fake
`spdk_tgt` that models restarts, asynchronous examine, RAID superblock matching by member UUID,
and error injection.

**Integration tests** (`make test-integration`) run against a real `spdk_tgt` from the shipped
image: rootless podman, `--no-huge`, file-backed disks through `bdev_aio`, a real velod (store,
API, reconciler), and SPDK's userspace NVMe/TCP initiator (`spdk_dd`, added by the `test` stage
of `deploy/containers/Containerfile.spdk`) to write and read data through real exports. No
root, hugepages or NVMe hardware needed.

| Scenario | Checks |
|---|---|
| `TestDataSurvivesCrashAndRestart` | bit-identical data after `kill -9` and after a clean stop; pool/volume ONLINE; lvstore UUID unchanged |
| `TestExportACLEnforced` | a host outside the ACL can't connect, before and after a restart |
| `TestSnapshotsAndClones` | snapshot isolation, clone contents, clone outliving its source, CBT reports 501 |
| `TestWriteCacheAndFlushAdvertised` | FLUSH support of disk, RAID and volume bdevs (known issue P0-4) |
| `TestDegradedRAID1` | missing member at boot: nothing re-created, pool DEGRADED, lvstore unchanged (P1-8) |
| `TestPoolDeleteThenReuseDisks` | a deleted pool's disks can build a new pool (R2) |
| `TestRAID5FRefused` | API refuses RAID5F; SPDK still can't put a volume store on raid5f (R3) |

All pass except the write-cache check, which is reported as the known issue P0-4.

**Appliance image** (`hack/build-qcow2.sh` disks, dev-04b3b31, 2026-11-16): boots with **BIOS**
(QEMU SeaBIOS on the lab host) and **UEFI** (velocor-1, Secure Boot off); first boot with no
network at all brings up SPDK and velod from the embedded images; `velocor-admin` key login works,
root and password SSH are refused. **Upgrade and rollback** (lab host): `bootc switch` from
dev-bddac0b to dev-04b3b31 loads the new embedded images, removes the old ones and keeps velod's
database; `bootc rollback` returns to dev-bddac0b with its own images and the same database. Not
yet covered: Secure Boot.

**CSI on OpenShift** (NVW, OpenShift 4.22, velocor-1 appliance, NVMe/TCP, 2026-11-16, dev-194b99c):
provision and mount; cross-node re-attach with identical data; online expansion; snapshot and
restore (point-in-time correct); PVC clone; raw block (`volumeMode: Block`) round trip; SPDK
`kill -9` with a pod writing: I/O stalls ~72 s and resumes, no errors, no lost acknowledged write;
KubeVirt: Fedora VM imported onto a Velocor block volume, boots, data survives a move to
another node, online VM snapshot (guest-agent freeze) and restore are point-in-time correct;
everything deleted cleanly (no PVs, snapshots, volumes or exports left). **iSCSI:** works on RHCOS since
d60e9ec + the SPDK WRITE SAME patch (P1-10, dev-559ea38): write, cross-node re-attach, verify. Not covered: RWX / live migration
(P1-12), Kasten.

**Not yet covered:** real NVMe drives (`bdev_nvme`, vfio), io_uring (the test containers
run confined, where the probe picks AIO, see P0-6; covered on the lab appliances), iSCSI data paths (needs an iSCSI initiator in the test image), power-loss
tests on real hardware, killing SPDK *during* a multi-step API operation, and hardware checks
of the boot-disk guard and CSI device resolution on NVMe native multipath.

---

## Done

- [x] **The initial admin password file stayed on disk** after the password was changed, with
  a permanent dashboard warning until someone deleted it by hand. velod now deletes it once
  the admin password no longer matches it: after a password change, and at startup (for
  appliances changed before). `TestBootstrapPasswordFileDeletedAfterChange`.

### Found by testing against real SPDK (second review)

These were invisible to unit tests because the fake encoded the same wrong assumptions as the
code. Each now has a real-SPDK scenario and a matching unit test.

- [x] **R1.** io_uring/AIO bdevs got a random UUID on every creation, and SPDK re-assembles a
  superblock RAID only from members whose UUID matches: **every SSD pool went offline after any
  `spdk_tgt` restart** (data intact, unserved). Fixed with a deterministic per-disk bdev UUID
  (`dataplane.DiskBdevUUID`); the fake now enforces UUID matching.
- [x] **R2.** Deleting a superblock RAID left its superblocks on the disks, so the deleted array
  re-assembled itself and blocked new pools (was F1). Fixed: `bdev_raid_delete` with `clear_sb`.
- [x] **R3.** SPDK `raid5f` accepts only full-stripe writes and the lvol blobstore can't create
  a volume store on it, so RAID5F pools could never work. Refused by the API, UI and CLI.
- [x] **R4.** SPDK v26.09 has no `bdev_lvol_get_fragmap`: CBT returns 501 rather than fabricated
  data (follow-up: P1-9, below).
- [x] **P1-9.** The CSI driver advertised and served `SnapshotMetadata` (CBT) although it could
  never work. CBT deferred (2026-11-16, now L-2): the service and capability are gone from the
  driver (guarded by `TestSnapshotMetadataNotAdvertised`), veloctl/UI/lab CBT commands removed,
  README/ARCHITECTURE/ROADMAP say it's unsupported. velod's CBT endpoints still return 501.
- [x] **R5.** SPDK validates NQN syntax; bad host NQNs now fail when entered instead of as an
  opaque "Internal error" at export time.

### Other second-review fixes

- [x] **P0-5.** SPDK was killed 10 s into every shutdown (podman default): `StopTimeout=120`.
- [x] **A data-plane restart failed I/O on every host** (NVW, 2026-11-16). SPDK was killed
  (its stop hung on the wedged iSCSI target, P1-10), and recovering the three volume stores
  took 16 minutes, because 64 KiB clusters mean one 4 KiB metadata page per 64 KiB of pool:
  ~30 GiB of metadata per 500 GB, all read during recovery (and 6% of capacity lost). Hosts
  connected with the kernel's default `ctrl_loss_tmo` (600 s) gave up and pods got EIO. Data
  was intact afterwards (both test volumes verified by checksum after remount). Fixed: default
  cluster size 1 MiB (~0.4% metadata); the CSI driver connects with `--ctrl-loss-tmo=-1
  --reconnect-delay=5` so I/O stalls instead of failing; `bdev_wait_for_examine` gets the long
  RPC timeout. Still open: iSCSI's `replacement_timeout` (120 s default) has the same effect for
  iSCSI hosts, and velod should report "recovering" rather than DEGRADED while volume stores load.
- [x] **P1-12. KubeVirt live migration** (was: no ReadWriteMany). Raw block volumes can be
  ReadWriteMany / ReadOnlyMany: one shared export per volume, nodes added and removed on the
  live subsystem (velod `shared` + `node_ids`); ReadWriteMany filesystems refused. Verified on
  NVW (064d85e): a Fedora VM on a Velocor RWX block disk migrated 3 times between nodes while
  writing one O_DIRECT write a second: 0 errors, slowest write 0.36 s, data intact, and the
  subsystem's hosts always exactly the current node afterwards. `TestSharedBlockVolumeAcrossNodes`.
- [x] **P2-9. CDI StorageProfile.** The chart's hook Job patches each Velocor class's profile
  (RWX block first, csi-clone, snapshot golden images) after install/upgrade, which also
  restores a profile reset by a recreated class; DataVolumes need no access/volume mode, and VMs
  get live-migratable disks by default (verified on NVW).
- [x] **P1-11. One stuck volume blocked the whole CSI node** (NVW, 2026-11-16): the node plugin
  serialized every stage/publish on one mutex, so a hung iSCSI format made the node's NVMe
  volumes time out too. Now per-volume locks (a busy volume answers Aborted, which the sidecars
  retry); `TestNodeVolumeLocksAreIndependent`. With it, from synology-csi's iSCSI handling:
  raw block devices are flushed (`blockdev --flushbufs`) before disconnect, and logout /
  disconnect failures are returned (the stage record is kept so kubelet retries the unstage)
  instead of being ignored.
- [x] **Slow operations were aborted halfway and left orphans** (velocor-1, 2026-11-16). Creating
  a pool on 2 × 500 GB took longer than the SPDK client's 15 s per-call timeout and veloctl's
  30 s: spdk_tgt finished the lvstore anyway, velod rolled back by deleting the RAID from under
  it, and the RAID hung half-deleted ("offline", both disks unusable until fixed by hand).
  Fixed: size-dependent SPDK methods get a 30 min timeout; mutating API requests are detached
  from the client connection (a disconnect no longer aborts a change); the pool rollback deletes
  the lvstore first; veloctl waits up to 30 min for changes; the CSI client follows the CSI
  request's deadline. The fake SPDK now models a RAID delete under a loaded lvstore the way
  spdk_tgt behaves (`TestPoolCreateRollbackAfterLateLvstore` fails with the old rollback).
- [x] **Upgrades silently failed on UEFI appliances.** systemd-gpt-auto-generator put an
  automount for the EFI System Partition on `/boot`, shadowing ostree's bind mount; at shutdown
  `ostree-finalize-staged` failed with "Remounting /boot read-write: Invalid argument" and the
  appliance rebooted into the old version (velocor-1, 2026-11-16; BIOS installs unaffected).
  The OS image now sets `systemd.gpt_auto=0` (bootc kargs.d) and masks the generator.
- [x] **P1-2.** A failed resume after a group snapshot left I/O paused forever: resume is now
  retried with backoff. (A velod crash between pause and resume can still leave a subsystem
  paused; SPDK doesn't expose pause state.)
- [x] **P1-7 (part).** `bootc switch` passed the image reference straight to bootc's command
  line: now validated.
- [x] Kernel-AIO backend for SSDs (`--ssd-bdev aio`), see P0-6.

### First review: critical (data loss / exposure)

- [x] **C1.** Reconciler could wipe pools on SPDK restart (created lvstores during examine) and
  re-created lost volumes as blank disks. The reconciler now never creates storage.
- [x] **C2.** iSCSI LUN masking broken: unique portal/initiator group tags, errors propagated,
  no implicit ANY, iSCSI targets restored after restart.
- [x] **C3.** Snapshot lvol names collided across volumes: SPDK names come from resource IDs;
  delete/CBT by UUID.
- [x] **C4.** SPDK errors swallowed while state was persisted: SPDK first, store second,
  rollback on failure, deletes SPDK-first; group snapshots fail instead of reporting ReadyToUse.
- [x] **C5.** CSI node could mount/format the wrong NVMe device: sysfs resolution by
  `subsysnqn` + nsid, namespace UUID must match the lvol UUID, strict `blkid`.
- [x] **C6.** Boot-disk guard gaps: LVM/LUKS/md resolution, any mounted/swap/held disk
  protected, multipath NVMe, stable SSD IDs from WWN/serial.
- [x] **C7.** CBT fabricated allocation data when SPDK lacked the RPC: now 501.
- [x] **C8.** CSI SnapshotMetadata route and field names didn't match the server.
- [x] **C9.** Boot guard failed open if the hardware manager couldn't start: velod now exits.
- [x] **C10.** Encryption/compression filter policies were silently ignored: now rejected.

### First review: high

- [x] **H1.** CSI publish/unpublish ignored node_id: exports are per (volume, node).
- [x] **H2.** CSI `PublishedNodeIds` returned initiator-group IDs.
- [x] **H3.** CSI DeleteVolume was blocked by snapshots; snapshots now outlive their source.
- [x] **H4.** RAIDs were created without superblocks.
- [x] **H5.** Lost updates from whole-record read-modify-write: atomic field-level `Mutate*`.
- [x] **H6.** Sizes were rounded down to MiB: now rounded up.

### First review: security

- [x] **S1.** Shell injection in container updates.
- [x] **S2.** JWTs not revocable: checked against the live user record, nanosecond `iat_ns`.
- [x] **S3.** Shared default admin password: random bootstrap password, forced change.
- [x] **S4.** Login rate limiting.
- [x] **S5.** Password hashing: PBKDF2-SHA256, 600k iterations.
- [x] **S6.** `/metrics` unauthenticated.
- [x] **S7.** Helm chart and `veloctl` skipped TLS verification by default; CA-cert support added.
- [x] **S8.** Request body limits and HTTP server timeouts.
- [x] **S9.** iSCSI node plugin overwrote the host's initiator name: dedicated iscsiadm iface.
- [x] **S11.** Resource names validated before use in SPDK names.
- [x] **S12.** vfio no-IOMMU enabled unconditionally: now an explicit opt-in.

### First review: operational

- [x] **M1.** SPDK auto-updated with the other containers (restarting the data plane).
- [x] **M2.** A bbolt write + fsync on every API-token request.
- [x] **M3.** Reconciler rewrote every record every 15 s.
- [x] **M4.** Partially created NVMe-oF subsystems weren't repaired.
- [x] **M5.** Group snapshots weren't crash-consistent: NVMe-oF subsystems paused.
- [x] **M6.** NVMe-oF listeners hard-coded to IPv4.
- [x] **M7.** No iSCSI rescan on volume expansion.
- [x] **M8.** Docs claimed a gRPC API that doesn't exist.

### Features and cleanup

- [x] Pre-release cleanup: all migration/compatibility code removed (no installed base).
- [x] Web UI rewritten in the nvweaver layout, with new APIs: `PATCH /exports/{id}`,
  `GET /exports/{id}/connections`, `PATCH /initiator-groups/{id}`, `GET /volumes/{id}/usage`,
  `warnings` in `/system/status`.
- [x] `veloctl`: `passwd`, `volume get|usage`, `export get|connections|access`,
  `initiator-group update`, `--cidrs`, `--ca-cert`, password prompts without echo.
- [x] Stateful fake SPDK (`spdktest`) and the real-SPDK integration harness.
