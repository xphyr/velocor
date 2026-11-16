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
  - [ ] Step 1 + 4 for NVMe owned by SPDK: Get/Set Features 06h through `bdev_nvme_send_cmd`
    (admin passthrough); until then the policy is refused for NVMe.
  - [ ] Pool-level flag in the pool API/UI (today: per-disk durability plus the status warning).
  - [ ] Step 5: power-pull test on real hardware.

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

- [ ] **P1-10. SPDK's iSCSI target stalls large writes (found on NVW, 2026-11-16).** After the
  login fix (d60e9ec), `mkfs.ext4` on an iSCSI volume hung: 19 writes in flight (the LUN allows
  4 MiB), never completed; the initiator aborted (`ABORT_TASK`: "abort task not supported"),
  reset the LUN and reconnected. The writes never reach the volume's bdev, so they stall in the
  iSCSI layer. NVMe/TCP over the same path and pool is fine. Suspects: the reduced iSCSI pools
  in `/etc/velocor/spdk.json` (`immediate_data_pool_size`/`data_out_pool_size` 512,
  `pdu_pool_size` 2048) with `-s 1024`, i.e. buffer starvation on large R2T/Data-Out writes.
  *Next:* reproduce on the lab host with an iSCSI initiator VM and vary pools/memory/burst sizes;
  add an iSCSI data-path scenario to the integration tests.
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
- [ ] **P2-7. The CSI driver logs nothing per request.** Stage/publish failures are only
  visible as Kubernetes events; the node plugin's log shows just its start line.
- [ ] **P2-8. CSI sidecar images are pinned by tag** (`csi-provisioner:v5.1.0`, ...), not digest.
- [ ] **P2-6. No health endpoint.** velod has no unauthenticated liveness/readiness route
  (found in the 2026-11-16 qcow2 boot test), so quadlets can't use `HealthCmd=` and monitoring
  can't tell "velod up but SPDK unreachable" from "all good". *Needed:* `/healthz` (process up)
  and `/readyz` (SPDK RPC answers, DB open), no auth and no detail leakage, plus `HealthCmd=` in
  the velod quadlet.

### Build, packaging and CI (review of 2026-11-16)

Everything here was written by Gemini and is in scope to rewrite.

- [ ] **B8 (rest). Publish a qcow2 with releases?** `make qcow2` now runs `hack/build-qcow2.sh`
  (rootless, KVM only; bootc-image-builder and its sudo are gone). Undecided whether CI should
  attach a qcow2 to releases (GitHub runners have KVM).
- [ ] **B14. RPM packages in image builds are not pinned.** Base images are pinned by digest, but
  `dnf install` takes whatever the Fedora mirrors have, so rebuilding a commit can produce
  different packages. *Option:* RPM lockfiles (e.g. rpm-lockfile-prototype) for the SPDK builder
  and the OS image.
- [ ] **B15. CI compiles SPDK on every run** (one compile per run, shared by the integration
  tests and the image). *Option:* reuse the previous `velocor-spdk` image when
  `Containerfile.spdk` hasn't changed.
- [ ] **B16. The new CI workflow has not run yet.** It passes actionlint and every step uses
  `make` targets verified locally, but the GitHub-side pieces (ghcr login, cosign, attestations,
  rootless podman with arm64 emulation on the runner, the PR local registry) are only proven by
  a first run. Also enable Renovate on the repository (`renovate.json`).
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

### Suggested order of work

1. **P0-4 durability, interim handling**: detect the write cache per disk, PLP declaration,
   optional cache disable, power-pull test (decided 2026-11-16; SPDK patch is L-1).
2. **P0-1 + P1-8 redundancy**: replace-disk with rebuild, and a supported degraded start.
3. **P0-2 capacity**: overcommit limits, reserve, thresholds.
4. **P0-3 metadata**: on-pool metadata copy, backups, recovery from disks.
5. **P1-1 alerting**: health metrics.
6. P1-3 network rollback, P1-4 deletion protection, P1-5 orphan reporting, P0-6 io_uring probe.

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
