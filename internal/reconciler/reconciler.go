package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/store"
)

// State captures the runtime status of the desired-state reconciler.
type State struct {
	InProgress         bool      `json:"in_progress"`
	LastReconciledAt   time.Time `json:"last_reconciled_at"`
	DriftRepairedCount uint64    `json:"drift_repaired_count"`
	DegradedResources  []string  `json:"degraded_resources"`
	LastError          string    `json:"last_error,omitempty"`
	// Unmanaged lists what the data plane holds that no store record accounts for (P1-5).
	Unmanaged []UnmanagedObject `json:"unmanaged_objects,omitempty"`
}

// Reconciler continuously converges the observed state of the hardware and the data plane
// toward the desired state persisted in bbolt.
//
// Safety rules (see TODO C1):
//   - It never creates arrays, thin pools or volumes (but each pool's metadata volume). The API
//     creates them, and after a reboot the data plane brings them back from the disks (Settle);
//     anything still missing is reported, never re-created, because re-creating it would hand
//     hosts blank storage under the old identity.
//   - It only writes store records whose observed status actually changed, and only the
//     status fields it owns, via atomic Mutate* calls.
type Reconciler struct {
	store  store.Store
	dp     dataplane.Plane
	hw     *hardware.Manager
	logger *slog.Logger

	passMu sync.Mutex // serialises passes
	meta   *metaSync  // on-pool metadata copies (nil: disabled)
	// discover replaces the hardware scan (tests).
	discover func() ([]*store.Disk, error)

	// cacheRefused: disks whose drive refused to turn its write cache off since velod started.
	// They are retried once per start, not every pass (the refusal is recorded on the disk and
	// reported in the durability warnings).
	cacheRefused map[string]bool

	now func() time.Time // replaces time.Now (tests)

	// unmanagedSeen: when each unaccounted data-plane object was first seen (reported after
	// unmanagedGrace); unmanagedLogged: those already logged.
	unmanagedSeen   map[string]time.Time
	unmanagedLogged map[string]bool
	unmanagedGrace  *time.Duration // nil: defaultUnmanagedGrace

	started time.Time

	mu       sync.Mutex
	state    State
	restored bool // a pass has brought up everything
}

// New creates a new Reconciler instance.
func New(st store.Store, dp dataplane.Plane, hwMgr *hardware.Manager, logger *slog.Logger) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{
		store:   st,
		dp:      dp,
		hw:      hwMgr,
		logger:  logger,
		started: time.Now(),
	}
}

// restoreTimeout is how long after velod's start Restoring can last: pools that can't be brought
// up by then are a fault, reported as such.
const restoreTimeout = 5 * time.Minute

// Restoring reports whether no pass since velod started has brought up everything the claimed
// disks hold, so after a reboot the pools, volumes and exports aren't all back yet.
func (r *Reconciler) Restoring() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.restored && time.Since(r.started) < restoreTimeout
}

// State returns a snapshot of the reconciler's current status.
func (r *Reconciler) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := r.state
	cp.DegradedResources = slices.Clone(r.state.DegradedResources)
	cp.Unmanaged = slices.Clone(r.state.Unmanaged)
	return cp
}

// Start runs a background reconciliation loop at the given interval until ctx is cancelled.
func (r *Reconciler) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	_, _ = r.ReconcileOnce(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = r.ReconcileOnce(ctx)
		}
	}
}

// pass accumulates the outcome of one reconciliation pass.
type pass struct {
	repaired uint64
	degraded []string
	errs     []string
	notes    map[string][]string // pool ID -> extra reasons for its status message
	lostVols map[string]string   // volume ID -> why it is DEGRADED: part of its data is lost

	unmanaged        []UnmanagedObject
	unmanagedChecked bool // false: keep the last report
}

// notePool adds a reason to a pool's status message (and the pass's degraded resources).
func (p *pass) notePool(poolID, reason string) {
	if p.notes == nil {
		p.notes = map[string][]string{}
	}
	p.notes[poolID] = append(p.notes[poolID], reason)
	p.degrade("pool:"+poolID, reason)
}

func (p *pass) degrade(resource, reason string) {
	p.degraded = append(p.degraded, resource)
	p.errs = append(p.errs, resource+": "+reason)
}

// ReconcileOnce executes a full topological reconciliation pass across Disks, Pools,
// Volumes, and SAN Exports. Concurrent callers wait for the in-flight pass.
func (r *Reconciler) ReconcileOnce(ctx context.Context) (State, error) {
	r.passMu.Lock()
	defer r.passMu.Unlock()
	r.mu.Lock()
	r.state.InProgress = true
	r.mu.Unlock()

	p := &pass{}
	discovered := r.syncHardware(p)
	r.enforceWriteCache(p)

	if _, err := r.dp.Version(ctx); err != nil {
		r.finishPass(0, []string{"data-plane"}, fmt.Sprintf("the data plane is unreachable: %v", err))
		return r.State(), err
	}

	if r.relinkMovedMembers(ctx, p, discovered) {
		r.enforceWriteCache(p) // the relinked disks inherit their policy: apply it before anything is written
	}

	r.reconcileDisks(ctx, p)
	r.nvmeWriteCache(ctx, p)
	r.applySettings(ctx, p)

	// Bring up what the claimed disks hold (arrays, thin pools, volumes) before anything below
	// concludes that it is missing.
	examined := true
	if err := r.dp.Settle(ctx, dataplane.ClaimedHandles(r.store)); err != nil {
		examined = false
		p.errs = append(p.errs, fmt.Sprintf("bring up the pools: %v", err))
	}

	poolOnline := r.reconcilePools(ctx, p, examined)
	r.purgeDeletedVolumes(ctx, p, poolOnline)
	r.purgeDeletedSnapshots(ctx, p, poolOnline)
	r.reconcileSpace(ctx, p, poolOnline)
	volOnline := r.reconcileVolumes(ctx, p, poolOnline)
	r.reconcileExports(ctx, p, volOnline)
	r.reconcileMeta(ctx, p, poolOnline)
	r.findUnmanaged(ctx, p, examined)

	lastErr := strings.Join(p.errs, "; ")
	r.finishPass(p.repaired, p.degraded, lastErr)
	if examined {
		r.mu.Lock()
		r.restored = true
		r.mu.Unlock()
	}
	if p.unmanagedChecked {
		r.mu.Lock()
		r.state.Unmanaged = p.unmanaged
		r.mu.Unlock()
	}
	if len(p.errs) > 0 {
		return r.State(), fmt.Errorf("%s", lastErr)
	}
	return r.State(), nil
}

// syncHardware records the disks a hardware scan finds and returns them (nil: no scan).
func (r *Reconciler) syncHardware(p *pass) []*store.Disk {
	discover := r.discover
	if discover == nil {
		if r.hw == nil {
			return nil
		}
		if err := r.hw.RefreshBootGuard(); err != nil {
			p.errs = append(p.errs, fmt.Sprintf("boot guard: %v", err))
		}
		discover = r.hw.DiscoverDisks
	}
	discovered, err := discover()
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("hardware scan: %v", err))
		return nil
	}
	for _, d := range discovered {
		if err := r.store.SyncDiscoveredDisk(d); err != nil {
			p.errs = append(p.errs, fmt.Sprintf("sync disk %s: %v", d.ID, err))
		}
	}
	return discovered
}

// enforceWriteCache keeps the volatile write cache off on disks whose policy says so (docs/review-history.md
// P0-4): drives can turn it back on after a power cycle or reset. A drive that refuses is
// recorded (Disk.WriteCacheError, shown in the durability warnings) and tried again only after
// velod restarts or the operator sets the policy again, so it doesn't fill every pass's errors.
func (r *Reconciler) enforceWriteCache(p *pass) {
	if r.hw == nil {
		return
	}
	disks, err := r.store.ListDisks()
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list disks: %v", err))
		return
	}
	for _, d := range disks {
		if d.Type == store.DiskTypeNVMe {
			continue // nvmeWriteCache
		}
		if d.WriteCachePolicy != store.WriteCachePolicyDisabled || d.IsBootDevice || d.WriteCache == store.WriteCacheWriteThrough {
			if d.WriteCacheError != "" {
				r.recordWriteCache(p, d.ID, d.WriteCache, "")
			}
			continue
		}
		if d.WriteCacheError != "" && r.cacheRefused[d.ID] {
			continue
		}
		state, err := r.hw.SetWriteCache(d, false)
		msg := ""
		if err != nil {
			msg = err.Error()
			if r.cacheRefused == nil {
				r.cacheRefused = map[string]bool{}
			}
			r.cacheRefused[d.ID] = true
			r.logger.Warn("the drive refuses to turn its write cache off; it stays at risk on power loss", "disk", d.ID, "err", err)
		} else {
			delete(r.cacheRefused, d.ID)
			p.repaired++
		}
		r.recordWriteCache(p, d.ID, state, msg)
	}
}

func (r *Reconciler) recordWriteCache(p *pass, id string, state store.WriteCacheState, msg string) {
	if _, err := r.store.MutateDisk(id, func(cur *store.Disk) error {
		if cur.WriteCache == state && cur.WriteCacheError == msg {
			return store.ErrSkipWrite
		}
		cur.WriteCache, cur.WriteCacheError = state, msg
		return nil
	}); err != nil {
		p.errs = append(p.errs, fmt.Sprintf("record write cache of %s: %v", id, err))
	}
}

func (r *Reconciler) bdevSet(ctx context.Context) (map[string]dataplane.Device, error) {
	bdevs, err := r.dp.Devices(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]dataplane.Device, len(bdevs))
	for _, b := range bdevs {
		m[b.Name] = b
		for _, a := range b.Aliases {
			m[a] = b
		}
	}
	return m, nil
}

func (r *Reconciler) reconcileDisks(ctx context.Context, p *pass) {
	bdevs, err := r.bdevSet(ctx)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list bdevs: %v", err))
		return
	}
	disks, err := r.store.ListDisks()
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list disks: %v", err))
		return
	}
	pools, err := r.store.ListPools()
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list pools: %v", err))
		return
	}
	raids, err := r.dp.Arrays(ctx)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list raid bdevs: %v", err))
		return
	}
	for _, d := range disks {
		if !d.Claimed || d.IsBootDevice {
			continue
		}
		bdevName, drv := d.Handle, d.DriverState
		health := store.HealthStatusOnline
		var smart *store.SmartHealth

		if _, ok := bdevs[bdevName]; !ok || bdevName == "" {
			if pool := leftPool(d, pools, raids); pool != nil {
				// SPDK re-adds a returning member to its empty slot and rebuilds onto it; a disk
				// that failed must not be trusted again implicitly. It stays out until an
				// operator releases it (and wipes it) or reuses it with replace.
				p.degrade("disk:"+d.ID, fmt.Sprintf("left pool %s; release it, then replace with it to reuse it", pool.Name))
				continue
			}
			name, st, err := r.dp.AttachDisk(ctx, d)
			if err != nil {
				health = store.HealthStatusDegraded
				p.degrade("disk:"+d.ID, err.Error())
			} else {
				bdevName, drv = name, st
				p.repaired++
			}
		}

		if health == store.HealthStatusOnline && d.Type == store.DiskTypeNVMe {
			dd := *d
			dd.Handle = bdevName
			if h, err := r.dp.NVMeHealth(ctx, &dd); err == nil {
				smart = h
				if smart.CriticalWarning {
					health = store.HealthStatusDegraded
					p.degrade("disk:"+d.ID, "NVMe SMART critical warning")
				}
			}
		}

		_, err := r.store.MutateDisk(d.ID, func(cur *store.Disk) error {
			if !cur.Claimed {
				return store.ErrSkipWrite // released concurrently
			}
			changed := cur.Handle != bdevName || cur.DriverState != drv || cur.HealthStatus != health
			cur.Handle, cur.DriverState, cur.HealthStatus = bdevName, drv, health
			if smart != nil {
				// SMART counters change constantly; only persist them alongside a real change
				// or when the critical flag flips, to avoid a bbolt fsync per disk per pass.
				if cur.Smart.CriticalWarning != smart.CriticalWarning || changed || cur.Smart.PowerOnHours != smart.PowerOnHours {
					cur.Smart = *smart
					changed = true
				}
			}
			if !changed {
				return store.ErrSkipWrite
			}
			return nil
		})
		if err != nil {
			p.errs = append(p.errs, fmt.Sprintf("update disk %s: %v", d.ID, err))
		}
	}
}

// reconcilePools returns the set of pool IDs whose lvstore is loaded and usable.
func (r *Reconciler) reconcilePools(ctx context.Context, p *pass, examined bool) map[string]bool {
	online := map[string]bool{}
	pools, err := r.store.ListPools()
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list pools: %v", err))
		return online
	}
	if len(pools) == 0 {
		return online
	}
	bdevs, err := r.bdevSet(ctx)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list bdevs: %v", err))
		return online
	}
	raids, err := r.dp.Arrays(ctx)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list raid bdevs: %v", err))
		return online
	}
	lvstores, err := r.dp.ThinPools(ctx, "", "")
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list lvstores: %v", err))
		return online
	}
	disks, err := r.store.ListDisks()
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list disks: %v", err))
		return online
	}

	for _, pool := range pools {
		if pool.HealthStatus == store.HealthStatusReconciling && pool.ThinPoolUUID == "" {
			continue // being created by the API right now; it owns the RAID/lvstore setup
		}
		res := "pool:" + pool.ID
		health := store.HealthStatusOnline
		var reasons []string
		fail := func(h store.HealthStatus, reason string) {
			if h == store.HealthStatusFaulted || health == store.HealthStatusOnline {
				health = h
			}
			reasons = append(reasons, reason)
			p.degrade(res, reason)
		}

		// 1. RAID bdev (assembled by SPDK examine from its on-disk superblock): member states
		// and RAID-level health.
		ri := slices.IndexFunc(raids, func(rb dataplane.Array) bool { return rb.Name == pool.ArrayName })
		raidOnline := false
		members := store.PoolMembers(pool)
		var unreadable uint64
		var lost []store.LostExtent
		if ri >= 0 {
			v := r.viewRaid(ctx, pool, &raids[ri], raids, disks, bdevs)
			raidOnline, members = v.online, v.members
			for _, reason := range v.reasons {
				fail(v.health, reason)
			}
			if v.health != store.HealthStatusOnline && len(v.reasons) == 0 {
				fail(v.health, "RAID "+pool.ArrayName+" is "+string(v.health))
			}
			unreadable = raids[ri].UnreadableBytes
			lost = r.badBlocks(ctx, p, pool, members, unreadable, fail)
		} else {
			fail(store.HealthStatusUnavailable, fmt.Sprintf("array %s isn't assembled (%d/%d member disks present)",
				pool.ArrayName, r.presentMembers(pool, bdevs), len(pool.DiskIDs)))
		}

		// 2. lvstore.
		var lvs *dataplane.ThinPool
		for i := range lvstores {
			if lvstores[i].UUID == pool.ThinPoolUUID {
				lvs = &lvstores[i]
			}
		}
		switch {
		case lvs != nil || !raidOnline:
			// Missing RAID already reported; never inspect lvstores on an incomplete array.
		case pool.ThinPoolUUID == "":
			fail(store.HealthStatusFaulted, "pool has no thin pool UUID; its creation never completed")
		case !examined:
			fail(store.HealthStatusDegraded, "waiting for the data plane to load thin pool "+pool.ThinPoolName)
		case slices.ContainsFunc(lvstores, func(l dataplane.ThinPool) bool { return l.Name == pool.ThinPoolName }):
			fail(store.HealthStatusFaulted, fmt.Sprintf("thin pool %s has a different UUID than recorded (%s); it may have been re-initialised", pool.ThinPoolName, pool.ThinPoolUUID))
		default:
			fail(store.HealthStatusFaulted, fmt.Sprintf("thin pool %s (%s) not found on %s", pool.ThinPoolName, pool.ThinPoolUUID, pool.ArrayName))
		}

		var total, free, metaTotal, metaUsed uint64
		if lvs != nil {
			total, free = lvs.TotalBytes, lvs.FreeBytes
			metaTotal, metaUsed = lvs.MetadataBytes, lvs.MetadataUsedBytes
			if raidOnline {
				online[pool.ID] = true
				metaTotal = r.growThinMetadata(ctx, p, pool, lvs, fail)
			}
			if metaTotal > 0 && metaUsed*100 >= metaTotal*thinMetadataDegradedPercent {
				fail(store.HealthStatusDegraded, fmt.Sprintf("thin pool metadata is %d%% full; when full, writes to new space fail: free space (delete snapshots and volumes)",
					metaUsed*100/metaTotal))
			}
		}
		reasons = append(reasons, p.notes[pool.ID]...)
		msg := strings.Join(reasons, "; ")
		durability := poolDurability(pool, disks)
		_, err := r.store.MutatePool(pool.ID, func(cur *store.Pool) error {
			if !sameSlots(cur, pool) {
				// A disk was replaced or released while this pass looked at the pool: what it
				// concluded is about the old members. The next pass looks again.
				return store.ErrSkipWrite
			}
			changed := cur.HealthStatus != health || cur.StatusMessage != msg || cur.Durability != durability
			cur.Durability = durability
			if ri >= 0 && (cur.UnreadableBytes != unreadable || !slices.Equal(cur.LostData, lost)) {
				changed = true
				cur.UnreadableBytes, cur.LostData = unreadable, lost
			}
			if !membersEqual(store.PoolMembers(cur), members) || len(cur.Members) == 0 {
				changed = true
				store.SetPoolMembers(cur, members)
			}
			if lvs != nil && (cur.ThinMetadataBytes != metaTotal || cur.ThinMetadataUsedBytes != metaUsed) {
				changed = true
				cur.ThinMetadataBytes, cur.ThinMetadataUsedBytes = metaTotal, metaUsed
			}
			if lvs != nil && (cur.TotalBytes != total || cur.FreeBytes != free) {
				changed = true
				cur.TotalBytes, cur.FreeBytes = total, free
				cur.AllocatedBytes = 0
				if total >= free {
					cur.AllocatedBytes = total - free
				}
			}
			if !changed {
				return store.ErrSkipWrite
			}
			cur.HealthStatus, cur.StatusMessage = health, msg
			return nil
		})
		if err != nil {
			p.errs = append(p.errs, fmt.Sprintf("update pool %s: %v", pool.ID, err))
		}
	}
	return online
}

// badBlocks reports the bad blocks the data plane recorded on a pool's members (md's bad-block
// log: ranges a member couldn't read and couldn't rewrite from another copy). Where every copy
// has them the data is lost and the pool is DEGRADED, until the data is written again (md
// clears a range on a member that takes the write: restoring the volume does); the volumes and
// snapshots it held are named, and those volumes DEGRADED too. Where only some copies have
// them, the pool has a copy fewer there: a note, as replacing the disk restores it.
func (r *Reconciler) badBlocks(ctx context.Context, p *pass, pool *store.Pool, members []store.PoolMember, unreadable uint64,
	fail func(store.HealthStatus, string)) []store.LostExtent {
	if unreadable == 0 {
		for _, m := range members {
			if m.BadBlockBytes > 0 && m.State == store.MemberStateActive {
				p.notePool(pool.ID, fmt.Sprintf("%s has %s of bad blocks recorded (read from the other copy instead): replace it to restore full redundancy there",
					diskLabel(m.DiskID), store.HumanBytes(m.BadBlockBytes)))
			}
		}
		return nil
	}
	msg := fmt.Sprintf("%s of the pool can't be read from any disk (bad blocks recorded on every copy): the data there is lost, and reading it fails until it is written again",
		store.HumanBytes(unreadable))
	found, err := r.dp.LostData(ctx, pool.ArrayName)
	if err != nil {
		fail(store.HealthStatusDegraded, msg+"; what it held couldn't be worked out ("+err.Error()+"): restore the affected volumes from backups")
		return nil
	}
	lost, what := r.describeLost(p, found)
	if len(what) > 0 {
		msg += "; it held " + strings.Join(what, ", ")
	}
	fail(store.HealthStatusDegraded, msg+": restore the affected volumes from backups")
	return lost
}

// describeLost resolves lost data to the store's volumes and snapshots, marks those volumes for
// reconcileVolumes, and describes each thing lost (in volume web: 4.0 KiB at 1.2 GiB).
func (r *Reconciler) describeLost(p *pass, found []dataplane.LostExtent) ([]store.LostExtent, []string) {
	vols, _ := r.store.ListVolumes("")
	snaps, _ := r.store.ListSnapshots("", "")
	type item struct {
		what, note    string
		bytes, offset uint64
	}
	var order []string
	items := map[string]*item{}
	add := func(key, what, note string, e dataplane.LostExtent) {
		if items[key] == nil {
			items[key] = &item{what: what, note: note, offset: e.Offset}
			order = append(order, key)
		}
		items[key].bytes += e.Length
		items[key].offset = min(items[key].offset, e.Offset)
	}
	var lost []store.LostExtent
	for _, e := range found {
		l := store.LostExtent{Kind: e.Kind, Name: e.Name, OffsetBytes: e.Offset, LengthBytes: e.Length}
		switch e.Kind {
		case dataplane.LostVolume:
			if i := slices.IndexFunc(vols, func(v *store.Volume) bool { return v.UUID == e.UUID }); i >= 0 {
				v := vols[i]
				l.VolumeID = v.ID
				what := "volume " + v.Name
				if v.Deleted() {
					what = "deleted volume " + v.Name
				}
				add(v.ID, what, "", e)
				if p.lostVols == nil {
					p.lostVols = map[string]string{}
				}
				p.lostVols[v.ID] = "" // described below, once its extents are summed
			} else if i := slices.IndexFunc(snaps, func(s *store.Snapshot) bool { return s.UUID == e.UUID }); i >= 0 {
				l.SnapshotID = snaps[i].ID
				add(snaps[i].ID, "snapshot "+snaps[i].Name, "", e)
			} else {
				add("uuid:"+e.UUID, "a volume velod doesn't manage", e.UUID, e)
			}
		case dataplane.LostThinMetadata:
			add(e.Kind, "the thin pool's metadata", "the volumes' block mappings", e)
		case dataplane.LostPoolMetadata:
			add(e.Kind, "the on-pool configuration copy", "rewritten on the next change", e)
		case dataplane.LostLVMMetadata:
			add(e.Kind, "LVM's metadata", "the pool may not be found after a restart", e)
		case dataplane.LostUnlocated:
			add(e.Kind, "data whose place in the pool isn't worked out", "RAID0 over disks of different sizes", e)
		default:
			add("other:"+e.Name, e.Name, "", e)
		}
		lost = append(lost, l)
	}
	var what []string
	for _, k := range order {
		it := items[k]
		desc := it.what + " (" + store.HumanBytes(it.bytes)
		if _, isVol := p.lostVols[k]; isVol || strings.HasPrefix(it.what, "snapshot ") {
			desc += " from " + store.HumanBytes(it.offset)
		}
		if it.note != "" {
			desc += "; " + it.note
		}
		desc += ")"
		if _, isVol := p.lostVols[k]; isVol {
			p.lostVols[k] = fmt.Sprintf("%s of its data, from %s, can't be read from any disk of its pool: lost; reads there fail until it is written again; restore it from a backup",
				store.HumanBytes(it.bytes), store.HumanBytes(it.offset))
		}
		what = append(what, desc)
	}
	return lost, what
}

// thinMetadataDegradedPercent: a pool whose thin metadata is this full is DEGRADED (growing it
// starts at half full, so this means growing couldn't keep up).
const thinMetadataDegradedPercent = 90

// growThinMetadata grows the thin pool's metadata when it is filling (Plane.GrowMetadata) and
// returns its size afterwards; a pool that needed it and couldn't is reported.
func (r *Reconciler) growThinMetadata(ctx context.Context, p *pass, pool *store.Pool, lvs *dataplane.ThinPool, fail func(store.HealthStatus, string)) uint64 {
	if lvs.MetadataBytes == 0 {
		return 0
	}
	grown, err := r.dp.GrowMetadata(ctx, lvs.UUID)
	switch {
	case err != nil && lvs.MetadataUsedBytes*100 >= lvs.MetadataBytes*80:
		fail(store.HealthStatusDegraded, "growing the thin pool metadata failed: "+err.Error())
	case err != nil:
		p.notePool(pool.ID, "growing the thin pool metadata failed: "+err.Error())
	case grown > 0:
		p.repaired++
		r.logger.Warn("thin pool metadata grown", "pool", pool.Name, "from", lvs.MetadataBytes, "to", grown)
		return grown
	}
	return lvs.MetadataBytes
}

// poolDurability is the weakest durability of the pool's member disks.
func poolDurability(pool *store.Pool, disks []*store.Disk) store.Durability {
	out := store.DurabilityDurable
	for _, id := range pool.DiskIDs {
		i := slices.IndexFunc(disks, func(d *store.Disk) bool { return d.ID == id })
		if i < 0 {
			return store.DurabilityUnknown
		}
		switch disks[i].Durability() {
		case store.DurabilityAtRisk:
			return store.DurabilityAtRisk
		case store.DurabilityUnknown:
			out = store.DurabilityUnknown
		}
	}
	return out
}

// leftPool returns the pool a disk is bound to but no longer a member of, or nil: its slot
// failed, it was removed, replaced or dropped by a degraded start, or SPDK's running array no
// longer contains it (it failed since the last pass, before the pool was updated). A disk an
// UNAVAILABLE pool waits for is still a member: it must be attached so the RAID can assemble.
// So is a rebuild target, so an interrupted rebuild can restart.
func leftPool(d *store.Disk, pools []*store.Pool, raids []dataplane.Array) *store.Pool {
	if d.PoolID == "" {
		return nil
	}
	for _, p := range pools {
		if p.ID != d.PoolID {
			continue
		}
		if slices.Contains(p.DiskIDs, d.ID) {
			ri := slices.IndexFunc(raids, func(rb dataplane.Array) bool { return rb.Name == p.ArrayName })
			rebuilding := slices.ContainsFunc(store.PoolMembers(p), func(m store.PoolMember) bool {
				return m.DiskID == d.ID && m.State == store.MemberStateRebuilding
			})
			if ri < 0 || !strings.EqualFold(raids[ri].State, "online") || rebuilding || inRaid(raids, d.Handle) {
				return nil
			}
			return p // the running array lost it: it failed
		}
		for _, m := range store.PoolMembers(p) {
			if m.DiskID == d.ID && m.State == store.MemberStateMissing && p.HealthStatus == store.HealthStatusUnavailable {
				return nil
			}
		}
		return p
	}
	return nil
}

func (r *Reconciler) memberBdevs(pool *store.Pool, bdevs map[string]dataplane.Device) ([]string, bool) {
	var members []string
	for _, id := range pool.DiskIDs {
		d, err := r.store.GetDisk(id)
		if err != nil || d.Handle == "" {
			continue
		}
		if _, ok := bdevs[d.Handle]; ok {
			members = append(members, d.Handle)
		}
	}
	return members, len(members) == len(pool.DiskIDs) && len(members) > 0
}

func (r *Reconciler) presentMembers(pool *store.Pool, bdevs map[string]dataplane.Device) int {
	m, _ := r.memberBdevs(pool, bdevs)
	return len(m)
}

// reconcileVolumes returns the set of volume IDs whose lvol bdev is present.
func (r *Reconciler) reconcileVolumes(ctx context.Context, p *pass, poolOnline map[string]bool) map[string]*store.Volume {
	online := map[string]*store.Volume{}
	volumes, err := r.store.ListVolumes("")
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list volumes: %v", err))
		return online
	}
	if len(volumes) == 0 {
		return online
	}
	bdevs, err := r.bdevSet(ctx)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list bdevs: %v", err))
		return online
	}

	for _, vol := range volumes {
		res := "volume:" + vol.ID
		health, msg := store.HealthStatusOnline, ""
		_, found := bdevs[vol.UUID]

		switch {
		case !poolOnline[vol.PoolID]:
			health, msg = store.HealthStatusDegraded, "pool is not online"
			p.degraded = append(p.degraded, res)
		case !found:
			health = store.HealthStatusFaulted
			msg = fmt.Sprintf("volume %s (%s) is missing from its thin pool; not re-creating it because that would present an empty disk", vol.Handle, vol.UUID)
			p.degrade(res, msg)
		default:
			if vol.Device != vol.Handle {
				if _, ok := bdevs[vol.Device]; !ok {
					health, msg = store.HealthStatusDegraded, fmt.Sprintf("filter device %s is not present", vol.Device)
					p.degrade(res, msg)
				}
			}
			if health == store.HealthStatusOnline {
				online[vol.ID] = vol
			}
		}

		if lostMsg := p.lostVols[vol.ID]; lostMsg != "" && health == store.HealthStatusOnline {
			// Still served (online stays set): the rest of it reads fine.
			health, msg = store.HealthStatusDegraded, lostMsg
			p.degrade(res, msg)
		}

		if _, err := r.store.MutateVolume(vol.ID, func(cur *store.Volume) error {
			if cur.HealthStatus == health && cur.StatusMessage == msg {
				return store.ErrSkipWrite
			}
			cur.HealthStatus, cur.StatusMessage = health, msg
			return nil
		}); err != nil && !errors.Is(err, store.ErrNotFound) {
			p.errs = append(p.errs, fmt.Sprintf("update volume %s: %v", vol.ID, err))
		}
	}
	return online
}

func (r *Reconciler) reconcileExports(ctx context.Context, p *pass, volOnline map[string]*store.Volume) {
	exports, err := r.store.ListExports("", "")
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list exports: %v", err))
		return
	}
	for _, exp := range exports {
		res := "export:" + exp.ID
		health, msg := store.HealthStatusOnline, ""
		if exp.Disabled {
			health, msg = store.HealthStatusOffline, "disabled: "+exp.DisabledReason
			if err := dataplane.RemoveExport(ctx, r.dp, exp); err != nil {
				msg += "; " + err.Error()
			}
			p.degrade(res, msg)
		} else if vol, ok := volOnline[exp.VolumeID]; !ok {
			health, msg = store.HealthStatusDegraded, "backing volume is not online"
			p.degraded = append(p.degraded, res)
		} else if err := dataplane.EnsureExport(ctx, r.dp, r.store, exp, vol); err != nil {
			health, msg = store.HealthStatusDegraded, err.Error()
			p.degrade(res, msg)
		}
		if _, err := r.store.MutateExport(exp.ID, func(cur *store.Export) error {
			if cur.HealthStatus == health && cur.StatusMessage == msg {
				return store.ErrSkipWrite
			}
			cur.HealthStatus, cur.StatusMessage = health, msg
			return nil
		}); err != nil && !errors.Is(err, store.ErrNotFound) {
			p.errs = append(p.errs, fmt.Sprintf("update export %s: %v", exp.ID, err))
		}
	}
}

func (r *Reconciler) finishPass(repaired uint64, degraded []string, lastErr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.InProgress = false
	r.state.LastReconciledAt = time.Now().UTC()
	r.state.DriftRepairedCount += repaired
	slices.Sort(degraded)
	r.state.DegradedResources = slices.Compact(degraded)
	r.state.LastError = lastErr
}
