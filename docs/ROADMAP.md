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

## Phase 4: Fedora Image Mode (`bootc`) & QEMU/KVM Lab
- [ ] `deploy/bootc/Containerfile` & systemd services
- [ ] `hack/vm-lab/` QEMU/KVM 2-node test environment
