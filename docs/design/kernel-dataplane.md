# Design: moving the data plane from SPDK to the Linux kernel storage stack

**Status:** decided 2026-11-20; design under review; no code yet.
**Decision:** Velocor's data plane moves from SPDK (RAID bdev, lvol, SPDK NVMe/TCP and iSCSI
targets) to the kernel's storage stack: md RAID, LVM thin provisioning, the kernel NVMe/TCP
target (`nvmet`) and LIO for iSCSI. velod, its API, the store, the CSI driver, the UI and
`veloctl` keep their shape; what changes is the implementation underneath the data-plane
interface. There is no installed base, so there is no data migration: the cut is clean.

This note records the discussion that led to the decision, the alternatives that were
rejected and why, the measurements, the target design, what we give up, and the plan. It is
deliberately long on the "why": the question will be asked again.

---

## 1. Where we were

Velocor's first design put everything in SPDK: drives claimed by SPDK (userspace NVMe driver
through vfio, or io_uring/AIO for SATA/SAS), redundancy by SPDK's `bdev_raid` (RAID0 and
RAID1 with superblocks), volumes by SPDK's lvol/blobstore (thin and thick, snapshots, clones,
QoS), exports by SPDK's NVMe-oF/TCP and iSCSI targets, and the on-pool metadata copy written
through SPDK's ublk. The attraction was a single, fast, userspace data path with no kernel
block layer in it, and one process to drive over JSON-RPC.

Two code reviews (2026-11-15/16) and five days of lab work turned that design into a working
appliance. They also produced a steady stream of problems that were SPDK's, not ours. By the
time of the decision we carried five SPDK patches and had six more items on the roadmap that
each needed another one:

| Problem | What we did | Kernel stack |
|---|---|---|
| iSCSI WRITE SAME deadlocked the target on thin volumes (P1-10) | patch 0001 | LIO handles it |
| RAID1 won't start with a member missing (P1-8) | patch 0002 | `mdadm --run` |
| No consistent group snapshot of volumes in use (P1-15) | patch 0003 | no primitive; see §5.3 |
| No scrub; no resync after an unclean stop (P2-2) | patch 0004, then a rate-limit fix | md `check`/`repair`, write-intent bitmap |
| No changed-block tracking (L-2) | patch 0005 | `thin_delta` |
| **One unreadable sector on the last good copy takes a mirror pool offline (P0-7)**, found 2026-11-20 with emulated disk errors; the auto-restarted rebuild then repeats it | would need patch 0006 | md fails that read (EIO) and keeps the array up; rewriting the block heals it (verified in the K-1 spike) |
| FLUSH never reaches the drives (P0-4), so hosts are told writes are durable while drives cache them | per-disk cache-off policy, PLP declaration; real fix deferred (L-1) | flush passes through dm and md |
| A full thin pool gives hosts EIO (L-6) | open | dm-thin `error_if_no_space`; nvmet maps ENOSPC to Capacity Exceeded |
| No RAID10 (L-4), no parity RAID (L-3: `raid5f` is full-stripe only and can't hold a volume store) | refused | md raid10, raid5/6 with PPL or journal |
| Pools can't grow (L-7) | design pending (needs a patch) | `mdadm --grow`, `pvresize`, `lvextend` |
| io_uring setup crashes `spdk_tgt` under SELinux (P0-6) | probe and fallback | n/a |
| 16 minutes of lvstore recovery after an unclean stop | 1 MiB clusters | thin-pool metadata check at activation |
| Metadata copy needs ublk, a helper process and crash recovery (P0-3) | built | write to an LV |
| Reactor sizing, hugepages, no `rpc.py` in the image, our own JSON-RPC tooling (P2-4) | documented | `iostat`, `bpftrace`, standard tools |

Four of the five patches disappear with the move and one (group snapshots) needs a different
implementation; six open roadmap items are solved by features md and LVM have shipped for a
decade. P0-7 was the trigger: a data-loss class failure in the RAID layer, in a case (a bad
sector during a rebuild) that is common on large drives, that we would have had to design and
patch ourselves, with the scrub only lowering the odds. The pattern was clear: SPDK's RAID
module is a fine piece of engineering with a short history, and we had become its maintainers.

## 2. The discussion

### 2.1 What SPDK was buying us

Per-core efficiency and tail latency from polling, and on real NVMe a userspace driver with no
kernel in the path. For a small appliance (one SPDK core on a four-vCPU VM) that efficiency
matters, and SPDK's NVMe/TCP target does more IOPS per core than the kernel's. We did not want
to give that up without measuring it.

### 2.2 Options

**A. Stay on SPDK end to end.** Keep writing BSD-licensed patches to `bdev_raid` (P0-7 next;
then bad-block handling, a resync bitmap, grow, RAID10, and eventually a read-modify-write
RAID5 with a write-hole journal, which SPDK's maintainers avoided on purpose and which is a
multi-month project). Known quantity, best peak numbers, but we own the redundancy layer.

**B. Hybrid, SPDK target on top.** Kernel md and LVM underneath; SPDK's NVMe/TCP and iSCSI
targets on top through `bdev_uring` or `bdev_aio`. Keeps velod's export code, QoS, per-bdev
stats and the quiesce primitive for group snapshots. Rejected after measurement (§3): the
hybrids had the fewest IOPS and the worst tails of the three stacks, and SPDK's own capability
report showed `bdev_uring` passes only reads, writes and write-zeroes (no FLUSH, no discard, so
thin volumes behind it could never reclaim space) and `bdev_aio` adds FLUSH but still no
discard. The hybrid also keeps the parts of SPDK that hurt most: the iSCSI target (P1-10,
P2-14, no SCSI persistent reservations) and a single process whose restart drops every export
(a 72 s stall measured under Kubernetes I/O).

**B'. Hybrid, md under lvol.** Kernel md only, SPDK lvol and target above it. Retires the RAID
problem while keeping lvol's features. Same kernel hop as B, so the same performance penalty
(the benchmark priced it), the same discard gap, and all of SPDK's operational baggage: the
worst of the three.

**C. Kernel end to end.** md, LVM thin, nvmet, LIO. No SPDK, no hugepages, no vfio, no ublk, no
reactor sizing; exports survive a velod restart because there is no data-plane process to
restart; standard tooling. Costs: lower peak write IOPS on a mirror (§3), a whole set of
features to re-implement (§5), group snapshots need our own quiesce (§5.3), and QoS has no
kernel equivalent (§6). **Chosen.**

**D. Port md and LVM into SPDK as out-of-tree modules.** Raised because md and dm are GPL and
the concepts are known. Rejected: (1) a GPL module linked into `spdk_tgt` is distributable but
puts the data-plane image under the GPL, next to an Apache-2.0 control plane, and can never go
upstream (SPDK accepts BSD only), so it would be a permanent fork beside SPDK's own raid
module; (2) it isn't a port: md and dm are written against the kernel block layer (`bio`
chaining, workqueues, spinlocks, sysfs) and SPDK against `bdev_io`, one thread per reactor and
pollers, so almost nothing carries across mechanically; it would be a rewrite that keeps the
design and the on-disk format, which is what patch 0004 already was; (3) the value of md is not
its design, which SPDK's raid module already copies in outline, but twenty years of fixes for
cases nobody designed for, and neither a GPL module nor a clean-room rewrite brings that
history along; (4) dm-thin's metadata engine would replace lvol, not extend it, losing
everything built on lvol; and lvm2's management layer is what velod already is.

### 2.3 Why a benchmark, and how

Everything above was argument. The one thing that could settle it was measuring, and the lab
has no real NVMe or fast NICs: only a VM appliance on virtual disks and a 10 GbE SAN. So the
comparison was built to be fair rather than absolute: the same appliance, two identical pairs
of disks on the same NAS, the same client connecting every target the same way, the same fio
job table, three rounds. Full method and tables in
[docs/performance.md](../performance.md), section "2026-11-20: SPDK against the kernel data
path (three ways)".

## 3. What the benchmark said

Medians of three rounds; QD1 = one job, queue depth 1; "deep" = 4 jobs × queue depth 16.

| | SPDK thin | SPDK thick | kernel thin | kernel thick | hybrid uring | hybrid aio |
|---|---|---|---|---|---|---|
| QD1 random read, mean / p99 µs | 543 / 905 | 549 / 946 | 552 / 922 | 530 / 897 | 573 / 1679 | 541 / 913 |
| QD1 random write, mean / p99 µs | 245 / 627 | 243 / 733 | 245 / 618 | 230 / 618 | 291 / 1597 | 247 / 692 |
| deep random read IOPS | 44.9k | 45.0k | 33.2k | 39.5k | 29.2k | 25.7k |
| deep random write IOPS | 29.5k | 28.6k | 18.1k | 19.7k | 22.2k | 21.1k |
| mixed 70/30 IOPS (r / w) | 18.8k / 8.1k | 19.7k / 8.5k | 16.3k / 7.0k | 18.5k / 8.0k | 15.7k / 6.8k | 14.8k / 6.4k |
| sequential write MB/s | 535 | 572 | 470 | 483 | 505 | 556 |

Follow-up, nvmet over the same two disks with nothing else in the way: a single raw disk gives
34.3k deep writes / 47.2k deep reads; bare md RAID1 21.3k / 43.6k; md RAID10 21.3k / 42.2k;
md RAID1 without its bitmap the same. So:

- **QD1 latency is identical for every stack.** The network and the virtual disks set the
  floor; the stack doesn't show (the `bdev_uring` hybrid's p99 is the one exception).
- **Deep reads are even** between SPDK and kernel thick (45k vs 39.5k, bare md 43.6k). Thin LVs
  cost about 15%; SPDK's thin lvol costs nothing measurable.
- **Deep writes are the kernel's one deficit: ~20k against SPDK's ~29.5k on a mirror.** It is
  md's mirror write path specifically: not nvmet (34k on a raw disk), not LVM (bare md gives the
  same), not the bitmap, not the RAID level.
- **CPU:** at depth SPDK does about 30% more IOPS per unit of CPU actually spent, but it holds
  one vCPU (a quarter of this appliance) polling while idle; the kernel stack idles at a few
  percent.
- **The hybrids are the worst of both** and lack flush/discard passthrough (§2.2 B).
- Sequential read was not usable on this lab (the NAS behind the virtual disks swung the same
  device between 186 and 640 MB/s), and real NVMe, where SPDK's userspace driver should shine,
  could not be measured.

**Reading:** the kernel stack is in the same class. The price of the move is roughly 28% of
deep random-write IOPS on a mirror and some CPU efficiency at high queue depth; the gain is
everything in §1. For a storage appliance whose first duty is not losing data, and whose
customers' workloads over a 10 GbE fabric are nowhere near either stack's ceiling, that is the
right trade. If peak mirror write IOPS on NVMe ever becomes the metric Velocor must win, this
decision should be revisited with real hardware; the benchmark method is in the repo.

## 4. Target architecture

```
hosts  ── NVMe/TCP ──>  nvmet-tcp  ─┐                       ┌─ thin LV ─┐
hosts  ── iSCSI   ──>  LIO (iscsi) ─┼─> dm-linear wrapper ──┤ (lvm2 thin pool) ├─ md RAID1/10 ─> drives
                                    │   per volume (velod)   └─ thick LV ─┘      (later 5/6)
velod ── mdadm / lvm2 / dmsetup / configfs ──^
```

- **Drives** stay on their kernel drivers. No vfio, no hugepages. Write-cache control (P0-4)
  stays as information and policy; see §5.1 for why it relaxes.
- **Pool = one md array** (metadata 1.2, write-intent bitmap) carrying one LVM
  volume group with one thin pool and any thick LVs. RAID1 and RAID10 at first; RAID5/6 with
  PPL (or a journal device) is now a configuration choice rather than a project (L-3).
- **Volume = an LV**, thin by default, wrapped in a trivial dm-linear device that velod owns
  (`/dev/mapper/velocor-<volume-id>`). The wrapper gives a stable device name independent of
  LVM activation, the quiesce point for group snapshots (§5.3), and a place for per-volume
  dm-crypt or dm-integrity later.
- **Exports** are nvmet subsystems (one per volume, allowed hosts, ANA groups per portal) and
  LIO iSCSI targets (TPGs, ACLs, LUNs), both configured through configfs by velod directly, no
  `nvmetcli`/`targetcli` dependency.
- **The on-pool metadata copy (P0-3)** is a small thick LV written with plain O_DIRECT I/O: the
  ublk device, helper process and recovery logic go away.
- **velod** runs as today (a privileged container in the host PID namespace) and executes the
  storage tools in the host's mount namespace (`nsenter -t 1 -m`): mdadm, lvm2 (JSON output),
  dmsetup, thin tools. The bootc image already ships them. Alternative, if the namespace dance
  proves awkward: run velod as a host service from the image; the API doesn't care.
- **Boot:** velod assembles and activates. The image disables md auto-assembly (`AUTO -all`)
  and LVM auto-activation, and sets the LVM filter to md devices only, so nothing races the
  reconciler or activates a pool velod has decided to hold back (disabled exports, degraded
  start).
- **Container images:** `velocor-spdk` and its patches, the SPDK unit-test stage and
  `hack/spdk-patch-unit-tests.sh` are retired at cutover (§8).

## 5. Feature mapping

| Today (SPDK) | Kernel stack | Notes |
|---|---|---|
| Claim / release a disk (uring, nvme bdev) | nothing to do; `mdadm --zero-superblock` / wipefs on release | the boot-disk guard and discovery stay |
| Pool create (RAID0/1), 1 MiB clusters | `mdadm --create`, `pvcreate`, `vgcreate`, thin pool | chunk size §6 |
| Member states, rebuild %, replace, start degraded, release detached (P0-1, P1-8) | `/proc/mdstat`, `/sys/block/md*/md/{sync_action,sync_completed,dev-*/state}`, `mdadm --add/--replace/--fail/--remove/--run` | md remembers failed members in its superblock; a returning disk is never re-added without velod |
| Scrub (patch 0004) | `echo check|repair > sync_action`, `mismatch_cnt`, `sync_speed_max` | monthly schedule, window, resume: unchanged in velod; md keeps progress in the bitmap |
| Moved disks (P1-14), import (P0-3) | md UUIDs (`mdadm --examine`), `vgimportdevices` / `vgchange -ay` | simpler: md matches by UUID itself |
| Thin / thick volumes, resize | `lvcreate -V` / `-L`, `lvextend`; nvmet `revalidate_size`, LIO re-reads size | online |
| Snapshots, clones, delete | `lvcreate -s`, thin snapshots are writable (clone = snapshot); `lvremove` | lineage is in thin metadata; velod's records as today |
| Group snapshot (patch 0003) | suspend wrappers, snapshot each LV, resume (§5.3) | |
| CBT fragmap (patch 0005) | `thin_delta` / `thin_dump` over a pool metadata snapshot (§5.4) | granularity = chunk size |
| QoS (`bdev_set_qos_limit`) | none | §6 |
| NVMe/TCP export, ACL, ANA, portals | nvmet subsystem, `allowed_hosts`, `ana_groups`, ports | TLS (`addr_tsas`), DH-HMAC-CHAP available for P2-1 |
| iSCSI export, portal groups, initiator groups | LIO target, TPGs, ACLs, LUNs; CHAP | persistent reservations come free |
| Export connections list | nvmet has no per-controller listing in configfs | §6 |
| Flush / write cache (P0-4, L-1) | passes through; nvmet advertises VWC when the device has a cache | L-1 closes |
| Discard | passes through dm-thin (reclaims) to md to drives | `issue_discards` in lvm.conf |
| Full pool | `error_if_no_space=1` on the thin pool (`lvchange --errorwhenfull y`; `lvcreate` refuses the flag for a thin pool) → ENOSPC → NVMe Capacity Exceeded | L-6 closes; thin **metadata** full is a new failure mode (§6) |
| Grow (L-7) | replace members with larger disks → `mdadm --grow --size max` → `pvresize` → `lvextend` the thin pool; or `mdadm --grow --raid-devices` on RAID10 | |
| RAID10 (L-4), parity (L-3) | md levels 10, 5, 6 (`--consistency-policy=ppl` or a journal) | a configuration choice |
| Bad sector on the last copy (P0-7) | md returns EIO for that I/O and keeps the array; rewriting the block heals it | the acceptance test for the new stack; passes in the spike (`TestLastCopyReadErrorKeepsThePool`) |
| Unclean shutdown | write-intent bitmap resync | SPDK had none |
| Encryption / compression (policies refused today) | dm-crypt per volume (in the wrapper slot), dm-vdo or none | later |
| Metrics | `/sys/block/*/stat`, `/proc/mdstat`, thin pool `status` (used/metadata), nvmet/LIO configfs | same exporter shape |
| Metadata copy (ublk) | O_DIRECT writes to a thick LV | P0-3 logic simplified |
| Recovery after unclean stop | thin pool `thin_check` at activation (seconds), md resync in the background | P2-10 re-measured |

### 5.1 Durability

Flush and FUA now reach the drives through dm and md, and nvmet advertises a volatile write
cache when the device has one, so hosts send flushes and get durable semantics with the cache
on. The per-disk write-cache policy (P0-4) stays as an option and the PLP declaration as
information; the default becomes "leave caches on". The power-pull test stays on the hardware
list.

### 5.2 Disk identity and assembly

md identifies members by UUID in the superblock, so disks that move between controllers or
change kernel names assemble without help (P1-14 becomes mostly unnecessary; velod still has to
map the new disk IDs onto its records). The LVM devices file is turned off in favour of a
filter on `/dev/md*`, because velod decides which disks are Velocor's. This has to be done per
call, not left to the host's `lvm.conf`: `pvcreate` quietly starts a devices file
(`/etc/lvm/devices/system.devices`) when none exists, after which LVM ignores every device not
listed in it. velod therefore passes its own `--config` on every LVM command (no devices file,
md-only filter).

The md arrays are created on kernel 7.x with metadata that warns it won't assemble on kernels
6.18 or older (configurable logical block size). That only matters if pool disks are moved to an
older system; the appliance image never goes back.

### 5.2.1 Sizing the thin pool

`lvcreate --extents 100%VG` (or any `N%VG`) for the thin pool leaves no room for its metadata LV
and the spare metadata copy LVM keeps for repair, and on a small VG trips an lvm2 assertion.
velod computes the data size itself from `vgs` (VG size and extent size), minus the metadata and
spare, and passes explicit sizes.

### 5.3 Consistent group snapshots

The kernel has no "quiesce these devices" primitive and `lvcreate -s` suspends and resumes the
origin itself, so a group can't be made consistent by suspending LVs. The wrapper layer gives
the primitive: velod suspends every member volume's dm-linear wrapper (`dmsetup suspend`, which
waits for in-flight I/O and queues new I/O, with velod's own timeout as in patch 0003), takes
`lvcreate -s` on each LV (no I/O is in flight to them), then resumes every wrapper. A wrapper
left suspended by a velod crash is resumed by the reconciler at start, which is the same
mitigation the SPDK design had. The SPDK integration scenario (`TestGroupSnapshots`: a group
taken under dependent writes is one point in time) becomes the acceptance test.

Spike result: this works as designed. LVM's md-only filter means `lvcreate -s` never opens the
suspended wrapper, so there is no deadlock; 8 of 8 group snapshots taken under ~37k dependent
write pairs were consistent, at about 200 ms per group (`TestGroupSnapshotConsistent`).

### 5.4 Changed-block tracking

`dmsetup message <pool> 0 reserve_metadata_snap`, then `thin_delta --snap1 <id> --snap2 <id>`
(or `thin_dump --dev-id` for a single snapshot's mappings) against the pool's metadata device,
then `release_metadata_snap`. Thin device IDs come from `lvs -o thin_id`. One metadata snapshot
at a time, serialized by velod. Output granularity is the pool's chunk size. The CSI
SnapshotMetadata layer above velod's `blocks:allocated` / `blocks:changed` stays as it is.

### 5.5 Optional layers

The volume stack, bottom to top, is md array, [pool feature], PV/VG, thin pool, LV, [volume
layers], dm-linear wrapper, export. The wrapper is always on top, so group snapshots and the
targets don't depend on what sits underneath. `internal/kernel/stack.go` has two seams, both
off unless configured and none implemented yet:

- **PoolFeature**, pool-wide: LUKS between md and the PV, or VDO deduplication and compression
  of the thin pool's data (`lvcreate --type thin-pool --pooldatavdo y`).
- **VolumeLayer**, per volume between the LV and the wrapper: dm-crypt with a per-volume key, or
  dm-integrity checksums.

The OS image already has `dm-vdo`, `dm-crypt`, `dm-integrity` and `cryptsetup`; VDO also needs
`vdoformat` from Fedora's `vdo` package.

### 5.6 Spike results (K-1)

`internal/kernel` implements the stack as a standalone package (not wired into velod), with unit
tests on output captured from the real tools and seven root integration tests
(`internal/kernel/integration_test.go`) that pass on the lab VM: exports over NVMe/TCP and
iSCSI with allowed and refused hosts, restore after a simulated reboot, rebuild onto a
replacement, consistent group snapshots (§5.3), exact CBT from `thin_delta`, ENOSPC within about
0.3 s on a full pool, and P0-7. On P0-7, md does not fail the last in-sync member of a RAID1:
the unreadable sector fails only that read with EIO, the array stays active, and writing the
block again heals it. (It does not record the sector in the bad-block log in that case, contrary
to what this note first assumed.)

## 6. What we give up or must decide

- **Deep random-write IOPS on a mirror:** about 28% lower than SPDK RAID1 at queue depth 64 on
  this lab (20k vs 29.5k). Accepted. Worth re-measuring on real hardware.
- **CPU efficiency at depth:** SPDK does ~30% more IOPS per CPU spent; in exchange a vCPU
  stops polling while idle. Accepted.
- **Userspace NVMe driver:** gone. Unmeasured on this lab; the one place SPDK clearly wins.
- **QoS limits:** nvmet submits I/O from kernel context, so cgroup `io.max` can't see it and
  there is no per-device throttle in the kernel target. **Decided (2026-11-20): dropped at the
  cutover**, the API's QoS fields refused with a clear message; revisit later (a small dm target
  is the likely route). Nothing in the lab or the CSI path uses QoS.
- **Connections per export:** nvmet doesn't list controllers in configfs; the UI's connections
  view becomes "connected hosts" from the network side (`ss` on the port) for NVMe/TCP, exact
  for iSCSI (LIO lists sessions). Accepted, noted in the API docs.
- **Thin pool metadata is finite** (16 GiB maximum) and a full metadata device is worse than a
  full data pool. velod sizes it generously, monitors it (`thin pool status`), alerts, and grows
  it while the pool has room; `lvm.conf` autoextend is a backstop.
- **Chunk size and zeroing: 64 KiB, zeroing on** (measured 2026-11-20,
  [performance.md](../performance.md) "LVM thin chunk size and zeroing"). Steady writes are the
  same at 64, 128 and 256 KiB; writes after a snapshot are best at 64 KiB (5.5k IOPS against
  3.3k at 256 KiB, half the p99), since the first write to a shared chunk copies the whole
  chunk; zeroing halves first writes into new space but must stay on, or a partial write into a
  new chunk exposes whatever a deleted volume left there. nvweaver (a sibling project on LVM
  thin with the same `thin_delta` CBT) had shown CBT is exact at any size but over-reports in
  whole chunks, which 64 KiB keeps smallest. Metadata at 64 KiB is about 2 MB per GiB written,
  so velod sizes the metadata device from the pool, with headroom, and monitors it.
- **iSCSI access is by IQN only (decided 2026-11-23).** LIO's ACLs name initiators; it has no
  per-initiator address filter. SPDK enforced a host's IQN and its address together; the kernel
  data plane enforces the IQN, doesn't enforce the address (which the CSI driver registers for
  every host), and refuses an export whose ACL has addresses but no IQN rather than open it.
  Address filtering, if wanted, would be host firewall rules per export.
- **iSCSI portals must use SELinux-labelled iSCSI ports (3260, 3205).** An enforcing
  initiator's iscsid (`iscsid_t`, RHCOS nodes included) can't connect to any other port
  (EACCES). velod should refuse other ports for iSCSI portals, or at least warn.
- **Two license regimes avoided:** all kernel pieces are used, not linked; velod stays
  Apache-2.0.

## 7. Testing

- **Unit tests:** the fake SPDK (`internal/spdk/spdktest`) is replaced by a fake data-plane
  command layer: recorded `mdadm`/`lvs --reportformat json`/sysfs trees with the same stateful
  model (members, rebuilds, scrubs, snapshots, failures). This is the largest single piece of
  the rewrite and the one that keeps velod's test suite meaningful.
- **Integration tests:** today's run rootless with file-backed disks; md and dm need
  `CAP_SYS_ADMIN` and block devices. The harness is a Go test binary (build tag
  `kernelintegration`) run as root against loop devices on sparse files, on the CI runner or
  the lab VM (`make test-kernel`); no privileged container is needed. The scenarios stay the
  same list: crash/restart, rebuild, missing at boot, scrub repair, group snapshot under
  writes, capacity to FULL, pool import, and the P0-7 case (an unreadable sector on the last
  copy) with `dm-dust` standing in for QEMU's `blkdebug`.
- **Lab:** the `blkdebug` recipe in [testing.md](../testing.md) and the CSI matrix on both
  clusters (x86 and Arm) unchanged; the three-way benchmark method for before/after numbers.

## 8. Plan

Phases, each ending in a merge; the SPDK stack keeps running in the lab until phase 3.

0. **Design review** (this note); QoS dropped and chunk size 64 KiB with zeroing on (both decided 2026-11-20).
1. **Spike** (a standalone package; the data-plane interface in velod comes first in phase 2, since velod calls SPDK directly today): md RAID1 + thin LV + nvmet export, enough for
   `TestDataSurvivesCrashAndRestart`, a rebuild and a group snapshot; the fake command layer
   for unit tests; the root integration harness in CI. Done as K-1 (§5.6). Exposes the real risks (§5.3,
   §5.4, §7) with code, not prose.
2. **Parity:** everything in §5 including iSCSI through LIO, scrub scheduling on md, metadata
   copy on an LV, pool import, CBT, metrics and alerts; `veloctl` and UI wording; docs.
3. **Validation:** CSI matrix on both clusters, KubeVirt, the P0-7 scenario, the benchmark,
   soak with the lab's pools.
4. **Cutover:** remove SPDK from the images (patches, `Containerfile.spdk`, hugepages, vfio,
   ublk, the SPDK unit-test stage and CI job), the SPDK client and fake, and the SPDK-specific
   TODO items; rebuild the lab appliances (pools don't convert; there is no installed base).

Rough size: comparable to P0-1, P0-3 and P2-2 together.

## 9. What this closes and what stays

Closed or resolved by the move: P0-7, L-1, L-3, L-4, L-6, L-7 (all four become configuration
or standard tool calls), P2-4 (SPDK sizing), P2-14 (SPDK iSCSI write split), the ublk parts of
P0-3, U-1 (upstreaming our patches becomes optional goodwill). Unchanged: P2-1 CHAP (now with
kernel DH-HMAC-CHAP and LIO CHAP), P1-13 iSCSI multipath, P2-13 virtio-blk discovery, B14, the
real-hardware list, P1-6 and L-5. Re-measured in the new stack: P2-10 recovery time, P2-16.

## 10. References

- [docs/performance.md](../performance.md): the three-way benchmark and its follow-ups.
- [docs/review-history.md](../review-history.md): the SPDK-era items by ID.
- [docs/testing.md](../testing.md): the `blkdebug` recipe that found P0-7.
- Earlier designs this supersedes in part: [P0-1 disk replacement](p0-1-disk-replacement.md),
  [P0-3 metadata recovery](p0-3-metadata-recovery.md) (the ublk mechanism), and the SPDK
  patches in `deploy/containers/spdk-patches/`.
