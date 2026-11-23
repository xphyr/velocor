# Velocor TODO

Open work only, grouped by when it's needed and ordered within each group. Finished items are
archived in [docs/review-history.md](docs/review-history.md) (IDs stay the same; an item that is
partly done keeps its ID here with "(rest)"), how things are tested is in
[docs/testing.md](docs/testing.md), and the direction beyond this list is in
[docs/ROADMAP.md](docs/ROADMAP.md).

An item is done when it's fixed **and** covered by a test; then it moves to the archive with its
commit. Keep entries short: the problem, where it stands, the next step.

---

## Direction change (2026-11-20): kernel data plane

Decided 2026-11-20 after P0-7 and the three-way benchmark: the data plane moves from SPDK to
md RAID + LVM thin + kernel nvmet/LIO. The reasons, the alternatives (incl. the hybrids and a
GPL port of md/LVM into SPDK), the numbers and the plan are in
[docs/design/kernel-dataplane.md](docs/design/kernel-dataplane.md). SPDK stays in the lab until
phase 3. Items below marked **(K)** are resolved by the move and close at the cutover; they stay
listed until then so nothing is lost if the plan changes.

- [x] **K-0. Design review** of the note (merged, PR #32). Decided 2026-11-20: QoS **dropped at the cutover**
  (revisit later); thin pools use **64 KiB chunks with zeroing on**, measured in
  [docs/performance.md](docs/performance.md) (best after snapshots, finest CBT; zeroing stays on
  so new volumes can't read old data).
- [x] **K-1. Spike** (done 2026-11-23): `internal/kernel`, a standalone package not yet wired
  into velod: md RAID, LVM thin (64 KiB, zeroing), a dm-linear wrapper per volume, group
  snapshots through the wrappers, CBT from `thin_delta`/`thin_dump`, nvmet and LIO through
  configfs, seams for optional LUKS/VDO/dm-crypt/dm-integrity layers. Unit tests on captured
  tool output; seven root integration tests (`make test-kernel`, CI job `kernel-tests`) pass on
  the lab VM, P0-7 included (EIO for that read only, array stays up, a rewrite heals it).
  Findings folded into the design note (§5.2, §5.2.1, §5.3, §5.5, §5.6, §6): `--errorwhenfull`
  needs `lvchange`; `N%VG` leaves no room for pool metadata; `pvcreate` starts an LVM devices
  file, so every LVM call carries velod's own `--config`; iSCSI portals must stay on
  SELinux-labelled ports (3260, 3205); md does not use the bad-block log on the last copy.
- [ ] **K-2. Parity**, starting with a **data-plane interface** in velod: velod calls SPDK
  directly in about 160 places and there is nothing to slot the kernel stack in behind, so K-2
  introduces that interface (modelled on `internal/kernel`'s API), moves the SPDK code behind
  it, then adds the kernel implementation. Then every row of the feature table (iSCSI via LIO, scrub on md, metadata copy
  on an LV, import, CBT via `thin_delta`, metrics, alerts, veloctl/UI wording, docs).
- [ ] **K-3. Validation:** CSI matrix on both clusters, KubeVirt, the P0-7 scenario (an
  unreadable sector on the last copy keeps the pool up), the benchmark, soak.
- [ ] **K-4. Cutover:** remove SPDK (images, patches, client, fake, CI stage); rebuild the lab
  appliances; archive the (K) items.


The first final release waits for these. Hardware validation (below) is part of the gate but
doesn't block the rest.

- [ ] **(K) P0-7. One unreadable sector on the last good copy takes a mirror pool offline.**
  Found 2026-11-20 with emulated disk errors (QEMU `blkdebug`, lab VM). SPDK v26.09's RAID1
  fails a member whenever none of the other copies can supply a read, and the rebuild process
  does the same on a source read error. So in a degraded mirror (one disk failed or being
  replaced), a host read or the rebuild hitting a single bad sector fails the **last** member:
  the RAID goes offline, the volume store closes, every volume in the pool is gone. After an
  SPDK restart the array re-assembles, velod restarts the rebuild, it hits the same sector, and
  the pool goes offline again; `start-degraded` doesn't apply (the RAID isn't assembled), so an
  operator can't even bring it up to copy the data off. Linux md fails just that read (EIO)
  and keeps the array running. Bad sectors during a rebuild are common on large drives.
  *Needed:* an SPDK patch so RAID1 fails the I/O, not the last member, on an unreadable read,
  and the rebuild records the unreadable range (counted, alerted, like the scrub's
  `unrecoverable_blocks`) and carries on instead of failing its source; velod stops
  restarting a rebuild that failed on source read errors and offers a way to bring such a pool
  online degraded; real-SPDK tests (the blkdebug lab recipe in
  [docs/testing.md](docs/testing.md) reproduces both cases). Scrub (P2-2) lowers the odds by
  rewriting bad sectors while both copies exist, but doesn't remove the failure mode.
- [ ] **P2-1. CHAP and real host identities.** Any host that can spoof an allowed NQN or IQN gets
  access (the API refuses `chap_user` rather than ignoring it). *Needed:* iSCSI CHAP auth groups
  and NVMe DH-HMAC-CHAP with secrets stored encrypted (and kept out of pool metadata copies, like
  today's CHAP fields); the CSI node plugin registers the host's real hostnqn / IQN instead of
  names derived from the node name.
- [ ] **B14. RPM packages in image builds are not pinned.** Base images are pinned (or rolling by
  decision, B17), but `dnf install` takes whatever the mirrors have. *Option:* RPM lockfiles
  (e.g. rpm-lockfile-prototype) for the SPDK builder and the OS image.
- [ ] **P2-16. Writes are slower than on 2026-11-18.** The 2026-11-19 baseline
  ([docs/performance.md](docs/performance.md)) has QD1 writes at about half and deep writes about
  20% lower than a day earlier, reads unchanged. Candidates: P0-4's write-cache handling, the
  built-in monitoring, the NAS behind the lab's virtual disks. *Next:* repeat the write jobs with
  monitoring off, then on the build of 2026-11-18, before calling it a regression.
  *Lead (2026-11-20):* the three-way benchmark ([docs/performance.md](docs/performance.md)), with
  monitoring **on**, got 3,705 QD1 random writes from the same pool through a client that
  connected to the SAN portal alone; the 2026-11-19 baseline went through the CSI driver, whose
  NVMe/TCP volumes then had two live paths (SAN and the 1500-MTU management network, P1-13
  check). Check whether the slow writes went over the management path; monitoring is cleared.
- [ ] **P1-6. State the single-node scope.** One appliance, one copy of each pool, no HA (ANA
  states are modelled but unused). Document it, with what hosts see during an appliance reboot
  (NVMe/TCP I/O stalls; the CSI driver never gives up the controller) and during an SPDK restart.
- [ ] **Hardware validation:** everything under [Waiting on real hardware](#waiting-on-real-hardware).
- [ ] **B16 (rest).** The pre-review `latest` tags on ghcr.io (build 977d4ae, 2026-11-14) stay
  until the first final release replaces them; check they did.

## Next

- [ ] **P2-2 (rest). Failing disks, emulated: RAID0 and repeat after P0-7.** Done on the lab
  VM (2026-11-20, dev-8311b3a, QEMU `blkdebug`): host reads over bad sectors are served from the
  other copy and the bad copy rewritten; a scrub repaired both bad windows (4096 blocks), failed
  the member whose repair write failed (pool DEGRADED, scrub paused), and the alerts fired
  (`VelocorScrubRepaired`, `VelocorScrubRepairFailed`, `VelocorPoolFaulted`). Found: P0-7, and
  two velod bugs fixed on the spot (archive). *Left:* a RAID0 scan counting unrecoverable
  blocks, and the rebuild cases again once P0-7 is fixed.
- [ ] **V-1. velod keeps stale member states when a RAID goes offline.** When SPDK fails the
  last member and the RAID bdev disappears, the pool turns UNAVAILABLE but its members still
  read ACTIVE / REBUILDING (with "rebuilding 0%"), because member states are only read from a
  RAID that exists. Show them as unknown or failed, and say why the pool is unavailable.
- [ ] **(K) L-7. Pools can't be expanded.** Evaluated 2026-11-20 against SPDK v26.09; *needs a
  decision.* (b) **Grow a RAID1 after every member was replaced by a larger disk** is small: a
  patch adding `bdev_raid_grow` (SPDK's own resize path, refused while degraded or rebuilding),
  then `bdev_lvol_grow_lvstore`; data doesn't move; both steps can be re-run after a crash.
  (a) **Add disks** needs a new pool layout (a concat of RAID1 pairs) and a patch appending a
  member to a concat: substantial, new pools only, growth by pairs. (c) spreading volumes over
  pools isn't expansion. *Recommendation:* (b) first; (a) as its own design if needed.
- [ ] **(K) L-6. A full pool should give hosts ENOSPC, not EIO.** SPDK fails writes to a full thin
  pool with NVMe Internal Error (EIO; filesystems shut down). Patch lvol ENOSPC to complete with
  NVMe Capacity Exceeded (0x81). Test with ext4 and XFS hosts.
- [ ] **(K, optional goodwill) U-1. Send our SPDK patches upstream** and drop each once merged: 0001 WRITE SAME fan-out
  ([guide](docs/upstream/spdk-scsi-write-same.md)), 0002 degraded start, 0003 group snapshot,
  0004 scrub, 0005 fragmap (coordinate with Longhorn, whose RPC it matches), and the uring
  channel crash ([report](docs/upstream/spdk-uring-channel-crash.md); carry it as a patch once
  `hack/spdk-patch-unit-tests.sh` accepts a module without unit tests).
- [ ] **P1-13 (rest). iSCSI multipath.** NVMe/TCP connects every listed portal; iSCSI logs in to
  one. Add dm-multipath on the node, and check two SAN paths in the lab for both protocols.
- [ ] **P1-14 (rest). NVMe drives moved to another PCIe slot** aren't relinked (their ID is the PCI
  address); relinking should also raise an event, not just a log line.
- [ ] **P2-13. virtio-blk disks are invisible.** Discovery drops `vdX` devices (`rotational=1`, no
  model). Treat `virtio_blk` as an SSD candidate; require a serial for pool members and say why
  a disk without one can't be used.
- [ ] **(K) P2-14. iSCSI splits 128K writes into two 64K disk writes** (177–209 vs 300 MB/s for
  NVMe/TCP). Find where (data segment or buffer size) and whether a larger
  `MaxRecvDataSegmentLength` or iobuf size avoids it.
- [ ] **P2-10 (re-measure in the kernel stack). Recovery after an unclean stop.** About 1 min per 500 GB on lab disks.
  Report a "recovering" state instead of DEGRADED while volume stores load; iSCSI hosts give up
  after `replacement_timeout` (120 s) where NVMe/TCP hosts wait, so raise it from the CSI node.
  Measure on real hardware before lowering the lvstore metadata reservation.
- [ ] **P2-3. Reconciler cost.** `EnsureExport` lists subsystems / target nodes per export
  (O(n²) RPCs); fetch once per pass.
- [ ] **P1-4 (rest).** A `deletionProtection` StorageClass parameter (clones from snapshots take
  another API path, so both need it); a retention period for snapshots without clones.
- [ ] **P1-3 (rest).** A pending network change is forgotten if velod restarts during its window
  (NetworkManager still rolls it back, but it can't be confirmed any more).
- [ ] **P1-7 (rest).** Exports aren't stopped before a reboot (SPDK's graceful stop closes them).
- [ ] **P1-1 (rest).** Alert delivery to a real email / webhook receiver is untested;
  Slack / PagerDuty-native receivers (webhook covers them through relays).
- [ ] **A-1. API audit log.** Who changed what and when (logins, creates, deletes, access and
  network changes), kept on the appliance and visible to administrators; listed as hardening
  in [docs/ROADMAP.md](docs/ROADMAP.md) and never done.
- [ ] **T-1. Test gaps.** iSCSI data paths in the integration tests (needs an initiator in the
  test image); killing SPDK in the middle of a multi-step API operation.

## Later

- [ ] **(K) L-1. Patch SPDK so flushes reach the drives** (the real fix for P0-4). lvols support FLUSH
  and pass it to the blobstore's device; RAID reports `write_cache` when a member has one, so
  NVMe-oF advertises a volatile cache and hosts send FLUSH/FUA. Then drive caches can stay on
  and the per-disk policy relaxes. Aim upstream; nvweaver's durability patch is prior art.
  `TestWriteCacheAndFlushAdvertised` is skipped as a known issue until then.
- [ ] **(K) P2-4. SPDK core sizing.** One core today; on lab disks a second gained nothing. Plan
  (2026-11-19): a boot step picks the core count (1 on a VM; about a quarter of the cores on
  hardware, never CPU 0, NUMA-local to NICs and NVMe), an operator override applied at the next
  SPDK restart, maybe SPDK's dynamic scheduler later. Tune on NVMe hardware.
- [ ] **(K) L-4. RAID10.** SPDK has no raid10; refused today. Build it as raid0 over raid1 pairs with
  superblocks; handle assembly after restart, a degraded pair, rebuild, deletion.
- [ ] **(K) L-3. Parity RAID (RAID5/6).** raid5f takes full-stripe writes only and can't hold a
  volume store. Options: a write-gathering layer, a read-modify-write RAID5 with a journal, or
  kernel md RAID5/6 underneath SPDK. Needs a design decision, then power-loss and rebuild tests.
- [ ] **P1-6 (feature). High availability.** Two appliances, ANA paths; after the release.
- [ ] **L-5. Delta OS upgrades.** Each `bootc upgrade` pulls most of the image (rolling base).
  Evaluate ostree static deltas, rechunking, zstd:chunked and OCI layer deltas; measure between
  two real builds; signing and a full-pull fallback.
- [ ] **L-2 (rest).** Retest CBT with a backup tool that uses the Kubernetes SnapshotMetadata API
  once one does (Kasten 9.0 doesn't; Velero, ...).
- [ ] **B24 (rest).** Move to Fedora 46 once it's released (not on request before); compare
  against the Fedora 45 baseline in [docs/performance.md](docs/performance.md).
- [ ] **T-2. Secure Boot** of the appliance image (BIOS and UEFI without it are tested).

## Waiting on real hardware

Blocked until suitable hardware is available; each entry is a ready test plan. The lab VM
covers the software paths (see [docs/testing.md](docs/testing.md)).

- [ ] **P0-1 (rest). Disk replacement on real drives.** Needs hot-swap bays. Pull a SATA/SAS SSD
  and an NVMe drive (surprise removal) under writes: pool DEGRADED, no host errors, replace,
  rebuild, reboot with the new member, the old drive stays detached.
- [ ] **P0-1 (rest). Consumer NVMe without NGUID/UUID.** Needs such a drive. Pool survives
  reboots (stable bdev UUID from serial + NSID).
- [ ] **P0-4 (rest). Power-pull test.** Needs a drive without power-loss protection and a way to
  cut power. Write with fsync under load, cut power, verify every acknowledged write, with the
  write-cache policy on and off.
- [ ] **P2-2 (rest). A drive that really fails.** Needs a drive with bad sectors. Scrub and
  rebuild against real read errors (after the emulated test under Next).
- [ ] **C5/C6 (rest). NVMe native multipath on hardware.** The boot-disk guard and CSI device
  resolution with multipathed NVMe devices.
- [ ] **P2-4 / P2-10 measurements.** SPDK core count and lvstore recovery time on NVMe hardware.

## Parked

Decided not to do for now; kept with the reason.

- [ ] **B15. CI compiles SPDK on every run** (Go-only PRs, `main`, tags). A cache keyed on the
  SPDK inputs and the rolling Fedora base would mostly hit within a day only; freezing SPDK's
  base would give up B17's daily fixes. Not worth it yet.
- [ ] **P2-5. Slow test suites.** Auth / server tests take ~50–100 s under `-race` because every
  login runs 600k PBKDF2 iterations; a test-only override would fix it.
