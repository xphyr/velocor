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

- [ ] **P0-2. Thin pools can be overcommitted without limit or warning.**
  Volume and clone creation never check pool capacity. When the pool fills, every thin and
  snapshot-backed volume in it starts failing writes at once, which typically corrupts host
  filesystems and databases. Snapshots keep old clusters alive, and nothing shows how much
  space they hold.
  *Needed:* an overcommit ratio per pool (refuse creation beyond it), a reserve so snapshots
  and metadata can't take the last free space, warning/critical thresholds as pool health and
  metrics, and per-volume/snapshot allocated space.

- [ ] **P0-3. The metadata database is a single point of failure, with no backup or recovery.**
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

- [ ] **P0-4. Hosts are told writes are durable when they may only be in a disk's volatile cache. (Verified)**
  In SPDK v26.09 the lvol module does not support FLUSH (`vbdev_lvol_io_type_supported`), and
  the RAID module always reports `write_cache = 0`. NVMe-oF advertises a volatile write cache
  only when the namespace's bdev reports one, so **every Velocor volume tells hosts there is no
  volatile write cache**. Hosts then never send FLUSH and treat a completed write as durable,
  but the member drives (NVMe without power-loss protection, nearly all SATA SSDs) do cache
  writes, so **a power cut can lose acknowledged writes**. Reproduced by
  `TestWriteCacheAndFlushAdvertised` (reported as a known issue until this is decided).
  *Options:*
  1. **Patch SPDK** (lvol: support FLUSH and pass it to the blobstore device; RAID: report
     `write_cache` if any member has one). The correct fix, worth upstreaming; nvweaver already
     carries an SPDK durability patch.
  2. **Turn the drives' volatile write caches off** at claim (NVMe Set Features VWC=0 via admin
     passthrough; `hdparm -W0` for SATA), trading write speed for safety.
  3. **Require power-loss-protected drives**: detect the volatile write cache at claim and
     refuse or loudly warn.

  *Recommendation:* 2 + 3 now (safe by default, trade-off visible per disk), then 1.
  **Decision needed.**

- [ ] **P0-6. A failed io_uring setup crashes `spdk_tgt`. (Verified)**
  Claiming an SSD with the io_uring backend where io_uring can't be set up (locked-memory
  limits, seccomp, `kernel.io_uring_disabled`) makes `spdk_tgt` segfault instead of returning an
  error, taking every export down. The production quadlet sets `memlock=-1`, which avoids the
  known cause, but nothing checks it. *Done:* an `aio` backend (`--ssd-bdev aio` /
  `VELOCOR_SSD_BDEV=aio`). *Needed:* velod should probe io_uring before using it and fall back
  to AIO; report the SPDK crash upstream.

### P1: outages, lock-outs, things operators can't see

- [ ] **P1-1. Nothing alerts anyone.** Degraded or faulted pools, SMART critical warnings, a
  filling pool or a stalled reconciler are visible only in the UI. `/metrics` exports capacity
  but no health. *Needed:* health gauges per disk/pool/volume/export (so Prometheus /
  Alertmanager can page), and optionally a webhook or e-mail notifier.

- [ ] **P1-3. Network changes can lock the appliance out.** `PUT /system/network/interfaces/{name}`
  applies at once; a wrong static address or disabling the management interface cuts the
  appliance off, and changing an interface that carries portals breaks the SAN silently.
  *Needed:* apply, then confirm from a new connection within ~120 s or roll back automatically;
  refuse or warn about interfaces that host portals.

- [ ] **P1-4. Deletion is immediate and irreversible.** With CSI automation and API tokens, one
  mistake is unrecoverable. *Needed:* per-volume deletion protection and/or a recycle period in
  which a deleted volume is unexported and hidden but kept for N hours.

- [ ] **P1-5. Orphaned SPDK objects are invisible.** Lvols, RAIDs or lvstores in SPDK but not
  in the store (failed rollback, crash between SPDK and store writes, manual changes) consume
  space silently, and after metadata loss they're the only copy of the data. *Needed:* the
  reconciler reports unmanaged objects (never deletes them).

- [ ] **P1-6. Single node, no high availability.** One appliance, one copy of each pool; ANA
  states are modelled but unused. A legitimate first-version scope, but it should be stated in
  the docs together with what hosts see during an appliance reboot (I/O stalls until
  `ctrl_loss_tmo`, then errors).

- [ ] **P1-7. OS lifecycle.** `bootc upgrade` runs inside the HTTP request (minutes) and should
  be a tracked background job. Reboot doesn't stop exports or SPDK cleanly first, and doesn't
  warn about connected hosts.

- [ ] **P1-8. A RAID1 missing a member at startup is not served. (Verified)** SPDK keeps a
  superblock RAID1 in `configuring` until every member is present; it does not start degraded.
  A disk that fails while the appliance is down makes the pool unavailable after boot (data
  intact; Velocor reports DEGRADED and re-creates nothing). Online failures degrade gracefully.
  *Needed:* a supported degraded start (SPDK investigation; possibly removing the absent member
  from the superblock with operator confirmation). Together with P0-1 this is "redundancy".

- [ ] **P1-9. Changed-block tracking is advertised but cannot work. (Verified)** SPDK v26.09
  has no `bdev_lvol_get_fragmap`. Velocor returns 501 instead of made-up extents, but the CSI
  driver still advertises the SnapshotMetadata service. *Needed:* stop advertising it (probe at
  startup) or add the RPC to the SPDK build (a small patch exposing a blob's allocated-cluster map).

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
  validated against the number of exports and connections.
- [ ] **P2-5. Slow test suites.** Auth/server tests take ~50–100 s under `-race` because every
  login runs 600k PBKDF2 iterations; consider a test-only iteration override (was F7).

### Suggested order of work

1. **P0-4 durability**: disable volatile write caches / require PLP (decision needed).
2. **P0-1 + P1-8 redundancy**: replace-disk with rebuild, and a supported degraded start.
3. **P0-2 capacity**: overcommit limits, reserve, thresholds.
4. **P0-3 metadata**: on-pool metadata copy, backups, recovery from disks.
5. **P1-1 alerting**: health metrics.
6. P1-3 network rollback, P1-4 deletion protection, P1-5 orphan reporting, P1-9 CBT, P0-6 io_uring probe.

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

**Not yet covered:** real NVMe drives (`bdev_nvme`, vfio), io_uring (needs a host where it
works, see P0-6), iSCSI data paths (needs an iSCSI initiator in the test image), power-loss
tests on real hardware, killing SPDK *during* a multi-step API operation, and hardware checks
of the boot-disk guard and CSI device resolution on NVMe native multipath.

---

## Done

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
  data (open follow-up: P1-9).
- [x] **R5.** SPDK validates NQN syntax; bad host NQNs now fail when entered instead of as an
  opaque "Internal error" at export time.

### Other second-review fixes

- [x] **P0-5.** SPDK was killed 10 s into every shutdown (podman default): `StopTimeout=120`.
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
