package kernelplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/kernel"
	"github.com/velocor/velocor/internal/store"
)

// isVolume: an LV velod made for a volume, snapshot, clone or its metadata copy, not the
// thin pool itself.
func isVolume(lv kernel.LV) bool { return lv.SegType == "thin" || lv.SegType == "linear" }

func isSnapshot(lv kernel.LV) bool { return slices.Contains(lv.Tags, tagSnapshot) }

// volumeUUID is the UUID velod knows a volume by: its tag, or LVM's for an LV velod didn't tag.
func volumeUUID(lv kernel.LV) string {
	for _, t := range lv.Tags {
		if u, ok := strings.CutPrefix(t, tagUUID); ok {
			return u
		}
	}
	return lv.UUID
}

// allocated is how much of the pool a volume holds: a thin volume's mapped share, a thick
// volume's size.
func allocated(lv kernel.LV) uint64 {
	if lv.SegType != "thin" {
		return lv.Size
	}
	return uint64(float64(lv.Size) * lv.DataPercent / 100)
}

// resolve finds a volume or snapshot by UUID or by "vg/name" alias.
func (p *Plane) resolve(ctx context.Context, ref string) (kernel.LV, error) {
	lvs, err := p.k.ListVolumes(ctx, "")
	if err != nil {
		return kernel.LV{}, err
	}
	vg, name, alias := strings.Cut(ref, "/")
	for _, lv := range lvs {
		if !isVolume(lv) {
			continue
		}
		if alias && lv.VG == vg && lv.Name == name || !alias && volumeUUID(lv) == ref {
			return lv, nil
		}
	}
	return kernel.LV{}, notFound("volume %s not found", ref)
}

// --- thin pools ---

// ThinPools lists each volume group's thin pool: the VG's name and UUID, the pool's size and
// free space, and the array it is on.
func (p *Plane) ThinPools(ctx context.Context, name, uuid string) ([]dataplane.ThinPool, error) {
	vgs, err := p.k.ListVGs(ctx)
	if err != nil {
		return nil, err
	}
	lvs, err := p.k.ListVolumes(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []dataplane.ThinPool
	for _, vg := range vgs {
		if name != "" && vg.Name != name || uuid != "" && vg.UUID != uuid {
			continue
		}
		i := slices.IndexFunc(lvs, func(lv kernel.LV) bool { return lv.VG == vg.Name && lv.SegType == "thin-pool" && lv.Name == thinLV })
		if i < 0 {
			continue
		}
		pool := lvs[i]
		used := uint64(float64(pool.Size) * pool.DataPercent / 100)
		out = append(out, dataplane.ThinPool{
			UUID: vg.UUID, Name: vg.Name, Array: p.arrayName(vg.PV), ChunkBytes: uint32(pool.ChunkSize),
			TotalBytes: pool.Size, FreeBytes: pool.Size - min(used, pool.Size),
			MetadataBytes: pool.MetadataSize, MetadataUsedBytes: uint64(float64(pool.MetadataSize) * pool.MetadataPercent / 100),
		})
	}
	return out, nil
}

// arrayName maps a PV device (/dev/md127) to its array's name.
func (p *Plane) arrayName(pv string) string {
	k := kernelName(pv)
	for _, name := range p.k.ListArrays() {
		if kernelName(p.k.ArrayPath(name)) == k {
			return name
		}
	}
	return pv
}

// CreateThinPool puts a volume group named name on the array, with its thin pool (64 KiB chunks,
// kernel.ChunkSize).
func (p *Plane) CreateThinPool(ctx context.Context, array, name string) (string, error) {
	if err := p.k.CreatePool(ctx, kernel.PoolSpec{VG: name, Device: p.k.ArrayPath(array), Thin: thinLV, DataPercent: dataPercent}); err != nil {
		return "", err
	}
	pools, err := p.ThinPools(ctx, name, "")
	if err != nil {
		return "", err
	}
	if len(pools) != 1 {
		return "", fmt.Errorf("thin pool %s not found after creating it", name)
	}
	return pools[0].UUID, nil
}

// GrowMetadata doubles a thin pool's metadata once it is MetadataGrowAt percent full, as far as
// the volume group's free space allows (LVM keeps a spare copy as large, so the growth needs
// twice its size free) and up to LVM's limit.
func (p *Plane) GrowMetadata(ctx context.Context, uuid string) (uint64, error) {
	vgs, err := p.k.ListVGs(ctx)
	if err != nil {
		return 0, err
	}
	i := slices.IndexFunc(vgs, func(vg kernel.VG) bool { return vg.UUID == uuid })
	if i < 0 {
		return 0, notFound("thin pool %s not found", uuid)
	}
	vg := vgs[i]
	lvs, err := p.k.ListVolumes(ctx, "")
	if err != nil {
		return 0, err
	}
	j := slices.IndexFunc(lvs, func(lv kernel.LV) bool { return lv.VG == vg.Name && lv.SegType == "thin-pool" && lv.Name == thinLV })
	if j < 0 || lvs[j].MetadataSize == 0 {
		return 0, nil
	}
	pool := lvs[j]
	if pool.MetadataPercent < p.MetadataGrowAt || pool.MetadataSize >= kernel.MaxMetadataSize {
		return 0, nil
	}
	ext := max(vg.ExtentSize, 1)
	add := min(pool.MetadataSize, kernel.MaxMetadataSize-pool.MetadataSize, vg.Free/2/ext*ext)
	if add < ext {
		return 0, fmt.Errorf("thin pool metadata is %.0f%% full and the volume group has no room to grow it (%d bytes free)", pool.MetadataPercent, vg.Free)
	}
	size := pool.MetadataSize + add
	if err := p.k.GrowPoolMetadata(ctx, vg.Name, thinLV, size); err != nil {
		return 0, err
	}
	return size, nil
}

// DeleteThinPool removes the volume group with everything in it (the wrappers first: an
// exported volume's wrapper is busy, which stops the delete).
func (p *Plane) DeleteThinPool(ctx context.Context, uuid, name string) error {
	vgs, err := p.k.ListVGs(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(vgs, func(vg kernel.VG) bool { return uuid != "" && vg.UUID == uuid || uuid == "" && vg.Name == name })
	if i < 0 {
		return notFound("thin pool %s%s not found", uuid, name)
	}
	vg := vgs[i]
	lvs, err := p.k.ListVolumes(ctx, vg.Name)
	if err != nil {
		return err
	}
	p.wrapMu.Lock() // until the LVs are gone, or Settle puts the wrappers back
	defer p.wrapMu.Unlock()
	for _, lv := range lvs {
		if err := p.removeWrapper(ctx, lv.Name); err != nil {
			return fmt.Errorf("remove the wrapper of %s: %w", lv.Name, err)
		}
	}
	return p.k.RemovePool(ctx, vg.Name, vg.PV)
}

// --- volumes and snapshots ---

func (p *Plane) Volumes(ctx context.Context) ([]dataplane.Volume, error) {
	vgs, err := p.k.ListVGs(ctx)
	if err != nil {
		return nil, err
	}
	vgUUID := map[string]string{}
	for _, vg := range vgs {
		vgUUID[vg.Name] = vg.UUID
	}
	lvs, err := p.k.ListVolumes(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []dataplane.Volume
	for _, lv := range lvs {
		if !isVolume(lv) {
			continue
		}
		out = append(out, dataplane.Volume{
			UUID: volumeUUID(lv), Name: lv.Name, Alias: lv.VG + "/" + lv.Name, Pool: vgUUID[lv.VG],
			Thin: lv.SegType == "thin", Snapshot: isSnapshot(lv), AllocatedBytes: allocated(lv), Seq: lv.ThinID,
		})
	}
	return out, nil
}

// CreateVolume creates the LV in the pool's volume group (a thick one zeroed) and its wrapper.
func (p *Plane) CreateVolume(ctx context.Context, pool, name string, sizeBytes uint64, thin bool) (string, error) {
	u := newUUID()
	if err := p.k.CreateVolume(ctx, pool, thinLV, name, sizeBytes, thin, tagUUID+u); err != nil {
		return "", err
	}
	if !thin {
		// A thick LV gets whatever its extents held: a deleted volume's data, or (seen on the lab
		// VM) an earlier pool's metadata copy. Zero it; the
		// thin pool zeroes new chunks itself.
		if _, err := p.k.Run.Run(ctx, "blkdiscard", "--zeroout", p.k.LVPath(pool, name)); err != nil {
			_ = p.k.RemoveVolume(context.WithoutCancel(ctx), pool, name)
			return "", fmt.Errorf("zero the new volume: %w", err)
		}
	}
	if name != metaLV {
		if err := p.ensureWrapper(ctx, kernel.LV{VG: pool, Name: name}); err != nil {
			_ = p.k.RemoveVolume(context.WithoutCancel(ctx), pool, name)
			return "", fmt.Errorf("wrapper: %w", err)
		}
	}
	return u, nil
}

// ResizeVolume grows the LV, then its wrapper, then tells nvmet (hosts see the new size).
func (p *Plane) ResizeVolume(ctx context.Context, volume string, sizeBytes uint64) error {
	lv, err := p.resolve(ctx, volume)
	if err != nil {
		return err
	}
	if err := p.k.ResizeVolume(ctx, lv.VG, lv.Name, sizeBytes); err != nil {
		return err
	}
	if err := p.ensureWrapper(ctx, lv); err != nil {
		return err
	}
	wrapper := p.k.WrapperPath(lv.Name)
	for _, nqn := range p.k.NVMeSubsystems() {
		if p.k.NVMeSubsystemDevice(nqn) == wrapper {
			if err := p.k.RevalidateNVMe(nqn); err != nil {
				return err
			}
		}
	}
	return nil
}

// DeleteVolume removes a volume's wrapper (busy while exported) and the LV.
func (p *Plane) DeleteVolume(ctx context.Context, volume string) error {
	lv, err := p.resolve(ctx, volume)
	if err != nil {
		return err
	}
	p.wrapMu.Lock() // until the LV is gone, or Settle puts the wrapper back
	defer p.wrapMu.Unlock()
	if err := p.removeWrapper(ctx, lv.Name); err != nil {
		return err
	}
	return p.k.RemoveVolume(ctx, lv.VG, lv.Name)
}

func (p *Plane) removeWrapper(ctx context.Context, volume string) error {
	if _, err := os.Stat(p.k.WrapperPath(volume)); err != nil {
		return nil
	}
	return p.k.RemoveWrapper(ctx, volume)
}

// Snapshot takes a thin snapshot; it stays inactive (LVM's skip-activation flag) and has no
// wrapper.
func (p *Plane) Snapshot(ctx context.Context, volume, name string) (string, error) {
	lv, err := p.resolve(ctx, volume)
	if err != nil {
		return "", err
	}
	u := newUUID()
	if err := p.k.Snapshot(ctx, lv.VG, lv.Name, name, tagUUID+u, tagSnapshot); err != nil {
		return "", err
	}
	return u, nil
}

// SnapshotGroup suspends every member's wrapper, snapshots them all, and resumes them
// (kernel.GroupSnapshotWithin). quiesce bounds the pause (in-flight I/O draining): running out
// fails it with ErrQuiesceTimeout and leaves no snapshot.
func (p *Plane) SnapshotGroup(ctx context.Context, entries []dataplane.GroupEntry, quiesce time.Duration) ([]string, error) {
	members := make([]kernel.GroupMember, len(entries))
	uuids := make([]string, len(entries))
	p.wrapMu.Lock()
	defer p.wrapMu.Unlock()
	for i, e := range entries {
		lv, err := p.resolve(ctx, e.Volume)
		if err != nil {
			return nil, err
		}
		if err := p.k.EnsureWrapper(ctx, lv.Name, p.k.LVPath(lv.VG, lv.Name)); err != nil {
			return nil, fmt.Errorf("wrapper of %s: %w", lv.Name, err)
		}
		uuids[i] = newUUID()
		members[i] = kernel.GroupMember{Volume: lv.Name, VG: lv.VG, LV: lv.Name, Snap: e.Snapshot, Tags: []string{tagUUID + uuids[i], tagSnapshot}}
	}
	if err := p.k.GroupSnapshotWithin(ctx, members, quiesce); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil { // not the caller's own deadline
			return nil, &tagged{err: err, kind: dataplane.ErrQuiesceTimeout}
		}
		return nil, err
	}
	return uuids, nil
}

// Clone creates a writable, active thin volume from a snapshot, with its wrapper.
func (p *Plane) Clone(ctx context.Context, snapshot, name string) (string, error) {
	lv, err := p.resolve(ctx, snapshot)
	if err != nil {
		return "", err
	}
	u := newUUID()
	if err := p.k.Clone(ctx, lv.VG, lv.Name, name, tagUUID+u); err != nil {
		return "", err
	}
	if err := p.ensureWrapper(ctx, kernel.LV{VG: lv.VG, Name: name}); err != nil {
		_ = p.k.RemoveVolume(context.WithoutCancel(ctx), lv.VG, name)
		return "", fmt.Errorf("wrapper: %w", err)
	}
	return u, nil
}

// --- changed-block tracking ---

func (p *Plane) AllocatedBlocks(ctx context.Context, pool *store.Pool, target *store.Snapshot) ([]dataplane.Extent, error) {
	t, err := p.resolve(ctx, target.UUID)
	if err != nil {
		return nil, err
	}
	ext, err := p.k.AllocatedBlocks(ctx, t.VG, thinLV, t.ThinID, target.SizeBytes)
	if err != nil {
		return nil, err
	}
	return extents(ext), nil
}

// ChangedBlocks compares two snapshots' mappings in the pool's metadata (thin_delta). Thin IDs
// grow with creation, so base must have the lower one.
func (p *Plane) ChangedBlocks(ctx context.Context, pool *store.Pool, base, target *store.Snapshot) ([]dataplane.Extent, error) {
	b, err := p.resolve(ctx, base.UUID)
	if err != nil {
		return nil, err
	}
	t, err := p.resolve(ctx, target.UUID)
	if err != nil {
		return nil, err
	}
	if b.VG != t.VG || b.ThinID >= t.ThinID {
		return nil, fmt.Errorf("%w: snapshot %s is not older than %s in its pool", store.ErrInvalidArgument, base.ID, target.ID)
	}
	ext, err := p.k.ChangedBlocks(ctx, t.VG, thinLV, b.ThinID, t.ThinID, target.SizeBytes)
	if err != nil {
		return nil, err
	}
	return extents(ext), nil
}

func extents(in []kernel.Extent) []dataplane.Extent {
	out := make([]dataplane.Extent, len(in))
	for i, e := range in {
		out[i] = dataplane.Extent{OffsetBytes: e.Offset, SizeBytes: e.Length}
	}
	return out
}

// VolumePath is the device of a volume or snapshot (an LV, active): for velod's own direct I/O
// on its metadata volume (meta.BlockDevice).
func (p *Plane) VolumePath(ctx context.Context, volume string) (string, error) {
	lv, err := p.resolve(ctx, volume)
	if err != nil {
		return "", err
	}
	return p.k.LVPath(lv.VG, lv.Name), nil
}
