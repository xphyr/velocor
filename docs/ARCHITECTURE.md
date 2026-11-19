# Velocor Architecture

**Velocor** is an open-source, API-first, all-flash SAN/NAS storage appliance distribution built on **SPDK** (Storage Performance Development Kit) and **Fedora Image Mode (`bootc`)**, written in Go.

## 1. Key Architectural Decisions

1. **Decoupled Data Plane (`velocor-spdk` container) vs. Control Plane (`velocor-velod` container) on `fedora-bootc:45`:**
   - **Base OS (`quay.io/fedora/fedora-bootc:45`):** Thin, immutable host image responsible only for the Linux kernel, `vfio-pci` / `nvme_tcp` / `ublk_drv` kernel modules, hugepage sysctls, and **Podman Quadlet** service definitions (`/usr/share/containers/systemd/*.container`).
   - **Data Plane (`velocor-spdk` container):** Runs `spdk_tgt` in C, owning the user-space block I/O path with `/dev/vfio`, `/dev/hugepages`, and `/dev` (`io_uring`) mounted.
   - **Control Plane (`velocor-velod` container):** Runs `velod` in Go, owning hardware discovery, VFIO device binding, hugepage allocation, desired-state persistence (`/var/lib/velocor/velocor.db`), crash/boot reconciliation, authentication, Prometheus metrics, and the REST/OpenAPI API.
   - **Zero-Disruption Control-Plane Upgrades:** `velod` and `spdk_tgt` communicate over a shared host socket (`/run/velocor/spdk.sock`), allowing `velocor-velod` to be upgraded or restarted in ~1 second without interrupting active NVMe-oF or iSCSI I/O in `velocor-spdk` or rebooting the host OS. The reverse is not true: restarting `velocor-spdk` drops all SAN sessions, so its quadlet carries no `AutoUpdate` label and it is only updated explicitly (`allow_disruptive=true`).

2. **Two-Tier Flash Support (NVMe + SATA/SAS SSDs):**
   - **NVMe Tier (`bdev_nvme`):** Unbound from the Linux kernel (`nvme` driver) and bound to `vfio-pci` (or, only when `VELOCOR_ALLOW_UNSAFE_NOIOMMU=true` is set for lab VMs without a vIOMMU, `uio_pci_generic` / `vfio-noiommu`). Polled directly in user space by SPDK. SMART health is queried via `bdev_nvme_get_controller_health_info`.
   - **SSD Tier (`bdev_uring`):** Remains attached to the Linux kernel (`/dev/sdX`) and claimed exclusively by SPDK using Linux `io_uring` (`bdev_uring_create`). Before each attach velod checks that `spdk_tgt` can set up io_uring (kernel, sysctl, SELinux, seccomp), because a failed setup crashes it, and uses Linux AIO (`bdev_aio_create`) instead when it can't (`--ssd-bdev auto`).
   - **Boot Disk Safety Guard:** `velod` resolves the backing block devices and PCIe BDF addresses for `/`, `/boot`, `/boot/efi`, and `/sysroot` on startup and permanently marks them `is_boot_device = true` so the OS drive can never be unbound or formatted.

3. **Pool, Volume & Filter Bdev Hierarchy:**
   - **Disks** (`bdev_nvme` or `bdev_uring`) are grouped into a **Pool** (`bdev_raid` RAID 0/1 + SPDK `lvol_store`; RAID10 and parity RAID are not supported yet, see TODO L-3/L-4).
   - Each Pool configures an `lvol_store` `cluster_size` (default `1 MiB`, rather than SPDK's 4 MiB; SPDK reserves a 4 KiB metadata page per cluster and reads all of them when recovering from an unclean stop, so smaller clusters cost capacity and recovery time) to keep thin-provisioning overhead low (the cluster is also the granularity any future changed-block tracking would report at).
   - **Volumes** (`bdev_lvol`) are thin- or thick-provisioned inside a Pool.
   - Every Pool and Volume tracks both its `base_bdev_name` (or `lvol_bdev_name`) and its `effective_bdev_name` so stackable SPDK filter bdevs (`bdev_crypto` for AES-XTS encryption and `bdev_compress` for LZ4/Deflate compression) can wrap volumes or pools transparently without changing the Export layer.
   - Every Volume maintains an exclusive `attachment_state` (`UNATTACHED`, `BLOCK_EXPORT`, `FILESYSTEM`) so a volume can be exported over SAN protocols (NVMe-oF / iSCSI) now, or attached locally via SPDK `ublk` (`/dev/ublkbX`) for XFS + NFS/SMB in Phase 2 NAS, without ever allowing concurrent conflicting attachments.

4. **Snapshots & Kubernetes CSI Readiness:**
   - Snapshots (`bdev_lvol_snapshot`) record their parent lineage (`parent_snapshot_id`, `sequence_number`) in `bbolt`.
   - **Changed Block Tracking (CBT) is deferred.** SPDK v26.09 has no RPC that reports a snapshot's allocated clusters (an earlier design assumed a `bdev_lvol_get_fragmap` RPC, which exists only in Longhorn's SPDK fork, never in upstream SPDK). Until it can be measured, the CSI driver neither serves nor advertises the `SnapshotMetadata` service (KEP-3314), and `velod`'s `blocks:allocated` / `blocks:changed` endpoints return `501 Not Implemented` rather than guessed extents, which would make incremental backups silently miss data. The planned route is a small read-only SPDK extension (`TODO.md` L-2): the lvol layer already knows which clusters each snapshot layer owns, so a snapshot's clusters relative to its parent in the same chain are exactly the blocks that changed.
   - Volume and Export creation endpoints are idempotent by name to satisfy Kubernetes CSI controller retry semantics.

5. **Single-Port API + Embedded Web UI + Unified Auth:**
   - `velod` serves REST/JSON (plain `net/http`), an OpenAPI v3 spec, Prometheus `/metrics` (READONLY token required unless `--metrics-public`), and the embedded Web UI (`go:embed`) on a single TLS port (`:8443`). The `.proto` files under `api/` describe the resource model but are not served; there is no gRPC API.
   - A single Auth Interceptor validates both human admin JWT sessions and scoped Personal/Service Access Tokens (`velo_pat_...`) with RBAC roles (`admin`, `csi-provisioner`, `readonly`).

## Data-Safety Invariants

These rules are enforced in code and covered by tests against a stateful fake `spdk_tgt`
(`internal/spdk/spdktest`), which models restarts, asynchronous examine and error injection.

1. **The reconciler never creates storage.** Pools are created atomically by the API with an on-disk RAID superblock, so after a restart SPDK examine re-assembles each array and loads its lvstore. The reconciler only re-attaches disks and re-applies QoS and exports; a missing RAID, lvstore or lvol is reported `DEGRADED`/`FAULTED`, never re-created. It always calls `bdev_wait_for_examine` before concluding that on-disk metadata is absent.
2. **SPDK first, store second.** API handlers only persist a resource after SPDK succeeded and roll back partially created SPDK objects on failure. Deletes check store preconditions, delete in SPDK (missing objects are fine), then delete the record.
3. **SPDK names come from resource IDs**, never user-supplied names, so snapshot/volume lvols can never collide and always fit SPDK's 63-character limit.
4. **Exports default to deny.** Every iSCSI export owns a unique initiator group (tag allocated by the store); a portal group exists per Velocor portal. An export with no resolvable initiators is rejected unless `allow_any_host` is set explicitly.
5. **Status writes are atomic and field-scoped.** The reconciler and handlers use `Mutate*` store calls that read-modify-write a single record inside one bbolt transaction, and only write when the observed status changed.
6. **Disks that are in use are never claimable.** The guard resolves every mount (via `major:minor`, through device-mapper/LVM/LUKS/md), swap device and holder down to physical disks and NVMe controllers (including native-multipath heads), refuses disks with partition tables or filesystem signatures unless `force=true`, and velod refuses to start if the guard cannot initialise.
7. **The CSI node verifies identity before formatting.** NVMe namespaces are resolved via sysfs (`subsysnqn` + `nsid`) and must carry the volume's lvol UUID; `mkfs` only runs when `blkid -p` positively reports a blank device.
