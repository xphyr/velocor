# Velocor Architecture

**Velocor** is an open-source, API-first, all-flash SAN/NAS storage appliance distribution built on the Linux kernel's storage stack and **Fedora Image Mode (`bootc`)**, with a control plane written in Go. Why the kernel and not a user-space data plane: [design/kernel-dataplane.md](design/kernel-dataplane.md).

## 1. Key Architectural Decisions

1. **Data plane (the kernel) vs. control plane (`velocor-velod` container) on `fedora-bootc:45`:**
   - **Base OS (`quay.io/fedora/fedora-bootc:45`):** Thin, immutable host image: the kernel, the storage tools (mdadm, LVM, device-mapper, nvme-cli), kernel settings, and **Podman Quadlet** service definitions (`/usr/share/containers/systemd/*.container`). The host assembles no arrays and activates no volume groups on its own, and its LVM never looks inside the volumes (they hold clients' data, which can be LVM itself): velod decides what comes up.
   - **Data plane:** md RAID on the claimed disks, an LVM volume group with one thin pool per array (64 KiB chunks, new chunks zeroed, a full pool fails writes at once), a dm-linear wrapper per volume, and the kernel's NVMe/TCP target (nvmet) and iSCSI target (LIO), configured through configfs. NICs with fewer receive queues than CPUs spread their receive work (RPS/RFS), as nvmet does its work on the CPU that received the packets.
   - **Control plane (`velocor-velod` container):** Runs `velod`, owning hardware discovery, desired-state persistence (`/var/lib/velocor/velocor.db`), reconciliation, authentication, Prometheus metrics, and the REST/OpenAPI API. It runs the storage tools in the host's namespaces and is never in the I/O path: restarting or upgrading velod doesn't interrupt I/O. An OS upgrade needs a reboot, which does.

2. **Disks:**
   - **NVMe** and **SATA/SAS SSDs** stay on their kernel drivers. velod knows a disk by a stable link (an NVMe drive's by-path link, else a by-id link, a WWN first).
   - **Boot Disk Safety Guard:** `velod` resolves the backing block devices and PCIe BDF addresses for `/`, `/boot`, `/boot/efi`, and `/sysroot` and marks them `is_boot_device = true` so the OS drive can never be claimed or formatted.

3. **Pool & volume hierarchy:**
   - **Disks** are grouped into a **Pool**: an md array (RAID 0, 1 or 10) named `raid_<pool>` with a 1.2 superblock and an internal write-intent bitmap, and on it a volume group with a thin pool, `lvs_<pool>`. The thin pool's data takes 90% of the group; the rest holds its metadata (which velod grows as it fills), LVM's spare copy of that, and a small LV with a copy of the pool's configuration (`velocor-meta`).
   - **Volumes** are thin LVs named by their velod ID, with the namespace UUID in an LVM tag. The targets export the volume's wrapper, `/dev/mapper/velocor-<id>`: suspending the wrappers is how a group snapshot pauses hosts' I/O to all its volumes at once.
   - Every Volume maintains an exclusive `attachment_state` (`UNATTACHED`, `BLOCK_EXPORT`, `FILESYSTEM`) so that a volume exported over SAN protocols can't also be attached locally (Phase 2 NAS).
   - velod reaches all of this through one interface, `dataplane.Plane` (`internal/dataplane`); `kernelplane` implements it on `internal/kernel`, and `planetest` in memory for tests.

4. **Snapshots & Kubernetes CSI Readiness:**
   - Snapshots (read-only thin LVs, inactive until needed) record their parent lineage (`parent_snapshot_id`, `sequence_number`) in `bbolt`.
   - **Changed Block Tracking (CBT) is opt-in.** `velod`'s `blocks:allocated` / `blocks:changed` come from the thin pool's own mappings (`thin_dump`, `thin_delta`), read from a snapshot of its metadata. The CSI driver serves `SnapshotMetadata` (KEP-3314) only when enabled, with the `external-snapshot-metadata` sidecar.
   - Volume and Export creation endpoints are idempotent by name to satisfy Kubernetes CSI controller retry semantics.

5. **Single-Port API + Embedded Web UI + Unified Auth:**
   - `velod` serves REST/JSON (plain `net/http`), an OpenAPI v3 spec, Prometheus `/metrics` (READONLY token required unless `--metrics-public`), and the embedded Web UI (`go:embed`) on a single TLS port (`:8443`). The `.proto` files under `api/` describe the resource model but are not served; there is no gRPC API.
   - A single Auth Interceptor validates both human admin JWT sessions and scoped Personal/Service Access Tokens (`velo_pat_...`) with RBAC roles (`admin`, `csi-provisioner`, `readonly`).

## Data-Safety Invariants

These rules are enforced in code and covered by tests: against an in-memory data plane
(`internal/dataplane/planetest`), which models reboots, member loss, rebuilds and error
injection, and against the real kernel (see [testing.md](testing.md)).

1. **The reconciler never creates storage.** Pools are created by the API; after a reboot the reconciler assembles the arrays whose members are all there, or all those the array still had when it last ran, activates their volume groups, and puts wrappers and exports back. Only disks velod has claimed take part. A missing array, thin pool or volume is reported, never re-created, and an array missing a member it had waits for it (or for an operator to start it degraded): the missing copy may be the newer one.
2. **Data plane first, store second.** API handlers only persist a resource after the data plane succeeded and roll back partially created objects on failure. Deletes check store preconditions, delete in the data plane (missing objects are fine), then delete the record.
3. **Data-plane names come from resource IDs**, never user-supplied names. The names that do come from users (NQNs, IQNs) become configfs directories: they are validated where they are entered, and the data plane refuses any that isn't a single path component.
4. **Exports default to deny.** An export with no resolvable initiators is rejected unless `allow_any_host` is set explicitly. NVMe/TCP hosts are admitted by host NQN. LIO admits iSCSI initiators by IQN only: a host's IP address or a group's allowed CIDRs are not enforced for iSCSI, and an export whose ACL has addresses but no IQN is refused.
5. **Status writes are atomic and field-scoped.** The reconciler and handlers use `Mutate*` store calls that read-modify-write a single record inside one bbolt transaction, and only write when the observed status changed.
6. **Disks that are in use are never claimable.** The guard resolves every mount (via `major:minor`, through device-mapper/LVM/LUKS/md), swap device and holder down to physical disks and NVMe controllers (including native-multipath heads), refuses disks with partition tables or filesystem signatures unless `force=true`, and velod refuses to start if the guard cannot initialise.
7. **The CSI node verifies identity before formatting.** NVMe namespaces are resolved via sysfs (`subsysnqn` + `nsid`) and must carry the volume's UUID; `mkfs` only runs when `blkid -p` positively reports a blank device.
