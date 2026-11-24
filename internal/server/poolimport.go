package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/meta"
	"github.com/velocor/velocor/internal/store"
)

// Pool import (TODO P0-3): bring back a pool whose disks carry a Velocor RAID but which this
// database doesn't know (lost or reinstalled boot disk, disks moved from another appliance).
//
//	GET  /api/v1/pool-imports            pools found on unclaimed disks
//	POST /api/v1/pool-imports {pool_id}  import one
//
// The configuration comes from the copy on the pool (internal/meta); without a readable copy
// the pool, its volumes and snapshots are rebuilt from what SPDK knows (the "skeleton").

// WithMetaDevice gives the server access to the on-pool metadata copies (pool import).
func WithMetaDevice(dev meta.Device) Option {
	return func(s *Server) { s.metaDev = dev }
}

// ImportCandidate is a pool found on unclaimed disks.
type ImportCandidate struct {
	PoolID     string         `json:"pool_id"`
	RaidName   string         `json:"raid_name"`
	RaidUUID   string         `json:"raid_uuid"`
	RaidLevel  string         `json:"raid_level"`
	Members    int            `json:"members"`
	Present    int            `json:"present"`
	Complete   bool           `json:"complete"`
	Disks      []ImportMember `json:"disks"`
	SeqNumber  uint64         `json:"sequence_number"`
	SizeBytes  uint64         `json:"size_bytes"`
	Importable bool           `json:"importable"`
	Reason     string         `json:"reason,omitempty"`
}

// ImportMember is one disk of a candidate.
type ImportMember struct {
	DiskID string `json:"disk_id"`
	Serial string `json:"serial_number,omitempty"`
	Slot   *int   `json:"slot,omitempty"` // known for io_uring/AIO disks
	// Stale: the disk's superblock is older than its array's (it left the array earlier).
	Stale bool `json:"stale,omitempty"`
}

// importCandidates groups the unclaimed disks carrying a Velocor array's metadata by array
// (Plane.ExamineDisks), with the arrays an earlier import attempt left claimed and assembled.
func (s *Server) importCandidates() ([]ImportCandidate, map[string][]dataplane.DiskMember, error) {
	disks, err := s.store.ListDisks()
	if err != nil {
		return nil, nil, err
	}
	ctx := context.Background()
	// An import that failed after claiming its disks leaves them claimed with the array
	// assembled (undoing that would degrade it): such arrays are candidates too.
	assembled := map[string]*dataplane.Array{} // by member handle
	if s.dp != nil {
		if raids, err := s.dp.Arrays(ctx); err == nil {
			for i := range raids {
				for _, b := range raids[i].Members {
					if b.Name != "" {
						assembled[b.Name] = &raids[i]
					}
				}
			}
		}
	}
	arrays := map[string]*dataplane.DiskArray{}
	var order []string
	add := func(a dataplane.DiskArray) {
		if !strings.HasPrefix(a.Name, "raid_pool-") {
			return
		}
		cur := arrays[a.UUID]
		if cur == nil {
			order = append(order, a.UUID)
			arrays[a.UUID] = &a
			return
		}
		// The disks' own metadata describes the array better than a running array's view.
		if a.Generation >= cur.Generation {
			a.Members = append(a.Members, cur.Members...)
			arrays[a.UUID] = &a
		} else {
			cur.Members = append(cur.Members, a.Members...)
		}
	}
	var unclaimed []*store.Disk
	for _, d := range disks {
		if d.IsBootDevice || d.PoolID != "" {
			continue
		}
		if !d.Claimed {
			unclaimed = append(unclaimed, d)
			continue
		}
		if r := assembled[d.Handle]; r != nil {
			slot := slices.IndexFunc(r.Members, func(m dataplane.ArrayMember) bool { return m.Name == d.Handle })
			current := 0
			for _, m := range r.Members {
				if m.Configured {
					current++
				}
			}
			add(dataplane.DiskArray{UUID: r.UUID, Name: r.Name, Level: store.RaidLevel(strings.ToUpper(r.Level)), Slots: int(r.Slots),
				Current: current, Members: []dataplane.DiskMember{{Disk: d, Slot: slot}}})
		}
	}
	if s.dp != nil && len(unclaimed) > 0 {
		examined, err := s.dp.ExamineDisks(ctx, unclaimed)
		if err != nil {
			return nil, nil, err
		}
		for _, a := range examined {
			add(a)
		}
	}

	out := []ImportCandidate{}
	members := map[string][]dataplane.DiskMember{}
	for _, uuid := range order {
		a := arrays[uuid]
		members[uuid] = a.Members
		c := ImportCandidate{
			PoolID:    strings.TrimPrefix(a.Name, "raid_"),
			RaidName:  a.Name,
			RaidUUID:  uuid,
			RaidLevel: string(a.Level),
			Members:   a.Slots,
			SeqNumber: a.Generation,
			SizeBytes: a.SizeBytes,
		}
		for _, m := range a.Members {
			im := ImportMember{DiskID: m.Disk.ID, Serial: m.Disk.SerialNumber, Stale: m.Stale}
			if m.Slot >= 0 {
				slot := m.Slot
				im.Slot = &slot
			}
			if !m.Stale {
				c.Present++
			}
			c.Disks = append(c.Disks, im)
		}
		slices.SortFunc(c.Disks, func(a, b ImportMember) int { return strings.Compare(a.DiskID, b.DiskID) })
		c.Complete = c.Present >= a.Current
		c.Importable = true
		switch {
		case s.dp == nil || s.dp.CheckLevel(a.Level, a.Slots) != nil:
			c.Importable, c.Reason = false, "unsupported RAID level "+c.RaidLevel
		case !c.Complete && !redundant(a.Level):
			c.Importable, c.Reason = false, fmt.Sprintf("%d of %d members present; a %s array can't run without all of them", c.Present, a.Current, c.RaidLevel)
		case !c.Complete && c.Present == 0:
			c.Importable, c.Reason = false, "no current member present"
		case !c.Complete:
			c.Reason = fmt.Sprintf("%d of %d members present: imports degraded (allow_degraded)", c.Present, a.Current)
		}
		if _, err := s.store.GetPool(c.PoolID); err == nil {
			c.Importable, c.Reason = false, "a pool with this ID already exists"
		} else if a.Unplaced != "" && c.Importable {
			c.Reason = strings.TrimPrefix(c.Reason+"; ", "; ") + a.Unplaced
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b ImportCandidate) int { return strings.Compare(a.PoolID, b.PoolID) })
	return out, members, nil
}

func (s *Server) handleListImportCandidates(w http.ResponseWriter, _ *http.Request) {
	list, _, err := s.importCandidates()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": list})
}

// ImportResult reports what an import recovered.
type ImportResult struct {
	Pool *store.Pool `json:"pool"`
	// Source is "manifest" (the copy on the pool) or "skeleton" (rebuilt from SPDK).
	Source           string    `json:"source"`
	Generation       uint64    `json:"generation,omitempty"`
	WrittenAt        time.Time `json:"written_at,omitzero"`
	Volumes          int       `json:"volumes"`
	Snapshots        int       `json:"snapshots"`
	ExportsEnabled   int       `json:"exports_enabled"`
	ExportsDisabled  int       `json:"exports_disabled"`
	Warnings         []string  `json:"warnings"`
	MissingVolumeIDs []string  `json:"missing_volume_ids,omitempty"`
}

var importMu sync.Mutex

func (s *Server) handleImportPool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PoolID string `json:"pool_id"`
		// Name renames the pool (when its name is taken here).
		Name string `json:"name"`
		// AllowDegraded starts a RAID1 that is missing members without them.
		AllowDegraded bool `json:"allow_degraded"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	importMu.Lock()
	defer importMu.Unlock()
	res, err := s.importPool(r.Context(), req.PoolID, req.Name, req.AllowDegraded)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// importRollback undoes the steps of a failed import, newest first.
type importRollback []func(context.Context)

func (rb importRollback) run(ctx context.Context) {
	for i := len(rb) - 1; i >= 0; i-- {
		rb[i](ctx)
	}
}

func (s *Server) importPool(ctx context.Context, poolID, rename string, allowDegraded bool) (res *ImportResult, err error) {
	cands, found, err := s.importCandidates()
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(cands, func(c ImportCandidate) bool { return c.PoolID == poolID })
	if i < 0 {
		return nil, fmt.Errorf("%w: no importable pool %q on the unclaimed disks", store.ErrNotFound, poolID)
	}
	cand := cands[i]
	if !cand.Importable {
		return nil, fmt.Errorf("%w: pool %s can't be imported: %s", store.ErrInvalidArgument, poolID, cand.Reason)
	}
	if !cand.Complete && !allowDegraded {
		return nil, fmt.Errorf("%w: %s; set allow_degraded to import it without the missing members", store.ErrInvalidArgument, cand.Reason)
	}

	cleanup := context.WithoutCancel(ctx)
	var rb importRollback
	defer func() {
		if err != nil {
			rb.run(cleanup)
		}
	}()

	// 1. Claim the members (no wipe: they carry our RAID by design). SPDK examines each one
	//    and assembles the array from the superblocks.
	var claimed []*store.Disk
	for _, f := range found[cand.RaidUUID] {
		if f.Stale {
			continue // a disk that left the array stays unclaimed
		}
		if f.Disk.Claimed { // claimed by an earlier attempt
			claimed = append(claimed, f.Disk)
			continue
		}
		disk := f.Disk
		if f.Identity != "" {
			// A member under a new disk ID keeps the identity its array knows it by, and must
			// keep it after restarts: record it before the data plane sees the disk.
			cp := *f.Disk
			cp.MemberUUID = f.Identity
			disk = &cp
			if _, merr := s.store.MutateDisk(f.Disk.ID, func(cur *store.Disk) error {
				cur.MemberUUID = f.Identity
				return nil
			}); merr != nil {
				return nil, fmt.Errorf("record the member UUID of disk %s: %w", f.Disk.ID, merr)
			}
		}
		d, cerr := s.attachAndClaim(ctx, disk, true)
		if cerr != nil {
			return nil, fmt.Errorf("claim disk %s: %w", f.Disk.ID, cerr)
		}
		claimed = append(claimed, d)
	}
	// From here a failure undoes only the database records. The disks stay claimed and the
	// array assembled, and a retry carries on from there: taking a running array apart again
	// would change it (detaching RAID1 members one by one records the first as failed in the
	// others' superblocks), and bdev_raid_delete under a loaded volume store leaves it
	// half-deleted.

	// 2. Wait for the array, start it degraded if asked, then wait for the volume store.
	raid, err := s.waitForRaid(ctx, cand.RaidName, allowDegraded)
	if err != nil {
		return nil, err
	}
	lvsName := "lvs_" + poolID
	var lvs *dataplane.ThinPool
	for deadline := time.Now().Add(30 * time.Second); ; {
		_ = s.dp.Settle(ctx, dataplane.ClaimedHandles(s.store))
		if stores, lerr := s.dp.ThinPools(ctx, lvsName, ""); lerr == nil && len(stores) > 0 {
			lvs = &stores[0]
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the volume store %s did not load from RAID %s", lvsName, cand.RaidName)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// 3. What SPDK has in the volume store.
	lvols, err := s.dp.Volumes(ctx)
	if err != nil {
		return nil, err
	}
	inLvs := map[string]dataplane.Volume{} // by lvol name
	for _, l := range lvols {
		if l.Pool == lvs.UUID {
			inLvs[l.Name] = l
		}
	}

	// 4. The copy on the pool, else the skeleton.
	res = &ImportResult{Warnings: []string{}}
	var plan *meta.Manifest
	metaUUID := ""
	if l, ok := inLvs[meta.LvolName]; ok {
		metaUUID = l.UUID
		if s.metaDev == nil {
			res.Warnings = append(res.Warnings, "on-pool metadata copies are disabled on this appliance; rebuilt the pool from the data plane")
		} else {
			slots, rerr := s.metaDev.ReadSlots(ctx, l.UUID)
			if rerr == nil {
				plan, rerr = meta.Newest(slots...)
			}
			if rerr != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("the metadata copy is unreadable (%v); rebuilt the pool from the data plane", rerr))
				plan = nil
			}
		}
	} else {
		res.Warnings = append(res.Warnings, "the pool has no metadata copy; rebuilt it from the data plane")
	}
	if plan != nil {
		res.Source, res.Generation, res.WrittenAt = "manifest", plan.Generation, plan.WrittenAt
		if plan.Pool.ID != poolID {
			return nil, fmt.Errorf("the metadata copy describes pool %s, not %s", plan.Pool.ID, poolID)
		}
	} else {
		res.Source = "skeleton"
		if plan, err = s.importSkeleton(ctx, poolID, inLvs, raid, res); err != nil {
			return nil, err
		}
	}

	// 5. The pool record: member disks in slot order from what SPDK assembled.
	pool := plan.Pool
	if rename != "" {
		pool.Name = rename
	}
	if err := validateDisplayName("pool", pool.Name); err != nil {
		return nil, err
	}
	diskByBdev := map[string]string{}
	for _, d := range claimed {
		diskByBdev[d.Handle] = d.ID
	}
	pool.DiskIDs, pool.DetachedDiskIDs, pool.Members = nil, nil, nil
	for _, b := range raid.Members {
		if id, ok := diskByBdev[b.Name]; ok {
			pool.DiskIDs = append(pool.DiskIDs, id)
		}
	}
	pool.ArrayName, pool.Device, pool.ThinPoolName, pool.ThinPoolUUID = cand.RaidName, cand.RaidName, lvsName, lvs.UUID
	pool.RaidLevel = store.RaidLevel(strings.ToUpper(raid.Level))
	pool.StripSizeKB = raid.StripKB
	pool.ChunkSizeBytes = lvs.ChunkBytes
	pool.TotalBytes, pool.FreeBytes = lvs.TotalBytes, lvs.FreeBytes
	pool.AllocatedBytes = pool.TotalBytes - pool.FreeBytes
	pool.CommittedBytes, pool.AvailableBytes, pool.SpaceState = 0, 0, ""
	pool.HealthStatus, pool.StatusMessage = store.HealthStatusReconciling, "imported"
	pool.MetaVolumeUUID, pool.MetaGeneration, pool.MetaHash, pool.MetaError = metaUUID, plan.Generation, "", ""
	pool.MetaWrittenAt = plan.WrittenAt

	// 6. Check every record before writing any.
	vols, snaps := s.importableLvols(plan, inLvs, res)
	if err := s.checkImportConflicts(&pool, plan); err != nil {
		return nil, err
	}

	// 7. Write the records; undo them all on failure.
	if err := s.store.CreatePool(&pool); err != nil {
		return nil, fmt.Errorf("record the pool: %w", err)
	}
	rb = append(rb, func(context.Context) {
		for _, x := range plan.Exports {
			_ = s.store.DeleteExport(x.ID)
		}
		for _, x := range snaps {
			_ = s.store.DeleteSnapshot(x.ID)
		}
		for _, x := range vols {
			_ = s.store.DeleteVolume(x.ID)
		}
		_ = s.store.DeletePool(pool.ID)
	})
	if err := s.recordLvols(vols, snaps); err != nil {
		return nil, err
	}
	res.Volumes, res.Snapshots = len(vols), len(snaps)
	have := map[string]bool{}
	for _, sn := range snaps {
		have[sn.ID] = true
	}
	for _, g := range plan.GroupSnapshots {
		if _, err := s.store.GetVolumeGroupSnapshot(g.ID); err == nil {
			continue
		}
		g.SnapshotIDs = slices.DeleteFunc(slices.Clone(g.SnapshotIDs), func(id string) bool { return !have[id] })
		if len(g.SnapshotIDs) == 0 {
			continue
		}
		if err := s.store.CreateVolumeGroupSnapshot(&g); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("group snapshot %s not restored: %v", g.ID, err))
		}
	}
	if err := s.importAccess(plan, vols, res); err != nil {
		return nil, err
	}
	// The member disks' durability settings travel with the pool (the reconciler applies the
	// write-cache policy).
	for _, md := range plan.Disks {
		if !slices.Contains(pool.DiskIDs, md.ID) {
			continue
		}
		if _, err := s.store.MutateDisk(md.ID, func(d *store.Disk) error {
			if d.PowerLossProtected == md.PowerLossProtected && d.WriteCachePolicy == md.WriteCachePolicy {
				return store.ErrSkipWrite
			}
			d.PowerLossProtected, d.WriteCachePolicy = md.PowerLossProtected, md.WriteCachePolicy
			return nil
		}); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("disk %s: settings not restored: %v", md.ID, err))
		}
	}

	s.logger.Warn("pool imported", "pool", pool.ID, "name", pool.Name, "source", res.Source,
		"generation", res.Generation, "volumes", res.Volumes, "snapshots", res.Snapshots)
	s.reconcileNow(cleanup)
	if p, gerr := s.store.GetPool(pool.ID); gerr == nil {
		res.Pool = p
	} else {
		res.Pool = &pool
	}
	return res, nil
}

// recordLvols records volumes and snapshots in dependency order: a snapshot after its volume,
// a clone after the snapshot it was made from.
func (s *Server) recordLvols(vols []*store.Volume, snaps []*store.Snapshot) error {
	done := map[string]bool{}
	for len(done) < len(vols)+len(snaps) {
		progress := false
		for _, v := range vols {
			if !done[v.ID] && (v.ParentSnapshotID == "" || done[v.ParentSnapshotID]) {
				if err := s.store.CreateVolume(v); err != nil {
					return fmt.Errorf("record volume %s: %w", v.ID, err)
				}
				done[v.ID], progress = true, true
			}
		}
		for _, sn := range snaps {
			if !done[sn.ID] && done[sn.VolumeID] && (sn.ParentSnapshotID == "" || done[sn.ParentSnapshotID]) {
				if err := s.store.CreateSnapshot(sn); err != nil {
					return fmt.Errorf("record snapshot %s: %w", sn.ID, err)
				}
				done[sn.ID], progress = true, true
			}
		}
		if !progress {
			// A parent outside the pool's records (e.g. a lost snapshot): record the rest
			// without the link rather than not at all.
			unlinked := false
			for _, v := range vols {
				if !done[v.ID] && v.ParentSnapshotID != "" {
					v.ParentSnapshotID, unlinked = "", true
					break
				}
			}
			for _, sn := range snaps {
				if !done[sn.ID] && done[sn.VolumeID] && sn.ParentSnapshotID != "" {
					sn.ParentSnapshotID, unlinked = "", true
				}
			}
			if !unlinked {
				return fmt.Errorf("volumes and snapshots reference each other in a cycle; nothing recorded")
			}
		}
	}
	return nil
}

// waitForRaid waits for SPDK to assemble name from its members' superblocks.
func (s *Server) waitForRaid(ctx context.Context, name string, allowDegraded bool) (*dataplane.Array, error) {
	startedDegraded := false
	for deadline := time.Now().Add(30 * time.Second); ; {
		_ = s.dp.Settle(ctx, dataplane.ClaimedHandles(s.store))
		raids, err := s.dp.Arrays(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range raids {
			if r.Name != name {
				continue
			}
			switch strings.ToLower(r.State) {
			case "online":
				return &r, nil
			case "configuring":
				if allowDegraded && !startedDegraded && r.Discovered > 0 {
					if err := s.dp.StartDegraded(ctx, name); err != nil {
						return nil, fmt.Errorf("start RAID %s degraded: %w", name, err)
					}
					s.logger.Warn("imported pool started degraded", "raid", name)
					startedDegraded = true
				}
			}
		}
		if time.Now().After(deadline) {
			if !allowDegraded {
				return nil, fmt.Errorf("%w: RAID %s did not come online with the disks present", store.ErrInvalidArgument, name)
			}
			return nil, fmt.Errorf("RAID %s did not come online", name)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// importableLvols returns the plan's volumes and snapshots whose lvols exist, ready to record.
func (s *Server) importableLvols(plan *meta.Manifest, inLvs map[string]dataplane.Volume, res *ImportResult) ([]*store.Volume, []*store.Snapshot) {
	lvsName := "lvs_" + plan.Pool.ID
	var vols []*store.Volume
	present := map[string]bool{}
	for _, v := range plan.Volumes {
		l, ok := inLvs[v.ID]
		if !ok || l.Snapshot {
			res.MissingVolumeIDs = append(res.MissingVolumeIDs, v.ID)
			res.Warnings = append(res.Warnings, fmt.Sprintf("volume %s (%s) isn't in the pool's data plane; not restored", v.ID, v.Name))
			continue
		}
		v.UUID = l.UUID
		v.Handle, v.Device = lvsName+"/"+v.ID, lvsName+"/"+v.ID
		v.AttachmentState, v.HealthStatus, v.StatusMessage = store.AttachmentStateUnattached, store.HealthStatusReconciling, ""
		present[v.ID] = true
		vols = append(vols, &v)
	}
	var snaps []*store.Snapshot
	for _, sn := range plan.Snapshots {
		l, ok := inLvs[sn.ID]
		if !ok || !present[sn.VolumeID] {
			res.Warnings = append(res.Warnings, fmt.Sprintf("snapshot %s (%s) not restored: it or its volume is missing from the data plane", sn.ID, sn.Name))
			continue
		}
		sn.UUID, sn.Handle = l.UUID, lvsName+"/"+sn.ID
		snaps = append(snaps, &sn)
	}
	return vols, snaps
}

// checkImportConflicts refuses an import whose records collide with this database's.
// Access objects with the same ID are taken as the same object (this appliance already has
// them); a different object under the same name, or the same portal address, is a conflict.
func (s *Server) checkImportConflicts(pool *store.Pool, plan *meta.Manifest) error {
	var conflicts []string
	pools, _ := s.store.ListPools()
	for _, p := range pools {
		if p.Name == pool.Name {
			conflicts = append(conflicts, fmt.Sprintf("pool name %q is taken (import it under another name)", pool.Name))
		}
	}
	hosts, _ := s.store.ListHosts()
	for _, h := range plan.Hosts {
		for _, x := range hosts {
			if x.ID != h.ID && x.Name == h.Name {
				conflicts = append(conflicts, fmt.Sprintf("host name %q is used by %s", h.Name, x.ID))
			}
		}
	}
	groups, _ := s.store.ListInitiatorGroups()
	for _, g := range plan.InitiatorGroup {
		for _, x := range groups {
			if x.ID != g.ID && x.Name == g.Name {
				conflicts = append(conflicts, fmt.Sprintf("initiator group name %q is used by %s", g.Name, x.ID))
			}
		}
	}
	portals, _ := s.store.ListPortals("")
	for _, p := range plan.Portals {
		for _, x := range portals {
			if x.ID == p.ID {
				continue
			}
			if x.Name == p.Name {
				conflicts = append(conflicts, fmt.Sprintf("portal name %q is used by %s", p.Name, x.ID))
			}
			if x.Protocol == p.Protocol && x.IPAddress == p.IPAddress && x.Port == p.Port {
				conflicts = append(conflicts, fmt.Sprintf("portal address %s:%d is used by %s", p.IPAddress, p.Port, x.ID))
			}
		}
	}
	exports, _ := s.store.ListExports("", "")
	for _, e := range plan.Exports {
		for _, x := range exports {
			if x.ID == e.ID || x.Name == e.Name {
				conflicts = append(conflicts, fmt.Sprintf("export %q (%s) already exists", e.Name, e.ID))
			}
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("%w: %s", store.ErrAlreadyExists, strings.Join(conflicts, "; "))
	}
	return nil
}

// importAccess restores the plan's hosts, initiator groups, portals and exports. An export is
// enabled only if every portal it uses has its address on this appliance and its groups need
// no secret that the copy doesn't hold.
func (s *Server) importAccess(plan *meta.Manifest, vols []*store.Volume, res *ImportResult) error {
	for _, h := range plan.Hosts {
		if _, err := s.store.GetHost(h.ID); err == nil {
			continue
		}
		if err := s.store.CreateHost(&h); err != nil {
			return fmt.Errorf("record host %s: %w", h.ID, err)
		}
	}
	needsSecret := map[string]bool{}
	for _, g := range plan.InitiatorGroup {
		needsSecret[g.ID] = g.CHAPUser != "" || g.MutualCHAPUser != ""
		if _, err := s.store.GetInitiatorGroup(g.ID); err == nil {
			continue
		}
		if err := s.store.CreateInitiatorGroup(&g); err != nil {
			return fmt.Errorf("record initiator group %s: %w", g.ID, err)
		}
	}
	portalOK := map[string]bool{}
	for _, p := range plan.Portals {
		portalOK[p.ID] = dataplane.AddressPresent(p.IPAddress)
		if _, err := s.store.GetPortal(p.ID); err == nil {
			continue
		}
		p.IscsiPgTag = 0 // tags are allocated per appliance
		if err := s.store.CreatePortal(&p); err != nil {
			return fmt.Errorf("record portal %s: %w", p.ID, err)
		}
	}
	restored := map[string]bool{}
	for _, v := range vols {
		restored[v.ID] = true
	}
	for _, e := range plan.Exports {
		if !restored[e.VolumeID] {
			continue
		}
		var why []string
		for _, pid := range e.PortalIDs {
			if !portalOK[pid] {
				why = append(why, "portal "+pid+" has an address that isn't on this appliance; give the export a portal on this appliance (or add the address here), then enable it")
			}
		}
		for _, gid := range e.InitiatorGroupIDs {
			if needsSecret[gid] {
				why = append(why, "initiator group "+gid+" uses CHAP; set its secrets again, then enable the export")
			}
		}
		e.IscsiIgTag = 0
		e.HealthStatus, e.StatusMessage = store.HealthStatusReconciling, ""
		e.Disabled, e.DisabledReason = len(why) > 0, strings.Join(why, "; ")
		if err := s.store.CreateExport(&e); err != nil {
			return fmt.Errorf("record export %s: %w", e.ID, err)
		}
		if e.Disabled {
			res.ExportsDisabled++
			res.Warnings = append(res.Warnings, fmt.Sprintf("export %s (%s) restored disabled: %s", e.ID, e.Name, e.DisabledReason))
		} else {
			res.ExportsEnabled++
		}
	}
	return nil
}

// importSkeleton rebuilds a pool from what the data plane knows when the pool has no readable
// copy: volumes and snapshots named by their IDs, with sizes and parentage (LVM's origins: a
// snapshot's volume, a clone's snapshot); no exports.
func (s *Server) importSkeleton(ctx context.Context, poolID string, inLvs map[string]dataplane.Volume, raid *dataplane.Array, res *ImportResult) (*meta.Manifest, error) {
	devices, err := s.dp.Devices(ctx)
	if err != nil {
		return nil, err
	}
	byUUID := map[string]dataplane.Device{}
	for _, d := range devices {
		if d.UUID != "" {
			byUUID[d.UUID] = d
		}
	}
	var vols []dataplane.Volume
	for name, v := range inLvs {
		if name != meta.LvolName {
			vols = append(vols, v)
		}
	}
	slices.SortFunc(vols, func(a, b dataplane.Volume) int {
		if a.Seq != b.Seq {
			return a.Seq - b.Seq
		}
		return strings.Compare(a.Name, b.Name)
	})
	m := &meta.Manifest{Format: meta.FormatVersion, Pool: store.Pool{
		ID: poolID, Name: poolID, RaidLevel: store.RaidLevel(strings.ToUpper(raid.Level)), CreatedAt: time.Now().UTC(),
	}}
	res.Warnings = append(res.Warnings, "recovered from the data plane only: names, labels, exports and access control are not restored; volumes and snapshots are named by their IDs")
	for _, v := range vols {
		if v.Snapshot {
			continue
		}
		dev := byUUID[v.UUID]
		out := store.Volume{ID: v.Name, Name: v.Name, PoolID: poolID, SizeBytes: dev.SizeBytes, ThinProvision: v.Thin}
		if o, ok := inLvs[dev.Origin]; ok && o.Snapshot {
			out.ParentSnapshotID = dev.Origin // a clone
		}
		m.Volumes = append(m.Volumes, out)
	}
	seq := uint64(0)
	for _, v := range vols {
		if !v.Snapshot {
			continue
		}
		dev := byUUID[v.UUID]
		owner, ok := inLvs[dev.Origin]
		if !ok || owner.Snapshot {
			// Its volume was deleted: there is nothing to record it under.
			res.Warnings = append(res.Warnings, fmt.Sprintf("snapshot %s is left out: the volume it was taken of is gone (clone it by hand if its data is needed)", v.Name))
			continue
		}
		seq++
		m.Snapshots = append(m.Snapshots, store.Snapshot{ID: v.Name, Name: v.Name, VolumeID: owner.Name, PoolID: poolID,
			SizeBytes: dev.SizeBytes, SequenceNumber: seq})
	}
	return m, nil
}
