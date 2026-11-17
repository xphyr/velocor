package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/meta"
	"github.com/velocor/velocor/internal/spdk"
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

type foundSB struct {
	disk *store.Disk
	sb   *hardware.RaidSuperblock
}

// readSuperblock reads a disk's RAID superblock through the kernel (nil: none).
func (s *Server) readSuperblock(d *store.Disk) (*hardware.RaidSuperblock, error) {
	if s.sbReader != nil {
		return s.sbReader(d)
	}
	if d.DevicePath == "" {
		return nil, nil // e.g. an NVMe drive bound to vfio-pci: no kernel view
	}
	return hardware.ReadRaidSuperblock(d.DevicePath)
}

// raidLevelName maps SPDK's enum raid_level to the store's names.
func raidLevelName(level uint32) store.RaidLevel {
	switch level {
	case 0:
		return store.RaidLevel0
	case 1:
		return store.RaidLevel1
	case 95:
		return store.RaidLevel5F
	case 99:
		return "CONCAT"
	}
	return store.RaidLevel(fmt.Sprintf("LEVEL-%d", level))
}

// importCandidates groups the unclaimed disks carrying a Velocor RAID superblock by array.
func (s *Server) importCandidates() ([]ImportCandidate, map[string][]foundSB, error) {
	disks, err := s.store.ListDisks()
	if err != nil {
		return nil, nil, err
	}
	// An import that failed after claiming its disks leaves them claimed with the array
	// assembled (undoing that would degrade it): such arrays are candidates too.
	assembled := map[string]*spdk.RaidBdevInfo{} // by member bdev name
	if s.spdk != nil {
		if raids, err := s.spdk.BdevRaidGetBdevs(context.Background(), "all"); err == nil {
			for i := range raids {
				for _, b := range raids[i].BaseBdevsList {
					if b.Name != "" {
						assembled[b.Name] = &raids[i]
					}
				}
			}
		}
	}
	byRaid := map[string][]foundSB{}
	for _, d := range disks {
		if d.IsBootDevice || d.PoolID != "" {
			continue
		}
		if d.Claimed {
			if r := assembled[d.SPDKBdevName]; r != nil && strings.HasPrefix(r.Name, "raid_pool-") {
				byRaid[r.UUID] = append(byRaid[r.UUID], foundSB{disk: d, sb: superblockOf(r)})
			}
			continue
		}
		sb, err := s.readSuperblock(d)
		if err != nil {
			s.logger.Warn("cannot read the RAID superblock", "disk", d.ID, "err", err)
			continue
		}
		if sb == nil || !strings.HasPrefix(sb.Name, "raid_pool-") {
			continue
		}
		byRaid[sb.UUID] = append(byRaid[sb.UUID], foundSB{disk: d, sb: sb})
	}
	out := []ImportCandidate{}
	for uuid, found := range byRaid {
		// The newest superblock describes the array; members with an older one left it.
		newest := found[0].sb
		for _, f := range found {
			if f.sb.SeqNumber > newest.SeqNumber {
				newest = f.sb
			}
		}
		c := ImportCandidate{
			PoolID:    strings.TrimPrefix(newest.Name, "raid_"),
			RaidName:  newest.Name,
			RaidUUID:  uuid,
			RaidLevel: string(raidLevelName(newest.Level)),
			Members:   newest.NumMembers,
			SeqNumber: newest.SeqNumber,
			SizeBytes: newest.RaidBlocks * uint64(newest.BlockSize),
		}
		configured := map[string]int{}
		for _, m := range newest.Members {
			if m.State == hardware.RaidSBMemberConfigured {
				configured[m.BdevUUID] = m.Slot
			}
		}
		for _, f := range found {
			m := ImportMember{DiskID: f.disk.ID, Serial: f.disk.SerialNumber, Stale: f.sb.SeqNumber < newest.SeqNumber && !f.disk.Claimed}
			if f.disk.Type == store.DiskTypeSSD {
				if slot, ok := configured[dataplane.DiskBdevUUID(f.disk)]; ok {
					m.Slot = &slot
				} else {
					m.Stale = true // not a configured member of the current array
				}
			}
			if !m.Stale {
				c.Present++
			}
			c.Disks = append(c.Disks, m)
		}
		slices.SortFunc(c.Disks, func(a, b ImportMember) int { return strings.Compare(a.DiskID, b.DiskID) })
		c.Complete = c.Present >= len(configured)
		c.Importable = true
		switch {
		case c.RaidLevel != string(store.RaidLevel0) && c.RaidLevel != string(store.RaidLevel1):
			c.Importable, c.Reason = false, "unsupported RAID level "+c.RaidLevel
		case !c.Complete && c.RaidLevel != string(store.RaidLevel1):
			c.Importable, c.Reason = false, fmt.Sprintf("%d of %d members present; a %s array can't run without all of them", c.Present, len(configured), c.RaidLevel)
		case !c.Complete && c.Present == 0:
			c.Importable, c.Reason = false, "no current member present"
		case !c.Complete:
			c.Reason = fmt.Sprintf("%d of %d members present: imports degraded (allow_degraded)", c.Present, len(configured))
		}
		if _, err := s.store.GetPool(c.PoolID); err == nil {
			c.Importable, c.Reason = false, "a pool with this ID already exists"
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b ImportCandidate) int { return strings.Compare(a.PoolID, b.PoolID) })
	return out, byRaid, nil
}

// superblockOf describes an assembled array the way its superblock would (seq 0: the kernel
// read superblocks of other members take precedence).
func superblockOf(r *spdk.RaidBdevInfo) *hardware.RaidSuperblock {
	sb := &hardware.RaidSuperblock{UUID: r.UUID, Name: r.Name, NumMembers: int(r.NumBaseBdevs)}
	switch strings.ToLower(r.RaidLevel) {
	case "raid0":
		sb.Level = 0
	case "raid1":
		sb.Level = 1
	default:
		sb.Level = 99
	}
	for i, b := range r.BaseBdevsList {
		st := uint32(hardware.RaidSBMemberMissing)
		if b.IsConfigured {
			st = hardware.RaidSBMemberConfigured
		}
		sb.Members = append(sb.Members, hardware.RaidSBMember{Slot: i, BdevUUID: b.UUID, State: st})
	}
	return sb
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
	if err := s.requireSPDK(); err != nil {
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
		if j := slices.IndexFunc(cand.Disks, func(m ImportMember) bool { return m.DiskID == f.disk.ID }); j < 0 || cand.Disks[j].Stale {
			continue // a disk that left the array stays unclaimed
		}
		if f.disk.Claimed { // claimed by an earlier attempt
			claimed = append(claimed, f.disk)
			continue
		}
		d, cerr := s.attachAndClaim(ctx, f.disk, true)
		if cerr != nil {
			return nil, fmt.Errorf("claim disk %s: %w", f.disk.ID, cerr)
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
	var lvs *spdk.LvolStoreInfo
	for deadline := time.Now().Add(30 * time.Second); ; {
		_ = s.spdk.BdevWaitForExamine(ctx)
		if stores, lerr := s.spdk.BdevLvolGetLvstores(ctx, lvsName, ""); lerr == nil && len(stores) > 0 {
			lvs = &stores[0]
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the volume store %s did not load from RAID %s", lvsName, cand.RaidName)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// 3. What SPDK has in the volume store.
	lvols, err := s.spdk.BdevLvolGetLvols(ctx)
	if err != nil {
		return nil, err
	}
	inLvs := map[string]spdk.LvolInfo{} // by lvol name
	for _, l := range lvols {
		if l.LVS.UUID == lvs.UUID {
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
			res.Warnings = append(res.Warnings, "on-pool metadata copies are disabled on this appliance; rebuilt the pool from SPDK")
		} else {
			slots, rerr := s.metaDev.ReadSlots(ctx, l.UUID)
			if rerr == nil {
				plan, rerr = meta.Newest(slots...)
			}
			if rerr != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("the metadata copy is unreadable (%v); rebuilt the pool from SPDK", rerr))
				plan = nil
			}
		}
	} else {
		res.Warnings = append(res.Warnings, "the pool has no metadata copy; rebuilt it from SPDK")
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
		diskByBdev[d.SPDKBdevName] = d.ID
	}
	pool.DiskIDs, pool.DetachedDiskIDs, pool.Members = nil, nil, nil
	for _, b := range raid.BaseBdevsList {
		if id, ok := diskByBdev[b.Name]; ok {
			pool.DiskIDs = append(pool.DiskIDs, id)
		}
	}
	pool.BaseBdevName, pool.EffectiveBdevName, pool.LVSName, pool.LVSUUID = cand.RaidName, cand.RaidName, lvsName, lvs.UUID
	pool.RaidLevel = store.RaidLevel(strings.ToUpper(raid.RaidLevel))
	pool.StripSizeKB = raid.StripSizeKb
	pool.ClusterSizeBytes = lvs.ClusterSize
	cs := uint64(lvs.ClusterSize)
	pool.TotalBytes, pool.FreeBytes = lvs.TotalDataClusters*cs, lvs.FreeClusters*cs
	pool.AllocatedBytes = pool.TotalBytes - pool.FreeBytes
	pool.CommittedBytes, pool.AvailableBytes, pool.SpaceState = 0, 0, ""
	pool.HealthStatus, pool.StatusMessage = store.HealthStatusReconciling, "imported"
	pool.MetaLvolUUID, pool.MetaGeneration, pool.MetaHash, pool.MetaError = metaUUID, plan.Generation, "", ""
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
func (s *Server) waitForRaid(ctx context.Context, name string, allowDegraded bool) (*spdk.RaidBdevInfo, error) {
	startedDegraded := false
	for deadline := time.Now().Add(30 * time.Second); ; {
		_ = s.spdk.BdevWaitForExamine(ctx)
		raids, err := s.spdk.BdevRaidGetBdevs(ctx, "all")
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
				if allowDegraded && !startedDegraded && r.NumBaseBdevsDiscovered > 0 {
					if err := s.spdk.BdevRaidStartDegraded(ctx, name); err != nil {
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
func (s *Server) importableLvols(plan *meta.Manifest, inLvs map[string]spdk.LvolInfo, res *ImportResult) ([]*store.Volume, []*store.Snapshot) {
	lvsName := "lvs_" + plan.Pool.ID
	var vols []*store.Volume
	present := map[string]bool{}
	for _, v := range plan.Volumes {
		l, ok := inLvs[v.ID]
		if !ok || l.IsSnapshot {
			res.MissingVolumeIDs = append(res.MissingVolumeIDs, v.ID)
			res.Warnings = append(res.Warnings, fmt.Sprintf("volume %s (%s) has no lvol in the pool; not restored", v.ID, v.Name))
			continue
		}
		v.UUID = l.UUID
		v.LvolBdevName, v.EffectiveBdevName = lvsName+"/"+v.ID, lvsName+"/"+v.ID
		v.AttachmentState, v.HealthStatus, v.StatusMessage = store.AttachmentStateUnattached, store.HealthStatusReconciling, ""
		present[v.ID] = true
		vols = append(vols, &v)
	}
	var snaps []*store.Snapshot
	for _, sn := range plan.Snapshots {
		l, ok := inLvs[sn.ID]
		if !ok || !present[sn.VolumeID] {
			res.Warnings = append(res.Warnings, fmt.Sprintf("snapshot %s (%s) not restored: its lvol or volume is missing", sn.ID, sn.Name))
			continue
		}
		sn.UUID, sn.LvolBdevName = l.UUID, lvsName+"/"+sn.ID
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
	local := localAddresses()
	portalOK := map[string]bool{}
	for _, p := range plan.Portals {
		portalOK[p.ID] = local[p.IPAddress] || p.IPAddress == "0.0.0.0" || p.IPAddress == "::"
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
				why = append(why, "portal "+pid+" has an address that isn't on this appliance; change its address, then enable the export")
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

// localAddresses are the IP addresses configured on this appliance.
func localAddresses() map[string]bool {
	out := map[string]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			out[n.IP.String()] = true
		}
	}
	return out
}

// importSkeleton rebuilds a pool from what SPDK knows when the pool has no readable copy:
// volumes and snapshots named by their IDs, with sizes and parentage; no exports.
func (s *Server) importSkeleton(ctx context.Context, poolID string, inLvs map[string]spdk.LvolInfo, raid *spdk.RaidBdevInfo, res *ImportResult) (*meta.Manifest, error) {
	bdevs, err := s.spdk.BdevGetBdevs(ctx, "")
	if err != nil {
		return nil, err
	}
	type node struct {
		l      spdk.LvolInfo
		size   uint64
		parent string // base snapshot's lvol name
	}
	nodes := map[string]*node{}
	for name, l := range inLvs {
		if name == meta.LvolName {
			continue
		}
		n := &node{l: l}
		for _, b := range bdevs {
			if b.UUID != l.UUID {
				continue
			}
			n.size = b.NumBlocks * uint64(b.BlockSize)
			if lv, ok := b.DriverSpecific["lvol"].(map[string]any); ok {
				n.parent, _ = lv["base_snapshot"].(string)
			}
		}
		nodes[name] = n
	}
	children := map[string][]string{}
	names := make([]string, 0, len(nodes))
	for name, n := range nodes {
		names = append(names, name)
		if n.parent != "" {
			children[n.parent] = append(children[n.parent], name)
		}
	}
	slices.Sort(names)
	for _, c := range children {
		slices.Sort(c)
	}
	// A snapshot belongs to the volume reached through its snapshot children (the chain of
	// later snapshots), else to its first volume child; other volume children are clones.
	var owner func(snap string) string
	owner = func(snap string) string {
		for _, c := range children[snap] {
			if nodes[c].l.IsSnapshot {
				if o := owner(c); o != "" {
					return o
				}
			}
		}
		var vols []string
		for _, c := range children[snap] {
			if !nodes[c].l.IsSnapshot {
				vols = append(vols, c)
			}
		}
		if len(vols) > 1 {
			// SPDK keeps no creation order: the volume a snapshot was taken of and the
			// clones made from it look alike.
			res.Warnings = append(res.Warnings, fmt.Sprintf("snapshot %s has volumes %s: recorded as taken of %s, the others as its clones; correct this if needed",
				snap, strings.Join(vols, ", "), vols[0]))
		}
		if len(vols) > 0 {
			return vols[0]
		}
		return ""
	}
	m := &meta.Manifest{Format: meta.FormatVersion, Pool: store.Pool{
		ID: poolID, Name: poolID, RaidLevel: store.RaidLevel(strings.ToUpper(raid.RaidLevel)), CreatedAt: time.Now().UTC(),
	}}
	snapOwner := map[string]string{}
	for _, name := range names {
		if nodes[name].l.IsSnapshot {
			snapOwner[name] = owner(name)
		}
	}
	res.Warnings = append(res.Warnings, "recovered from SPDK only: names, labels, QoS, exports and access control are not restored; volumes and snapshots are named by their IDs")
	for _, name := range names {
		n := nodes[name]
		if n.l.IsSnapshot {
			continue
		}
		v := store.Volume{ID: name, Name: name, PoolID: poolID, SizeBytes: n.size, ThinProvision: n.l.IsThinProvisioned}
		if n.parent != "" && snapOwner[n.parent] != name {
			v.ParentSnapshotID = n.parent // a clone
		}
		m.Volumes = append(m.Volumes, v)
	}
	seq := uint64(0)
	for _, name := range names {
		n := nodes[name]
		if !n.l.IsSnapshot || snapOwner[name] == "" {
			continue
		}
		seq++
		m.Snapshots = append(m.Snapshots, store.Snapshot{ID: name, Name: name, VolumeID: snapOwner[name], PoolID: poolID,
			SizeBytes: n.size, ParentSnapshotID: n.parent, SequenceNumber: seq})
	}
	return m, nil
}
