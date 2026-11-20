# Review history

Finished work from the code reviews of 2026-11-15 and 2026-11-16 and what followed, archived out
of [TODO.md](../TODO.md) on 2026-11-20. Item IDs are stable: code comments cite them
("docs/review-history.md P1-14"), and anything still open about an item is in TODO.md under the
same ID. Each entry says what was wrong, what changed and how it was checked; the commits have
the details. "Lab" means the test appliance (a KVM VM), the QEMU lab VM used for disk tests, or
the two OpenShift test clusters (x86 and Arm).

Design notes: [P0-1](design/p0-1-disk-replacement.md), [P0-2](design/p0-2-capacity.md),
[P0-3](design/p0-3-metadata-recovery.md). Upstream write-ups: [docs/upstream](upstream/).
How things are tested: [testing.md](testing.md).

## Data loss (P0)

- **P0-1. Disks could never be replaced.** A mirror that lost a disk stayed degraded until the
  next failure destroyed it. Now: pool members with slot states (ACTIVE / REBUILDING / FAILED /
  MISSING) and rebuild progress, `POST /pools/{id}/replace` (claims and wipes a new disk;
  `allow_degraded` for proactive replacement), rebuild speed setting, release of detached disks,
  CLI and UI. A disk that left its pool is never re-attached on its own (SPDK would re-add it and
  trust stale data). Found on the way: a degraded mirror showed ONLINE; NVMe drives weren't
  re-bound after a reboot; NVMe namespaces without NGUID/UUID got a new bdev UUID at every start
  (`generate_uuids=true`); Fedora leaves the Intel IOMMU off (kargs.d now turns it on).
  Real-SPDK tests `TestReplaceFailedMemberAndRebuild`, `TestMissingAtBootStartDegradedAndReplace`,
  `TestInterruptedRebuildRestarts`; lab on SATA, virtio-SCSI and NVMe (vfio) members: hot-unplug
  under writes, replace, rebuild, reboot. Commits ccbd342, 3aaccf9, 7309a18, fe45c4d (2026-11-17).
  Open: real-hardware hot-swap (TODO.md, waiting on hardware).
- **P1-8. A RAID1 missing a member at startup wasn't served.** SPDK keeps such an array
  configuring. SPDK patch 0002 (`bdev_raid_start_degraded`) and `POST /pools/{id}/start-degraded`
  (confirmed by pool name) start it degraded on an operator's decision. Done with P0-1.
- **P0-2. Thin pools could be overcommitted without limit.** A full pool failed writes in every
  thin volume at once. Now: per-pool space policy (max overcommit 1.0, 5% reserve, warning 80%,
  critical 90%), admission control on every growth path (create, clone, expand, snapshot, group
  snapshot) with per-object reservations, space states OK / WARNING / CRITICAL / FULL separate
  from RAID health, allocated bytes per volume and snapshot, metrics, CSI pool choice by
  available bytes (507 -> RESOURCE_EXHAUSTED). `TestPoolSpaceLifecycle`; lab fill to FULL and
  recovery. b083815 (2026-11-17). Open: L-6 (ENOSPC instead of EIO).
- **P0-3. The metadata database was a single point of failure.** Now: an A/B-slot manifest of
  each pool's configuration on the pool itself (8 MiB `velocor-meta` lvol written through ublk
  by a helper process; no accounts or secrets), local database backups (hourly x24, daily x7,
  before upgrade and restore) with download and restore, and pool import from the manifest or,
  failing that, from SPDK's own objects (`GET|POST /pool-imports`). Lab: database deleted and
  everything imported back with data intact; disks moved to a fresh install. 87c2c85, 497a397,
  085d5ed (2026-11-17).
- **P0-4 (interim). Hosts are told writes are durable while drives cache them.** SPDK v26.09
  passes no FLUSH through lvols and RAID reports no write cache. Decision 2026-11-16: no SPDK
  patch yet (that is L-1); instead each disk's volatile write cache is detected (kernel, or NVMe
  Identify/Get Features through SPDK), operators can declare power-loss protection, and a
  per-disk policy switches the cache off (SCSI mode select; NVMe Set Features, saved when
  supported) and keeps it off. Disks and pools carry `durability` (DURABLE / AT_RISK / UNKNOWN);
  a drive refusing the change is recorded once, not retried every pass. 2b564cb (2026-11-16),
  d9cdd95, dfa39a5, 6ef4016, 981f6b3 (2026-11-18). Open: power-pull test (waiting on hardware),
  L-1.
- **P0-5. SPDK was killed 10 s into every shutdown** (podman's default): `StopTimeout=120`.
- **P0-6. A failed io_uring setup crashed `spdk_tgt`.** Cause: SELinux (`container_t` may not use
  io_uring) plus a missing NULL check in SPDK's uring module
  ([report](upstream/spdk-uring-channel-crash.md)). `--ssd-bdev auto` (default) probes before
  every SSD attach and falls back to AIO when unsure. 15c252b (2026-11-19). Open: upstream.

## Outages, lock-outs, things operators couldn't see (P1)

- **P1-1. Nothing alerted anyone.** Health metrics for disks, pools, volumes, exports, SMART,
  rebuilds, durability, metadata copies, backups, data plane and reconciler; Prometheus rules
  with promtool tests ([monitoring.md](monitoring.md)); an optional built-in Prometheus and
  Alertmanager (off by default, loopback only, off SPDK's cores); receivers (email, webhook,
  heartbeat), per-rule settings, silences, Alerts page and history graphs. 653167e, 7ffea94,
  86258fe, 99eece0 (2026-11-19). Open: delivery to a real receiver untested (TODO.md).
- **P1-2. A failed resume after a group snapshot left I/O paused.** Superseded by P1-15 (no
  pause/resume any more).
- **P1-3. Network changes could lock the appliance out.** Changes apply under a NetworkManager
  checkpoint and roll back unless confirmed within 120 s (NetworkManager restores it even if
  velod is gone); interfaces carrying portals need `force`. Lab: wrong address rolled back by
  itself. 5a3608e (2026-11-19).
- **P1-4. Deletion was immediate and irreversible.** Deleted volumes are kept (default 24 h,
  restorable, purged by the reconciler) and per-volume deletion protection refuses deletes.
  Snapshots with clones are hidden at once and deleted when nothing depends on them. 15c252b
  (2026-11-19).
- **P1-5. Orphaned SPDK objects were invisible.** The reconciler reports lvstores, lvols, RAIDs,
  bdevs, subsystems and targets the store doesn't know (status, System page, metric), never
  deleting them. 15c252b (2026-11-19).
- **P1-7. OS lifecycle.** Upgrades are background jobs with live output; velod records what it
  staged and warns after a reboot that booted something else; reboot names connected hosts and
  needs `force`; image references are validated. Also fixed: upgrades silently failed on UEFI
  appliances (systemd's gpt-auto automount shadowed `/boot`; `systemd.gpt_auto=0`). 0992bd0
  (2026-11-16), d9b5c6b (2026-11-19).
- **P1-9. The CSI driver advertised CBT that couldn't work.** Removed 2026-11-16 (353aa94);
  CBT came back properly with L-2.
- **P1-10. iSCSI WRITE SAME on freshly discarded thin space deadlocked SPDK.** SPDK ran each
  WRITE SAME as one bdev write per block; the flood emptied the bdev_io pool and starved the
  blobstore metadata thread's cluster allocations. SPDK patch 0001 bounds the fan-out (all-zero
  WRITE SAME becomes one write-zeroes). Lab: `mkfs.ext4` over existing data in under a second.
  559ea38 (2026-11-17); [upstream guide](upstream/spdk-scsi-write-same.md). Open: upstream.
- **P1-11. One stuck volume blocked a whole CSI node.** Per-volume locks; block devices flushed
  before disconnect; detach failures returned. fbf9748 (2026-11-16).
- **P1-12. KubeVirt live migration.** Raw block volumes can be ReadWriteMany (one shared export,
  nodes added and removed live). Lab: a VM migrated three times under writes, no errors.
  064d85e (2026-11-16).
- **P1-13. The CSI driver picked a volume's portal by chance.** StorageClass parameter `portals`
  and chart `defaultPortals`; several portals and no choice is refused; exports get every listed
  portal; NVMe/TCP nodes connect every path (native multipath). 7c26f5c, 4a38ddd (2026-11-19).
  Open: iSCSI multipath (TODO.md).
- **P1-14. A pool whose disks came back under new IDs stayed UNAVAILABLE.** The reconciler and
  pool import match members by the RAID superblocks on the disks (only when certain; a stripe is
  never guessed) and relink them, keeping their bdev UUIDs. Lab: three pools came back on their
  own after their disks moved from emulated SATA to virtio-SCSI. a548637 (2026-11-18).
  Open: NVMe drives moved to another PCIe slot (TODO.md).
- **P1-15. Group snapshots of volumes in use failed** (velod called an RPC SPDK doesn't have).
  SPDK patch 0003 `bdev_lvol_snapshot_group` quiesces every member bdev, snapshots all, resumes,
  in one call (5 s timeout -> 503 / CSI Unavailable). Lab: dependent writes over NVMe/TCP and
  iSCSI, group snapshots through the API and Kubernetes VolumeGroupSnapshots, all consistent.
  1b4513e, 01e8a55 (2026-11-19). Open: upstream.

## Hardening (P2)

- **P2-2. No background scrub.** SPDK patch 0004 adds a RAID scrub: RAID1 windows are quiesced,
  read from every member and compared; unreadable copies are rewritten, mismatching copies made
  equal (SPDK has no resync after an unclean stop), a failed rewrite fails the member; RAID0 gets
  a read scan; rate-limited, resumable at an offset, stops for member removal, rebuilds and
  destruct. velod schedules it (default monthly, 01:00-06:00, 200 MB/s, one pool at a time),
  pauses at the window's end and resumes; API, UI, CLI, metrics and four alerts.
  `TestScrubRepairsDifferingMirrorCopies`; lab: all pools scrubbed under checksummed writes,
  window pause and exact resume; a rate-limit overflow after ~7 GB found and fixed; host I/O cost
  at 200 MB/s about 16% ([performance.md](performance.md)). 170adab, 8311b3a (2026-11-20).
  Open: emulated and real failing disks (TODO.md).
- **P2-6. No health endpoint.** `/healthz`, `/readyz` (no auth, no detail), quadlet health
  check. d9b5c6b (2026-11-19).
- **P2-7. The CSI driver logged nothing per request.** One line per changing call. d9b5c6b.
- **P2-8. CSI sidecars pinned by tag.** All pinned by digest at versions tested on both
  clusters; `check-pins` compares chart and static manifest. bf1ca8e (2026-11-19).
- **P2-9. CDI StorageProfile.** The chart's hook sets RWX block, csi-clone and snapshot golden
  images for Velocor classes. 064d85e (2026-11-16).
- **P2-11. velod said ONLINE before a restarted data plane was restored.** Status is RECONCILING
  until a pass covers the running `spdk_tgt` (PID and start time). 3816826 (2026-11-19).
- **P2-12. An export on a portal whose address was gone retried forever.** Portals whose
  address isn't on the appliance are skipped (export DEGRADED with a clear message). 91f9415
  (2026-11-18).
- **P2-15. iSCSI sessions could outlive their volumes on a node.** Logout by session ID for every
  session of the target; unstage fails while one remains. 867ddaa (2026-11-19).
- **Data-plane restarts failed I/O on hosts** (2026-11-16): 64 KiB clusters meant 16 minutes of
  lvstore recovery. Default cluster size 1 MiB; CSI connects with `--ctrl-loss-tmo=-1`. 194b99c.
  Open: iSCSI `replacement_timeout`, a "recovering" state (TODO.md P2-10).
- **Slow operations were aborted halfway and left orphans.** Long SPDK calls get a 30 min
  timeout, API changes are detached from the client connection, pool rollback deletes the
  lvstore first. 033321d (2026-11-16).
- **The initial admin password file stayed on disk** after the password changed: deleted once it
  no longer matches. 981f6b3 (2026-11-18).

## Changed-block tracking (L-2)

- **L-2. CBT for incremental backups.** SPDK patch 0005 `bdev_lvol_get_fragmap` (Longhorn's RPC,
  same name and result, reading the blob's cluster table); velod walks SPDK's own snapshot chain
  (which fixed a clone's snapshot missing its origin's data and failures after deleting a middle
  snapshot); CSI SnapshotMetadata service behind `--snapshot-metadata`, chart option
  `snapshotMetadata.enabled` with the `external-snapshot-metadata` sidecar; veloctl and UI views.
  Lab on both clusters with upstream's verifier: block lists exact, reconstructed volumes
  identical. The chart's `--reuse-values` upgrade bug found there was fixed (11a14b2). 3705715
  (2026-11-20). Open: a backup tool that uses the API (TODO.md).

## Build, packaging and CI (B)

- **B1/B2.** The OS image embeds the velod and SPDK images built in the same run, referenced by
  digest; velod's own container-update path removed. 1869d26, 2380faa (2026-11-16).
- **B3, B4, B5, B6, B7.** Base images, SPDK and actions pinned; builds with the golang image
  matching go.mod's toolchain; CI runs check-pins, vet, golangci-lint, shellcheck, helm lint and
  the real-SPDK integration tests; per-job permissions; Makefile tags `dev-<sha>` by default and
  needs `RELEASE=true` for release tags. 1869d26, 5ade8f8 (2026-11-16).
- **B8.** `make qcow2` is rootless; decision 2026-11-19: no qcow2 with releases (the README
  explains how to build one). 7c26f5c.
- **B9, B10.** No internal registry names in the repo; images have no default arguments.
- **B11, B12.** Secrets kept out of build contexts and disk images. edaa144, bddac0b.
- **B13.** Dev builds and pruning scripts (`hack/build-dev-images.sh`, `hack/prune-dev-tags.sh`).
  edfad5f (2026-11-16).
- **B16.** The CI workflow proven end to end: `main` run with ghcr push, cosign and attestations,
  and the `v0.1.0-rc.1` pre-release. 11acba8 (2026-11-19). Open: Renovate, old `latest` tags.
- **B17.** Pinned Fedora base digests vanish from quay.io: decision 2026-11-17 to follow the
  rolling `:45` tags, resolved to a digest per build and recorded in labels. 16f25b0.
- **B18.** CI after the SPDK patch: one base resolution per run, SPDK patch unit tests gate
  publishing, actionlint. aa8411d (2026-11-17).
- **B19, B20, B21, B22.** First CI runs: veloctl password prompts on macOS/Windows (9778130), extra-tag
  push (3aaccf9), registry credentials for the OS image build (7309a18), attestations skipped
  while the repository was private (fe45c4d).
- **B23.** Pull requests run only the jobs their changes need. 71f4a27 (2026-11-19).
- **B24.** The Makefile overrode the Containerfiles' Fedora release. c0d0611 (2026-11-19); a
  Fedora 46 build passed but 46 is still pre-release (move when released; TODO.md).

## Found by testing against real SPDK (second review, 2026-11-16, 044f6e9)

- **R1.** SSD bdevs got a random UUID on every creation, so every SSD pool went offline after a
  `spdk_tgt` restart: deterministic per-disk bdev UUIDs.
- **R2.** A deleted RAID's superblocks re-assembled it: `bdev_raid_delete` with `clear_sb`.
- **R3.** raid5f can't hold a volume store: RAID5F refused (see L-3).
- **R4.** SPDK v26.09 had no fragmap RPC: CBT returned 501 (see L-2, now done).
- **R5.** Host NQNs validated when entered.

## First review (2026-11-15; fixed in 102324d, 0470f20, 2026-11-16)

- **Critical:** C1 reconciler could wipe pools on SPDK restart (it now never creates storage);
  C2 iSCSI LUN masking; C3 snapshot name collisions; C4 SPDK errors swallowed while state was
  saved; C5 CSI could mount the wrong NVMe device; C6 boot-disk guard gaps; C7 CBT fabricated
  data; C8 CSI CBT routes mismatched; C9 boot guard failed open; C10 filter policies ignored.
- **High:** H1 publish ignored node_id; H2 wrong `PublishedNodeIds`; H3 DeleteVolume blocked by
  snapshots; H4 RAIDs without superblocks; H5 lost updates (field-level `Mutate*`); H6 sizes
  rounded down.
- **Security:** S1 shell injection in container updates; S2 JWT revocation; S3 shared default
  admin password; S4 login rate limiting; S5 PBKDF2-SHA256 600k; S6 `/metrics` unauthenticated;
  S7 TLS verification skipped by default; S8 body limits and timeouts; S9 iSCSI initiator name
  overwritten; S11 resource names validated; S12 vfio no-IOMMU opt-in. (S10 became P2-1.)
- **Operational:** M1 SPDK auto-updated; M2 a write per API-token request; M3 reconciler
  rewrote every record; M4 partial NVMe-oF subsystems not repaired; M5 group snapshot
  consistency (later P1-15); M6 IPv4-only listeners; M7 no iSCSI rescan on expansion; M8 docs
  claimed a gRPC API.
- **Cleanup:** migration code removed before the first release (7b524ac); web UI rewritten
  (2038f8d); veloctl commands (763bb21); the stateful fake SPDK and the real-SPDK harness.
