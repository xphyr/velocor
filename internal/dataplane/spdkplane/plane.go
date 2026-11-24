// Package spdkplane is the SPDK implementation of dataplane.Plane: spdk_tgt over its JSON-RPC
// socket, with RAID bdevs for arrays, lvstores for thin pools, lvols for volumes and snapshots,
// and SPDK's NVMe-oF and iSCSI targets for exports.
package spdkplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

// Plane drives spdk_tgt.
type Plane struct {
	c  *spdk.Client
	hw *hardware.Manager
	// ReadSuperblock replaces reading a disk's RAID superblock from it (tests).
	ReadSuperblock func(*store.Disk) (*hardware.RaidSuperblock, error)
}

var _ dataplane.Plane = (*Plane)(nil)

// New returns a Plane on an SPDK client. hw binds NVMe drives to a userspace driver when they
// are attached; it may be nil where no NVMe drive is attached (tests).
func New(c *spdk.Client, hw *hardware.Manager) *Plane { return &Plane{c: c, hw: hw} }

// Client is the underlying SPDK client, for SPDK-specific callers (the ublk metadata device).
func (p *Plane) Client() *spdk.Client { return p.c }

// tagged keeps an SPDK error's text and RPC details while also matching a dataplane error.
type tagged struct {
	err  error
	kind error
}

func (t *tagged) Error() string   { return t.err.Error() }
func (t *tagged) Unwrap() []error { return []error{t.err, t.kind} }

// wrap tags SPDK's "no such object" errors as dataplane.ErrNotFound.
func wrap(err error) error {
	if err != nil && spdk.IsNotFound(err) {
		return &tagged{err: err, kind: dataplane.ErrNotFound}
	}
	return err
}

func (p *Plane) Version(ctx context.Context) (string, error) { return p.c.GetVersion(ctx) }

// Settle waits for SPDK's examine. SPDK assembles only arrays on bdevs velod attached, which are
// the claimed disks, so claimed isn't needed.
func (p *Plane) Settle(ctx context.Context, _ []string) error { return p.c.BdevWaitForExamine(ctx) }

func (p *Plane) Endpoint() string { return p.c.SocketPath() }

// PollingCPUs lists the CPUs SPDK's reactors poll on.
func (p *Plane) PollingCPUs(ctx context.Context) ([]int, error) {
	var res struct {
		Reactors []struct {
			Lcore int `json:"lcore"`
		} `json:"reactors"`
	}
	if err := p.c.Call(ctx, "framework_get_reactors", nil, &res); err != nil {
		return nil, err
	}
	out := make([]int, len(res.Reactors))
	for i, r := range res.Reactors {
		out[i] = r.Lcore
	}
	return out, nil
}

// --- disks ---

func (p *Plane) AttachDisk(ctx context.Context, d *store.Disk) (string, store.DiskDriverState, error) {
	return AttachDisk(ctx, p.c, p.hw, d)
}

func (p *Plane) DetachDisk(ctx context.Context, d *store.Disk) error {
	return DetachDisk(ctx, p.c, d)
}

func (p *Plane) NVMeHealth(ctx context.Context, d *store.Disk) (*store.SmartHealth, error) {
	h, err := p.c.BdevNvmeGetControllerHealthInfo(ctx, NVMeControllerFromBdev(d.Handle))
	if err != nil {
		return nil, wrap(err)
	}
	return &store.SmartHealth{
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
	}, nil
}

func (p *Plane) NVMeWriteCache(ctx context.Context, d *store.Disk) (store.WriteCacheState, error) {
	return NVMeWriteCache(ctx, p.c, NVMeControllerFromBdev(d.Handle))
}

func (p *Plane) SetNVMeWriteCache(ctx context.Context, d *store.Disk, enabled bool) (store.WriteCacheState, error) {
	return SetNVMeWriteCache(ctx, p.c, NVMeControllerFromBdev(d.Handle), enabled)
}

func (p *Plane) Devices(ctx context.Context) ([]dataplane.Device, error) {
	bdevs, err := p.c.BdevGetBdevs(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]dataplane.Device, len(bdevs))
	for i, b := range bdevs {
		out[i] = dataplane.Device{
			Name:      b.Name,
			Aliases:   b.Aliases,
			UUID:      b.UUID,
			Product:   b.ProductName,
			SizeBytes: b.NumBlocks * uint64(b.BlockSize),
			QoS: store.QosPolicy{
				RwIosPerSec:    b.AssignedRateLimits.RwIosPerSec,
				RwMbytesPerSec: b.AssignedRateLimits.RwMbytesPerSec,
				RMbytesPerSec:  b.AssignedRateLimits.RMbytesPerSec,
				WMbytesPerSec:  b.AssignedRateLimits.WMbytesPerSec,
			},
		}
		if lvol, ok := b.DriverSpecific["lvol"].(map[string]any); ok {
			out[i].ParentSnapshot, _ = lvol["base_snapshot"].(string)
		}
	}
	return out, nil
}

// --- arrays ---

func (p *Plane) Arrays(ctx context.Context) ([]dataplane.Array, error) {
	raids, err := p.c.BdevRaidGetBdevs(ctx, "all")
	if err != nil {
		return nil, err
	}
	out := make([]dataplane.Array, len(raids))
	for i, rb := range raids {
		out[i] = arrayOf(&rb)
	}
	return out, nil
}

func arrayOf(rb *spdk.RaidBdevInfo) dataplane.Array {
	a := dataplane.Array{
		Name:        rb.Name,
		UUID:        rb.UUID,
		State:       rb.State,
		Level:       rb.RaidLevel,
		StripKB:     rb.StripSizeKb,
		Slots:       rb.NumBaseBdevs,
		Discovered:  rb.NumBaseBdevsDiscovered,
		Operational: rb.NumBaseBdevsOperational,
		Members:     make([]dataplane.ArrayMember, len(rb.BaseBdevsList)),
	}
	for i, m := range rb.BaseBdevsList {
		a.Members[i] = dataplane.ArrayMember{Name: m.Name, UUID: m.UUID, Configured: m.IsConfigured}
	}
	if rb.Process != nil {
		a.Rebuild = &dataplane.Rebuild{Target: rb.Process.Target, Percent: rb.Process.Progress.Percent}
	}
	if sc := rb.Scrub; sc != nil {
		a.Scrub = &dataplane.Scrub{
			State:                  sc.State,
			Repair:                 sc.Repair,
			Status:                 sc.Status,
			StartOffsetBlocks:      sc.StartOffsetBlocks,
			OffsetBlocks:           sc.OffsetBlocks,
			TotalBlocks:            sc.TotalBlocks,
			RepairedBlocks:         sc.RepairedBlocks,
			MismatchedBlocks:       sc.MismatchedBlocks,
			MismatchRepairedBlocks: sc.MismatchRepairedBlocks,
			UnrecoverableBlocks:    sc.UnrecoverableBlocks,
			WriteErrors:            sc.WriteErrors,
		}
	}
	return a
}

func (p *Plane) CreateArray(ctx context.Context, name string, level store.RaidLevel, stripKB uint32, members []string) error {
	return p.c.BdevRaidCreate(ctx, name, string(level), stripKB, members, true)
}

func (p *Plane) DeleteArray(ctx context.Context, name string) error {
	return wrap(p.c.BdevRaidDelete(ctx, name, true))
}

func (p *Plane) AddMember(ctx context.Context, array, member string) error {
	return wrap(p.c.BdevRaidAddBaseBdev(ctx, array, member))
}

func (p *Plane) RemoveMember(ctx context.Context, member string) error {
	return wrap(p.c.BdevRaidRemoveBaseBdev(ctx, member))
}

func (p *Plane) StartDegraded(ctx context.Context, array string) error {
	return wrap(p.c.BdevRaidStartDegraded(ctx, array))
}

func (p *Plane) RebuildLimit(ctx context.Context) (uint32, error) {
	return p.c.BdevRaidGetMaxBandwidth(ctx)
}

func (p *Plane) SetRebuildLimit(ctx context.Context, mbps uint32) error {
	return p.c.BdevRaidSetOptions(ctx, mbps)
}

func (p *Plane) StartScrub(ctx context.Context, array string, repair bool, mbps uint32, offsetBlocks uint64) error {
	return wrap(p.c.BdevRaidScrubStart(ctx, array, repair, mbps, offsetBlocks))
}

func (p *Plane) StopScrub(ctx context.Context, array string) error {
	return wrap(p.c.BdevRaidScrubStop(ctx, array))
}

// --- thin pools (lvstores) ---

func (p *Plane) ThinPools(ctx context.Context, name, uuid string) ([]dataplane.ThinPool, error) {
	stores, err := p.c.BdevLvolGetLvstores(ctx, name, uuid)
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]dataplane.ThinPool, len(stores))
	for i, l := range stores {
		out[i] = dataplane.ThinPool{
			UUID:       l.UUID,
			Name:       l.Name,
			Array:      l.BaseBdev,
			ChunkBytes: l.ClusterSize,
			TotalBytes: l.TotalDataClusters * uint64(l.ClusterSize),
			FreeBytes:  l.FreeClusters * uint64(l.ClusterSize),
		}
	}
	return out, nil
}

func (p *Plane) CreateThinPool(ctx context.Context, array, name string, chunkBytes uint32) (string, error) {
	return p.c.BdevLvolCreateLvstore(ctx, array, name, chunkBytes, "")
}

// GrowMetadata: an lvstore keeps its metadata in its own clusters; nothing to grow.
func (p *Plane) GrowMetadata(context.Context, string) (uint64, error) { return 0, nil }

// LostData: SPDK keeps no bad-block log (Array.UnreadableBytes is always 0).
func (p *Plane) LostData(context.Context, string) ([]dataplane.LostExtent, error) { return nil, nil }

func (p *Plane) DeleteThinPool(ctx context.Context, uuid, name string) error {
	return wrap(p.c.BdevLvolDeleteLvstore(ctx, uuid, name))
}

// --- volumes and snapshots (lvols) ---

func (p *Plane) Volumes(ctx context.Context) ([]dataplane.Volume, error) {
	lvols, err := p.c.BdevLvolGetLvols(ctx)
	if err != nil {
		return nil, err
	}
	// Allocation is counted in clusters; the cluster size is the lvstore's.
	stores, err := p.c.BdevLvolGetLvstores(ctx, "", "")
	if err != nil {
		return nil, err
	}
	cluster := make(map[string]uint64, len(stores))
	for _, s := range stores {
		cluster[s.UUID] = uint64(s.ClusterSize)
	}
	out := make([]dataplane.Volume, len(lvols))
	for i, l := range lvols {
		out[i] = dataplane.Volume{
			UUID:           l.UUID,
			Name:           l.Name,
			Alias:          l.Alias,
			Pool:           l.LVS.UUID,
			Thin:           l.IsThinProvisioned,
			Snapshot:       l.IsSnapshot,
			AllocatedBytes: l.NumAllocatedClusters * cluster[l.LVS.UUID],
		}
	}
	return out, nil
}

func (p *Plane) CreateVolume(ctx context.Context, pool, name string, sizeBytes uint64, thin bool) (string, error) {
	return p.c.BdevLvolCreate(ctx, pool, name, sizeBytes, thin, "")
}

func (p *Plane) ResizeVolume(ctx context.Context, volume string, sizeBytes uint64) error {
	return wrap(p.c.BdevLvolResize(ctx, volume, sizeBytes))
}

func (p *Plane) DeleteVolume(ctx context.Context, volume string) error {
	return wrap(p.c.BdevLvolDelete(ctx, volume))
}

func (p *Plane) Snapshot(ctx context.Context, volume, name string) (string, error) {
	uuid, err := p.c.BdevLvolSnapshot(ctx, volume, name)
	return uuid, wrap(err)
}

func (p *Plane) SnapshotGroup(ctx context.Context, entries []dataplane.GroupEntry, quiesce time.Duration) ([]string, error) {
	req := make([]spdk.SnapshotGroupEntry, len(entries))
	for i, e := range entries {
		req[i] = spdk.SnapshotGroupEntry{LvolName: e.Volume, SnapshotName: e.Snapshot}
	}
	uuids, err := p.c.BdevLvolSnapshotGroup(ctx, req, quiesce)
	var rpcErr *spdk.RPCError
	if errors.As(err, &rpcErr) {
		switch rpcErr.Code {
		case -110: // ETIMEDOUT: no snapshot was taken
			return nil, &tagged{err: err, kind: dataplane.ErrQuiesceTimeout}
		case -32601: // method not found: an SPDK without patch 0003
			return nil, &tagged{err: err, kind: dataplane.ErrUnsupported}
		}
	}
	return uuids, wrap(err)
}

func (p *Plane) MaxSnapshotGroup() int { return spdk.MaxSnapshotGroup }

func (p *Plane) Clone(ctx context.Context, snapshot, name string) (string, error) {
	uuid, err := p.c.BdevLvolClone(ctx, snapshot, name)
	return uuid, wrap(err)
}

// AllocatedBlocks is the union of the fragmaps over the snapshot's whole chain (into a clone's
// origin). The chain is SPDK's own (each lvol's base_snapshot), which is where the clusters are:
// it includes snapshots velod keeps hidden for clones and reflects the merges SPDK does when a
// snapshot is deleted.
func (p *Plane) AllocatedBlocks(ctx context.Context, pool *store.Pool, target *store.Snapshot) ([]dataplane.Extent, error) {
	return p.blocks(ctx, pool, nil, target)
}

// ChangedBlocks is the union of the fragmaps over the layers after base, up to target.
func (p *Plane) ChangedBlocks(ctx context.Context, pool *store.Pool, base, target *store.Snapshot) ([]dataplane.Extent, error) {
	return p.blocks(ctx, pool, base, target)
}

func (p *Plane) blocks(ctx context.Context, pool *store.Pool, base, target *store.Snapshot) ([]dataplane.Extent, error) {
	baseName := ""
	if base != nil {
		baseName = strings.TrimPrefix(base.Handle, pool.ThinPoolName+"/")
	}
	layers := []string{target.UUID}
	reachedBase := false
	for cur := target.UUID; ; {
		parent, err := p.c.BdevLvolParent(ctx, cur)
		if err != nil {
			return nil, fmt.Errorf("snapshot chain of %s: %w", target.ID, wrap(err))
		}
		if parent == "" {
			break
		}
		if base != nil && parent == baseName {
			reachedBase = true
			break
		}
		if len(layers) >= 4096 {
			return nil, fmt.Errorf("snapshot chain of %s is longer than 4096 layers", target.ID)
		}
		cur = pool.ThinPoolName + "/" + parent
		layers = append(layers, cur)
	}
	if base != nil && !reachedBase {
		return nil, fmt.Errorf("%w: snapshot %s is not older than %s in its chain", store.ErrInvalidArgument, base.ID, target.ID)
	}
	var extentSets [][]spdk.BlockExtent
	for _, layer := range layers {
		frag, err := p.c.BdevLvolGetFragmap(ctx, layer)
		if errors.Is(err, spdk.ErrCBTUnsupported) {
			return nil, &tagged{err: err, kind: dataplane.ErrCBTUnsupported}
		}
		if err != nil {
			return nil, wrap(err)
		}
		ext, err := spdk.DecodeFragmapExtents(frag, target.SizeBytes)
		if err != nil {
			return nil, fmt.Errorf("fragmap of %s: %w", layer, err)
		}
		extentSets = append(extentSets, ext)
	}
	merged := spdk.MergeBlockExtents(extentSets...)
	out := make([]dataplane.Extent, len(merged))
	for i, e := range merged {
		out[i] = dataplane.Extent{OffsetBytes: e.OffsetBytes, SizeBytes: e.SizeBytes}
	}
	return out, nil
}

func (p *Plane) SetQoS(ctx context.Context, device string, q store.QosPolicy) error {
	return wrap(p.c.BdevSetQoSLimit(ctx, device, q.RwIosPerSec, q.RwMbytesPerSec, q.RMbytesPerSec, q.WMbytesPerSec))
}

// --- exports ---

func (p *Plane) ApplyExport(ctx context.Context, exp *store.Export, vol *store.Volume, acl dataplane.ACL, portals []*store.Portal) error {
	if dataplane.IsNVMe(exp.Protocol) {
		return ensureNVMe(ctx, p.c, exp, vol, acl, portals)
	}
	return ensureIscsi(ctx, p.c, exp, vol, acl, portals)
}

func (p *Plane) RemoveTarget(ctx context.Context, exp *store.Export) error {
	switch {
	case dataplane.IsNVMe(exp.Protocol):
		if err := p.c.NvmfDeleteSubsystem(ctx, exp.SubsystemNQN); err != nil && !spdk.IsNotFound(err) {
			return fmt.Errorf("delete nvmf subsystem %s: %w", exp.SubsystemNQN, err)
		}
	case exp.Protocol == store.ExportProtocolISCSI:
		if err := p.c.IscsiDeleteTargetNode(ctx, exp.TargetIQN); err != nil && !spdk.IsNotFound(err) {
			return fmt.Errorf("delete iscsi target %s: %w", exp.TargetIQN, err)
		}
		if exp.IscsiIgTag != 0 {
			if err := p.c.IscsiDeleteInitiatorGroup(ctx, exp.IscsiIgTag); err != nil && !spdk.IsNotFound(err) {
				return fmt.Errorf("delete iscsi initiator group %d: %w", exp.IscsiIgTag, err)
			}
		}
	}
	return nil
}

func (p *Plane) Connections(ctx context.Context, exp *store.Export) ([]dataplane.Connection, error) {
	out := []dataplane.Connection{}
	switch {
	case dataplane.IsNVMe(exp.Protocol):
		ctrls, err := p.c.NvmfSubsystemGetControllers(ctx, exp.SubsystemNQN)
		if spdk.IsNotFound(err) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		for _, ct := range ctrls {
			out = append(out, dataplane.Connection{Host: ct.HostNQN, Queues: int(ct.NumIOQpairs)})
		}
	case exp.Protocol == store.ExportProtocolISCSI:
		conns, err := p.c.IscsiGetConnections(ctx)
		if err != nil {
			return nil, err
		}
		for _, cn := range conns {
			if cn.TargetNodeName == exp.TargetIQN {
				out = append(out, dataplane.Connection{Address: cn.InitiatorAddr})
			}
		}
	}
	return out, nil
}

// --- observability ---

func (p *Plane) IOStats(ctx context.Context, device string) (*dataplane.IOStats, error) {
	res, err := p.c.BdevGetIostat(ctx, device)
	if err != nil {
		return nil, wrap(err)
	}
	out := &dataplane.IOStats{TickRate: res.TickRate, Devices: make([]dataplane.DeviceIO, len(res.Bdevs))}
	for i, b := range res.Bdevs {
		out.Devices[i] = dataplane.DeviceIO(b)
	}
	return out, nil
}
