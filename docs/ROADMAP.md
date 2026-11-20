# Velocor Implementation Roadmap & Status

This file tracks implementation progress directly inside the Git repository so any developer or AI session can resume seamlessly.

## Phase 1: Foundation, API Contract & Embedded State Store (`bbolt`)
- [x] Initialize Git repository (`~/source/velocor`)
- [x] Document architecture & roadmap (`docs/ARCHITECTURE.md`, `docs/ROADMAP.md`)
- [x] Project build configuration (`.gitignore`, `Makefile`, `buf.yaml`, `buf.gen.yaml`, `go.mod`)
- [x] Protobuf v1 API definitions (`api/proto/velocor/v1/*.proto`):
  - [x] `common.proto`
  - [x] `auth.proto`
  - [x] `disk.proto`
  - [x] `pool.proto`
  - [x] `volume.proto` (including Snapshots)
  - [x] `export.proto` (Portals, InitiatorGroups, NVMe-oF & iSCSI Exports)
  - [x] `system.proto`
- [x] `bbolt` State Store (`internal/store`):
  - [x] Domain models (`models.go`)
  - [x] Store interface (`store.go`)
  - [x] Transactional `bbolt` implementation with referential integrity (`bbolt.go`)
  - [x] Unit tests (`bbolt_test.go`)

## Phase 2: SPDK JSON-RPC 2.0 Client & Hardware Manager
- [x] SPDK JSON-RPC 2.0 socket client (`internal/spdk/client.go`)
- [x] Typed SPDK RPC methods (`bdev`, `raid`, `lvol`, `nvmf`, `iscsi`, `iostat`, SMART)
- [x] Linux Hardware Manager (`internal/hardware`) with boot-disk guard, hugepages, and VFIO/noiommu binding

## Phase 3: Reconciler, `velod` Control Plane & `veloctl` CLI
- [x] Startup & continuous state reconciler (`internal/reconciler`)
- [x] Auth manager & token hashing (`internal/auth`)
- [x] REST server (`cmd/velod`) — the gRPC API described by the protos is not implemented
- [x] CLI client (`cmd/veloctl`)

## Phase 4: Fedora Image Mode (`bootc` 45), Podman Quadlets & QEMU/KVM Lab
- [x] Thin `fedora-bootc:45` host OS image (`deploy/bootc/Containerfile`)
- [x] Decoupled SPDK & `velod` workload containers (`deploy/containers/Containerfile.spdk`, `deploy/containers/Containerfile.velod`)
- [x] Systemd Podman Quadlet units (`deploy/quadlets/velocor-spdk.container`, `deploy/quadlets/velocor-velod.container`)
- [x] `hack/vm-lab/` QEMU/KVM 2-node test environment

## Phase 5: Embedded Web UI Console
- [x] Zero-dependency HTML/CSS/JS SPA embedded in `velod` via `go:embed` (`internal/server/web/`)
- [x] JWT login, password management & Personal Access Token (`velo_pat_...`) UI
- [x] Live SPDK bdev I/O telemetry (`bdev_get_iostat` polling with IOPS, MB/s, and latency)
- [x] Disks (boot-disk guard, claim/release), Pools, Volumes (QoS/resize), Snapshots, and SAN Exports (with copyable `nvme`/`iscsiadm` commands)

## Phase 6: Kubernetes CSI Driver (`velocor-csi`), Helm Chart & Release Pipeline
- [x] CSI v1.x driver (`cmd/velocor-csi`, `internal/csi`) with NVMe-oF/TCP & iSCSI support, online expansion, CoW snapshots, cloning, and usage reporting (`NodeGetVolumeStats`, `GetCapacity`)
- [x] Per-node Host & InitiatorGroup ACL automation during `ControllerPublishVolume`
- [x] Volume Group Snapshots (`GroupController` / KEP-3476)
- [x] Changed Block Tracking (`SnapshotMetadata` / KEP-3314), opt-in (SPDK patch 0005; TODO.md L-2)
- [x] Official CSI sanity suite (`csi-test/v5`: 82 passed, 0 failed) & `k3s` VM lab end-to-end verification
- [x] Multi-arch (`linux/amd64`, `linux/arm64`) `velocor-csi` container build & Helm chart (`deploy/charts/velocor-csi`)
- [x] GitHub Actions CI/CD release pipeline (`.github/workflows/ci-release.yml`) with Cosign keyless signing, SPDX SBOMs, SLSA build provenance, and OCI Helm publishing
- [x] OpenShift cluster validation (SCC/SELinux, NVMe-oF/TCP, `VolumeSnapshot`, online expansion; `VolumeGroupSnapshot` skipped where CRD absent)

## Phase 7: Operator Parity & Appliance Lifecycle Management (`veloctl` & Web UI)
- [x] `veloctl` CLI and Web UI support for Volume Group Snapshots (Consistency Groups)
- [x] Web UI & `veloctl` parity for `velocor-os` (`bootc`) status, upgrades, and rollbacks
- [x] Containers ship inside the OS image, pinned by digest; they change only with `bootc upgrade` (the separate container-update path was removed)

## Phase 8: Hardening & Storage Failure-Mode Testing
- [ ] Control-plane & API hardening (audit logging, rate limiting, TLS/auth hardening, input validation, least-privilege container/host posture)
- [ ] Resolve degraded RAID / lvol reclaim error on `spdk_tgt` restart (`bdev Nvme_* already claimed by module lvol` / `raid_bdev_configure_base_bdev` ordering during reconciler recovery)
- [ ] Storage failure & fault-injection test suite (NVMe/SSD removal & fault handling, degraded RAID pool behavior, `spdk_tgt` & `velod` restart/crash recovery)
- [ ] Initiator & CSI resilience testing (network drop/reconnect, multi-path/portal failover, node reboot & ungraceful detach recovery)
