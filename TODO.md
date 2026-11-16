# Velocor remediation TODO

Source: full code review (2026-11-15). Items are ordered by fix priority.
Mark `[x]` when fixed **and** covered by a test. "HW" = needs verification on real hardware/cluster.

## Critical — data loss / exposure

- [x] **C1. Reconciler can wipe pools on SPDK restart** — `internal/reconciler/reconciler.go`
  - [x] lvstore list fetched before RAID re-created → `bdev_lvol_create_lvstore` on a RAID whose lvstore is still being examined (HW: confirm examine timing on SPDK v26.09)
  - [x] Never create an lvstore for a pool that already has `LVSUUID`; call `bdev_wait_for_examine` after RAID create, re-fetch lvstores; mark DEGRADED instead
  - [x] Never re-create a missing lvol that had data (`LvolUUID` set) — mark volume DEGRADED/MISSING instead of creating a blank one
- [x] **C2. iSCSI LUN masking broken** — `internal/server/server.go` (iSCSI export)
  - [x] Unique portal-group / initiator-group tags per export (not hard-coded 1/1)
  - [x] Propagate errors from PG/IG/target-node creation
  - [x] No implicit `ANY` access — require explicit `AllowAnyHost`
  - [x] Reconciler restores iSCSI targets after SPDK restart
- [x] **C3. Snapshot lvol names collide across volumes** — snapshot create/delete/CBT
  - [x] Namespace SPDK snapshot names (e.g. by snapshot ID) and store the SPDK alias/UUID on the record
  - [x] Delete/CBT by stored UUID, not by `lvs/<name>`
- [x] **C4. SPDK errors swallowed; state persisted anyway** — `internal/server/server.go`
  - [x] Claim disk (fabricated bdev names, NVMe left unbound on failure)
  - [x] Create pool (ONLINE with no RAID when bdev name missing)
  - [x] Create volume / clone / restore-from-snapshot
  - [x] Resize, QoS, snapshot, export
  - [x] Deletes: call SPDK first, then store (no leaked lvols)
  - [x] Group snapshot: member names exceed SPDK 63-char lvol limit (HW: confirm external-snapshotter name format); fail instead of reporting ReadyToUse
- [x] **C5. CSI node may mount/format wrong NVMe device** — `internal/csi/mounter.go`
  - [x] Resolve block device via sysfs `subsysnqn` + nsid (multipath-aware naming) (HW)
  - [x] Verify namespace UUID == lvol UUID before use
  - [x] `detectFilesystem`: only treat blkid exit 2 as "empty"; refuse to format if PTTYPE present or blkid errors otherwise
- [x] **C6. Boot-disk guard gaps** — `internal/hardware/manager.go`
  - [x] Resolve `/dev/mapper/*` (LVM/LUKS) sources to physical disks
  - [x] Protect any disk with mounted partitions, holders, active swap, FS/partition signatures (unless forced)
  - [x] NVMe multipath `nvmeXcYnZ` sysfs entries when resolving boot NVMe PCI BDF (HW)
  - [x] Stable SSD IDs via `/dev/disk/by-id` (WWN/serial) instead of `sdX`

- [x] **C7. CBT fabricated allocation data** — `BdevLvolGetFragmap` fallback synthesised a bitmap marking the first N clusters as changed when SPDK lacked the RPC → incremental backups silently wrong. Now fails with 501.
- [x] **C8. CSI SnapshotMetadata route/field mismatch** — CSI client called `:allocated-blocks` / `:changed-blocks` (server only had `/blocks:*`) and read `length_bytes` (server sent `size_bytes`).
- [x] **C9. Boot guard failed open** — velod ran with `hw == nil` (no guard at all) if hardware manager init failed. Now exits.
- [x] **C10. Encryption/compression filter policies silently ignored** — requests now rejected until implemented.

## High

- [x] **H1. CSI publish/unpublish ignore node_id** — exports must be per (volume, node); unpublish removes only that node's access
- [x] **H2. CSI `PublishedNodeIds` returns initiator-group IDs** (ListVolumes + ControllerGetVolume)
- [x] **H3. CSI DeleteVolume blocked by snapshots/clones** — snapshots must outlive source PVC
- [x] **H4. RAID created with `superblock=false`**; reconciler requires all members → redundant pools never come back degraded
- [x] **H5. Lost updates** — handlers/reconciler read-modify-write whole records outside a transaction; add field-level/atomic update helpers in store
- [x] **H6. Sizes rounded down to MiB** — `internal/spdk/lvol.go` create/resize must round up

## Security

- [x] **S1. Shell injection in container update** — `internal/server/lifecycle.go` (`%q` in `sh -c`); validate unit names, pass argv
- [x] **S2. JWT not revocable** — re-check user existence/role/password-change epoch per request
- [x] **S3. Default admin password** `velocor-admin-changeme` — generate random first-boot password / force change
- [x] **S4. Login rate limiting**
- [x] **S5. Password hashing** — replace iterated SHA-256 with PBKDF2-SHA256 (600k iterations, stdlib)
- [x] **S6. `/metrics` unauthenticated**
- [x] **S7. Helm chart defaults `insecure: "true"`**
- [x] **S8. Request body limits + HTTP server Read/Write/Idle timeouts**
- [x] **S9. iSCSI node plugin overwrites host `/etc/iscsi/initiatorname.iscsi`** — use an iscsiadm iface
- [ ] **S10. CHAP modelled but not wired**; predictable NVMe host NQNs — *interim:* API now rejects `chap_user` instead of silently ignoring it. Remaining: iSCSI CHAP auth groups + NVMe DH-HMAC-CHAP with encrypted secrets; node plugin should register the host's real hostnqn/IQN instead of names derived from the node name.
- [x] **S11. Validate resource names** before using them in SPDK names (charset/length)
- [x] **S12. modprobe.d enables vfio noiommu unconditionally**

## Operational / medium

- [x] **M1. SPDK quadlet `AutoUpdate=registry` + `:latest`** — "apply all updates" restarts data plane under I/O; exclude SPDK, pin tag
- [x] **M2. PAT use does a bbolt write+fsync per request** — throttle last-used updates
- [x] **M3. Reconciler rewrites every record every 15s** — write only on change
- [x] **M4. Reconciler doesn't repair partially-created NVMe-oF subsystems** (missing ns/listener)
- [x] **M5. Group snapshots not crash-consistent** — pause subsystems during snapshot
- [x] **M6. NVMe-oF listener hard-coded IPv4** — detect adrfam
- [x] **M7. No iSCSI rescan on NodeExpandVolume**
- [x] **M8. Docs claim gRPC/grpc-gateway** — update docs (or remove unused protos)

## Follow-ups discovered during remediation

- [ ] **F1.** HW: confirm `bdev_raid_delete` on a superblock RAID wipes member superblocks; otherwise released disks auto-assemble an orphan array on next attach.
- [ ] **F2.** HW: confirm SPDK assembles a superblock RAID1/5F *degraded* when a member is missing at boot (pool currently reported DEGRADED, never re-created).
- [ ] **F5.** Reconciler `EnsureExport` lists subsystems/target nodes per export (O(n²) RPCs); prefetch once per pass if export counts grow.
- [x] **F6.** Web UI / veloctl: surface `force=true` for disk claim, `allow_any_host` for exports, `status_message`, forced initial password change, data-plane update consent.

- [ ] **F7.** Auth/server test suites are slow (~50-100 s under `-race`) because every login runs 600k PBKDF2 iterations; consider a test-only iteration override.
- [x] **F9.** Session revocation precision: `iat` was second- (then millisecond-) granular, so a token issued just before a password/role change survived it. Now nanosecond `iat_ns`.

- [x] **F10.** Pre-release cleanup: removed all migration/compatibility code (legacy SSD ID migration, iSCSI tag backfill, non-superblock RAID re-creation, reconciler lvstore initialisation, v1 password hashes, second-granular JWTs, legacy CSI export attribution, duplicate CBT routes, whole-record `Update*` store methods).

## Testing

- [x] **T1. Fake SPDK JSON-RPC socket for tests** supporting error injection and "restart" (empty state) to cover C1–C4
