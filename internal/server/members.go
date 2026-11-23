package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/store"
)

// Disk replacement, rebuild and degraded start (TODO P0-1 / P1-8;
// docs/design/p0-1-disk-replacement.md).

// maxRebuildMBps bounds the rebuild speed limit to something a typo can't exceed.
const maxRebuildMBps = 100_000

func redundant(level store.RaidLevel) bool {
	return strings.EqualFold(string(level), string(store.RaidLevel1)) || strings.EqualFold(string(level), string(store.RaidLevel10))
}

// poolRaid returns the data plane's view of the pool's array and of all arrays.
func (s *Server) poolRaid(ctx context.Context, pool *store.Pool) (*dataplane.Array, []dataplane.Array, error) {
	raids, err := s.dp.Arrays(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list raid bdevs: %w", err)
	}
	i := slices.IndexFunc(raids, func(rb dataplane.Array) bool { return rb.Name == pool.BaseBdevName })
	if i < 0 {
		return nil, raids, fmt.Errorf("%w: RAID %s of pool %s is not assembled", store.ErrInvalidArgument, pool.BaseBdevName, pool.ID)
	}
	return &raids[i], raids, nil
}

func memberOf(raids []dataplane.Array, bdev string) string {
	for _, rb := range raids {
		for _, b := range rb.Members {
			if bdev != "" && b.Name == bdev {
				return rb.Name
			}
		}
	}
	return ""
}

func (s *Server) reconcileNow(ctx context.Context) {
	if s.reconciler != nil {
		_, _ = s.reconciler.ReconcileOnce(ctx)
	}
}

// handleReplacePoolDisk puts a new disk into a pool's RAID and starts rebuilding it:
//
//	POST /api/v1/pools/{id}/replace {new_disk_id, old_disk_id?, allow_degraded?}
//
// Without old_disk_id the first failed or missing slot is filled. Naming a disk that still
// works (proactive replacement) removes it first, which leaves the pool without redundancy
// until the rebuild completes: allow_degraded must say so. An unclaimed new disk has any old
// metadata wiped (so SPDK can't assemble it into an old array) and is claimed.
func (s *Server) handleReplacePoolDisk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NewDiskID     string `json:"new_disk_id"`
		OldDiskID     string `json:"old_disk_id"`
		AllowDegraded bool   `json:"allow_degraded"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NewDiskID == "" {
		writeError(w, fmt.Errorf("%w: body must be JSON with new_disk_id", store.ErrInvalidArgument))
		return
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	ctx := r.Context()
	pool, err := s.store.GetPool(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if !redundant(pool.RaidLevel) {
		writeError(w, fmt.Errorf("%w: pool %s is %s, which has no redundancy to rebuild a disk from", store.ErrInvalidArgument, pool.ID, pool.RaidLevel))
		return
	}
	rb, raids, err := s.poolRaid(ctx, pool)
	if err != nil {
		writeError(w, err)
		return
	}
	switch {
	case !strings.EqualFold(rb.State, "online"):
		writeError(w, fmt.Errorf("%w: RAID %s is %s; a disk can only be replaced in a running pool (start it degraded first if it is waiting for a missing disk)",
			store.ErrInvalidArgument, rb.Name, rb.State))
		return
	case rb.Rebuild != nil:
		writeError(w, fmt.Errorf("%w: RAID %s is already rebuilding %s (%d%%)", store.ErrInUse, rb.Name, rb.Rebuild.Target, rb.Rebuild.Percent))
		return
	}

	members := store.PoolMembers(pool)
	if len(members) != len(rb.Members) {
		writeError(w, fmt.Errorf("%w: pool %s records %d slots but RAID %s has %d; wait for the reconciler", store.ErrInvalidArgument, pool.ID, len(members), rb.Name, len(rb.Members)))
		return
	}
	var slot int
	removeBdev := ""
	if req.OldDiskID != "" {
		slot = slices.IndexFunc(members, func(m store.PoolMember) bool { return m.DiskID == req.OldDiskID })
		if slot < 0 {
			writeError(w, fmt.Errorf("%w: disk %s is not in a slot of pool %s", store.ErrNotFound, req.OldDiskID, pool.ID))
			return
		}
		if b := rb.Members[slot]; b.Name != "" && b.Configured {
			if !req.AllowDegraded {
				writeError(w, fmt.Errorf("%w: disk %s is working; replacing it removes the pool's redundancy until the rebuild completes (pass allow_degraded=true)", store.ErrInvalidArgument, req.OldDiskID))
				return
			}
			if rb.Operational < 2 {
				writeError(w, fmt.Errorf("%w: disk %s is the pool's only working member", store.ErrInvalidArgument, req.OldDiskID))
				return
			}
			removeBdev = b.Name
		}
	} else {
		slot = slices.IndexFunc(rb.Members, func(b dataplane.ArrayMember) bool { return b.Name == "" })
		if slot < 0 {
			writeError(w, fmt.Errorf("%w: pool %s has no failed or missing disk; name old_disk_id to replace a working one", store.ErrInvalidArgument, pool.ID))
			return
		}
	}

	newDisk, err := s.store.GetDisk(req.NewDiskID)
	if err != nil {
		writeError(w, err)
		return
	}
	if newDisk.PoolID == pool.ID {
		// The slot's own disk came back (it was missing, or failed and was left out): put it
		// back and rebuild onto it. The array never takes a returning member back by itself.
		own := slices.IndexFunc(members, func(m store.PoolMember) bool { return m.DiskID == newDisk.ID })
		switch {
		case own < 0:
			writeError(w, fmt.Errorf("%w: disk %s belongs to pool %s but holds none of its slots; release it first", store.ErrInUse, newDisk.ID, pool.ID))
		case req.OldDiskID != "" && req.OldDiskID != newDisk.ID:
			writeError(w, fmt.Errorf("%w: disk %s holds slot %d of pool %s, not %s's", store.ErrInvalidArgument, newDisk.ID, own, pool.ID, req.OldDiskID))
		case rb.Members[own].Name != "":
			writeError(w, fmt.Errorf("%w: disk %s is already in RAID %s", store.ErrInvalidArgument, newDisk.ID, rb.Name))
		default:
			s.putBack(w, r, pool, rb, raids, newDisk)
		}
		return
	}
	switch {
	case newDisk.IsBootDevice:
		writeError(w, store.ErrBootDeviceProtected)
		return
	case newDisk.PoolID != "":
		writeError(w, fmt.Errorf("%w: disk %s belongs to pool %s", store.ErrInUse, newDisk.ID, newDisk.PoolID))
		return
	}
	for _, m := range members {
		if d, err := s.store.GetDisk(m.DiskID); err == nil && m.State == store.MemberStateActive && newDisk.SizeBytes > 0 && newDisk.SizeBytes < d.SizeBytes {
			writeError(w, fmt.Errorf("%w: disk %s (%d bytes) is smaller than member %s (%d bytes)", store.ErrInvalidArgument, newDisk.ID, newDisk.SizeBytes, d.ID, d.SizeBytes))
			return
		}
	}
	if !newDisk.Claimed {
		if s.hw != nil {
			if err := s.hw.WipeMetadata(newDisk); err != nil {
				writeError(w, fmt.Errorf("wipe disk %s before use: %w", newDisk.ID, err))
				return
			}
		}
		if newDisk, err = s.attachAndClaim(ctx, newDisk, false); err != nil {
			writeError(w, err)
			return
		}
		if raids, err = s.dp.Arrays(ctx); err != nil {
			writeError(w, err)
			return
		}
	}
	if other := memberOf(raids, newDisk.SPDKBdevName); other != "" {
		writeError(w, fmt.Errorf("%w: disk %s carries metadata of RAID %s and the data plane assembled it into that array; release it with a wipe first", store.ErrInUse, newDisk.ID, other))
		return
	}

	if removeBdev != "" {
		if err := s.dp.RemoveMember(ctx, removeBdev); err != nil {
			writeError(w, fmt.Errorf("remove %s from RAID %s: %w", req.OldDiskID, rb.Name, err))
			return
		}
	}
	prev := members[slot]
	updated, err := s.store.ReplacePoolMember(pool.ID, slot, newDisk.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.dp.AddMember(ctx, rb.Name, newDisk.SPDKBdevName); err != nil {
		s.undoReplace(pool.ID, prev, newDisk.ID)
		writeError(w, fmt.Errorf("add disk %s to RAID %s: %w", newDisk.ID, rb.Name, err))
		return
	}
	s.reconcileNow(ctx)
	if p, err := s.store.GetPool(pool.ID); err == nil {
		updated = p
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pool": updated})
}

// putBack adds a member's own disk back to its array, which rebuilds onto it: attached to the
// data plane first if it isn't (the reconciler leaves a disk that left a running array alone).
func (s *Server) putBack(w http.ResponseWriter, r *http.Request, pool *store.Pool, rb *dataplane.Array, raids []dataplane.Array, d *store.Disk) {
	ctx := r.Context()
	if other := memberOf(raids, d.SPDKBdevName); other != "" {
		writeError(w, fmt.Errorf("%w: disk %s is in RAID %s", store.ErrInUse, d.ID, other))
		return
	}
	devs, err := s.dp.Devices(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	present := d.SPDKBdevName != "" && slices.ContainsFunc(devs, func(x dataplane.Device) bool {
		return x.Name == d.SPDKBdevName || slices.Contains(x.Aliases, d.SPDKBdevName)
	})
	if !present {
		h, drv, err := s.dp.AttachDisk(ctx, d)
		if err != nil {
			writeError(w, fmt.Errorf("attach disk %s: %w", d.ID, err))
			return
		}
		if d, err = s.store.MutateDisk(d.ID, func(cur *store.Disk) error {
			cur.SPDKBdevName, cur.DriverState = h, drv
			return nil
		}); err != nil {
			writeError(w, err)
			return
		}
	}
	// SPDK's examine may already have put an attached member back into its slot.
	if now, err := s.dp.Arrays(ctx); err != nil || memberOf(now, d.SPDKBdevName) != rb.Name {
		if err := s.dp.AddMember(ctx, rb.Name, d.SPDKBdevName); err != nil {
			writeError(w, fmt.Errorf("add disk %s back to RAID %s: %w", d.ID, rb.Name, err))
			return
		}
	}
	s.logger.Warn("pool member put back; rebuilding onto it", "pool", pool.ID, "disk", d.ID)
	s.reconcileNow(ctx)
	if p, err := s.store.GetPool(pool.ID); err == nil {
		pool = p
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pool": pool})
}

// undoReplace restores a slot after SPDK refused the new disk; the disk stays claimed.
func (s *Server) undoReplace(poolID string, prev store.PoolMember, newDiskID string) {
	_, _ = s.store.MutatePool(poolID, func(p *store.Pool) error {
		m := store.PoolMembers(p)
		if prev.Slot < len(m) {
			m[prev.Slot] = prev
		}
		p.DetachedDiskIDs = slices.DeleteFunc(p.DetachedDiskIDs, func(id string) bool { return id == prev.DiskID })
		store.SetPoolMembers(p, m)
		return nil
	})
	_, _ = s.store.MutateDisk(newDiskID, func(d *store.Disk) error { d.PoolID = ""; return nil })
}

// handleStartPoolDegraded starts a pool whose RAID waits for a missing disk, without it:
//
//	POST /api/v1/pools/{id}/start-degraded {confirm: "<pool name>"}
//
// Never automatic: a disk that is only slow to appear would be dropped from the array.
func (s *Server) handleStartPoolDegraded(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm string `json:"confirm"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	ctx := r.Context()
	pool, err := s.store.GetPool(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if req.Confirm != pool.Name {
		writeError(w, fmt.Errorf("%w: confirm must be the pool name (%q): the missing disk is dropped from the array; if it returns, Velocor leaves it out until you release or reuse it", store.ErrInvalidArgument, pool.Name))
		return
	}
	rb, _, err := s.poolRaid(ctx, pool)
	if err != nil {
		writeError(w, err)
		return
	}
	if !strings.EqualFold(rb.State, "configuring") {
		writeError(w, fmt.Errorf("%w: RAID %s is %s, not waiting for a missing disk", store.ErrInvalidArgument, rb.Name, rb.State))
		return
	}
	if err := s.dp.StartDegraded(ctx, rb.Name); err != nil {
		writeError(w, fmt.Errorf("start RAID %s degraded: %w", rb.Name, err))
		return
	}
	s.logger.Warn("pool started degraded by operator", "pool", pool.ID, "raid", rb.Name)
	s.reconcileNow(ctx)
	if p, err := s.store.GetPool(pool.ID); err == nil {
		pool = p
	}
	writeJSON(w, http.StatusOK, map[string]any{"pool": pool})
}

// handleReleasePoolDisk returns a disk that left a pool's RAID (failed, removed or
// replaced) to the unclaimed list, wiping its metadata so it can't be mistaken for a member:
//
//	DELETE /api/v1/pools/{id}/disks/{disk_id}
func (s *Server) handleReleasePoolDisk(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	ctx := r.Context()
	poolID, diskID := r.PathValue("id"), r.PathValue("disk_id")
	d, err := s.store.GetDisk(diskID)
	if err != nil {
		writeError(w, err)
		return
	}
	if d.Claimed {
		raids, err := s.dp.Arrays(ctx)
		if err != nil {
			writeError(w, err)
			return
		}
		if rb := memberOf(raids, d.SPDKBdevName); rb != "" {
			writeError(w, fmt.Errorf("%w: disk %s is still a member of RAID %s", store.ErrInUse, diskID, rb))
			return
		}
	}
	if _, err := s.store.ReleasePoolDisk(poolID, diskID); err != nil {
		writeError(w, err)
		return
	}
	if d.Claimed {
		if err := s.dp.DetachDisk(ctx, d); err != nil {
			writeError(w, err)
			return
		}
		if _, err := s.store.ReleaseDisk(diskID); err != nil {
			writeError(w, err)
			return
		}
		if d.Type == store.DiskTypeNVMe && s.hw != nil && d.PCIAddress != "" {
			if err := s.hw.BindNVMeToKernel(d.PCIAddress); err != nil {
				writeError(w, fmt.Errorf("disk %s released but rebinding %s to the kernel failed: %w", diskID, d.PCIAddress, err))
				return
			}
		}
	}
	wiped := false
	if s.hw != nil && d.DevicePath != "" {
		if err := s.hw.WipeMetadata(d); err != nil {
			s.logger.Warn("released disk not wiped", "disk", diskID, "error", err)
		} else {
			wiped = true
		}
	}
	out, err := s.store.GetDisk(diskID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disk": out, "wiped": wiped})
}

// handleGetRebuildSettings / handlePutRebuildSettings: GET|PUT /api/v1/system/rebuild
// {max_mb_per_sec} (0 = unlimited). SPDK applies the limit to every RAID, but only to rebuilds
// that start after it changes (a running rebuild keeps its rate); the reconciler re-applies it
// after spdk_tgt restarts.
func (s *Server) handleGetRebuildSettings(w http.ResponseWriter, _ *http.Request) {
	st, err := s.store.GetSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"max_mb_per_sec": st.RebuildMaxMBps})
}

func (s *Server) handlePutRebuildSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MaxMBps *uint32 `json:"max_mb_per_sec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MaxMBps == nil || *req.MaxMBps > maxRebuildMBps {
		writeError(w, fmt.Errorf("%w: body must be {\"max_mb_per_sec\": 0..%d} (0 = unlimited)", store.ErrInvalidArgument, maxRebuildMBps))
		return
	}
	st, err := s.store.GetSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	st.RebuildMaxMBps = *req.MaxMBps
	if err := s.store.SaveSettings(st); err != nil {
		writeError(w, err)
		return
	}
	if s.dp != nil {
		if err := s.dp.SetRebuildLimit(r.Context(), st.RebuildMaxMBps); err != nil {
			writeError(w, fmt.Errorf("saved, but applying it to the data plane failed (the reconciler retries): %w", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"max_mb_per_sec": st.RebuildMaxMBps})
}
