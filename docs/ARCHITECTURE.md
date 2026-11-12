# Velocor Architecture

**Velocor** is an open-source, API-first, all-flash SAN/NAS storage appliance distribution built on **SPDK** (Storage Performance Development Kit) and **Fedora Image Mode (`bootc`)**, written in Go.

## 1. Key Architectural Decisions

1. **Decoupled Data Plane (`velocor-spdk` container) vs. Control Plane (`velocor-velod` container) on `fedora-bootc:45`:**
   - **Base OS (`quay.io/fedora/fedora-bootc:45`):** Thin, immutable host image responsible only for the Linux kernel, `vfio-pci` / `nvme_tcp` / `ublk_drv` kernel modules, hugepage sysctls, and **Podman Quadlet** service definitions (`/usr/share/containers/systemd/*.container`).
   - **Data Plane (`velocor-spdk` container):** Runs `spdk_tgt` in C, owning the user-space block I/O path with `/dev/vfio`, `/dev/hugepages`, and `/dev` (`io_uring`) mounted.
   - **Control Plane (`velocor-velod` container):** Runs `velod` in Go, owning hardware discovery, VFIO device binding, hugepage allocation, desired-state persistence (`/var/lib/velocor/velocor.db`), crash/boot reconciliation, authentication, Prometheus metrics, and the gRPC + REST/OpenAPI API.
   - **Zero-Disruption Control-Plane Upgrades:** `velod` and `spdk_tgt` communicate over a shared host socket (`/run/velocor/spdk.sock`), allowing `velocor-velod` to be upgraded or restarted in ~1 second without interrupting active NVMe-oF or iSCSI I/O in `velocor-spdk` or rebooting the host OS.

2. **Two-Tier Flash Support (NVMe + SATA/SAS SSDs):**
   - **NVMe Tier (`bdev_nvme`):** Unbound from the Linux kernel (`nvme` driver) and bound to `vfio-pci` (or `uio_pci_generic` / `vfio-noiommu` in VMs without vIOMMU). Polled directly in user space by SPDK. SMART health is queried via `bdev_nvme_get_controller_health_info`.
   - **SSD Tier (`bdev_uring`):** Remains attached to the Linux kernel (`/dev/sdX`) and claimed exclusively by SPDK using Linux `io_uring` (`bdev_uring_create`).
   - **Boot Disk Safety Guard:** `velod` resolves the backing block devices and PCIe BDF addresses for `/`, `/boot`, `/boot/efi`, and `/sysroot` on startup and permanently marks them `is_boot_device = true` so the OS drive can never be unbound or formatted.

3. **Pool, Volume & Filter Bdev Hierarchy:**
   - **Disks** (`bdev_nvme` or `bdev_uring`) are grouped into a **Pool** (`bdev_raid` RAID 0/1/10/5f + SPDK `lvol_store`).
   - Each Pool configures an `lvol_store` `cluster_size` (defaulting to `64 KiB` or `1 MiB` rather than SPDK's 4 MiB default) to optimize thin provisioning and **Changed Block Tracking (CBT)**.
   - **Volumes** (`bdev_lvol`) are thin- or thick-provisioned inside a Pool.
   - Every Pool and Volume tracks both its `base_bdev_name` (or `lvol_bdev_name`) and its `effective_bdev_name` so stackable SPDK filter bdevs (`bdev_crypto` for AES-XTS encryption and `bdev_compress` for LZ4/Deflate compression) can wrap volumes or pools transparently without changing the Export layer.
   - Every Volume maintains an exclusive `attachment_state` (`UNATTACHED`, `BLOCK_EXPORT`, `FILESYSTEM`) so a volume can be exported over SAN protocols (NVMe-oF / iSCSI) now, or attached locally via SPDK `ublk` (`/dev/ublkbX`) for XFS + NFS/SMB in Phase 2 NAS, without ever allowing concurrent conflicting attachments.

4. **Changed Block Tracking (CBT) & Kubernetes CSI Readiness:**
   - Snapshots (`bdev_lvol_snapshot`) record their parent lineage (`parent_snapshot_id`, `sequence_number`) in `bbolt`.
   - `velod` exposes `GetSnapshotAllocatedBlocks` and `GetSnapshotChangedBlocks` using SPDK's `bdev_lvol_get_fragmap` RPC (`spdk_bdev_seek_data` / `spdk_bdev_seek_hole`), mapping directly to the Kubernetes CSI `SnapshotMetadata` API (`GetMetadataAllocated` and `GetMetadataDelta`, KEP-3314).
   - Volume and Export creation endpoints are idempotent by name to satisfy Kubernetes CSI controller retry semantics.

5. **Single-Port API + Embedded Web UI + Unified Auth:**
   - `velod` serves gRPC, REST/JSON (`grpc-gateway`), OpenAPI v3 specs, Prometheus `/metrics`, and the embedded Web UI (`go:embed`) on a single TLS port (`:8443`).
   - A single Auth Interceptor validates both human admin JWT sessions and scoped Personal/Service Access Tokens (`velo_pat_...`) with RBAC roles (`admin`, `csi-provisioner`, `readonly`).
