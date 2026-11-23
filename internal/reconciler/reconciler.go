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
	// Unmanaged lists what spdk_tgt holds that no store record accounts for (P1-5).
	Unmanaged []UnmanagedObject `json:"unmanaged_objects,omitempty"`
	// SPDKInstance is the spdk_tgt process the last completed pass reconciled
	// (hardware.SPDKInstance; "" when unknown). When the running one differs, the data plane
	// was restarted after that pass and isn't restored yet (docs/review-history.md P2-11).
	SPDKInstance string `json:"spdk_instance,omitempty"`
}

// Reconciler continuously converges observed state in Linux sysfs and spdk_tgt
// toward the desired state persisted in bbolt.
//
// Safety rules (see TODO C1):
//   - It never creates RAID bdevs, lvstores or lvols. Pools are created atomically by the
//     API with an on-disk RAID superblock, so after a restart SPDK examine re-assembles the
//     array and loads its lvstore; anything still missing is reported, never re-created,
//     because re-creating it would hand hosts blank storage under the old identity.
//   - It only writes store records whose observed status actually changed, and only the
//     status fields it owns, via atomic Mutate* calls.
type Reconciler struct {
	store  store.Store
	dp     dataplane.Plane
	hw     *hardware.Manager
	logger *slog.Logger

	passMu sync.Mutex // serialises passes
	meta   *metaSync  // on-pool metadata copies (nil: disabled)
	// sbReader and discover replace reading RAID superblocks from disks and the hardware
	// scan (tests).
	sbReader func(*store.Disk) (*hardware.RaidSuperblock, error)
	discover func() ([]*store.Disk, error)

	// cacheRefused: disks whose drive refused to turn its write cache off since velod started.
	// They are retried once per start, not every pass (the refusal is recorded on the disk and
	// reported in the durability warnings).
	cacheRefused map[string]bool

	now func() time.Time // replaces time.Now (tests)

	// instance identifies the running spdk_tgt (hardware.SPDKInstance; replaceable in tests).
	instance func() string

	// unmanagedSeen: when each unaccounted SPDK object was first seen (reported after
	// unmanagedGrace); unmanagedLogged: those already logged.
	unmanagedSeen   map[string]time.Time
	unmanagedLogged map[string]bool
	unmanagedGrace  *time.Duration // nil: defaultUnmanagedGrace

	mu    sync.Mutex
	state State
}

// New creates a new Reconciler instance.
func New(st store.Store, dp dataplane.Plane, hwMgr *hardware.Manager, logger *slog.Logger) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{
		store:    st,
		dp:       dp,
		hw:       hwMgr,
		logger:   logger,
		instance: hardware.SPDKInstance,
	}
}

// SPDKInstance identifies the running spdk_tgt process ("" when unknown).
func (r *Reconciler) SPDKInstance() string {
	if r.instance == nil {
		return ""
	}
	return r.instance()
}

// SetSPDKInstance replaces how the running spdk_tgt is identified (tests).
func (r *Reconciler) SetSPDKInstance(f func() string) {
	r.passMu.Lock()
	defer r.passMu.Unlock()
	r.instance = f
}

// Restoring reports whether spdk_tgt was restarted since the last completed pass, so what
// velod built in it (pools' RAIDs and volume stores, exports) isn't all back yet.
func (r *Reconciler) Restoring() bool {
	cur := r.SPDKInstance()
	r.mu.Lock()
	defer r.mu.Unlock()
	return cur != "" && cur != r.state.SPDKInstance
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
	// A restarted spdk_tgt has lost everything velod built in it: restore at once rather than
	// at the next tick (docs/review-history.md P2-11).
	watch := time.NewTicker(2 * time.Second)
	defer watch.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = r.ReconcileOnce(ctx)
		case <-watch.C:
			if r.Restoring() {
				r.logger.Warn("spdk_tgt was restarted: restoring pools, volumes and exports now", "instance", r.SPDKInstance())
				_, _ = r.ReconcileOnce(ctx)
				ticker.Reset(interval)
			}
		}
	}
}

// pass accumulates the outcome of one reconciliation pass.
type pass struct {
	repaired uint64
	degraded []string
	errs     []string
	notes    map[string][]string // pool ID -> extra reasons for its status message

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
	// The spdk_tgt this pass reconciles, read before talking to it: a restart during the pass
	// then shows as a different instance afterwards.
	instance := r.SPDKInstance()
	discovered := r.syncHardware(p)
	r.enforceWriteCache(p)

	if _, err := r.dp.Version(ctx); err != nil {
		r.finishPass(0, []string{"spdk_tgt"}, fmt.Sprintf("spdk_tgt unreachable: %v", err))
		return r.State(), err
	}

	if r.relinkMovedMembers(ctx, p, discovered) {
		r.enforceWriteCache(p) // the relinked disks inherit their policy: apply it before SPDK writes
	}

	r.reconcileDisks(ctx, p)
	r.nvmeWriteCache(ctx, p)
	r.applySettings(ctx, p)

	// Let SPDK finish examining every base bdev (RAID superblock assembly, lvstore load)
	// before anything below concludes that on-disk metadata is missing.
	examined := true
	if err := r.dp.Settle(ctx); err != nil {
		examined = false
		p.errs = append(p.errs, fmt.Sprintf("bdev_wait_for_examine: %v", err))
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
	r.mu.Lock()
	r.state.SPDKInstance = instance
	r.mu.Unlock()
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
		bdevName, drv := d.SPDKBdevName, d.DriverState
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
			dd.SPDKBdevName = bdevName
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
			changed := cur.SPDKBdevName != bdevName || cur.DriverState != drv || cur.HealthStatus != health
			cur.SPDKBdevName, cur.DriverState, cur.HealthStatus = bdevName, drv, health
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
		if pool.HealthStatus == store.HealthStatusReconciling && pool.LVSUUID == "" {
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
		ri := slices.IndexFunc(raids, func(rb dataplane.Array) bool { return rb.Name == pool.BaseBdevName })
		raidOnline := false
		members := store.PoolMembers(pool)
		if ri >= 0 {
			v := r.viewRaid(ctx, pool, &raids[ri], raids, disks, bdevs)
			raidOnline, members = v.online, v.members
			for _, reason := range v.reasons {
				fail(v.health, reason)
			}
			if v.health != store.HealthStatusOnline && len(v.reasons) == 0 {
				fail(v.health, "RAID "+pool.BaseBdevName+" is "+string(v.health))
			}
		} else {
			fail(store.HealthStatusUnavailable, fmt.Sprintf("RAID %s not assembled by SPDK examine (%d/%d member bdevs present)",
				pool.BaseBdevName, r.presentMembers(pool, bdevs), len(pool.DiskIDs)))
		}

		// 2. lvstore.
		var lvs *dataplane.ThinPool
		for i := range lvstores {
			if lvstores[i].UUID == pool.LVSUUID {
				lvs = &lvstores[i]
			}
		}
		switch {
		case lvs != nil || !raidOnline:
			// Missing RAID already reported; never inspect lvstores on an incomplete array.
		case pool.LVSUUID == "":
			fail(store.HealthStatusFaulted, "pool has no lvstore UUID; its creation never completed")
		case !examined:
			fail(store.HealthStatusDegraded, "waiting for SPDK examine to load lvstore "+pool.LVSName)
		case slices.ContainsFunc(lvstores, func(l dataplane.ThinPool) bool { return l.Name == pool.LVSName }):
			fail(store.HealthStatusFaulted, fmt.Sprintf("lvstore %s has a different UUID than recorded (%s); it may have been re-initialised", pool.LVSName, pool.LVSUUID))
		default:
			fail(store.HealthStatusFaulted, fmt.Sprintf("lvstore %s (%s) not found on %s", pool.LVSName, pool.LVSUUID, pool.BaseBdevName))
		}

		var total, free uint64
		if lvs != nil {
			total, free = lvs.TotalBytes, lvs.FreeBytes
			if raidOnline {
				online[pool.ID] = true
			}
		}
		reasons = append(reasons, p.notes[pool.ID]...)
		msg := strings.Join(reasons, "; ")
		durability := poolDurability(pool, disks)
		_, err := r.store.MutatePool(pool.ID, func(cur *store.Pool) error {
			changed := cur.HealthStatus != health || cur.StatusMessage != msg || cur.Durability != durability
			cur.Durability = durability
			if !membersEqual(store.PoolMembers(cur), members) || len(cur.Members) == 0 {
				changed = true
				store.SetPoolMembers(cur, members)
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
			ri := slices.IndexFunc(raids, func(rb dataplane.Array) bool { return rb.Name == p.BaseBdevName })
			rebuilding := slices.ContainsFunc(store.PoolMembers(p), func(m store.PoolMember) bool {
				return m.DiskID == d.ID && m.State == store.MemberStateRebuilding
			})
			if ri < 0 || !strings.EqualFold(raids[ri].State, "online") || rebuilding || inRaid(raids, d.SPDKBdevName) {
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
		if err != nil || d.SPDKBdevName == "" {
			continue
		}
		if _, ok := bdevs[d.SPDKBdevName]; ok {
			members = append(members, d.SPDKBdevName)
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
		info, found := bdevs[vol.UUID]

		switch {
		case !poolOnline[vol.PoolID]:
			health, msg = store.HealthStatusDegraded, "pool is not online"
			p.degraded = append(p.degraded, res)
		case !found:
			health = store.HealthStatusFaulted
			msg = fmt.Sprintf("lvol %s (%s) is missing from its lvstore; not re-creating it because that would present an empty disk", vol.LvolBdevName, vol.UUID)
			p.degrade(res, msg)
		default:
			if vol.EffectiveBdevName != vol.LvolBdevName {
				if _, ok := bdevs[vol.EffectiveBdevName]; !ok {
					health, msg = store.HealthStatusDegraded, fmt.Sprintf("filter bdev %s is not present", vol.EffectiveBdevName)
					p.degrade(res, msg)
				}
			}
			if health == store.HealthStatusOnline {
				online[vol.ID] = vol
				q := vol.QosPolicy
				have := info.QoS
				if eff, ok := bdevs[vol.EffectiveBdevName]; ok {
					have = eff.QoS
				}
				if have != q {
					if err := r.dp.SetQoS(ctx, vol.EffectiveBdevName, q); err != nil {
						p.errs = append(p.errs, fmt.Sprintf("%s: apply QoS: %v", res, err))
					} else {
						p.repaired++
					}
				}
			}
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
