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
  - [x] `volume.proto` (including Snapshots & CBT)
  - [x] `export.proto` (Portals, InitiatorGroups, NVMe-oF & iSCSI Exports)
  - [x] `system.proto`
- [x] `bbolt` State Store (`internal/store`):
  - [x] Domain models (`models.go`)
  - [x] Store interface (`store.go`)
  - [x] Transactional `bbolt` implementation with referential integrity (`bbolt.go`)
  - [x] Unit tests (`bbolt_test.go`)

## Phase 2: SPDK JSON-RPC 2.0 Client & Hardware Manager
- [x] SPDK JSON-RPC 2.0 socket client (`internal/spdk/client.go`)
- [x] Typed SPDK RPC methods (`bdev`, `raid`, `lvol`, `fragmap` CBT, `nvmf`, `iscsi`, `iostat`, SMART)
- [x] Linux Hardware Manager (`internal/hardware`) with boot-disk guard, hugepages, and VFIO/noiommu binding

## Phase 3: Reconciler, `velod` Control Plane & `veloctl` CLI
- [x] Startup & continuous state reconciler (`internal/reconciler`)
- [x] Auth manager & token hashing (`internal/auth`)
- [x] gRPC + REST server (`cmd/velod`)
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
- [x] Disks (boot-disk guard, claim/release), Pools, Volumes (QoS/resize), Snapshots & CBT inspector, and SAN Exports (with copyable `nvme`/`iscsiadm` commands)
