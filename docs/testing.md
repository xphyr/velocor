# Testing

How Velocor is tested, what the lab has covered, and pitfalls met on the way. Open test gaps are
tracked in [TODO.md](../TODO.md); finished review items in [review-history.md](review-history.md).

## Unit tests

`go test ./...` runs against `internal/spdk/spdktest`, a stateful fake `spdk_tgt` that models
restarts, asynchronous examine, RAID superblocks and member matching by UUID, rebuilds, scrubs,
snapshot chains and error injection. The fake must reject what real SPDK rejects (e.g.
`bdev_get_iostat` with `name`): bugs the fake shares with the code are invisible here, which is
why the integration tests exist.

`make spdk-unit-tests` builds SPDK with our patches (`deploy/containers/spdk-patches`) and runs
SPDK's own unit tests for every library a patch touches; CI gates image publication on it.

## Integration tests

`make test-integration` runs against a real `spdk_tgt` from the shipped image: rootless podman,
`--no-huge`, file-backed disks, a real velod (store, API, reconciler), and SPDK's userspace
NVMe/TCP initiator (`spdk_dd`, from the `test` stage of `Containerfile.spdk`) writing and reading
data through real exports, verified by reading back. No root, hugepages or NVMe hardware needed.

| Test | Checks |
|---|---|
| `TestDataSurvivesCrashAndRestart` | identical data after `kill -9` and a clean stop; RECONCILING until restored |
| `TestExportACLEnforced` | a host outside the ACL can't connect, before and after a restart |
| `TestSnapshotsAndClones` | snapshot isolation, clones, purge, exact CBT extents |
| `TestGroupSnapshots` | a group snapshot of exported volumes under writes is one point in time |
| `TestScrubRepairsDifferingMirrorCopies` | a changed mirror copy is found and repaired; second pass clean |
| `TestReplaceFailedMemberAndRebuild` | replace a member that failed online; rebuilt copy serves the data |
| `TestMissingAtBootStartDegradedAndReplace` | missing at boot -> start degraded -> replace -> restart |
| `TestInterruptedRebuildRestarts` | `kill -9` mid-rebuild; the rebuild restarts and completes |
| `TestDegradedRAID1` | a member missing at boot: nothing re-created, lvstore unchanged |
| `TestPoolSpaceLifecycle` | admission limits, space states to FULL, recovery after a delete |
| `TestImportSkeletonAfterLosingTheDatabase` | pool import from SPDK's objects, data read back |
| `TestPoolDeleteThenReuseDisks` | a deleted pool's disks can build a new pool |
| `TestRAID5FRefused` | RAID5F refused; SPDK still can't put a volume store on raid5f |
| `TestWriteCacheAndFlushAdvertised` | FLUSH support of disk, RAID and volume bdevs (skipped as a known issue until L-1) |

## Lab coverage so far

- **Appliance image:** boots with BIOS and UEFI (Secure Boot off); first boot without network;
  only `velocor-admin` key logins; `bootc switch` / rollback keep the database; upgrades through
  the API with pre-upgrade backups.
- **Disks (QEMU lab VM):** AHCI SATA, virtio-SCSI and NVMe (vfio, vIOMMU) members; hot-unplug
  under writes, replace, rebuild, reboots, start-degraded, pool import, write-cache control.
- **CSI on OpenShift (x86 and Arm clusters):** provision, mount, cross-node re-attach, online
  expansion, snapshots and restore, clones, raw block, RWX block and KubeVirt live migration,
  group snapshots, CBT with upstream's verifier, NVMe/TCP and iSCSI, SPDK `kill -9` under writes
  (I/O stalls ~70 s and resumes, nothing lost).
- **Performance:** [performance.md](performance.md).

## Emulated disk errors

QEMU's `blkdebug` driver fails chosen I/O on a lab VM disk, which is how scrub, RAID1 read-error
handling and rebuilds were tested against real SPDK in an appliance. Give the disk a raw file
and a rules file, and attach it as
`-drive if=none,id=D,cache=none,rerror=report,werror=report,driver=raw,file.driver=blkdebug,file.config=D.dbg,file.image.driver=file,file.image.filename=D.raw`
with a `scsi-hd` device. A rule fails reads (or writes) touching one sector, every time:

```
[inject-error]
event = "read_aio"
iotype = "read"
sector = "2097152"
errno = "5"
```

Always set `iotype`: without it, a rule armed by any read also fails writes at its offset
until another error rule fires, which looks like a failed repair write. Check rules with
`qemu-io -c "read 1G 4k" "json:{...}"` before booting a VM with them. The errors persist (a real
drive remaps a sector once it's rewritten), so a rewritten bad sector is still bad the next
time it's read.

## Pitfalls

- **busybox `dd oflag=direct` isn't always direct.** On a device whose DMA alignment its buffer
  misses (an iSCSI disk wanting 64 bytes) busybox quietly drops `O_DIRECT` and writes through the
  page cache, so "completed" writes reach the target later. Use GNU dd (e.g. a UBI image) for
  ordering or durability tests.
- **`spdk_dd` reports success for failed writes:** verify by reading back.
- **The fake SPDK can agree with a wrong client.** Several bugs (missing RPCs, wrong parameter
  names) were found only on real SPDK; add an integration check when touching an RPC.
- **external-snapshot-metadata v1.1.0's lister** pins an old client library (v1alpha1) and gets a
  404 against the v1beta1 CRD; build it with `replace` directives to the tag's tree. The verifier
  is fine.
- **Kasten 9.0 doesn't use the CSI SnapshotMetadata API** for generic CSI drivers (its CBT paths
  are Azure Disk, AWS EBS, vSphere and KubeVirt's VM-level CBT), so it can't test Velocor's CBT.
- **`helm upgrade --reuse-values`** keeps only the old release's values; prefer
  `--reset-then-reuse-values`.
