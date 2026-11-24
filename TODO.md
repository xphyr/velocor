# Velocor TODO

Open work only, grouped by when it's needed and ordered within each group. Finished items are
archived in [docs/review-history.md](docs/review-history.md) (IDs stay the same; an item that is
partly done keeps its ID here with "(rest)"), how things are tested is in
[docs/testing.md](docs/testing.md), and the direction beyond this list is in
[docs/ROADMAP.md](docs/ROADMAP.md).

An item is done when it's fixed **and** covered by a test; then it moves to the archive with its
commit. Keep entries short: the problem, where it stands, the next step.

---

## Kernel data plane (K-4, the last phase)

The data plane is the kernel's (md, LVM thin, nvmet, LIO) since 2026-11-24; SPDK is gone. K-0 to
K-3 and what each found are in [docs/review-history.md](docs/review-history.md), the design in
[docs/design/kernel-dataplane.md](docs/design/kernel-dataplane.md).

- [ ] **K-4 (rest).**
  - the soak (12 h, then 24 h) on both clusters with the fixes below;
  - rebuild the P0-1 lab VM, which still runs SPDK;
  - *decision:* pool durability. Flushes and FUA writes now reach the drives, so a volatile write
    cache no longer loses writes a host flushed: the AT_RISK rating and the write-cache policy
    (P0-4) can become information instead of a warning;
  - *decision:* [docs/upstream](docs/upstream) (our SPDK findings): keep as a record, send, or
    delete;
  - the `thin` StorageClass parameter goes with the next chart that changes the classes anyway (a
    class's parameters can't be changed by an upgrade).
- [ ] **K-3 (rest). Performance to look at on real hardware:**
  - iSCSI 4k random read 23.6k IOPS (SPDK 34.3k): LIO runs a connection in one receive thread,
    and the host opens one session. More sessions per volume (P1-13) would spread it.
  - NVMe/TCP 128k sequential read 425-485 MiB/s with CPUs and disks mostly idle (2.7 GB/s locally):
    one TCP flow with an inflated RTT. Re-measure on real NICs before digging further.
  - the gain of `init_on_alloc=0` isn't measured yet.
  - Lab: a multiqueue virtio-net for velocor-1's SAN NIC would make it behave like a real NIC.

## Review of 2026-11-24

An independent review of main (5897481): 47 findings and 19 performance items. All findings are
fixed, gone with SPDK, or listed here ([docs/review-history.md](docs/review-history.md)).

- [ ] **Multi-namespace NVMe drives** aren't supported: discovery, claims and the disk's link use
  one namespace. Decide: support them, or refuse them with a reason.
- [ ] **Performance, not done:** `Connections` runs `ss` per portal (on request only); by-name
  lookups read whole buckets; the metadata manifest is built every pass; CSI's controller lock
  is global and `reserveSpace`'s spans pools (both hide races: tests first); Prometheus queries
  for graphs run one after the other.

## Before the first final release

The first final release waits for these. Hardware validation (below) is part of the gate but
doesn't block the rest.

- [ ] **P2-1. CHAP and real host identities.** Any host that can spoof an allowed NQN or IQN gets
  access (the API refuses `chap_user` rather than ignoring it). *Needed:* iSCSI CHAP auth groups
  and NVMe DH-HMAC-CHAP with secrets stored encrypted (and kept out of pool metadata copies, like
  today's CHAP fields); the CSI node plugin registers the host's real hostnqn / IQN instead of
  names derived from the node name.
- [ ] **B14. RPM packages in image builds are not pinned.** Base images are pinned (or rolling by
  decision, B17), but `dnf install` takes whatever the mirrors have. *Option:* RPM lockfiles
  (e.g. rpm-lockfile-prototype) for the OS image.
- [ ] **P1-6. State the single-node scope.** One appliance, one copy of each pool, no HA (ANA
  states are modelled but unused). Document it, with what hosts see during an appliance reboot
  (NVMe/TCP I/O stalls; the CSI driver never gives up the controller).
- [ ] **Hardware validation:** everything under [Waiting on real hardware](#waiting-on-real-hardware).
- [ ] **B16 (rest).** The pre-review `latest` tags on ghcr.io (build 977d4ae, 2026-11-14) stay
  until the first final release replaces them; check they did.

## Next

- [ ] **P2-2 (rest). Failing disks, emulated: RAID0 and repeat after P0-7.** Done on the lab
  VM (2026-11-20, SPDK, QEMU `blkdebug`): host reads over bad sectors are served from the
  other copy and the bad copy rewritten; a scrub repaired both bad windows (4096 blocks), failed
  the member whose repair write failed (pool DEGRADED, scrub paused), and the alerts fired
  (`VelocorScrubRepaired`, `VelocorScrubRepairFailed`, `VelocorPoolFaulted`). On md: the bad-sector and rebuild cases (K-3). *Left:* the
  scrub's repair of a bad sector and a failing repair write on md.
- [ ] **V-1. Stale member states of a pool whose array is gone.** With none of its disks there, the
  pool is UNAVAILABLE but its members still read ACTIVE (their states are read from the array).
- [ ] **L-7. Pools can't be expanded.** On md: (b) grow a RAID1 after every member was replaced by a
  larger disk (`mdadm --grow --size=max`, `pvresize`, extend the thin pool); (a) add disks (a
  second array as another PV of the group, or a reshape). *Recommendation:* (b) first.
- [ ] **P1-13 (rest). iSCSI multipath.** NVMe/TCP connects every listed portal; iSCSI logs in to
  one. Add dm-multipath on the node, and check two SAN paths in the lab for both protocols.
- [ ] **P1-14 (rest). NVMe drives moved to another PCIe slot** aren't relinked (their ID is the PCI
  address); relinking should also raise an event, not just a log line.
- [ ] **P2-13. virtio-blk disks are invisible.** Discovery drops `vdX` devices (`rotational=1`, no
  model). Treat `virtio_blk` as an SSD candidate; require a serial for pool members and say why
  a disk without one can't be used.
- [ ] **P2-10 (rest). Recovery after an unclean stop** on the kernel stack: measure (md resyncs dirty
  regions by its bitmap). iSCSI hosts give up after `replacement_timeout` (120 s) where NVMe/TCP
  hosts wait, so raise it from the CSI node.
- [ ] **P1-4 (rest).** A `deletionProtection` StorageClass parameter (clones from snapshots take
  another API path, so both need it); a retention period for snapshots without clones.
- [ ] **P1-3 (rest).** A pending network change is forgotten if velod restarts during its window
  (NetworkManager still rolls it back, but it can't be confirmed any more).
- [ ] **P1-7 (rest).** Exports aren't stopped before a reboot.
- [ ] **P1-1 (rest).** Alert delivery to a real email / webhook receiver is untested;
  Slack / PagerDuty-native receivers (webhook covers them through relays).
- [ ] **A-1. API audit log.** Who changed what and when (logins, creates, deletes, access and
  network changes), kept on the appliance and visible to administrators; listed as hardening
  in [docs/ROADMAP.md](docs/ROADMAP.md) and never done.
- [ ] **T-1. Test gaps.** Killing velod in the middle of a multi-step API operation; the CSI driver
  against a real data plane outside a cluster.

## Later

- [ ] **L-3. Parity RAID (RAID5/6).** md has both; needs a decision on the write hole (journal or
  PPL), then power-loss and rebuild tests.
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
  reboots.
- [ ] **P0-4 (rest). Power-pull test.** Needs a drive without power-loss protection and a way to
  cut power. Write with fsync under load, cut power, verify every acknowledged write, with the
  write-cache policy on and off.
- [ ] **P2-2 (rest). A drive that really fails.** Needs a drive with bad sectors. Scrub and
  rebuild against real read errors (after the emulated test under Next).
- [ ] **C5/C6 (rest). NVMe native multipath on hardware.** The boot-disk guard and CSI device
  resolution with multipathed NVMe devices.

## Parked

Decided not to do for now; kept with the reason.

- [ ] **P2-5. Slow test suites.** Auth / server tests take ~50–100 s under `-race` because every
  login runs 600k PBKDF2 iterations; a test-only override would fix it.
