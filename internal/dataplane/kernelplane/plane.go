// Package kernelplane is the Linux kernel implementation of dataplane.Plane
// (docs/design/kernel-dataplane.md): md RAID arrays, an LVM volume group with one thin pool per
// array, thin (or thick) LVs for volumes and snapshots, a dm-linear wrapper per volume, and the
// kernel's NVMe/TCP target (nvmet) and iSCSI target (LIO), all through internal/kernel.
//
// Handles, as velod stores them:
//   - a disk: a stable /dev/disk/by-path (NVMe) or by-id link to it (diskHandle);
//   - an array: its md name (DevRoot/md/<name>), Pool.ArrayName;
//   - a thin pool: its volume group, named Pool.ThinPoolName, with the VG's UUID;
//   - a volume or snapshot: the LV named by its velod ID in that VG, alias "vg/id", and a UUID
//     velod can use as the NVMe namespace UUID, kept in an LVM tag (LVM's own UUIDs aren't
//     RFC 4122);
//   - a volume's device: its wrapper, DevRoot/mapper/velocor-<id>, which the targets export.
package kernelplane

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/kernel"
	"github.com/velocor/velocor/internal/store"
)

const (
	// thinLV is the thin pool's LV name in each volume group.
	thinLV = "thin"
	// tagUUID prefixes the LVM tag holding a volume's or snapshot's velod UUID.
	tagUUID = "velocor.uuid."
	// tagSnapshot marks an LV as a snapshot (read-only to velod; clones don't carry it).
	tagSnapshot = "velocor.snapshot"
	// arrayPrefix is how velod names arrays (Pool.ArrayName): Settle assembles only these.
	arrayPrefix = "raid_"
	// dataPercent of an array's volume group goes to the thin pool's data; the rest holds the
	// pool's metadata (and LVM's spare copy), thick volumes and the on-pool metadata copy.
	dataPercent = 90
	// maxGroup is the most volumes one group snapshot takes, as with SPDK.
	maxGroup = 64
)

// Plane is the kernel data plane.
type Plane struct {
	k  *kernel.Plane
	hw *hardware.Manager
	// MetadataGrowAt is how full (percent) a thin pool's metadata gets before GrowMetadata
	// doubles it.
	MetadataGrowAt float64
	// StateDir, if set, keeps the scrubs started (scrubs.json) across velod restarts; md itself
	// keeps only its last action. A directory cleared at boot (/run/velocor), as md's is.
	StateDir string

	mu sync.Mutex
	// waiting are arrays Settle found incomplete: md doesn't assemble an array with members
	// missing (without --run), so the plane keeps what it found until an operator starts it
	// degraded or the members come back (SPDK's "configuring" RAID).
	waiting map[string]kernel.ExaminedArray
	// scrubs are the scrubs started since velod started, by array: md keeps only the last
	// action, not where it began or whether it was stopped.
	scrubs map[string]*scrubRun
	// lost caches LostData by array, while its unreadable ranges stay the same.
	lost map[string]lostData
}

type scrubRun struct {
	Repair  bool   `json:"repair"`
	Start   uint64 `json:"start"` // sectors
	Stopped bool   `json:"stopped"`
}

var _ dataplane.Plane = (*Plane)(nil)

// New returns a kernel data plane. hw may be nil (tests); it rebinds NVMe drives left on a
// userspace driver to the kernel's nvme driver.
func New(k *kernel.Plane, hw *hardware.Manager) *Plane {
	return &Plane{k: k, hw: hw, MetadataGrowAt: 50, waiting: map[string]kernel.ExaminedArray{}, scrubs: map[string]*scrubRun{}}
}

// tagged keeps an error's text while also matching a dataplane error.
type tagged struct {
	err  error
	kind error
}

func (t *tagged) Error() string   { return t.err.Error() }
func (t *tagged) Unwrap() []error { return []error{t.err, t.kind} }

func notFound(format string, args ...any) error {
	return &tagged{err: fmt.Errorf(format, args...), kind: dataplane.ErrNotFound}
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Version is the running kernel's release.
func (p *Plane) Version(_ context.Context) (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "", err
	}
	return "linux " + strings.TrimSpace(string(b)), nil
}

func (p *Plane) Endpoint() string { return "kernel (md, LVM thin, nvmet, LIO)" }

// PollingCPUs: the kernel doesn't busy-poll.
func (p *Plane) PollingCPUs(context.Context) ([]int, error) { return nil, nil }

// MaxSnapshotGroup bounds a group snapshot (every member's I/O waits while all are taken).
func (p *Plane) MaxSnapshotGroup() int { return maxGroup }

// SetQoS: the kernel targets have no per-volume limits; QoS is dropped with the move
// (docs/design/kernel-dataplane.md §6). Clearing limits succeeds.
func (p *Plane) SetQoS(_ context.Context, _ string, q store.QosPolicy) error {
	if q == (store.QosPolicy{}) {
		return nil
	}
	return fmt.Errorf("%w: QoS limits", dataplane.ErrUnsupported)
}

// Settle brings up what the claimed disks hold, as SPDK's examine did: the targets' modules,
// every velod array on claimed disks whose members are all present (incomplete ones are kept
// as waiting), every volume group on them, a wrapper for every volume, and any wrapper a crash
// left suspended resumed. An array with a member on a disk velod hasn't claimed is left alone
// (an import candidate, or disks from elsewhere). It is idempotent and runs on every
// reconciler pass.
func (p *Plane) Settle(ctx context.Context, claimed []string) error {
	if err := p.k.LoadTargetModules(ctx); err != nil {
		return fmt.Errorf("load target modules: %w", err)
	}
	examined, err := p.k.ExamineArrays(ctx)
	if err != nil {
		return fmt.Errorf("examine md metadata: %w", err)
	}
	running := p.k.ListArrays()
	owned := map[string]bool{}
	for _, h := range claimed {
		if k := kernelName(h); k != "" {
			owned[k] = true
		}
	}
	waiting := map[string]kernel.ExaminedArray{}
	for _, a := range examined {
		if !strings.HasPrefix(a.Name, arrayPrefix) || slices.Contains(running, a.Name) {
			continue
		}
		// Only velod's disks: a copy of the array on a disk it hasn't claimed (an import
		// candidate, a member that left, disks from elsewhere) takes no part.
		a.Found = slices.DeleteFunc(a.Found, func(dev string) bool { return !owned[kernelName(dev)] })
		if len(a.Found) == 0 {
			continue
		}
		if len(a.Found) < a.Devices {
			waiting[a.Name] = a
			continue
		}
		if err := p.k.AssembleArray(ctx, a.Name, a.UUID, a.Found, false); err != nil {
			waiting[a.Name] = a
		}
	}
	p.mu.Lock()
	p.waiting = waiting
	p.mu.Unlock()

	vgs, err := p.k.ListVGs(ctx)
	if err != nil {
		return fmt.Errorf("list volume groups: %w", err)
	}
	var errs []error
	for _, vg := range vgs {
		if err := p.k.ActivatePool(ctx, vg.Name); err != nil {
			errs = append(errs, fmt.Errorf("activate %s: %w", vg.Name, err))
		}
	}
	lvs, err := p.k.ListVolumes(ctx, "")
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("list volumes: %w", err))...)
	}
	for _, lv := range lvs {
		if !isVolume(lv) || isSnapshot(lv) || lv.Name == metaLV {
			continue
		}
		if err := p.k.EnsureWrapper(ctx, lv.Name, p.k.LVPath(lv.VG, lv.Name)); err != nil {
			errs = append(errs, fmt.Errorf("wrapper of %s/%s: %w", lv.VG, lv.Name, err))
			continue
		}
		if s, err := p.k.WrapperSuspended(ctx, lv.Name); err == nil && s {
			if err := p.k.ResumeWrapper(ctx, lv.Name); err != nil {
				errs = append(errs, fmt.Errorf("resume the wrapper of %s: %w", lv.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// metaLV is the on-pool metadata copy's volume (meta.LvolName): velod reads and writes it
// directly, so it gets no wrapper.
const metaLV = "velocor-meta"

// --- disks ---

// kernelName resolves a device path (or link) to the kernel's name for it: sdb, nvme0n1.
func kernelName(path string) string {
	t, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}

// diskHandle is the stable link velod keeps for a disk with kernel name kname: the NVMe
// namespace's by-path link (by PCI address, like SPDK's controller names), else the disk's
// by-id link, a WWN first; /dev/<kname> if there is none.
func (p *Plane) diskHandle(kname string) string {
	links := func(dir string) []string {
		es, _ := os.ReadDir(filepath.Join(p.k.DevRoot, "disk", dir))
		var out []string
		for _, e := range es {
			l := filepath.Join(p.k.DevRoot, "disk", dir, e.Name())
			if kernelName(l) == kname {
				out = append(out, l)
			}
		}
		sort.Strings(out)
		return out
	}
	if strings.HasPrefix(kname, "nvme") {
		for _, l := range links("by-path") {
			if strings.HasSuffix(l, "-nvme-1") {
				return l
			}
		}
	}
	ids := links("by-id")
	for _, l := range ids {
		if strings.HasPrefix(filepath.Base(l), "wwn-") {
			return l
		}
	}
	if len(ids) > 0 {
		return ids[0]
	}
	return filepath.Join(p.k.DevRoot, kname)
}

// AttachDisk: the disk stays on its kernel driver (an NVMe drive SPDK left on vfio-pci goes
// back to nvme); its handle is its stable link.
func (p *Plane) AttachDisk(ctx context.Context, d *store.Disk) (string, store.DiskDriverState, error) {
	if d.IsBootDevice {
		return "", "", store.ErrBootDeviceProtected
	}
	path := d.DevicePath
	switch d.Type {
	case store.DiskTypeNVMe:
		if p.hw != nil && d.PCIAddress != "" && p.hw.PCIDriver(d.PCIAddress) != "nvme" {
			if err := p.hw.BindNVMeToKernel(d.PCIAddress); err != nil {
				return "", "", fmt.Errorf("bind %s to the nvme driver: %w", d.PCIAddress, err)
			}
		}
		if d.PCIAddress != "" {
			byPath := filepath.Join(p.k.DevRoot, "disk", "by-path", "pci-"+d.PCIAddress+"-nvme-1")
			for deadline := time.Now().Add(10 * time.Second); ; {
				if _, err := os.Stat(byPath); err == nil {
					path = byPath
					break
				}
				if time.Now().After(deadline) || ctx.Err() != nil {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
		}
	case store.DiskTypeSSD:
	default:
		return "", "", fmt.Errorf("%w: unsupported disk type %q", store.ErrInvalidArgument, d.Type)
	}
	kname := kernelName(path)
	if path == "" || kname == "" {
		return "", "", fmt.Errorf("disk %s has no block device", d.ID)
	}
	return p.diskHandle(kname), store.DiskDriverStateKernel, nil
}

// DetachDisk: nothing holds the disk but its array, which velod has stopped or left.
func (p *Plane) DetachDisk(context.Context, *store.Disk) error { return nil }

// nvmeController is the controller device (/dev/nvme0) of an NVMe disk's namespace.
func (p *Plane) nvmeController(d *store.Disk) (string, error) {
	kname := kernelName(d.Handle)
	if kname == "" {
		kname = kernelName(d.DevicePath)
	}
	i := strings.LastIndex(kname, "n")
	if !strings.HasPrefix(kname, "nvme") || i <= len("nvme") {
		return "", fmt.Errorf("disk %s is not an NVMe namespace (%q)", d.ID, kname)
	}
	return filepath.Join(p.k.DevRoot, kname[:i]), nil
}

// --- devices and I/O statistics ---

// Devices lists arrays (by name), volumes and snapshots (by alias, with their UUIDs), and every
// whole disk by its handle.
func (p *Plane) Devices(ctx context.Context) ([]dataplane.Device, error) {
	var out []dataplane.Device
	for _, name := range p.k.ListArrays() {
		out = append(out, dataplane.Device{Name: name, Aliases: []string{p.k.ArrayPath(name)}, Product: "md array",
			SizeBytes: p.sizeOf(kernelName(p.k.ArrayPath(name)))})
	}
	lvs, err := p.k.ListVolumes(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, lv := range lvs {
		if !isVolume(lv) {
			continue
		}
		dev := dataplane.Device{Name: lv.VG + "/" + lv.Name, Aliases: []string{volumeUUID(lv)}, Product: "LVM " + lv.SegType,
			SizeBytes: lv.Size, ParentSnapshot: lv.Origin}
		out = append(out, dev)
	}
	for _, kname := range p.wholeDisks() {
		out = append(out, dataplane.Device{Name: p.diskHandle(kname), Aliases: []string{filepath.Join(p.k.DevRoot, kname)},
			Product: "disk", SizeBytes: p.sizeOf(kname)})
	}
	return out, nil
}

// wholeDisks lists the host's whole disks by kernel name: not partitions (sysfs lists those
// under their disk), arrays, device-mapper devices, zram or optical drives.
func (p *Plane) wholeDisks() []string {
	es, _ := os.ReadDir(filepath.Join(p.k.SysRoot, "block"))
	var out []string
	for _, e := range es {
		kname := e.Name()
		if strings.HasPrefix(kname, "md") || strings.HasPrefix(kname, "dm-") || strings.HasPrefix(kname, "zram") || strings.HasPrefix(kname, "sr") {
			continue
		}
		out = append(out, kname)
	}
	return out
}

func (p *Plane) sizeOf(kname string) uint64 {
	b, _ := os.ReadFile(filepath.Join(p.k.SysRoot, "block", kname, "size"))
	n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n * 512
}

// IOStats reads /sys/block/*/stat for arrays (by name), volumes (their wrappers, by alias) and
// whole disks (by handle).
// Times are in milliseconds, so the tick rate is 1000.
func (p *Plane) IOStats(ctx context.Context, device string) (*dataplane.IOStats, error) {
	out := &dataplane.IOStats{TickRate: 1000, Devices: []dataplane.DeviceIO{}}
	add := func(name, kname string) {
		if device != "" && device != name {
			return
		}
		if io, ok := p.blockStat(kname); ok {
			io.Name = name
			out.Devices = append(out.Devices, io)
		}
	}
	for _, name := range p.k.ListArrays() {
		add(name, kernelName(p.k.ArrayPath(name)))
	}
	lvs, err := p.k.ListVolumes(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, lv := range lvs {
		if isVolume(lv) && !isSnapshot(lv) {
			add(lv.VG+"/"+lv.Name, kernelName(p.k.WrapperPath(lv.Name)))
		}
	}
	for _, kname := range p.wholeDisks() {
		add(p.diskHandle(kname), kname)
	}
	if device != "" && len(out.Devices) == 0 {
		return nil, notFound("device %s not found", device)
	}
	return out, nil
}

// blockStat parses /sys/block/<kname>/stat (Documentation/block/stat.rst).
func (p *Plane) blockStat(kname string) (dataplane.DeviceIO, bool) {
	if kname == "" {
		return dataplane.DeviceIO{}, false
	}
	b, err := os.ReadFile(filepath.Join(p.k.SysRoot, "block", kname, "stat"))
	if err != nil {
		return dataplane.DeviceIO{}, false
	}
	f := strings.Fields(string(b))
	n := func(i int) uint64 {
		if i >= len(f) {
			return 0
		}
		v, _ := strconv.ParseUint(f[i], 10, 64)
		return v
	}
	return dataplane.DeviceIO{
		NumReadOps: n(0), BytesRead: n(2) * 512, ReadLatencyTicks: n(3),
		NumWriteOps: n(4), BytesWritten: n(6) * 512, WriteLatencyTicks: n(7),
		NumUnmapOps: n(11), BytesUnmapped: n(13) * 512, UnmapLatencyTicks: n(14),
	}, true
}
