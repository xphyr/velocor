package dataplane

import (
	"context"
	"errors"
	"time"

	"github.com/velocor/velocor/internal/store"
)

// Plane is velod's data plane: the claimed disks, the RAID arrays built from them, the thin
// pools on the arrays, the volumes and snapshots in the pools, and the NVMe-oF and iSCSI
// targets that export them. velod's API handlers and reconciler reach the data plane only
// through this interface: kernelplane, on the kernel's md, LVM thin, nvmet and LIO
// (docs/design/kernel-dataplane.md), and planetest, an in-memory one for tests.
//
// Objects are named by the handles velod keeps in its store records: an array by
// Pool.ArrayName, a thin pool by Pool.ThinPoolUUID / Pool.ThinPoolName, a volume or snapshot by its
// UUID (or its "pool/name" alias), a disk by Disk.Handle. The handles are opaque to
// velod; the backend chooses and interprets them.
//
// Every method is safe for concurrent use. Errors for objects that don't exist satisfy
// IsNotFound.
type Plane interface {
	// Version returns the data plane's version; an error means it isn't reachable.
	Version(ctx context.Context) (string, error)
	// Settle waits until the data plane has examined its disks: arrays assembled from their
	// on-disk metadata and thin pools loaded from the arrays. claimed are the handles of the
	// disks velod has claimed: only arrays on those are assembled, never one on disks velod
	// doesn't own.
	Settle(ctx context.Context, claimed []string) error
	// Endpoint describes how velod reaches the data plane (a socket path), for status output.
	Endpoint() string

	// AttachDisk hands a claimed disk to the data plane and returns its handle (stored as
	// Disk.Handle) and the driver state it is now in.
	AttachDisk(ctx context.Context, d *store.Disk) (string, store.DiskDriverState, error)
	// DetachDisk gives the disk back to the host. A disk that isn't attached is not an error.
	DetachDisk(ctx context.Context, d *store.Disk) error
	// NVMeHealth reads SMART health from an attached NVMe disk the host can no longer see.
	NVMeHealth(ctx context.Context, d *store.Disk) (*store.SmartHealth, error)
	// NVMeWriteCache reads an attached NVMe disk's volatile write cache state, and
	// SetNVMeWriteCache sets it (saved across power cycles where the drive allows) and returns
	// the state read back.
	NVMeWriteCache(ctx context.Context, d *store.Disk) (store.WriteCacheState, error)
	SetNVMeWriteCache(ctx context.Context, d *store.Disk, enabled bool) (store.WriteCacheState, error)

	// ExamineDisks reads the array metadata on disks and groups them by array: which array each
	// belongs to, its slot, and whether its copy is current. For pool import and for pool
	// members that come back under a new disk ID (docs/review-history.md P1-14). Disks without
	// array metadata are left out.
	ExamineDisks(ctx context.Context, disks []*store.Disk) ([]DiskArray, error)

	// Devices lists the block devices the data plane holds: attached disks, arrays, volumes
	// and snapshots, and anything else it has open.
	Devices(ctx context.Context) ([]Device, error)

	// Arrays lists the RAID arrays the data plane has assembled or is assembling.
	Arrays(ctx context.Context) ([]Array, error)
	// CheckLevel says whether the data plane can build an array of level from disks disks.
	CheckLevel(level store.RaidLevel, disks int) error
	// CreateArray builds an array from attached disks (by handle), with on-disk metadata.
	CreateArray(ctx context.Context, name string, level store.RaidLevel, stripKB uint32, members []string) error
	// DeleteArray stops an array and erases its metadata from the members.
	DeleteArray(ctx context.Context, name string) error
	// AddMember adds an attached disk to an array; the array rebuilds onto it.
	AddMember(ctx context.Context, array, member string) error
	// RemoveMember takes a member out of whichever array holds it.
	RemoveMember(ctx context.Context, member string) error
	// StartDegraded starts an array that is waiting for missing members without them.
	StartDegraded(ctx context.Context, array string) error
	// RebuildLimit and SetRebuildLimit read and set the rebuild bandwidth limit for all arrays
	// in MB/s (0 = unlimited).
	RebuildLimit(ctx context.Context) (uint32, error)
	SetRebuildLimit(ctx context.Context, mbps uint32) error
	// StartScrub starts (or resumes, from offsetBlocks) a scrub of the array, checking that
	// every copy agrees; repair rewrites what doesn't. StopScrub stops it. Progress is in
	// Array.Scrub.
	StartScrub(ctx context.Context, array string, repair bool, mbps uint32, offsetBlocks uint64) error
	StopScrub(ctx context.Context, array string) error

	// ThinPools lists thin pools, all of them or the one with the given name or UUID.
	ThinPools(ctx context.Context, name, uuid string) ([]ThinPool, error)
	// CreateThinPool creates a thin pool named name on an array and returns its UUID.
	CreateThinPool(ctx context.Context, array, name string) (string, error)
	// GrowMetadata grows a thin pool's metadata when it is filling, as far as the room under it
	// allows, and returns its new size (0: unchanged). An error says it needed to and couldn't.
	GrowMetadata(ctx context.Context, uuid string) (uint64, error)
	// LostData says what the part of an array no member can supply (Array.UnreadableBytes)
	// held: which volumes and snapshots, and where in them. Nil when nothing is lost or the
	// backend keeps no bad-block log.
	LostData(ctx context.Context, array string) ([]LostExtent, error)
	// DeleteThinPool deletes a thin pool and everything in it, by UUID or else by name.
	DeleteThinPool(ctx context.Context, uuid, name string) error

	// Volumes lists every volume and snapshot in every thin pool.
	Volumes(ctx context.Context) ([]Volume, error)
	// CreateVolume creates a volume named name in a thin pool (by name) and returns its UUID. A
	// volume that isn't thin is outside the pool and fully allocated, with no snapshots: velod's
	// own metadata volume only.
	CreateVolume(ctx context.Context, pool, name string, sizeBytes uint64, thin bool) (string, error)
	// ResizeVolume grows a volume (by UUID or alias).
	ResizeVolume(ctx context.Context, volume string, sizeBytes uint64) error
	// DeleteVolume deletes a volume or snapshot (by UUID or alias).
	DeleteVolume(ctx context.Context, volume string) error
	// Snapshot takes a read-only snapshot named name of a volume and returns its UUID.
	Snapshot(ctx context.Context, volume, name string) (string, error)
	// SnapshotGroup snapshots several volumes at one point in time: I/O to all of them is
	// held while the snapshots are taken, for at most quiesce. It returns the snapshots' UUIDs
	// in entry order. A quiesce that times out fails with ErrQuiesceTimeout and leaves no
	// snapshots; a data plane that can't do it fails with ErrUnsupported.
	SnapshotGroup(ctx context.Context, entries []GroupEntry, quiesce time.Duration) ([]string, error)
	// MaxSnapshotGroup is the most volumes one SnapshotGroup can take.
	MaxSnapshotGroup() int
	// Clone creates a writable thin clone named name of a snapshot and returns its UUID.
	Clone(ctx context.Context, snapshot, name string) (string, error)
	// AllocatedBlocks lists the ranges of a snapshot (in pool) that hold data;
	// ChangedBlocks lists the ranges that differ between base and target, two snapshots of
	// the same volume with base the older. Both are in bytes, merged and sorted, and fail
	// with ErrCBTUnsupported when the data plane can't tell.
	AllocatedBlocks(ctx context.Context, pool *store.Pool, target *store.Snapshot) ([]Extent, error)
	ChangedBlocks(ctx context.Context, pool *store.Pool, base, target *store.Snapshot) ([]Extent, error)

	// ApplyExport converges the target for exp so that it exposes vol to exactly acl on the
	// given portals. Portals whose address isn't on this host are left out and reported with
	// an AddressMissingError (joined, one per portal); the rest still converge.
	ApplyExport(ctx context.Context, exp *store.Export, vol *store.Volume, acl ACL, portals []*store.Portal) error
	// RemoveTarget tears down the target objects exp owns. Missing objects are not an error.
	RemoveTarget(ctx context.Context, exp *store.Export) error
	// Connections lists the hosts connected to exp's target.
	Connections(ctx context.Context, exp *store.Export) ([]Connection, error)

	// IOStats returns I/O counters for one device, or all when device is "".
	IOStats(ctx context.Context, device string) (*IOStats, error)

	// Unmanaged lists data-plane objects velod has no record of, given what it does know.
	// It never changes anything.
	Unmanaged(ctx context.Context, known Known) ([]UnmanagedObject, error)
}

// DiskArray is an array as the metadata on a set of disks describes it.
type DiskArray struct {
	UUID string
	Name string
	// Level is the store's name for it (RAID0, RAID1, ...); unknown levels as LEVEL-<n>.
	Level store.RaidLevel
	// Slots is the array's member count; Current how many its newest metadata has in use.
	Slots   int
	Current int
	// Generation orders metadata copies: a member that left the array has an older one.
	Generation uint64
	SizeBytes  uint64
	Members    []DiskMember
}

// DiskMember is one disk carrying an array's metadata.
type DiskMember struct {
	Disk *store.Disk
	Slot int // -1 when its metadata doesn't say
	// Stale: the disk left the array (an older copy, or not one of its current members).
	Stale bool
}

// Device is a block device the data plane holds.
type Device struct {
	Name      string
	Aliases   []string
	UUID      string
	Product   string // what the backend calls this kind of device, for reports
	SizeBytes uint64
	// Origin is, for a snapshot, the volume it was taken of, and for a clone the snapshot it was
	// made from: names within the pool ("" when it has none, or when that is deleted).
	Origin string
}

// Array states.
const (
	ArrayOnline      = "online"      // running, possibly degraded
	ArrayConfiguring = "configuring" // waiting for members
	ArrayOffline     = "offline"
)

// Array is a RAID array.
type Array struct {
	Name    string
	UUID    string
	State   string // ArrayOnline, ArrayConfiguring, ArrayOffline, or a backend-specific state
	Level   string // "raid0", "raid1", ... as the backend names it
	StripKB uint32
	// Slots is the number of member slots; Discovered how many have a member present;
	// Operational how many the array runs (or, while configuring, will run) with. Fewer
	// operational than slots means degraded.
	Slots       uint32
	Discovered  uint32
	Operational uint32
	// Members are in slot order; a missing member has Name "".
	Members []ArrayMember
	// Rebuild is the rebuild in progress, if any.
	Rebuild *Rebuild
	// Scrub is the running or last scrub since the array was assembled.
	Scrub *Scrub
	// UnreadableBytes is how much of the array no member can supply: bad blocks recorded on
	// every copy. Reads (and writes) there fail; the data is lost. 0 where the backend keeps
	// no bad-block log.
	UnreadableBytes uint64
}

// What lost data held (LostExtent.Kind).
const (
	LostVolume       = "volume"        // a volume or snapshot (UUID; Offset within it)
	LostThinMetadata = "thin-metadata" // the thin pool's own metadata: every volume's mappings
	LostPoolMetadata = "pool-metadata" // the on-pool copy of velod's configuration (P0-3)
	LostLVMMetadata  = "lvm-metadata"  // LVM's label and metadata, before the first extent
	LostOther        = "other"         // another LV (Name)
	LostUnlocated    = "unlocated"     // where in the array isn't worked out (Length only)
)

// LostExtent is a range of data lost from an array, and what held it. Unused space (free
// extents, thin blocks nothing maps, LVM's spare copy of the thin metadata) isn't reported.
type LostExtent struct {
	Kind   string
	UUID   string // LostVolume: the volume's or snapshot's UUID
	Name   string // LostOther: the LV
	Offset uint64 // within the volume (LostVolume) or LV (LostOther, LostPoolMetadata); otherwise informative only
	Length uint64
}

// ArrayMember is one slot of an array.
type ArrayMember struct {
	Name       string // the disk's handle
	UUID       string // the member's identity in the array metadata
	Configured bool   // in sync and in use
	// BadBlockBytes is the size of the member's bad blocks: ranges it couldn't read or
	// rewrite, which the array serves from another copy, if any, from then on.
	BadBlockBytes uint64
}

// Rebuild is an array rebuilding onto a member (or another background process the backend
// runs on the array).
type Rebuild struct {
	Target  string // the member being rebuilt
	Percent uint32
}

// Scrub states.
const (
	ScrubRunning   = "running"
	ScrubCompleted = "completed"
	ScrubStopped   = "stopped" // on request, by a member removal or a rebuild
	ScrubFailed    = "failed"
)

// Why a scrub stopped (Scrub.Status, negative errno).
const (
	ScrubStatusMemberRemoved = -19  // ENODEV: a member left the array
	ScrubStatusRebuild       = -16  // EBUSY: a rebuild started
	ScrubStatusCanceled      = -125 // ECANCELED: on request
)

// Scrub is an array's running or last scrub. Offsets and counts are in blocks of the members'
// data area; the counters cover the run since StartOffsetBlocks.
type Scrub struct {
	State                  string
	Repair                 bool
	Status                 int32 // why it stopped or failed: 0, or a negative errno
	StartOffsetBlocks      uint64
	OffsetBlocks           uint64
	TotalBlocks            uint64
	RepairedBlocks         uint64
	MismatchedBlocks       uint64
	MismatchRepairedBlocks uint64
	UnrecoverableBlocks    uint64
	WriteErrors            uint64
}

// ThinPool is a thin pool on an array, holding volumes and snapshots.
type ThinPool struct {
	UUID       string
	Name       string
	Array      string // the device it is on
	ChunkBytes uint32 // allocation unit
	TotalBytes uint64
	FreeBytes  uint64
	// MetadataBytes and MetadataUsedBytes are the thin pool's own metadata (the kernel's
	// dm-thin), finite and worse to fill than the data: 0 where there is none to watch.
	MetadataBytes     uint64
	MetadataUsedBytes uint64
}

// Volume is a volume or snapshot in a thin pool.
type Volume struct {
	UUID           string
	Name           string
	Alias          string // "pool/name"
	Pool           string // the thin pool's UUID
	Thin           bool
	Snapshot       bool
	AllocatedBytes uint64
	// Seq grows with each volume and snapshot created in the pool: the order they were made in.
	Seq int
}

// GroupEntry is one volume of a SnapshotGroup: the volume (UUID or alias) and the name for its
// snapshot.
type GroupEntry struct {
	Volume   string
	Snapshot string
}

// Extent is a byte range of a volume.
type Extent struct {
	OffsetBytes uint64
	SizeBytes   uint64
}

// IOStats are a data plane's I/O counters.
type IOStats struct {
	// TickRate is how many latency ticks make a second.
	TickRate uint64     `json:"tick_rate"`
	Devices  []DeviceIO `json:"bdevs"`
}

// DeviceIO are one device's I/O counters since the data plane started.
type DeviceIO struct {
	Name              string `json:"name"`
	BytesRead         uint64 `json:"bytes_read"`
	NumReadOps        uint64 `json:"num_read_ops"`
	BytesWritten      uint64 `json:"bytes_written"`
	NumWriteOps       uint64 `json:"num_write_ops"`
	BytesUnmapped     uint64 `json:"bytes_unmapped"`
	NumUnmapOps       uint64 `json:"num_unmap_ops"`
	ReadLatencyTicks  uint64 `json:"read_latency_ticks"`
	WriteLatencyTicks uint64 `json:"write_latency_ticks"`
	UnmapLatencyTicks uint64 `json:"unmap_latency_ticks"`
}

// Known is what velod's store accounts for, by handle; for Plane.Unmanaged.
type Known struct {
	ThinPools map[string]bool // UUIDs
	Devices   map[string]bool // array, disk and volume device names
	Volumes   map[string]bool // volume and snapshot UUIDs
	Targets   map[string]bool // NQNs and IQNs
	// Skip are volume names that belong to velod without a record (its metadata volume).
	Skip map[string]bool
}

// UnmanagedObject is a data-plane object velod has no record of.
type UnmanagedObject struct {
	// Kind is what the backend calls it (MD_ARRAY, VOLUME_GROUP, LV, NVMET_SUBSYSTEM,
	// ISCSI_TARGET).
	Kind string `json:"kind"`
	Name string `json:"name"`
	UUID string `json:"uuid,omitempty"`
	// Detail says where it is or what it holds.
	Detail    string    `json:"detail,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
}

var (
	// ErrNotFound is matched by errors for data-plane objects that don't exist.
	ErrNotFound = errors.New("not found in the data plane")
	// ErrQuiesceTimeout: in-flight I/O didn't drain in time for a group snapshot.
	ErrQuiesceTimeout = errors.New("timed out quiescing I/O")
	// ErrUnsupported: the data plane can't do this.
	ErrUnsupported = errors.New("not supported by the data plane")
	// ErrCBTUnsupported: the data plane can't report allocated or changed blocks.
	ErrCBTUnsupported = errors.New("changed-block tracking is not supported by the data plane")
)

// IsNotFound reports whether err is for a data-plane object that doesn't exist.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// ClaimedHandles lists the handles of the disks velod has claimed, for Plane.Settle.
func ClaimedHandles(st store.Store) []string {
	disks, err := st.ListDisks()
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range disks {
		if d.Claimed && d.Handle != "" {
			out = append(out, d.Handle)
		}
	}
	return out
}
