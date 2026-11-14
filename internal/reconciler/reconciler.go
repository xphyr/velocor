package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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
type Reconciler struct {
	store  store.Store
	spdk   *spdk.Client
	hw     *hardware.Manager
	logger *slog.Logger

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

// ReconcileOnce executes a full topological reconciliation pass across Disks, Pools,
// Volumes, and SAN Exports.
func (r *Reconciler) ReconcileOnce(ctx context.Context) (State, error) {
	r.mu.Lock()
	if r.state.InProgress {
		st := r.state
		r.mu.Unlock()
		return st, nil
	}
	r.state.InProgress = true
	r.mu.Unlock()

	var repaired uint64
	var degraded []string
	var errs []string

	// 1. Discover physical disks from sysfs if HardwareManager is available.
	if r.hw != nil {
		_ = r.hw.RefreshBootGuard()
		if discovered, err := r.hw.DiscoverDisks(); err == nil {
			for _, d := range discovered {
				if existing, err := r.store.GetDisk(d.ID); err == nil {
					// Preserve user claim and SPDK bdev metadata across hardware scans.
					if existing.Claimed && !d.IsBootDevice {
						d.Claimed = true
						d.DriverState = existing.DriverState
						d.SPDKBdevName = existing.SPDKBdevName
						d.PoolID = existing.PoolID
					}
				}
				_ = r.store.SyncDiscoveredDisk(d)
			}
		} else {
			errs = append(errs, fmt.Sprintf("hardware scan: %v", err))
		}
	}

	// Check SPDK reachability before attempting data-plane RPCs.
	if _, err := r.spdk.GetVersion(ctx); err != nil {
		r.finishPass(0, []string{"spdk_tgt"}, fmt.Sprintf("spdk_tgt unreachable: %v", err))
		return r.State(), err
	}

	bdevs, err := r.spdk.BdevGetBdevs(ctx, "")
	if err != nil {
		r.finishPass(0, []string{"spdk_bdevs"}, err.Error())
		return r.State(), err
	}
	bdevMap := make(map[string]spdk.BdevInfo, len(bdevs))
	for _, b := range bdevs {
		bdevMap[b.Name] = b
		for _, alias := range b.Aliases {
			bdevMap[alias] = b
		}
	}

	// 2. Reconcile Claimed Disks.
	disks, err := r.store.ListDisks()
	if err != nil {
		errs = append(errs, fmt.Sprintf("list disks: %v", err))
	}
	for _, d := range disks {
		if !d.Claimed || d.IsBootDevice {
			continue
		}
		if _, exists := bdevMap[d.SPDKBdevName]; !exists || d.SPDKBdevName == "" {
			switch d.Type {
			case store.DiskTypeNVMe:
				if r.hw != nil && d.PCIAddress != "" {
					if drvState, err := r.hw.BindNVMeToUserspace(d.PCIAddress, ""); err == nil {
						d.DriverState = drvState
					}
				}
				ctrlName := "Nvme_" + strings.ReplaceAll(strings.ReplaceAll(d.PCIAddress, ":", "_"), ".", "_")
				attached, err := r.spdk.BdevNvmeAttachController(ctx, ctrlName, "PCIe", d.PCIAddress)
				if err != nil || len(attached) == 0 {
					d.HealthStatus = store.HealthStatusDegraded
					degraded = append(degraded, "disk:"+d.ID)
					if err != nil {
						errs = append(errs, fmt.Sprintf("attach nvme %s: %v", d.ID, err))
					}
				} else {
					d.SPDKBdevName = attached[0]
					d.HealthStatus = store.HealthStatusOnline
					bdevMap[d.SPDKBdevName] = spdk.BdevInfo{Name: d.SPDKBdevName}
					repaired++
				}
			case store.DiskTypeSSD:
				bdevName := d.SPDKBdevName
				if bdevName == "" {
					bdevName = "uring_" + d.Name
				}
				created, err := r.spdk.BdevUringCreate(ctx, bdevName, d.DevicePath, d.BlockSize)
				if err != nil {
					d.HealthStatus = store.HealthStatusDegraded
					degraded = append(degraded, "disk:"+d.ID)
					errs = append(errs, fmt.Sprintf("create uring %s: %v", d.ID, err))
				} else {
					d.SPDKBdevName = created
					d.DriverState = store.DiskDriverStateUringClaimed
					d.HealthStatus = store.HealthStatusOnline
					bdevMap[d.SPDKBdevName] = spdk.BdevInfo{Name: d.SPDKBdevName}
					repaired++
				}
			}
		}

		// Refresh NVMe SMART health if controller is attached.
		if d.Type == store.DiskTypeNVMe && d.SPDKBdevName != "" {
			ctrlName := strings.TrimSuffix(d.SPDKBdevName, "n1")
			if smart, err := r.spdk.BdevNvmeGetControllerHealthInfo(ctx, ctrlName); err == nil {
				d.Smart = store.SmartHealth{
					TemperatureCelsius:             smart.TemperatureCelsius,
					AvailableSparePercent:          smart.AvailableSpare,
					AvailableSpareThresholdPercent: smart.AvailableSpareThreshold,
					PercentageUsed:                 smart.PercentageUsed,
					DataUnitsRead:                  smart.DataUnitsRead,
					DataUnitsWritten:               smart.DataUnitsWritten,
					MediaErrors:                    smart.MediaErrors,
					PowerOnHours:                   smart.PowerOnHours,
					UnsafeShutdowns:                smart.UnsafeShutdowns,
					CriticalWarning:                smart.CriticalWarning != 0,
				}
				if d.Smart.CriticalWarning {
					d.HealthStatus = store.HealthStatusDegraded
					degraded = append(degraded, "disk:"+d.ID)
				}
			}
		}
		_ = r.store.UpsertDisk(d)
	}

	// 3. Reconcile Pools (bdev_raid + lvol_store).
	raidBdevs, _ := r.spdk.BdevRaidGetBdevs(ctx, "all")
	raidMap := make(map[string]spdk.RaidBdevInfo, len(raidBdevs))
	for _, rb := range raidBdevs {
		raidMap[rb.Name] = rb
	}

	lvstores, _ := r.spdk.BdevLvolGetLvstores(ctx, "", "")
	lvsMap := make(map[string]spdk.LvolStoreInfo, len(lvstores))
	for _, lvs := range lvstores {
		lvsMap[lvs.Name] = lvs
	}

	pools, err := r.store.ListPools()
	if err != nil {
		errs = append(errs, fmt.Sprintf("list pools: %v", err))
	}
	for _, pool := range pools {
		var poolReasons []string
		if _, exists := raidMap[pool.BaseBdevName]; !exists {
			var baseBdevs []string
			for _, diskID := range pool.DiskIDs {
				if d, err := r.store.GetDisk(diskID); err == nil && d.SPDKBdevName != "" {
					baseBdevs = append(baseBdevs, d.SPDKBdevName)
				}
			}
			if len(baseBdevs) == len(pool.DiskIDs) && len(baseBdevs) > 0 {
				if err := r.spdk.BdevRaidCreate(ctx, pool.BaseBdevName, string(pool.RaidLevel), pool.StripSizeKB, baseBdevs, false); err != nil {
					reason := fmt.Sprintf("create raid %s: %v", pool.BaseBdevName, err)
					pool.HealthStatus = store.HealthStatusDegraded
					poolReasons = append(poolReasons, reason)
					degraded = append(degraded, "pool:"+pool.ID)
					errs = append(errs, reason)
				} else {
					repaired++
				}
			} else {
				reason := fmt.Sprintf("waiting for member disk SPDK bdevs (%d/%d ready)", len(baseBdevs), len(pool.DiskIDs))
				pool.HealthStatus = store.HealthStatusDegraded
				poolReasons = append(poolReasons, reason)
				degraded = append(degraded, "pool:"+pool.ID)
				errs = append(errs, fmt.Sprintf("pool %s: %s", pool.ID, reason))
			}
		}

		lvsInfo, lvsExists := lvsMap[pool.LVSName]
		if !lvsExists {
			uuid, err := r.spdk.BdevLvolCreateLvstore(ctx, pool.EffectiveBdevName, pool.LVSName, pool.ClusterSizeBytes, "")
			if err != nil {
				reason := fmt.Sprintf("create lvstore %s: %v", pool.LVSName, err)
				pool.HealthStatus = store.HealthStatusDegraded
				poolReasons = append(poolReasons, reason)
				degraded = append(degraded, "pool:"+pool.ID)
				errs = append(errs, reason)
			} else {
				pool.LVSUUID = uuid
				pool.HealthStatus = store.HealthStatusOnline
				repaired++
				if stores, err := r.spdk.BdevLvolGetLvstores(ctx, pool.LVSName, ""); err == nil && len(stores) > 0 {
					lvsInfo = stores[0]
					lvsExists = true
				}
			}
		}
		if lvsExists {
			clusterSz := uint64(lvsInfo.ClusterSize)
			if clusterSz == 0 {
				clusterSz = uint64(pool.ClusterSizeBytes)
			}
			pool.LVSUUID = lvsInfo.UUID
			pool.TotalBytes = lvsInfo.TotalDataClusters * clusterSz
			pool.FreeBytes = lvsInfo.FreeClusters * clusterSz
			if pool.TotalBytes >= pool.FreeBytes {
				pool.AllocatedBytes = pool.TotalBytes - pool.FreeBytes
			}
			pool.HealthStatus = store.HealthStatusOnline
		}
		if pool.HealthStatus == store.HealthStatusOnline {
			pool.StatusMessage = ""
		} else {
			pool.StatusMessage = strings.Join(poolReasons, "; ")
		}
		_ = r.store.UpdatePool(pool)
	}

	// 4. Reconcile Volumes & QoS policies.
	volumes, err := r.store.ListVolumes("")
	if err != nil {
		errs = append(errs, fmt.Sprintf("list volumes: %v", err))
	}
	for _, vol := range volumes {
		pool, err := r.store.GetPool(vol.PoolID)
		if err != nil {
			continue
		}
		if _, exists := bdevMap[vol.LvolBdevName]; !exists {
			uuid, err := r.spdk.BdevLvolCreate(ctx, pool.LVSName, vol.Name, vol.SizeBytes, vol.ThinProvision, "")
			if err != nil {
				vol.HealthStatus = store.HealthStatusDegraded
				degraded = append(degraded, "volume:"+vol.ID)
				errs = append(errs, fmt.Sprintf("recreate volume %s: %v", vol.LvolBdevName, err))
			} else {
				vol.UUID = uuid
				vol.HealthStatus = store.HealthStatusOnline
				bdevMap[vol.LvolBdevName] = spdk.BdevInfo{Name: vol.LvolBdevName}
				repaired++
			}
		} else {
			vol.HealthStatus = store.HealthStatusOnline
		}

		if vol.QosPolicy.RwIosPerSec > 0 || vol.QosPolicy.RwMbytesPerSec > 0 ||
			vol.QosPolicy.RMbytesPerSec > 0 || vol.QosPolicy.WMbytesPerSec > 0 {
			_ = r.spdk.BdevSetQoSLimit(ctx, vol.EffectiveBdevName,
				vol.QosPolicy.RwIosPerSec,
				vol.QosPolicy.RwMbytesPerSec,
				vol.QosPolicy.RMbytesPerSec,
				vol.QosPolicy.WMbytesPerSec,
			)
		}
		_ = r.store.UpdateVolume(vol)
	}

	// 5. Reconcile NVMe-oF & iSCSI Exports.
	exports, err := r.store.ListExports("", "")
	if err != nil {
		errs = append(errs, fmt.Sprintf("list exports: %v", err))
	}
	if len(exports) > 0 {
		subsystems, _ := r.spdk.NvmfGetSubsystems(ctx)
		subsysMap := make(map[string]spdk.NvmfSubsystem, len(subsystems))
		for _, sub := range subsystems {
			subsysMap[sub.Nqn] = sub
		}

		transports, _ := r.spdk.NvmfGetTransports(ctx)
		transportMap := make(map[string]bool, len(transports))
		for _, tr := range transports {
			transportMap[strings.ToUpper(tr.Trtype)] = true
		}

		for _, exp := range exports {
			vol, err := r.store.GetVolume(exp.VolumeID)
			if err != nil {
				exp.HealthStatus = store.HealthStatusDegraded
				degraded = append(degraded, "export:"+exp.ID)
				_ = r.store.UpdateExport(exp)
				continue
			}

			switch exp.Protocol {
			case store.ExportProtocolNVMeoFTCP, store.ExportProtocolNVMeoFRDMA:
				trtype := "TCP"
				if exp.Protocol == store.ExportProtocolNVMeoFRDMA {
					trtype = "RDMA"
				}
				if !transportMap[trtype] {
					if err := r.spdk.NvmfCreateTransport(ctx, trtype); err == nil {
						transportMap[trtype] = true
						repaired++
					}
				}

				if _, exists := subsysMap[exp.SubsystemNQN]; !exists {
					serial := "VELO" + strings.ToUpper(strings.ReplaceAll(exp.ID, "-", ""))
					if len(serial) > 20 {
						serial = serial[:20]
					}
					if err := r.spdk.NvmfCreateSubsystem(ctx, exp.SubsystemNQN, serial, "", exp.AllowAnyHost, 32); err != nil {
						exp.HealthStatus = store.HealthStatusDegraded
						degraded = append(degraded, "export:"+exp.ID)
						errs = append(errs, fmt.Sprintf("create nvmf subsystem %s: %v", exp.SubsystemNQN, err))
						_ = r.store.UpdateExport(exp)
						continue
					}
					if nsid, err := r.spdk.NvmfSubsystemAddNs(ctx, exp.SubsystemNQN, vol.EffectiveBdevName, exp.NamespaceID, ""); err == nil {
						exp.NamespaceID = nsid
					}
					for _, gid := range exp.InitiatorGroupIDs {
						if ig, err := r.store.GetInitiatorGroup(gid); err == nil {
							for _, hid := range ig.HostIDs {
								if h, err := r.store.GetHost(hid); err == nil && h.HostNQN != "" {
									_ = r.spdk.NvmfSubsystemAddHost(ctx, exp.SubsystemNQN, h.HostNQN)
								}
							}
							for _, nqn := range ig.HostNQNs {
								_ = r.spdk.NvmfSubsystemAddHost(ctx, exp.SubsystemNQN, nqn)
							}
						}
					}
					for _, pid := range exp.PortalIDs {
						if p, err := r.store.GetPortal(pid); err == nil {
							_ = r.spdk.NvmfSubsystemAddListener(ctx, exp.SubsystemNQN, trtype, "IPv4", p.IPAddress, strconv.Itoa(int(p.Port)))
						}
					}
					exp.HealthStatus = store.HealthStatusOnline
					repaired++
				} else {
					exp.HealthStatus = store.HealthStatusOnline
				}
				_ = r.store.UpdateExport(exp)
			}
		}
	}

	lastErr := strings.Join(errs, "; ")
	r.finishPass(repaired, degraded, lastErr)
	if len(errs) > 0 {
		return r.State(), fmt.Errorf("%s", lastErr)
	}
	return r.State(), nil
}

func (r *Reconciler) finishPass(repaired uint64, degraded []string, lastErr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.InProgress = false
	r.state.LastReconciledAt = time.Now().UTC()
	r.state.DriftRepairedCount += repaired
	r.state.DegradedResources = degraded
	r.state.LastError = lastErr
}
