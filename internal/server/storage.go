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
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

var errDataPlaneUnavailable = errors.New("spdk data plane is not configured")

func (s *Server) requireSPDK() error {
	if s.spdk == nil {
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
	if err := s.requireSPDK(); err != nil {
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
	if d.WriteCachePolicy == store.WriteCachePolicyDisabled && s.hw != nil {
		if _, err := s.applyWriteCachePolicy(d); err != nil {
			return nil, err
		}
	}
	bdev, drv, err := dataplane.AttachDisk(ctx, s.spdk, s.hw, d)
	if err != nil {
		return nil, err
	}
	updated, err := s.store.ClaimDisk(d.ID, drv, bdev)
	if err != nil {
		attached := *d
		attached.SPDKBdevName = bdev
		_ = dataplane.DetachDisk(context.WithoutCancel(ctx), s.spdk, &attached)
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
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	if err := dataplane.DetachDisk(r.Context(), s.spdk, d); err != nil {
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
			writeError(w, fmt.Errorf("disk released from SPDK but rebinding %s to the kernel nvme driver failed: %w", d.PCIAddress, err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"disk": updated})
}

// --- Pools ---

func validRaidLevel(l store.RaidLevel, disks int) error {
	if l == store.RaidLevel5F {
		// SPDK's raid5f only accepts full-stripe writes, and the lvol blobstore issues smaller
		// ones, so a volume store can't even be created on it (verified against SPDK v26.09).
		return fmt.Errorf("%w: RAID5F is not supported: SPDK raid5f cannot hold a volume store", store.ErrInvalidArgument)
	}
	if l == store.RaidLevel10 {
		// SPDK v26.09's bdev_raid has raid0, raid1, raid5f and concat: no raid10. It has to be
		// built as raid0 over raid1 pairs (TODO.md L-4).
		return fmt.Errorf("%w: RAID10 is not supported yet: SPDK has no raid10 level (use RAID1)", store.ErrInvalidArgument)
	}
	minDisks := map[store.RaidLevel]int{store.RaidLevel0: 1, store.RaidLevel1: 2}
	n, ok := minDisks[l]
	if !ok {
		return fmt.Errorf("%w: unsupported raid_level %q", store.ErrInvalidArgument, l)
	}
	if disks < n {
		return fmt.Errorf("%w: %s requires at least %d disks", store.ErrInvalidArgument, l, n)
	}
	return nil
}

func (s *Server) handleCreatePool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name             string             `json:"name"`
		RaidLevel        store.RaidLevel    `json:"raid_level"`
		DiskIDs          []string           `json:"disk_ids"`
		StripSizeKB      uint32             `json:"strip_size_kb"`
		ClusterSizeBytes uint32             `json:"cluster_size_bytes"`
		FilterPolicy     store.FilterPolicy `json:"filter_policy"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.RaidLevel == "" {
		req.RaidLevel = store.RaidLevel0
	}
	if req.StripSizeKB == 0 {
		req.StripSizeKB = 64
	}
	if req.ClusterSizeBytes == 0 {
		req.ClusterSizeBytes = store.DefaultClusterSizeBytes
	}
	for _, err := range []error{
		validateDisplayName("pool", req.Name),
		validRaidLevel(req.RaidLevel, len(req.DiskIDs)),
		rejectFilters(req.FilterPolicy),
		s.requireSPDK(),
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
		if !d.Claimed || d.SPDKBdevName == "" {
			writeError(w, fmt.Errorf("%w: disk %s is not claimed by SPDK", store.ErrInvalidArgument, diskID))
			return
		}
		totalRaw += d.SizeBytes
		baseBdevs = append(baseBdevs, d.SPDKBdevName)
	}

	id := newResourceID("pool")
	pool := &store.Pool{
		ID:                id,
		Name:              req.Name,
		RaidLevel:         req.RaidLevel,
		DiskIDs:           req.DiskIDs,
		StripSizeKB:       req.StripSizeKB,
		ClusterSizeBytes:  req.ClusterSizeBytes,
		BaseBdevName:      "raid_" + id,
		EffectiveBdevName: "raid_" + id,
		LVSName:           "lvs_" + id,
		TotalBytes:        totalRaw,
		FreeBytes:         totalRaw,
		FilterPolicy:      req.FilterPolicy,
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
			// The lvstore may exist even though creating it "failed" (e.g. a timeout while
			// spdk_tgt went on and finished), and a RAID can't be deleted from under it: the
			// delete would hang half-done. Remove the lvstore first.
			if derr := s.spdk.BdevLvolDeleteLvstore(cleanup, "", pool.LVSName); derr != nil && !spdk.IsNotFound(derr) {
				err = fmt.Errorf("%w (rollback: delete lvstore %s: %v; RAID %s left in place)", err, pool.LVSName, derr, pool.BaseBdevName)
				_ = s.store.DeletePool(pool.ID)
				writeError(w, err)
				return
			}
			_ = s.spdk.BdevRaidDelete(cleanup, pool.BaseBdevName, true)
		}
		_ = s.store.DeletePool(pool.ID)
		writeError(w, err)
	}
	if err := s.spdk.BdevRaidCreate(ctx, pool.BaseBdevName, string(pool.RaidLevel), pool.StripSizeKB, baseBdevs, true); err != nil {
		fail(false, fmt.Errorf("create raid bdev: %w", err))
		return
	}
	lvsUUID, err := s.spdk.BdevLvolCreateLvstore(ctx, pool.EffectiveBdevName, pool.LVSName, pool.ClusterSizeBytes, "")
	if err != nil {
		fail(true, fmt.Errorf("create lvstore: %w", err))
		return
	}
	updated, err := s.store.MutatePool(pool.ID, func(p *store.Pool) error {
		p.LVSUUID = lvsUUID
		p.HealthStatus, p.StatusMessage = store.HealthStatusOnline, ""
		if stores, err := s.spdk.BdevLvolGetLvstores(ctx, "", lvsUUID); err == nil && len(stores) > 0 {
			cs := uint64(stores[0].ClusterSize)
			p.TotalBytes, p.FreeBytes = stores[0].TotalDataClusters*cs, stores[0].FreeClusters*cs
			p.AllocatedBytes = p.TotalBytes - p.FreeBytes
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
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
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	// SPDK treats uuid and lvs_name as mutually exclusive.
	lvsUUID, lvsName := pool.LVSUUID, ""
	if lvsUUID == "" {
		lvsName = pool.LVSName
	}
	if err := s.spdk.BdevLvolDeleteLvstore(r.Context(), lvsUUID, lvsName); err != nil && !spdk.IsNotFound(err) {
		writeError(w, fmt.Errorf("delete lvstore: %w", err))
		return
	}
	if err := s.spdk.BdevRaidDelete(r.Context(), pool.BaseBdevName, true); err != nil && !spdk.IsNotFound(err) {
		writeError(w, fmt.Errorf("delete raid bdev: %w", err))
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
	case pool.LVSUUID == "",
		pool.HealthStatus == store.HealthStatusFaulted,
		pool.HealthStatus == store.HealthStatusReconciling,
		pool.HealthStatus == store.HealthStatusUnavailable: // RAID waiting for a missing disk
		return fmt.Errorf("%w: pool %s is not usable (%s %s)", store.ErrInvalidArgument, pool.ID, pool.HealthStatus, pool.StatusMessage)
	}
	return nil
}

func qosSet(q store.QosPolicy) bool {
	return q != (store.QosPolicy{})
}

func (s *Server) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string             `json:"name"`
		PoolID        string             `json:"pool_id"`
		SizeBytes     uint64             `json:"size_bytes"`
		ThinProvision bool               `json:"thin_provision"`
		FilterPolicy  store.FilterPolicy `json:"filter_policy"`
		QosPolicy     store.QosPolicy    `json:"qos_policy"`
		Labels        map[string]string  `json:"labels"`
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
	if err := rejectFilters(req.FilterPolicy); err != nil {
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
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}

	id := newResourceID("vol")
	alias := pool.LVSName + "/" + id
	uuid, err := s.spdk.BdevLvolCreate(r.Context(), pool.LVSName, id, req.SizeBytes, req.ThinProvision, "")
	if err != nil {
		writeError(w, fmt.Errorf("create lvol: %w", err))
		return
	}
	vol := &store.Volume{
		ID:                id,
		Name:              req.Name,
		PoolID:            req.PoolID,
		UUID:              uuid,
		LvolBdevName:      alias,
		EffectiveBdevName: alias,
		SizeBytes:         req.SizeBytes,
		ThinProvision:     req.ThinProvision,
		FilterPolicy:      req.FilterPolicy,
		QosPolicy:         req.QosPolicy,
		AttachmentState:   store.AttachmentStateUnattached,
		HealthStatus:      store.HealthStatusOnline,
		Labels:            req.Labels,
	}
	s.finishVolumeCreate(w, r, vol, http.StatusCreated)
}

// finishVolumeCreate applies QoS and persists a volume whose lvol already exists in SPDK,
// deleting the lvol again if either step fails.
func (s *Server) finishVolumeCreate(w http.ResponseWriter, r *http.Request, vol *store.Volume, status int) {
	rollback := func(err error) {
		_ = s.spdk.BdevLvolDelete(context.WithoutCancel(r.Context()), vol.UUID)
		writeError(w, err)
	}
	if qosSet(vol.QosPolicy) {
		q := vol.QosPolicy
		if err := s.spdk.BdevSetQoSLimit(r.Context(), vol.EffectiveBdevName, q.RwIosPerSec, q.RwMbytesPerSec, q.RMbytesPerSec, q.WMbytesPerSec); err != nil {
			rollback(fmt.Errorf("apply QoS: %w", err))
			return
		}
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
	vol, err := s.store.GetVolume(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if req.NewSizeBytes < vol.SizeBytes {
		writeError(w, fmt.Errorf("%w: new_size_bytes (%d) must be >= current size (%d)", store.ErrInvalidArgument, req.NewSizeBytes, vol.SizeBytes))
		return
	}
	if req.NewSizeBytes == vol.SizeBytes {
		writeJSON(w, http.StatusOK, map[string]any{"volume": vol})
		return
	}
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	if err := s.spdk.BdevLvolResize(r.Context(), vol.UUID, req.NewSizeBytes); err != nil {
		writeError(w, fmt.Errorf("resize lvol: %w", err))
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

func (s *Server) handleSetVolumeQoS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		QosPolicy store.QosPolicy `json:"qos_policy"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	vol, err := s.store.GetVolume(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	q := req.QosPolicy
	if err := s.spdk.BdevSetQoSLimit(r.Context(), vol.EffectiveBdevName, q.RwIosPerSec, q.RwMbytesPerSec, q.RMbytesPerSec, q.WMbytesPerSec); err != nil {
		writeError(w, fmt.Errorf("apply QoS: %w", err))
		return
	}
	updated, err := s.store.MutateVolume(id, func(v *store.Volume) error {
		v.QosPolicy = q
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volume": updated})
}

func (s *Server) handleDeleteVolume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	vol, err := s.store.GetVolume(id)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.CheckVolumeDeletable(id); err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	if err := s.spdk.BdevLvolDelete(r.Context(), vol.UUID); err != nil && !spdk.IsNotFound(err) {
		writeError(w, fmt.Errorf("delete lvol: %w", err))
		return
	}
	if err := s.store.DeleteVolume(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
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

	vol, err := s.store.GetVolume(volID)
	if err != nil {
		writeError(w, err)
		return
	}
	pool, err := s.store.GetPool(vol.PoolID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	snap, err := s.takeSnapshot(r.Context(), vol, pool, req.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.CreateSnapshot(snap); err != nil {
		_ = s.spdk.BdevLvolDelete(context.WithoutCancel(r.Context()), snap.UUID)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"snapshot": snap})
}

// takeSnapshot creates the SPDK snapshot lvol (named after the new snapshot ID) and
// returns an unsaved Snapshot record for it.
func (s *Server) takeSnapshot(ctx context.Context, vol *store.Volume, pool *store.Pool, name string) (*store.Snapshot, error) {
	id := newResourceID("snap")
	uuid, err := s.spdk.BdevLvolSnapshot(ctx, vol.UUID, id)
	if err != nil {
		return nil, fmt.Errorf("snapshot volume %s: %w", vol.ID, err)
	}
	return &store.Snapshot{
		ID:           id,
		Name:         name,
		VolumeID:     vol.ID,
		PoolID:       pool.ID,
		UUID:         uuid,
		LvolBdevName: pool.LVSName + "/" + id,
		SizeBytes:    vol.SizeBytes,
	}, nil
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
		QosPolicy    store.QosPolicy    `json:"qos_policy"`
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
	if err := rejectFilters(req.FilterPolicy); err != nil {
		writeError(w, err)
		return
	}
	snap, err := s.store.GetSnapshot(snapID)
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
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}

	id := newResourceID("vol")
	alias := pool.LVSName + "/" + id
	uuid, err := s.spdk.BdevLvolClone(r.Context(), snap.UUID, id)
	if err != nil {
		writeError(w, fmt.Errorf("clone snapshot %s: %w", snap.ID, err))
		return
	}
	clone := &store.Volume{
		ID:                id,
		Name:              req.CloneName,
		PoolID:            pool.ID,
		UUID:              uuid,
		LvolBdevName:      alias,
		EffectiveBdevName: alias,
		SizeBytes:         snap.SizeBytes,
		ThinProvision:     true,
		ParentSnapshotID:  snap.ID,
		FilterPolicy:      req.FilterPolicy,
		QosPolicy:         req.QosPolicy,
		AttachmentState:   store.AttachmentStateUnattached,
		HealthStatus:      store.HealthStatusOnline,
		Labels:            req.Labels,
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
	snap, err := s.store.GetSnapshot(tail)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snap})
}

func (s *Server) handleSnapshotBlocks(w http.ResponseWriter, r *http.Request, targetID string, changed bool) {
	target, err := s.store.GetSnapshot(targetID)
	if err != nil {
		writeError(w, err)
		return
	}
	stopAt := ""
	var base *store.Snapshot
	if changed {
		baseID := r.URL.Query().Get("base_snapshot_id")
		if baseID == "" {
			writeError(w, fmt.Errorf("%w: base_snapshot_id query parameter is required", store.ErrInvalidArgument))
			return
		}
		if base, err = s.store.GetSnapshot(baseID); err != nil {
			writeError(w, err)
			return
		}
		if base.VolumeID != target.VolumeID {
			writeError(w, fmt.Errorf("%w: snapshots %s and %s belong to different volumes", store.ErrInvalidArgument, baseID, targetID))
			return
		}
		stopAt = base.ID
	}
	pool, err := s.store.GetPool(target.PoolID)
	if err != nil {
		writeError(w, err)
		return
	}
	lineage, err := s.collectSnapshotLineage(target, stopAt)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	var extentSets [][]spdk.BlockExtent
	for _, layer := range lineage {
		frag, err := s.spdk.BdevLvolGetFragmap(r.Context(), layer.UUID, 0, layer.SizeBytes)
		if err != nil {
			writeError(w, err)
			return
		}
		ext, err := spdk.DecodeFragmapExtents(frag)
		if err != nil {
			writeError(w, err)
			return
		}
		extentSets = append(extentSets, ext)
	}
	resp := map[string]any{
		"snapshot_id":        target.ID,
		"volume_size_bytes":  target.SizeBytes,
		"cluster_size_bytes": pool.ClusterSizeBytes,
		"extents":            formatBlockExtents(spdk.MergeBlockExtents(extentSets...)),
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
	if err := s.store.CheckSnapshotDeletable(id); err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	if err := s.spdk.BdevLvolDelete(r.Context(), snap.UUID); err != nil && !spdk.IsNotFound(err) {
		writeError(w, fmt.Errorf("delete snapshot lvol: %w", err))
		return
	}
	if err := s.store.DeleteSnapshot(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// --- Volume group snapshots ---

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
	var nqns []string
	for _, volID := range volIDs {
		vol, err := s.store.GetVolume(volID)
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
		exps, err := s.store.ListExports(volID, "")
		if err != nil {
			writeError(w, err)
			return
		}
		for _, e := range exps {
			if dataplane.IsNVMe(e.Protocol) && !containsString(nqns, e.SubsystemNQN) {
				nqns = append(nqns, e.SubsystemNQN)
			}
		}
	}
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}

	ctx := r.Context()
	cleanup := context.WithoutCancel(ctx)

	// Quiesce NVMe-oF I/O on every member so all snapshots capture the same instant.
	// (iSCSI exports have no pause RPC; for those the group is best-effort ordered.)
	var paused []string
	resume := func() {
		for _, nqn := range paused {
			s.resumeSubsystem(cleanup, nqn)
		}
		paused = nil
	}
	defer resume()
	for _, nqn := range nqns {
		if err := s.spdk.NvmfSubsystemPause(ctx, nqn); err != nil {
			writeError(w, fmt.Errorf("pause %s for consistent group snapshot: %w", nqn, err))
			return
		}
		paused = append(paused, nqn)
	}

	var snaps []*store.Snapshot
	rollback := func(err error) {
		resume()
		for _, sn := range snaps {
			_ = s.store.DeleteSnapshot(sn.ID)
			_ = s.spdk.BdevLvolDelete(cleanup, sn.UUID)
		}
		writeError(w, err)
	}
	for _, t := range targets {
		snap, err := s.takeSnapshot(ctx, t.vol, t.pool, req.Name+"-"+t.vol.ID)
		if err != nil {
			rollback(err)
			return
		}
		snaps = append(snaps, snap)
	}
	resume()

	snapIDs := make([]string, 0, len(snaps))
	for i, sn := range snaps {
		if err := s.store.CreateSnapshot(sn); err != nil {
			// Only roll back store records that were actually written.
			for _, done := range snaps[:i] {
				_ = s.store.DeleteSnapshot(done.ID)
			}
			for _, x := range snaps {
				_ = s.spdk.BdevLvolDelete(cleanup, x.UUID)
			}
			writeError(w, err)
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

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Server) handleDeleteVolumeGroupSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	vgs, err := s.store.GetVolumeGroupSnapshot(id)
	if err != nil {
		writeError(w, err)
		return
	}
	var snaps []*store.Snapshot
	for _, snapID := range vgs.SnapshotIDs {
		snap, err := s.store.GetSnapshot(snapID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			writeError(w, err)
			return
		}
		if err := s.store.CheckSnapshotDeletable(snapID); err != nil {
			writeError(w, err)
			return
		}
		snaps = append(snaps, snap)
	}
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	for _, snap := range snaps {
		if err := s.spdk.BdevLvolDelete(r.Context(), snap.UUID); err != nil && !spdk.IsNotFound(err) {
			writeError(w, fmt.Errorf("delete snapshot %s: %w", snap.ID, err))
			return
		}
		if err := s.store.DeleteSnapshot(snap.ID); err != nil {
			writeError(w, err)
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
	if err := spdk.ValidateObjectName(req.Name); err != nil {
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

	vol, err := s.store.GetVolume(req.VolumeID)
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

	if dataplane.IsNVMe(req.Protocol) && req.SubsystemNQN == "" {
		req.SubsystemNQN = "nqn.2026-11.io.velocor:" + req.Name
	}
	if req.NamespaceID == 0 {
		req.NamespaceID = 1
	}
	if req.Protocol == store.ExportProtocolISCSI && req.TargetIQN == "" {
		req.TargetIQN = "iqn.2026-11.io.velocor:" + strings.ToLower(req.Name)
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
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	// The store allocates the iSCSI initiator-group tag and flips the volume to BLOCK_EXPORT.
	if err := s.store.CreateExport(exp); err != nil {
		writeError(w, err)
		return
	}
	if err := dataplane.EnsureExport(r.Context(), s.spdk, s.store, exp, vol); err != nil {
		cleanup := context.WithoutCancel(r.Context())
		_ = dataplane.RemoveExport(cleanup, s.spdk, exp)
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
	if err := s.requireSPDK(); err != nil {
		writeError(w, err)
		return
	}
	if err := dataplane.RemoveExport(r.Context(), s.spdk, exp); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteExport(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// resumeSubsystem resumes I/O on a subsystem paused for a group snapshot. A paused subsystem
// holds every host's I/O, so a failed resume is retried (in the background, with backoff)
// rather than left for an operator to notice.
func (s *Server) resumeSubsystem(ctx context.Context, nqn string) {
	if err := s.spdk.NvmfSubsystemResume(ctx, nqn); err == nil || spdk.IsNotFound(err) {
		return
	} else {
		s.logger.Error("resume nvmf subsystem after group snapshot failed; retrying", "nqn", nqn, "error", err)
	}
	go func() {
		delay := 200 * time.Millisecond
		for attempt := 1; attempt <= 60; attempt++ {
			time.Sleep(delay)
			err := s.spdk.NvmfSubsystemResume(ctx, nqn)
			if err == nil || spdk.IsNotFound(err) {
				s.logger.Info("resumed nvmf subsystem", "nqn", nqn, "attempt", attempt)
				return
			}
			delay = min(delay*2, 5*time.Second)
		}
		s.logger.Error("nvmf subsystem is still paused after retries; hosts' I/O is blocked", "nqn", nqn)
	}()
}
