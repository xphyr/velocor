package server

// Data-plane handlers (disks, pools, volumes, snapshots, exports).
//
// Invariants enforced here (see TODO C2-C4):
//   - SPDK is called first; the store is only updated after SPDK succeeds, and any
//     partially created SPDK object is rolled back if the store write fails.
//   - Deletes validate store preconditions, then delete in SPDK (missing objects are
//     fine), then delete the record, so a failed SPDK delete never leaks an object
//     that the store has forgotten.
//   - SPDK object names are derived from resource IDs, never from user-supplied names,
//     so they are unique across volumes/snapshots and always fit SPDK's limits.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/store"
)

var errDataPlaneUnavailable = errors.New("the data plane is not configured")

func (s *Server) requireDataPlane() error {
	if s.dp == nil {
		return errDataPlaneUnavailable
	}
	return nil
}

// validateDisplayName checks user-facing resource names (not used in SPDK identifiers).
func validateDisplayName(kind, name string) error {
	if name == "" || len(name) > 128 {
		return fmt.Errorf("%w: %s name must be 1-128 characters", store.ErrInvalidArgument, kind)
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '/' {
			return fmt.Errorf("%w: %s name %q contains an invalid character", store.ErrInvalidArgument, kind, name)
		}
	}
	return nil
}

func decodeJSON(r *http.Request, dst any) error {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return fmt.Errorf("%w: invalid json body: %v", store.ErrInvalidArgument, err)
	}
	return nil
}

// decodeOptionalJSON is decodeJSON for requests whose fields are all optional: an empty body
// means the defaults.
func decodeOptionalJSON(r *http.Request, dst any) error {
	err := json.NewDecoder(r.Body).Decode(dst)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: invalid json body: %v", store.ErrInvalidArgument, err)
	}
	return nil
}

func rejectFilters(fp store.FilterPolicy) error {
	if fp.EncryptionEnabled || fp.CompressionEnabled {
		// Accepting these silently would store data in plaintext / uncompressed while
		// telling the operator otherwise.
		return fmt.Errorf("%w: encryption and compression filter policies are not implemented yet", store.ErrInvalidArgument)
	}
	return nil
}

// --- Disks ---

func (s *Server) handleScanDisks(w http.ResponseWriter, _ *http.Request) {
	if s.hw != nil {
		if err := s.hw.RefreshBootGuard(); err != nil {
			writeError(w, fmt.Errorf("refresh boot guard: %w", err))
			return
		}
		discovered, err := s.hw.DiscoverDisks()
		if err != nil {
			writeError(w, err)
			return
		}
		for _, d := range discovered {
			if err := s.store.SyncDiscoveredDisk(d); err != nil {
				writeError(w, err)
				return
			}
		}
	}
	disks, err := s.store.ListDisks()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disks": disks})
}

func (s *Server) claimDisk(w http.ResponseWriter, r *http.Request, id string) {
	d, err := s.store.GetDisk(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if d.IsBootDevice {
		writeError(w, store.ErrBootDeviceProtected)
		return
	}
	if d.Claimed {
		writeJSON(w, http.StatusOK, map[string]any{"disk": d})
		return
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	updated, err := s.attachAndClaim(r.Context(), d, r.URL.Query().Get("force") == "true")
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disk": updated})
}

// attachAndClaim hands an unclaimed disk to SPDK and records it as claimed; on failure the
// disk is returned to the kernel. force skips the existing-data check (never the in-use or
// boot-disk checks).
func (s *Server) attachAndClaim(ctx context.Context, d *store.Disk, force bool) (*store.Disk, error) {
	if s.hw != nil {
		if err := s.hw.CheckClaimable(d, force); err != nil {
			return nil, err
		}
	}
	if d.WriteCachePolicy == store.WriteCachePolicyDisabled && s.hw != nil && d.Type != store.DiskTypeNVMe {
		if _, err := s.applyWriteCachePolicy(ctx, d); err != nil {
			return nil, err
		}
	}
	bdev, drv, err := s.dp.AttachDisk(ctx, d)
	if err != nil {
		return nil, err
	}
	cacheState := store.WriteCacheUnknown
	if d.WriteCachePolicy == store.WriteCachePolicyDisabled && d.Type == store.DiskTypeNVMe {
		// The kernel no longer sees the drive: switch its cache off through the data plane
		// before anything is written to it.
		attached := *d
		attached.Handle = bdev
		if cacheState, err = s.dp.SetNVMeWriteCache(ctx, &attached, false); err != nil {
			_ = s.dp.DetachDisk(context.WithoutCancel(ctx), &attached)
			if s.hw != nil {
				_ = s.hw.BindNVMeToKernel(d.PCIAddress)
			}
			return nil, fmt.Errorf("%w: %v", store.ErrInvalidArgument, err)
		}
	}
	updated, err := s.store.ClaimDisk(d.ID, drv, bdev)
	if err == nil && cacheState != store.WriteCacheUnknown {
		updated, err = s.store.MutateDisk(d.ID, func(cur *store.Disk) error { cur.WriteCache = cacheState; return nil })
	}
	if err != nil {
		attached := *d
		attached.Handle = bdev
		_ = s.dp.DetachDisk(context.WithoutCancel(ctx), &attached)
		if d.Type == store.DiskTypeNVMe && s.hw != nil {
			_ = s.hw.BindNVMeToKernel(d.PCIAddress)
		}
		return nil, err
	}
	return updated, nil
}

func (s *Server) releaseDisk(w http.ResponseWriter, r *http.Request, id string) {
	d, err := s.store.GetDisk(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if d.PoolID != "" {
		writeError(w, fmt.Errorf("%w: disk %s belongs to pool %s", store.ErrInUse, id, d.PoolID))
		return
	}
	if !d.Claimed {
		writeJSON(w, http.StatusOK, map[string]any{"disk": d})
		return
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	if err := s.dp.DetachDisk(r.Context(), d); err != nil {
		writeError(w, err)
		return
	}
	updated, err := s.store.ReleaseDisk(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if d.Type == store.DiskTypeNVMe && s.hw != nil && d.PCIAddress != "" {
		if err := s.hw.BindNVMeToKernel(d.PCIAddress); err != nil {
			writeError(w, fmt.Errorf("disk released from the data plane but rebinding %s to the kernel nvme driver failed: %w", d.PCIAddress, err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"disk": updated})
}

// --- Pools ---

func (s *Server) handleCreatePool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string             `json:"name"`
		RaidLevel    store.RaidLevel    `json:"raid_level"`
		DiskIDs      []string           `json:"disk_ids"`
		StripSizeKB  uint32             `json:"strip_size_kb"`
		FilterPolicy store.FilterPolicy `json:"filter_policy"`
		SpacePolicy  store.SpacePolicy  `json:"space_policy"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if err := req.SpacePolicy.Validate(); err != nil {
		writeError(w, err)
		return
	}
	if req.RaidLevel == "" {
		req.RaidLevel = store.RaidLevel0
	}
	if req.StripSizeKB == 0 {
		req.StripSizeKB = 64
	}
	for _, err := range []error{
		validateDisplayName("pool", req.Name),
		s.requireDataPlane(),
		s.checkLevel(req.RaidLevel, len(req.DiskIDs)),
		rejectFilters(req.FilterPolicy),
	} {
		if err != nil {
			writeError(w, err)
			return
		}
	}

	var totalRaw uint64
	baseBdevs := make([]string, 0, len(req.DiskIDs))
	for _, diskID := range req.DiskIDs {
		d, err := s.store.GetDisk(diskID)
		if err != nil {
			writeError(w, fmt.Errorf("disk %s: %w", diskID, err))
			return
		}
		if !d.Claimed || d.Handle == "" {
			writeError(w, fmt.Errorf("%w: disk %s is not claimed", store.ErrInvalidArgument, diskID))
			return
		}
		totalRaw += d.SizeBytes
		baseBdevs = append(baseBdevs, d.Handle)
	}

	id := newResourceID("pool")
	pool := &store.Pool{
		ID:           id,
		Name:         req.Name,
		RaidLevel:    req.RaidLevel,
		DiskIDs:      req.DiskIDs,
		StripSizeKB:  req.StripSizeKB,
		ArrayName:    "raid_" + id,
		Device:       "raid_" + id,
		ThinPoolName: "lvs_" + id,
		TotalBytes:   totalRaw,
		FreeBytes:    totalRaw,
		FilterPolicy: req.FilterPolicy,
		SpacePolicy:  req.SpacePolicy,
		// The reconciler leaves RECONCILING pools without an lvstore UUID alone.
		HealthStatus:  store.HealthStatusReconciling,
		StatusMessage: "creating",
	}
	// The store transaction validates and reserves the member disks atomically.
	if err := s.store.CreatePool(pool); err != nil {
		writeError(w, err)
		return
	}
	ctx := r.Context()
	cleanup := context.WithoutCancel(ctx)
	fail := func(raidCreated bool, err error) {
		if raidCreated {
			// The thin pool may exist even though creating it "failed" (e.g. on a timeout), and
			// the array can't be deleted from under it: remove the thin pool first.
			if derr := s.dp.DeleteThinPool(cleanup, "", pool.ThinPoolName); derr != nil && !dataplane.IsNotFound(derr) {
				err = fmt.Errorf("%w (rollback: delete thin pool %s: %v; array %s left in place)", err, pool.ThinPoolName, derr, pool.ArrayName)
				_ = s.store.DeletePool(pool.ID)
				writeError(w, err)
				return
			}
			_ = s.dp.DeleteArray(cleanup, pool.ArrayName)
		}
		_ = s.store.DeletePool(pool.ID)
		writeError(w, err)
	}
	if err := s.dp.CreateArray(ctx, pool.ArrayName, pool.RaidLevel, pool.StripSizeKB, baseBdevs); err != nil {
		fail(false, fmt.Errorf("create the array: %w", err))
		return
	}
	lvsUUID, err := s.dp.CreateThinPool(ctx, pool.Device, pool.ThinPoolName)
	if err != nil {
		fail(true, fmt.Errorf("create the thin pool: %w", err))
		return
	}
	updated, err := s.store.MutatePool(pool.ID, func(p *store.Pool) error {
		p.ThinPoolUUID = lvsUUID
		p.HealthStatus, p.StatusMessage = store.HealthStatusOnline, ""
		if stores, err := s.dp.ThinPools(ctx, "", lvsUUID); err == nil && len(stores) > 0 {
			p.TotalBytes, p.FreeBytes = stores[0].TotalBytes, stores[0].FreeBytes
			p.ChunkSizeBytes = stores[0].ChunkBytes
			p.AllocatedBytes = p.TotalBytes - p.FreeBytes
		}
		p.SpaceState = store.SpaceStateOf(p)
		p.AvailableBytes = store.AvailableBytes(p, 0) // a new pool has nothing committed
		return nil
	})
	if err != nil {
		fail(true, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"pool": updated})
}

func (s *Server) handleDeletePool(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	pool, err := s.store.GetPool(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.CheckPoolDeletable(id); err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	lvsUUID, lvsName := pool.ThinPoolUUID, ""
	if lvsUUID == "" {
		lvsName = pool.ThinPoolName
	}
	if err := s.dp.DeleteThinPool(r.Context(), lvsUUID, lvsName); err != nil && !dataplane.IsNotFound(err) {
		writeError(w, fmt.Errorf("delete the thin pool: %w", err))
		return
	}
	if err := s.dp.DeleteArray(r.Context(), pool.ArrayName); err != nil && !dataplane.IsNotFound(err) {
		writeError(w, fmt.Errorf("delete the array: %w", err))
		return
	}
	if err := s.store.DeletePool(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// --- Volumes ---

func poolUsable(pool *store.Pool) error {
	switch {
	case pool.ThinPoolUUID == "",
		pool.HealthStatus == store.HealthStatusFaulted,
		pool.HealthStatus == store.HealthStatusReconciling,
		pool.HealthStatus == store.HealthStatusUnavailable: // RAID waiting for a missing disk
		return fmt.Errorf("%w: pool %s is not usable (%s %s)", store.ErrInvalidArgument, pool.ID, pool.HealthStatus, pool.StatusMessage)
	}
	return nil
}

// rejectQoS refuses I/O limits: the kernel targets have none, so they can't be honoured
// (docs/design/kernel-dataplane.md, section 6). Limits of zero ask for nothing.
func rejectQoS(raw json.RawMessage) error {
	var q map[string]uint64
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return fmt.Errorf("%w: qos_policy: %v", store.ErrInvalidArgument, err)
	}
	for k, v := range q {
		if v != 0 {
			return fmt.Errorf("%w: I/O limits (qos_policy.%s) are not supported", store.ErrInvalidArgument, k)
		}
	}
	return nil
}

// volumeSizeUnit is the data plane's allocation unit for volume sizes (an LVM extent).
const volumeSizeUnit = 4 << 20

// roundVolumeSize rounds a volume's size up to what the data plane will make it, so that the
// record says what hosts see.
func roundVolumeSize(n uint64) uint64 {
	return (n + volumeSizeUnit - 1) / volumeSizeUnit * volumeSizeUnit
}

func (s *Server) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		PoolID    string `json:"pool_id"`
		SizeBytes uint64 `json:"size_bytes"`
		// ThinProvision is accepted and ignored: every volume is thin, and the space accounting
		// counts its full size as committed.
		ThinProvision bool               `json:"thin_provision"`
		FilterPolicy  store.FilterPolicy `json:"filter_policy"`
		QosPolicy     json.RawMessage    `json:"qos_policy"` // refused
		Labels        map[string]string  `json:"labels"`
		// DeletionProtection refuses deletion until an administrator turns it off.
		DeletionProtection bool `json:"deletion_protection"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if err := validateDisplayName("volume", req.Name); err != nil {
		writeError(w, err)
		return
	}
	if req.SizeBytes == 0 {
		writeError(w, fmt.Errorf("%w: size_bytes must be > 0", store.ErrInvalidArgument))
		return
	}
	req.SizeBytes = roundVolumeSize(req.SizeBytes)
	if err := errors.Join(rejectFilters(req.FilterPolicy), rejectQoS(req.QosPolicy)); err != nil {
		writeError(w, err)
		return
	}

	// Idempotency check by (PoolID, Name) for Kubernetes CSI CreateVolume retry semantics.
	if existing, err := s.store.GetVolumeByName(req.PoolID, req.Name); err == nil {
		if existing.SizeBytes == req.SizeBytes && existing.ParentSnapshotID == "" {
			writeJSON(w, http.StatusOK, map[string]any{"volume": existing})
			return
		}
		writeError(w, fmt.Errorf("%w: volume %s already exists with size %d", store.ErrAlreadyExists, req.Name, existing.SizeBytes))
		return
	}

	pool, err := s.store.GetPool(req.PoolID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := poolUsable(pool); err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}

	id := newResourceID("vol")
	release, err := s.reserveSpace(r.Context(), pool.ID, req.SizeBytes, volumeRecorded(id, req.SizeBytes))
	if err != nil {
		writeError(w, err)
		return
	}
	defer release()
	alias := pool.ThinPoolName + "/" + id
	uuid, err := s.dp.CreateVolume(r.Context(), pool.ThinPoolName, id, req.SizeBytes, true)
	if err != nil {
		writeError(w, fmt.Errorf("create the volume: %w", err))
		return
	}
	vol := &store.Volume{
		ID:                 id,
		Name:               req.Name,
		PoolID:             req.PoolID,
		UUID:               uuid,
		Handle:             alias,
		Device:             alias,
		SizeBytes:          req.SizeBytes,
		ThinProvision:      true,
		FilterPolicy:       req.FilterPolicy,
		AttachmentState:    store.AttachmentStateUnattached,
		HealthStatus:       store.HealthStatusOnline,
		Labels:             req.Labels,
		DeletionProtection: req.DeletionProtection,
	}
	s.finishVolumeCreate(w, r, vol, http.StatusCreated)
}

// finishVolumeCreate persists a volume that already exists in the data plane, deleting it there
// again if that fails.
func (s *Server) finishVolumeCreate(w http.ResponseWriter, r *http.Request, vol *store.Volume, status int) {
	rollback := func(err error) {
		_ = s.dp.DeleteVolume(context.WithoutCancel(r.Context()), vol.UUID)
		writeError(w, err)
	}
	if err := s.store.CreateVolume(vol); err != nil {
		rollback(err)
		return
	}
	writeJSON(w, status, map[string]any{"volume": vol})
}

func (s *Server) handleVolumeAction(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/volumes/")
	if id, ok := strings.CutSuffix(tail, ":resize"); ok {
		s.resizeVolume(w, r, id)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) resizeVolume(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		NewSizeBytes uint64 `json:"new_size_bytes"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	vol, err := s.liveVolume(id)
	if err != nil {
		writeError(w, err)
		return
	}
	req.NewSizeBytes = roundVolumeSize(req.NewSizeBytes)
	if req.NewSizeBytes < vol.SizeBytes {
		writeError(w, fmt.Errorf("%w: new_size_bytes (%d) must be >= current size (%d)", store.ErrInvalidArgument, req.NewSizeBytes, vol.SizeBytes))
		return
	}
	if req.NewSizeBytes == vol.SizeBytes {
		writeJSON(w, http.StatusOK, map[string]any{"volume": vol})
		return
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	release, err := s.reserveSpace(r.Context(), vol.PoolID, req.NewSizeBytes-vol.SizeBytes, volumeRecorded(vol.ID, req.NewSizeBytes))
	if err != nil {
		writeError(w, err)
		return
	}
	defer release()
	if err := s.dp.ResizeVolume(r.Context(), vol.UUID, req.NewSizeBytes); err != nil {
		writeError(w, fmt.Errorf("resize the volume: %w", err))
		return
	}
	updated, err := s.store.MutateVolume(id, func(v *store.Volume) error {
		v.SizeBytes = max(v.SizeBytes, req.NewSizeBytes)
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volume": updated})
}

// --- Snapshots ---

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	volID := r.PathValue("volume_id")
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if err := validateDisplayName("snapshot", req.Name); err != nil {
		writeError(w, err)
		return
	}

	// Idempotent by (VolumeID, Name) for Kubernetes CSI CreateSnapshot retries.
	if existing, err := s.store.GetSnapshotByName(volID, req.Name); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"snapshot": existing})
		return
	}

	vol, err := s.liveVolume(volID)
	if err != nil {
		writeError(w, err)
		return
	}
	pool, err := s.store.GetPool(vol.PoolID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	snap, release, err := s.takeSnapshot(r.Context(), vol, pool, req.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	defer release()
	if err := s.store.CreateSnapshot(snap); err != nil {
		_ = s.dp.DeleteVolume(context.WithoutCancel(r.Context()), snap.UUID)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"snapshot": snap})
}

// takeSnapshot creates the SPDK snapshot lvol (named after the new snapshot ID) and
// returns an unsaved Snapshot record for it.
//
// The snapshot takes over the clusters the volume holds, which then count as committed space:
// the space is reserved first, and release must be called once the snapshot is in the store
// (or was rolled back).
func (s *Server) takeSnapshot(ctx context.Context, vol *store.Volume, pool *store.Pool, name string) (*store.Snapshot, func(), error) {
	held, err := s.lvolHeldBytes(ctx, vol.UUID)
	if err != nil {
		return nil, nil, err
	}
	id := newResourceID("snap")
	release, err := s.reserveSpace(ctx, pool.ID, held, snapshotRecorded(id))
	if err != nil {
		return nil, nil, err
	}
	uuid, err := s.dp.Snapshot(ctx, vol.UUID, id)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("snapshot volume %s: %w", vol.ID, err)
	}
	return &store.Snapshot{
		ID:             id,
		Name:           name,
		VolumeID:       vol.ID,
		PoolID:         pool.ID,
		UUID:           uuid,
		Handle:         pool.ThinPoolName + "/" + id,
		SizeBytes:      vol.SizeBytes,
		AllocatedBytes: held,
	}, release, nil
}

func (s *Server) handleSnapshotAction(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/snapshots/")
	if snapID, ok := strings.CutSuffix(tail, ":clone"); ok {
		s.cloneSnapshot(w, r, snapID)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) cloneSnapshot(w http.ResponseWriter, r *http.Request, snapID string) {
	var req struct {
		CloneName    string             `json:"clone_name"`
		FilterPolicy store.FilterPolicy `json:"filter_policy"`
		QosPolicy    json.RawMessage    `json:"qos_policy"` // refused
		Labels       map[string]string  `json:"labels"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if err := validateDisplayName("volume", req.CloneName); err != nil {
		writeError(w, err)
		return
	}
	if err := errors.Join(rejectFilters(req.FilterPolicy), rejectQoS(req.QosPolicy)); err != nil {
		writeError(w, err)
		return
	}
	snap, err := s.liveSnapshot(snapID)
	if err != nil {
		writeError(w, err)
		return
	}
	pool, err := s.store.GetPool(snap.PoolID)
	if err != nil {
		writeError(w, err)
		return
	}
	if existing, err := s.store.GetVolumeByName(pool.ID, req.CloneName); err == nil {
		if existing.ParentSnapshotID == snapID {
			writeJSON(w, http.StatusOK, map[string]any{"volume": existing})
			return
		}
		writeError(w, fmt.Errorf("%w: volume %s already exists", store.ErrAlreadyExists, req.CloneName))
		return
	}
	if err := poolUsable(pool); err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}

	id := newResourceID("vol")
	release, err := s.reserveSpace(r.Context(), pool.ID, snap.SizeBytes, volumeRecorded(id, snap.SizeBytes))
	if err != nil {
		writeError(w, err)
		return
	}
	defer release()
	alias := pool.ThinPoolName + "/" + id
	uuid, err := s.dp.Clone(r.Context(), snap.UUID, id)
	if err != nil {
		writeError(w, fmt.Errorf("clone snapshot %s: %w", snap.ID, err))
		return
	}
	clone := &store.Volume{
		ID:               id,
		Name:             req.CloneName,
		PoolID:           pool.ID,
		UUID:             uuid,
		Handle:           alias,
		Device:           alias,
		SizeBytes:        snap.SizeBytes,
		ThinProvision:    true,
		ParentSnapshotID: snap.ID,
		FilterPolicy:     req.FilterPolicy,
		AttachmentState:  store.AttachmentStateUnattached,
		HealthStatus:     store.HealthStatusOnline,
		Labels:           req.Labels,
	}
	s.finishVolumeCreate(w, r, clone, http.StatusCreated)
}

func (s *Server) handleSnapshotGetOrBlocks(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/snapshots/")
	if id, ok := strings.CutSuffix(tail, "/blocks:allocated"); ok {
		s.handleSnapshotBlocks(w, r, id, false)
		return
	}
	if id, ok := strings.CutSuffix(tail, "/blocks:changed"); ok {
		s.handleSnapshotBlocks(w, r, id, true)
		return
	}
	snap, err := s.liveSnapshot(tail)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snap})
}

// handleSnapshotBlocks answers blocks:allocated (every block of the snapshot that holds data)
// and blocks:changed (the blocks written between base_snapshot_id and the snapshot), from the
// data plane's allocation maps (Plane.AllocatedBlocks / ChangedBlocks).
func (s *Server) handleSnapshotBlocks(w http.ResponseWriter, r *http.Request, targetID string, changed bool) {
	target, err := s.liveSnapshot(targetID)
	if err != nil {
		writeError(w, err)
		return
	}
	var base *store.Snapshot
	if changed {
		baseID := r.URL.Query().Get("base_snapshot_id")
		if baseID == "" {
			writeError(w, fmt.Errorf("%w: base_snapshot_id query parameter is required", store.ErrInvalidArgument))
			return
		}
		if base, err = s.liveSnapshot(baseID); err != nil {
			writeError(w, err)
			return
		}
		if base.VolumeID != target.VolumeID || base.ID == target.ID {
			writeError(w, fmt.Errorf("%w: snapshots %s and %s aren't two snapshots of one volume", store.ErrInvalidArgument, baseID, targetID))
			return
		}
	}
	pool, err := s.store.GetPool(target.PoolID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	var extents []dataplane.Extent
	if base != nil {
		extents, err = s.dp.ChangedBlocks(r.Context(), pool, base, target)
	} else {
		extents, err = s.dp.AllocatedBlocks(r.Context(), pool, target)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	resp := map[string]any{
		"snapshot_id":       target.ID,
		"volume_size_bytes": target.SizeBytes,
		"chunk_size_bytes":  pool.ChunkSizeBytes,
		"extents":           formatBlockExtents(extents),
	}
	if base != nil {
		resp["base_snapshot_id"] = base.ID
		resp["target_snapshot_id"] = target.ID
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snap, err := s.store.GetSnapshot(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.deleteSnapshot(r.Context(), snap); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// deleteSnapshot deletes a snapshot, or, while cloned volumes depend on it, hides it and
// leaves its deletion to the reconciler once they are gone (docs/review-history.md P1-4).
func (s *Server) deleteSnapshot(ctx context.Context, snap *store.Snapshot) error {
	deferred, err := s.store.DeferSnapshotDelete(snap.ID, time.Now(), subjectFromContext(ctx))
	if err != nil || deferred {
		return err
	}
	if err := s.requireDataPlane(); err != nil {
		return err
	}
	if err := s.dp.DeleteVolume(ctx, snap.UUID); err != nil && !dataplane.IsNotFound(err) {
		return fmt.Errorf("delete the snapshot: %w", err)
	}
	if err := s.store.DeleteSnapshot(snap.ID); err != nil {
		return err
	}
	s.updatePoolSpace(snap.PoolID)
	return nil
}

// --- Volume group snapshots ---

// groupSnapshotQuiesceTimeout bounds how long a group snapshot waits for I/O already in flight to
// the members to complete. Hosts' new I/O waits meanwhile, so it stays well under their I/O
// timeouts (30 s for Linux NVMe/TCP and iSCSI).
const groupSnapshotQuiesceTimeout = 5 * time.Second

// groupSnapshotError explains the group snapshot failures that callers can act on.
func groupSnapshotError(err error) error {
	switch {
	case errors.Is(err, dataplane.ErrQuiesceTimeout): // no snapshot was taken
		return fmt.Errorf("%w: I/O to the volumes didn't pause within %s (a disk may be slow); no snapshot was taken, try again: %v",
			errDataPlaneUnavailable, groupSnapshotQuiesceTimeout, err)
	case errors.Is(err, dataplane.ErrUnsupported):
		return fmt.Errorf("%w: this data plane can't take group snapshots: %v", errDataPlaneUnavailable, err)
	}
	return fmt.Errorf("group snapshot: %w", err)
}

func (s *Server) handleCreateVolumeGroupSnapshot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string   `json:"name"`
		VolumeIDs []string `json:"volume_ids"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if err := validateDisplayName("group snapshot", req.Name); err != nil {
		writeError(w, err)
		return
	}
	if len(req.VolumeIDs) == 0 {
		writeError(w, fmt.Errorf("%w: non-empty volume_ids are required", store.ErrInvalidArgument))
		return
	}
	seen := make(map[string]bool, len(req.VolumeIDs))
	volIDs := make([]string, 0, len(req.VolumeIDs))
	for _, id := range req.VolumeIDs {
		if id == "" {
			writeError(w, fmt.Errorf("%w: volume_ids must not contain empty values", store.ErrInvalidArgument))
			return
		}
		if !seen[id] {
			seen[id] = true
			volIDs = append(volIDs, id)
		}
	}

	// Idempotent by Name when volume sets match; AlreadyExists on mismatch.
	if existing, err := s.store.GetVolumeGroupSnapshotByName(req.Name); err == nil {
		if !equalStringSets(existing.VolumeIDs, volIDs) {
			writeError(w, fmt.Errorf("%w: volume group snapshot %s already exists with different volumes", store.ErrAlreadyExists, req.Name))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"volume_group_snapshot": existing,
			"snapshots":             s.resolveGroupSnapshots(existing.SnapshotIDs),
		})
		return
	}

	type target struct {
		vol  *store.Volume
		pool *store.Pool
	}
	targets := make([]target, 0, len(volIDs))
	for _, volID := range volIDs {
		vol, err := s.liveVolume(volID)
		if err != nil {
			writeError(w, err)
			return
		}
		pool, err := s.store.GetPool(vol.PoolID)
		if err != nil {
			writeError(w, err)
			return
		}
		targets = append(targets, target{vol, pool})
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	if max := s.dp.MaxSnapshotGroup(); len(targets) > max {
		writeError(w, fmt.Errorf("%w: a group snapshot can cover at most %d volumes", store.ErrInvalidArgument, max))
		return
	}

	ctx := r.Context()
	cleanup := context.WithoutCancel(ctx)

	// Reserve the space each snapshot pins (as takeSnapshot does) before taking any.
	var releases []func()
	defer func() {
		for _, rel := range releases {
			rel()
		}
	}()
	snaps := make([]*store.Snapshot, 0, len(targets))
	entries := make([]dataplane.GroupEntry, 0, len(targets))
	for _, t := range targets {
		held, err := s.lvolHeldBytes(ctx, t.vol.UUID)
		if err != nil {
			writeError(w, err)
			return
		}
		id := newResourceID("snap")
		release, err := s.reserveSpace(ctx, t.pool.ID, held, snapshotRecorded(id))
		if err != nil {
			writeError(w, err)
			return
		}
		releases = append(releases, release)
		snaps = append(snaps, &store.Snapshot{
			ID:             id,
			Name:           req.Name + "-" + t.vol.ID,
			VolumeID:       t.vol.ID,
			PoolID:         t.pool.ID,
			Handle:         t.pool.ThinPoolName + "/" + id,
			SizeBytes:      t.vol.SizeBytes,
			AllocatedBytes: held,
		})
		entries = append(entries, dataplane.GroupEntry{Volume: t.vol.UUID, Snapshot: id})
	}

	// The data plane pauses I/O to every member (hosts' writes wait, nothing fails), snapshots
	// them all at that instant and resumes I/O, so the snapshots are consistent with each other
	// whether or not hosts are writing (docs/review-history.md P1-15, SPDK patch 0003).
	uuids, err := s.dp.SnapshotGroup(ctx, entries, groupSnapshotQuiesceTimeout)
	if err != nil {
		// It may stop at the first failed snapshot and leave earlier ones; remove them.
		for _, sn := range snaps {
			_ = s.dp.DeleteVolume(cleanup, sn.Handle)
		}
		writeError(w, groupSnapshotError(err))
		return
	}
	for i, sn := range snaps {
		sn.UUID = uuids[i]
	}
	rollback := func(err error) {
		for _, sn := range snaps {
			_ = s.store.DeleteSnapshot(sn.ID)
			_ = s.dp.DeleteVolume(cleanup, sn.UUID)
		}
		writeError(w, err)
	}

	snapIDs := make([]string, 0, len(snaps))
	for _, sn := range snaps {
		if err := s.store.CreateSnapshot(sn); err != nil {
			rollback(err)
			return
		}
		snapIDs = append(snapIDs, sn.ID)
	}
	vgs := &store.VolumeGroupSnapshot{
		ID:          newResourceID("vgs"),
		Name:        req.Name,
		VolumeIDs:   volIDs,
		SnapshotIDs: snapIDs,
		ReadyToUse:  true,
	}
	if err := s.store.CreateVolumeGroupSnapshot(vgs); err != nil {
		rollback(err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"volume_group_snapshot": vgs,
		"snapshots":             snaps,
	})
}

func (s *Server) handleDeleteVolumeGroupSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	vgs, err := s.store.GetVolumeGroupSnapshot(id)
	if err != nil {
		writeError(w, err)
		return
	}
	for _, snapID := range vgs.SnapshotIDs {
		snap, err := s.store.GetSnapshot(snapID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err == nil {
			err = s.deleteSnapshot(r.Context(), snap)
		}
		if err != nil {
			writeError(w, fmt.Errorf("delete snapshot %s: %w", snapID, err))
			return
		}
	}
	if err := s.store.DeleteVolumeGroupSnapshot(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// --- Exports ---

func (s *Server) handleCreateExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name              string               `json:"name"`
		VolumeID          string               `json:"volume_id"`
		Protocol          store.ExportProtocol `json:"protocol"`
		SubsystemNQN      string               `json:"subsystem_nqn"`
		NamespaceID       uint32               `json:"namespace_id"`
		TargetIQN         string               `json:"target_iqn"`
		LUNID             uint32               `json:"lun_id"`
		PortalIDs         []string             `json:"portal_ids"`
		InitiatorGroupIDs []string             `json:"initiator_group_ids"`
		AllowAnyHost      bool                 `json:"allow_any_host"`
		NodeID            string               `json:"node_id"`
		Shared            bool                 `json:"shared"`
		NodeIDs           []string             `json:"node_ids"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Protocol == "" {
		req.Protocol = store.ExportProtocolNVMeoFTCP
	}
	if !dataplane.IsNVMe(req.Protocol) && req.Protocol != store.ExportProtocolISCSI {
		writeError(w, fmt.Errorf("%w: unsupported protocol %q", store.ErrInvalidArgument, req.Protocol))
		return
	}
	// The export name is embedded in the default NQN / IQN.
	if err := dataplane.ValidateObjectName(req.Name); err != nil {
		writeError(w, fmt.Errorf("%w: export %v", store.ErrInvalidArgument, err))
		return
	}

	if req.Shared && req.NodeID != "" {
		writeError(w, fmt.Errorf("%w: a shared export lists its nodes in node_ids, not node_id", store.ErrInvalidArgument))
		return
	}
	if !req.Shared && len(req.NodeIDs) > 0 {
		writeError(w, fmt.Errorf("%w: node_ids is only for shared exports", store.ErrInvalidArgument))
		return
	}
	// Idempotent by Name for Kubernetes CSI ControllerPublishVolume retries.
	if existing, err := s.store.GetExportByName(req.Name); err == nil {
		if existing.VolumeID == req.VolumeID && existing.NodeID == req.NodeID && existing.Shared == req.Shared {
			writeJSON(w, http.StatusOK, map[string]any{"export": existing})
			return
		}
		writeError(w, fmt.Errorf("%w: export %s already exists for volume %s", store.ErrAlreadyExists, req.Name, existing.VolumeID))
		return
	}

	vol, err := s.liveVolume(req.VolumeID)
	if err != nil {
		writeError(w, err)
		return
	}
	if len(req.PortalIDs) == 0 {
		writeError(w, fmt.Errorf("%w: at least one portal is required", store.ErrInvalidArgument))
		return
	}
	for _, pid := range req.PortalIDs {
		p, err := s.store.GetPortal(pid)
		if err != nil {
			writeError(w, fmt.Errorf("portal %s: %w", pid, err))
			return
		}
		if p.Protocol != req.Protocol {
			writeError(w, fmt.Errorf("%w: portal %s is %s, export is %s", store.ErrInvalidArgument, pid, p.Protocol, req.Protocol))
			return
		}
	}

	if dataplane.IsNVMe(req.Protocol) {
		if req.SubsystemNQN == "" {
			req.SubsystemNQN = "nqn.2026-11.io.velocor:" + req.Name
		}
		if err := dataplane.ValidateNQN(req.SubsystemNQN); err != nil {
			writeError(w, fmt.Errorf("%w: %v", store.ErrInvalidArgument, err))
			return
		}
		req.TargetIQN = ""
	}
	if req.NamespaceID == 0 {
		req.NamespaceID = 1
	}
	if req.Protocol == store.ExportProtocolISCSI {
		if req.TargetIQN == "" {
			req.TargetIQN = "iqn.2026-11.io.velocor:" + strings.ToLower(req.Name)
		}
		if err := dataplane.ValidateIQN(req.TargetIQN); err != nil {
			writeError(w, fmt.Errorf("%w: %v", store.ErrInvalidArgument, err))
			return
		}
		req.SubsystemNQN = ""
	}

	exp := &store.Export{
		ID:                newResourceID("exp"),
		Name:              req.Name,
		VolumeID:          req.VolumeID,
		Protocol:          req.Protocol,
		SubsystemNQN:      req.SubsystemNQN,
		NamespaceID:       req.NamespaceID,
		TargetIQN:         req.TargetIQN,
		LUNID:             req.LUNID,
		PortalIDs:         req.PortalIDs,
		InitiatorGroupIDs: req.InitiatorGroupIDs,
		AllowAnyHost:      req.AllowAnyHost,
		NodeID:            req.NodeID,
		Shared:            req.Shared,
		NodeIDs:           slices.Compact(slices.Sorted(slices.Values(req.NodeIDs))),
		HealthStatus:      store.HealthStatusOnline,
	}
	acl, err := dataplane.ResolveACL(s.store, exp.InitiatorGroupIDs)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := dataplane.CheckACL(exp, acl); err != nil {
		writeError(w, fmt.Errorf("%w: %v", store.ErrInvalidArgument, err))
		return
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	// The store allocates the iSCSI initiator-group tag and flips the volume to BLOCK_EXPORT.
	if err := s.store.CreateExport(exp); err != nil {
		writeError(w, err)
		return
	}
	if err := dataplane.EnsureExport(r.Context(), s.dp, s.store, exp, vol); err != nil {
		cleanup := context.WithoutCancel(r.Context())
		_ = dataplane.RemoveExport(cleanup, s.dp, exp)
		_ = s.store.DeleteExport(exp.ID)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"export": exp})
}

func (s *Server) handleDeleteExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	exp, err := s.store.GetExport(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	if err := dataplane.RemoveExport(r.Context(), s.dp, exp); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteExport(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// checkLevel asks the data plane whether it can build the level (nil without one: the caller
// refuses that first).
func (s *Server) checkLevel(l store.RaidLevel, disks int) error {
	if s.dp == nil {
		return nil
	}
	return s.dp.CheckLevel(l, disks)
}
