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

## Phase 2: Data Plane & Hardware Manager
- [x] SPDK data plane (removed at the K-4 cutover, 2026-11-24; see Phase 9)
- [x] Linux Hardware Manager (`internal/hardware`) with boot-disk guard

## Phase 3: Reconciler, `velod` Control Plane & `veloctl` CLI
- [x] Startup & continuous state reconciler (`internal/reconciler`)
- [x] Auth manager & token hashing (`internal/auth`)
- [x] REST server (`cmd/velod`) — the gRPC API described by the protos is not implemented
- [x] CLI client (`cmd/veloctl`)

## Phase 4: Fedora Image Mode (`bootc` 45), Podman Quadlets & QEMU/KVM Lab
- [x] Thin `fedora-bootc:45` host OS image (`deploy/bootc/Containerfile`)
- [x] `velod` workload container (`deploy/containers/Containerfile.velod`) and its Podman Quadlet unit (`deploy/quadlets/velocor-velod.container`)
- [x] `hack/vm-lab/` QEMU/KVM 2-node test environment

## Phase 5: Embedded Web UI Console
- [x] Zero-dependency HTML/CSS/JS SPA embedded in `velod` via `go:embed` (`internal/server/web/`)
- [x] JWT login, password management & Personal Access Token (`velo_pat_...`) UI
- [x] Live I/O telemetry (IOPS, MB/s, and latency)
- [x] Disks (boot-disk guard, claim/release), Pools, Volumes (resize), Snapshots, and SAN Exports (with copyable `nvme`/`iscsiadm` commands)

## Phase 6: Kubernetes CSI Driver (`velocor-csi`), Helm Chart & Release Pipeline
- [x] CSI v1.x driver (`cmd/velocor-csi`, `internal/csi`) with NVMe-oF/TCP & iSCSI support, online expansion, CoW snapshots, cloning, and usage reporting (`NodeGetVolumeStats`, `GetCapacity`)
- [x] Per-node Host & InitiatorGroup ACL automation during `ControllerPublishVolume`
- [x] Volume Group Snapshots (`GroupController` / KEP-3476)
- [x] Changed Block Tracking (`SnapshotMetadata` / KEP-3314), opt-in (docs/review-history.md L-2)
- [x] Official CSI sanity suite (`csi-test/v5`: 82 passed, 0 failed) & `k3s` VM lab end-to-end verification
- [x] Multi-arch (`linux/amd64`, `linux/arm64`) `velocor-csi` container build & Helm chart (`deploy/charts/velocor-csi`)
- [x] GitHub Actions CI/CD release pipeline (`.github/workflows/ci-release.yml`) with Cosign keyless signing, SPDX SBOMs, SLSA build provenance, and OCI Helm publishing
- [x] OpenShift cluster validation (SCC/SELinux, NVMe-oF/TCP, `VolumeSnapshot`, online expansion; `VolumeGroupSnapshot` skipped where CRD absent)

## Phase 7: Operator Parity & Appliance Lifecycle Management (`veloctl` & Web UI)
- [x] `veloctl` CLI and Web UI support for Volume Group Snapshots (Consistency Groups)
- [x] Web UI & `veloctl` parity for `velocor-os` (`bootc`) status, upgrades, and rollbacks
- [x] Containers ship inside the OS image, pinned by digest; they change only with `bootc upgrade` (the separate container-update path was removed)

## Phase 8: Hardening & Storage Failure-Mode Testing
Open work is tracked in [TODO.md](../TODO.md); finished items in [review-history.md](review-history.md).
- [x] Code reviews of 2026-11-15/16 and their fixes (data loss, security, operational; review-history.md)
- [x] RAID re-assembly after reboots, disk replacement, rebuild and degraded start (P0-1, P1-8)
- [x] Integration suite on a real data plane: crash/restart, rebuild, degraded start, scrub repair, group snapshots, capacity, pool import
- [x] Lab failure testing on a VM appliance: hot-unplug under writes, reboots without members, metadata loss and import
- [ ] Emulated and real disk faults (read errors, failing drives), power-loss tests (TODO.md P2-2, P0-1, P0-4)
- [ ] API audit log (TODO.md A-1); CHAP and real host identities (TODO.md P2-1)
- [ ] Initiator resilience: iSCSI multipath, two SAN paths (TODO.md P1-13); single-node scope documented (TODO.md P1-6)

## Phase 9: Kernel data plane (decided 2026-11-20)
The data plane moves from SPDK to md RAID, LVM thin and the kernel NVMe/TCP and iSCSI targets; the control plane, API, CSI driver and UI keep their shape. Why, what was measured and the plan: [kernel-dataplane.md](design/kernel-dataplane.md).
- [x] Design review, QoS and chunk-size decisions (K-0)
- [x] Spike behind the data-plane interface with the new test harness (K-1)
- [x] Feature parity, including iSCSI via LIO, scrub on md, CBT via `thin_delta` (K-2)
- [x] Validation on both clusters, P0-7 scenario, benchmark (K-3)
- [ ] Cutover: SPDK removed from code, images and CI (done 2026-11-24); soak (K-4)
