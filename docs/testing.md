# Testing

How Velocor is tested, what the lab has covered, and pitfalls met on the way. Open test gaps are
tracked in [TODO.md](../TODO.md); finished review items in [review-history.md](review-history.md).

## Unit tests

`go test ./...` runs the API, reconciler, CSI driver and CLI against `internal/dataplane/planetest`,
an in-memory data plane that models reboots, member loss, waiting and degraded arrays, rebuilds,
scrubs, snapshot chains, thin allocation and error injection. `internal/kernel` and `kernelplane`
are tested on captured tool output and scripted commands. A fake can share a wrong idea with the
code, which is why the tests on the real kernel exist.

## Tests on a real kernel

`internal/kernel` and `internal/dataplane/kernelplane` have integration tests (build tag
`kernelintegration`) against real md, LVM thin, device-mapper, nvmet and LIO on loop devices, with
hosts connecting over NVMe/TCP and iSCSI and data read back. They need root:
`make test-kernel` runs them on this machine, `make test-kernel-vm` in a throwaway Fedora VM
under KVM (no root needed; CI runs this and publishes no images unless it passes).

| Covered | |
|---|---|
| Volumes | lifecycle, resize, snapshots, clones, exact allocated/changed blocks, a full pool's ENOSPC |
| Group snapshots | one point in time under dependent writes to two volumes; a reconciler pass in between |
| Exports | allowed and refused hosts, open and closed again, several per volume, several at once, connections listed |
| Arrays | rebuild onto a replacement, scrub, RAID10, a reboot (complete, degraded, a member missing), pool import |
| Bad sectors | an unreadable last copy fails that read only; what it held is named |
| Metadata | the thin pool's grown; a metadata snapshot left by a crash recovered |

## Lab coverage so far

- **Appliance image:** boots with BIOS and UEFI (Secure Boot off); first boot without network;
  only `velocor-admin` key logins; `bootc switch` / rollback keep the database; upgrades through
  the API with pre-upgrade backups.
- **Disks (QEMU lab VM):** AHCI SATA, virtio-SCSI and NVMe members; hot-unplug
  under writes, replace, rebuild, reboots, start-degraded, pool import, write-cache control.
- **CSI on OpenShift (x86 and Arm clusters):** provision, mount, cross-node re-attach, online
  expansion, snapshots and restore, clones, raw block, RWX block and KubeVirt live migration,
  group snapshots, CBT with upstream's verifier, NVMe/TCP and iSCSI.
- **Performance:** [performance.md](performance.md).

## Emulated disk errors

QEMU's `blkdebug` driver fails chosen I/O on a lab VM disk, which is how scrub, RAID1 read-error
handling and rebuilds were tested in an appliance. Give the disk a raw file
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
- **external-snapshot-metadata v1.1.0's lister** pins an old client library (v1alpha1) and gets a
  404 against the v1beta1 CRD; build it with `replace` directives to the tag's tree. The verifier
  is fine.
- **Kasten 9.0 doesn't use the CSI SnapshotMetadata API** for generic CSI drivers (its CBT paths
  are Azure Disk, AWS EBS, vSphere and KubeVirt's VM-level CBT), so it can't test Velocor's CBT.
- **`helm upgrade --reuse-values`** keeps only the old release's values; prefer
  `--reset-then-reuse-values`.
