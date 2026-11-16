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
	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

// State captures the runtime status of the desired-state reconciler.
type State struct {
	InProgress         bool      `json:"in_progress"`
	LastReconciledAt   time.Time `json:"last_reconciled_at"`
	DriftRepairedCount uint64    `json:"drift_repaired_count"`
	DegradedResources  []string  `json:"degraded_resources"`
	LastError          string    `json:"last_error,omitempty"`
}

// Reconciler continuously converges observed state in Linux sysfs and spdk_tgt
// toward the desired state persisted in bbolt.
//
// Safety rules (see TODO C1):
//   - It never initialises an lvstore for a pool that has ever had one (LVSUUID set).
//   - It never creates an lvol for a volume record; a missing lvol is reported FAULTED,
//     because re-creating it would hand hosts a blank disk under the old identity.
//   - It never re-creates a superblock RAID; SPDK examine assembles those itself.
//   - It only writes store records whose observed status actually changed, and only the
//     status fields it owns, via atomic Mutate* calls.
type Reconciler struct {
	store  store.Store
	spdk   *spdk.Client
	hw     *hardware.Manager
	logger *slog.Logger

	passMu sync.Mutex // serialises passes

	mu    sync.Mutex
	state State
}

// New creates a new Reconciler instance.
func New(st store.Store, spdkClient *spdk.Client, hwMgr *hardware.Manager, logger *slog.Logger) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{
		store:  st,
		spdk:   spdkClient,
		hw:     hwMgr,
		logger: logger,
	}
}

// State returns a snapshot of the reconciler's current status.
func (r *Reconciler) State() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := r.state
	cp.DegradedResources = slices.Clone(r.state.DegradedResources)
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
	r.syncHardware(p)

	if _, err := r.spdk.GetVersion(ctx); err != nil {
		r.finishPass(0, []string{"spdk_tgt"}, fmt.Sprintf("spdk_tgt unreachable: %v", err))
		return r.State(), err
	}

	r.reconcileDisks(ctx, p)

	// Let SPDK finish examining every base bdev (RAID superblock assembly, lvstore load)
	// before anything below concludes that on-disk metadata is missing.
	examined := true
	if err := r.spdk.BdevWaitForExamine(ctx); err != nil {
		examined = false
		p.errs = append(p.errs, fmt.Sprintf("bdev_wait_for_examine: %v", err))
	}

	poolOnline := r.reconcilePools(ctx, p, examined)
	volOnline := r.reconcileVolumes(ctx, p, poolOnline)
	r.reconcileExports(ctx, p, volOnline)

	lastErr := strings.Join(p.errs, "; ")
	r.finishPass(p.repaired, p.degraded, lastErr)
	if len(p.errs) > 0 {
		return r.State(), fmt.Errorf("%s", lastErr)
	}
	return r.State(), nil
}

func (r *Reconciler) syncHardware(p *pass) {
	if r.hw == nil {
		return
	}
	if err := r.hw.RefreshBootGuard(); err != nil {
		p.errs = append(p.errs, fmt.Sprintf("boot guard: %v", err))
	}
	discovered, err := r.hw.DiscoverDisks()
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("hardware scan: %v", err))
		return
	}
	for _, d := range discovered {
		if err := r.store.SyncDiscoveredDisk(d); err != nil {
			p.errs = append(p.errs, fmt.Sprintf("sync disk %s: %v", d.ID, err))
		}
	}
}

func (r *Reconciler) bdevSet(ctx context.Context) (map[string]spdk.BdevInfo, error) {
	bdevs, err := r.spdk.BdevGetBdevs(ctx, "")
	if err != nil {
		return nil, err
	}
	m := make(map[string]spdk.BdevInfo, len(bdevs))
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
	for _, d := range disks {
		if !d.Claimed || d.IsBootDevice {
			continue
		}
		bdevName, drv := d.SPDKBdevName, d.DriverState
		health := store.HealthStatusOnline
		var smart *store.SmartHealth

		if _, ok := bdevs[bdevName]; !ok || bdevName == "" {
			name, st, err := dataplane.AttachDisk(ctx, r.spdk, r.hw, d)
			if err != nil {
				health = store.HealthStatusDegraded
				p.degrade("disk:"+d.ID, err.Error())
			} else {
				bdevName, drv = name, st
				p.repaired++
			}
		}

		if health == store.HealthStatusOnline && d.Type == store.DiskTypeNVMe {
			if h, err := r.spdk.BdevNvmeGetControllerHealthInfo(ctx, dataplane.NVMeControllerFromBdev(bdevName)); err == nil {
				smart = &store.SmartHealth{
					TemperatureCelsius:             h.TemperatureCelsius,
					AvailableSparePercent:          h.AvailableSpare,
					AvailableSpareThresholdPercent: h.AvailableSpareThreshold,
					PercentageUsed:                 h.PercentageUsed,
					DataUnitsRead:                  h.DataUnitsRead,
					DataUnitsWritten:               h.DataUnitsWritten,
					MediaErrors:                    h.MediaErrors,
					PowerOnHours:                   h.PowerOnHours,
					UnsafeShutdowns:                h.UnsafeShutdowns,
					CriticalWarning:                h.CriticalWarning != 0,
				}
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
	raids, err := r.spdk.BdevRaidGetBdevs(ctx, "all")
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list raid bdevs: %v", err))
		return online
	}
	lvstores, err := r.spdk.BdevLvolGetLvstores(ctx, "", "")
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list lvstores: %v", err))
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

		// 1. RAID bdev.
		ri := slices.IndexFunc(raids, func(rb spdk.RaidBdevInfo) bool { return rb.Name == pool.BaseBdevName })
		raidOnline := false
		switch {
		case ri >= 0 && strings.EqualFold(raids[ri].State, "online"):
			raidOnline = true
		case ri >= 0:
			fail(store.HealthStatusDegraded, fmt.Sprintf("RAID %s is %s (%d/%d members present)",
				pool.BaseBdevName, raids[ri].State, raids[ri].NumBaseBdevsDiscovered, raids[ri].NumBaseBdevs))
		case pool.RaidSuperblock:
			fail(store.HealthStatusDegraded, fmt.Sprintf("RAID %s not assembled by SPDK examine (%d/%d member bdevs present)",
				pool.BaseBdevName, r.presentMembers(pool, bdevs), len(pool.DiskIDs)))
		default:
			// Legacy array without superblock: SPDK cannot assemble it, so re-create it with the
			// exact original geometry and member order. superblock=false is mandatory here.
			members, ok := r.memberBdevs(pool, bdevs)
			if !ok {
				fail(store.HealthStatusDegraded, fmt.Sprintf("waiting for member disk SPDK bdevs (%d/%d ready)", len(members), len(pool.DiskIDs)))
				break
			}
			if err := r.spdk.BdevRaidCreate(ctx, pool.BaseBdevName, string(pool.RaidLevel), pool.StripSizeKB, members, false); err != nil {
				fail(store.HealthStatusDegraded, fmt.Sprintf("re-create legacy RAID %s: %v", pool.BaseBdevName, err))
				break
			}
			p.repaired++
			raidOnline = true
			// The re-created RAID must be examined before we look for its lvstore.
			if err := r.spdk.BdevWaitForExamine(ctx); err != nil {
				examined = false
			}
			if fresh, err := r.spdk.BdevLvolGetLvstores(ctx, "", ""); err == nil {
				lvstores = fresh
			} else {
				examined = false
			}
			if fresh, err := r.bdevSet(ctx); err == nil {
				bdevs = fresh
			}
		}
		if raidOnline && pool.EffectiveBdevName != pool.BaseBdevName {
			if _, ok := bdevs[pool.EffectiveBdevName]; !ok {
				raidOnline = false
				fail(store.HealthStatusDegraded, fmt.Sprintf("filter bdev %s is not present", pool.EffectiveBdevName))
			}
		}

		// 2. lvstore.
		var lvs *spdk.LvolStoreInfo
		for i := range lvstores {
			if (pool.LVSUUID != "" && lvstores[i].UUID == pool.LVSUUID) || (pool.LVSUUID == "" && lvstores[i].Name == pool.LVSName) {
				lvs = &lvstores[i]
			}
		}
		newLVSUUID := pool.LVSUUID
		switch {
		case lvs != nil:
		case !raidOnline:
			// Already reported; never touch lvstores on an incomplete array.
		case pool.LVSUUID != "":
			if slices.ContainsFunc(lvstores, func(l spdk.LvolStoreInfo) bool { return l.Name == pool.LVSName }) {
				fail(store.HealthStatusFaulted, fmt.Sprintf("lvstore %s has a different UUID than recorded (%s); it may have been re-initialised", pool.LVSName, pool.LVSUUID))
			} else {
				fail(store.HealthStatusFaulted, fmt.Sprintf("lvstore %s (%s) not found on %s; refusing to re-initialise it because that would destroy all volumes",
					pool.LVSName, pool.LVSUUID, pool.EffectiveBdevName))
			}
		case !examined:
			fail(store.HealthStatusDegraded, "waiting for SPDK examine to complete before initialising lvstore")
		default:
			// The pool was never successfully initialised (no lvstore UUID ever recorded) and
			// examine found nothing on the bdev: initialising is safe.
			uuid, err := r.spdk.BdevLvolCreateLvstore(ctx, pool.EffectiveBdevName, pool.LVSName, pool.ClusterSizeBytes, "")
			if err != nil {
				fail(store.HealthStatusDegraded, fmt.Sprintf("initialise lvstore %s: %v", pool.LVSName, err))
				break
			}
			p.repaired++
			newLVSUUID = uuid
			if fresh, err := r.spdk.BdevLvolGetLvstores(ctx, "", uuid); err == nil && len(fresh) > 0 {
				lvs = &fresh[0]
			}
		}

		var total, free uint64
		if lvs != nil {
			newLVSUUID = lvs.UUID
			cs := uint64(lvs.ClusterSize)
			if cs == 0 {
				cs = uint64(pool.ClusterSizeBytes)
			}
			total, free = lvs.TotalDataClusters*cs, lvs.FreeClusters*cs
			if raidOnline {
				online[pool.ID] = true
			}
		}
		msg := strings.Join(reasons, "; ")
		_, err := r.store.MutatePool(pool.ID, func(cur *store.Pool) error {
			changed := cur.HealthStatus != health || cur.StatusMessage != msg || cur.LVSUUID != newLVSUUID
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
			cur.HealthStatus, cur.StatusMessage, cur.LVSUUID = health, msg, newLVSUUID
			return nil
		})
		if err != nil {
			p.errs = append(p.errs, fmt.Sprintf("update pool %s: %v", pool.ID, err))
		}
	}
	return online
}

func (r *Reconciler) memberBdevs(pool *store.Pool, bdevs map[string]spdk.BdevInfo) ([]string, bool) {
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

func (r *Reconciler) presentMembers(pool *store.Pool, bdevs map[string]spdk.BdevInfo) int {
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
		uuid := vol.UUID
		var info spdk.BdevInfo
		found := false
		if vol.UUID != "" {
			info, found = bdevs[vol.UUID]
		} else {
			info, found = bdevs[vol.LvolBdevName]
		}

		switch {
		case !poolOnline[vol.PoolID]:
			health, msg = store.HealthStatusDegraded, "pool is not online"
			p.degraded = append(p.degraded, res)
		case !found:
			health = store.HealthStatusFaulted
			msg = fmt.Sprintf("lvol %s (%s) is missing from its lvstore; not re-creating it because that would present an empty disk", vol.LvolBdevName, vol.UUID)
			p.degrade(res, msg)
		default:
			if uuid == "" {
				uuid = info.UUID // backfill records created before UUIDs were tracked
			}
			if vol.EffectiveBdevName != vol.LvolBdevName {
				if _, ok := bdevs[vol.EffectiveBdevName]; !ok {
					health, msg = store.HealthStatusDegraded, fmt.Sprintf("filter bdev %s is not present", vol.EffectiveBdevName)
					p.degrade(res, msg)
				}
			}
			if health == store.HealthStatusOnline {
				online[vol.ID] = vol
				q := vol.QosPolicy
				want := spdk.BdevRateLimits{RwIosPerSec: q.RwIosPerSec, RwMbytesPerSec: q.RwMbytesPerSec, RMbytesPerSec: q.RMbytesPerSec, WMbytesPerSec: q.WMbytesPerSec}
				have := info.AssignedRateLimits
				if eff, ok := bdevs[vol.EffectiveBdevName]; ok {
					have = eff.AssignedRateLimits
				}
				if have != want {
					if err := r.spdk.BdevSetQoSLimit(ctx, vol.EffectiveBdevName, q.RwIosPerSec, q.RwMbytesPerSec, q.RMbytesPerSec, q.WMbytesPerSec); err != nil {
						p.errs = append(p.errs, fmt.Sprintf("%s: apply QoS: %v", res, err))
					} else {
						p.repaired++
					}
				}
			}
		}

		if _, err := r.store.MutateVolume(vol.ID, func(cur *store.Volume) error {
			if cur.HealthStatus == health && cur.StatusMessage == msg && cur.UUID == uuid {
				return store.ErrSkipWrite
			}
			cur.HealthStatus, cur.StatusMessage, cur.UUID = health, msg, uuid
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
		if vol, ok := volOnline[exp.VolumeID]; !ok {
			health, msg = store.HealthStatusDegraded, "backing volume is not online"
			p.degraded = append(p.degraded, res)
		} else if err := dataplane.EnsureExport(ctx, r.spdk, r.store, exp, vol); err != nil {
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
