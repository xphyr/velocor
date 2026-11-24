// Package planetest is an in-memory data plane (dataplane.Plane) for tests, behaving like the
// kernel's: md arrays, LVM thin pools, nvmet and LIO targets.
//
// It tells what is on the disks (array metadata with event counts, thin pools, volumes) from what
// runs (assembled arrays, targets). Reboot drops what runs; Settle assembles again what the
// claimed disks hold, under md's rules: an array starts only when every member its newest
// metadata counts as active is present and current, a member that left has an older event count
// and takes no part, and an interrupted rebuild goes on by itself.
//
// Methods can be made to fail (Fail, FailWith, FailAfterApplying), by Plane method name, and the
// calls that change something are recorded (Calls, CallCount).
package planetest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/store"
)

// ChunkBytes is the thin pools' chunk size: larger than the kernel data plane's 64 KiB, so tests
// fill pools with few chunks.
const ChunkBytes = 1 << 20

// extent is what LVM rounds volume sizes up to.
const extent = 4 << 20

// defaultChunks is a thin pool's size in chunks unless SetPoolChunks changed it (1 TiB).
const defaultChunks = 1 << 40 / ChunkBytes

// ErrInjected is what Fail makes methods return.
var ErrInjected = errors.New("injected failure")

// NVMeCache is an NVMe drive's volatile write cache.
type NVMeCache struct {
	Present  bool // the drive has one
	Enabled  bool
	Saveable bool
	Saved    bool // the setting was saved across power cycles
	Refuse   bool // the drive refuses to change it
}

type disk struct {
	key     string // PCI address or device path (AddDisk)
	handle  string
	present bool
	cache   NVMeCache
	member  *member // the array metadata on it
}

// member is the array metadata one disk carries.
type member struct {
	array  string // the array's UUID
	slot   int
	events uint64
}

// slot is one member slot of an array, as its current metadata has it.
type slot struct {
	disk    *disk // nil: empty (failed or removed)
	rebuild bool  // being rebuilt: in its slot, not in sync
}

type array struct {
	name, uuid, level string
	stripKB           uint32
	slots             []*slot
	events            uint64
	running           bool
	percent           uint32 // of the rebuild
	scrub             *dataplane.Scrub
	scrubStarts       []uint64
	pool              *pool
}

type pool struct {
	name, uuid string
	chunks     uint64
	generation int // how many times a thin pool was created under this name
	volumes    map[string]*volume
	nextChunk  uint64
	metaBytes  uint64
	metaUsed   uint64
}

type volume struct {
	uuid, name string
	pool       *pool
	size       uint64
	thin       bool
	snapshot   bool
	parent     string            // the snapshot a clone comes from, or the volume a snapshot is of (by name)
	chunks     map[uint64]uint64 // volume chunk -> pool chunk
	order      int               // creation order, as thin device IDs are
}

// Target is an export as the kernel targets have it.
type Target struct {
	Device    string // the volume's UUID
	AnyHost   bool
	Hosts     []string // host NQNs or initiator IQNs
	Listeners []string // "address:port"
	Serial    string
}

// Fake is an in-memory data plane.
type Fake struct {
	// Down, when set, makes every method fail with it: the data plane is unreachable.
	Down error

	mu           sync.Mutex
	disks        []*disk
	arrays       []*array
	pools        map[string]int // thin pools ever created, by name
	targets      map[string]*Target
	conns        map[string][]dataplane.Connection
	rebuildLimit uint32
	scrubStep    uint64
	calls        []string
	failN        map[string]int
	failErr      map[string]error
	failAfter    map[string]int
	volumes      int
}

var _ dataplane.Plane = (*Fake)(nil)

// New returns an empty data plane.
func New(t testing.TB) *Fake {
	t.Helper()
	return &Fake{pools: map[string]int{}, targets: map[string]*Target{}, conns: map[string][]dataplane.Connection{},
		failN: map[string]int{}, failErr: map[string]error{}, failAfter: map[string]int{}}
}

// Handle is the handle of the disk added under key (a PCI address or a device path).
func Handle(key string) string {
	if strings.HasPrefix(key, "/") {
		return key
	}
	return "/dev/disk/by-path/pci-" + key + "-nvme-1"
}

// --- what tests do to the hardware ---

// AddDisk plugs in a disk: an NVMe drive (by PCI address) or an SSD (by device path).
func (f *Fake) AddDisk(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.disks {
		if d.key == key {
			d.present = true
			return
		}
	}
	f.disks = append(f.disks, &disk{key: key, handle: Handle(key), present: true})
}

// RemoveDisk pulls a disk out; a running array that has it loses the member.
func (f *Fake) RemoveDisk(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.byKey(key)
	d.present = false
	f.memberGone(d)
}

// FailDisk makes a disk fail: it is still there, but a running array drops it.
func (f *Fake) FailDisk(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.memberGone(f.byKey(key))
}

// MoveDisk moves a disk to another place: a new key and handle, the same contents.
func (f *Fake) MoveDisk(from, to string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.byKey(from)
	d.key, d.handle = to, Handle(to)
}

// WipeDisk erases a disk's array metadata.
func (f *Fake) WipeDisk(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byKey(key).member = nil
}

func (f *Fake) byKey(key string) *disk {
	for _, d := range f.disks {
		if d.key == key {
			return d
		}
	}
	panic("planetest: no disk " + key)
}

// memberGone takes a disk out of the running array that has it, which records it.
func (f *Fake) memberGone(d *disk) {
	for _, a := range f.arrays {
		if !a.running {
			continue
		}
		for _, s := range a.slots {
			if s.disk == d {
				s.disk, s.rebuild = nil, false
				a.percent = 0
				f.stamp(a)
				if a.scrub != nil && a.scrub.State == dataplane.ScrubRunning {
					a.scrub.State, a.scrub.Status = dataplane.ScrubStopped, dataplane.ScrubStatusMemberRemoved
				}
				if a.survivors() == 0 || a.level == "raid0" {
					a.running = false // nothing left to run from
				}
			}
		}
	}
}

// survivors is how many in-sync members an array has.
func (a *array) survivors() int {
	n := 0
	for _, s := range a.slots {
		if s.disk != nil && !s.rebuild {
			n++
		}
	}
	return n
}

// stamp records the array's current membership on its members, as md does on every change.
func (f *Fake) stamp(a *array) {
	a.events++
	for i, s := range a.slots {
		if s.disk != nil {
			s.disk.member = &member{array: a.uuid, slot: i, events: a.events}
		}
	}
}

// Reboot restarts the appliance: arrays stop and targets and connections go, as does the rebuild
// limit; what is on the disks stays.
func (f *Fake) Reboot() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.arrays {
		a.running = false
		a.scrub = nil
	}
	f.targets = map[string]*Target{}
	f.conns = map[string][]dataplane.Connection{}
	f.rebuildLimit = 0
}

// --- failure injection and call records ---

// Fail makes the next n calls of a Plane method fail with ErrInjected, before it does anything.
func (f *Fake) Fail(method string, n int) { f.FailWith(method, n, ErrInjected) }

// FailWith is Fail with an error of the test's choice.
func (f *Fake) FailWith(method string, n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failN[method], f.failErr[method] = n, err
}

// FailAfterApplying makes the next n calls of a method fail after doing their work: the caller
// can't tell it was done.
func (f *Fake) FailAfterApplying(method string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAfter[method] = n
}

// Calls lists the calls so far that change something, by method name.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CallCount is how often a method was called.
func (f *Fake) CallCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

// begin locks the fake for a call of method that changes something: recorded, and failed if the
// test asked for that. The caller unlocks with the returned function, whose error result wraps
// FailAfterApplying.
func (f *Fake) begin(method string) (done func(error) error, err error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	if f.Down != nil {
		f.mu.Unlock()
		return nil, f.Down
	}
	if f.failN[method] > 0 {
		f.failN[method]--
		err := f.failErr[method]
		f.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	return func(err error) error {
		if err == nil && f.failAfter[method] > 0 {
			f.failAfter[method]--
			err = fmt.Errorf("%s: %w", method, ErrInjected)
		}
		f.mu.Unlock()
		return err
	}, nil
}

// read locks the fake for a call that only reads: failed like begin, not recorded.
func (f *Fake) read(method string) (func(), error) {
	f.mu.Lock()
	if f.Down != nil {
		f.mu.Unlock()
		return nil, f.Down
	}
	if f.failN[method] > 0 {
		f.failN[method]--
		err := f.failErr[method]
		f.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	return f.mu.Unlock, nil
}

type tagged struct {
	msg  string
	kind error
}

func (t *tagged) Error() string { return t.msg }
func (t *tagged) Unwrap() error { return t.kind }

func notFound(format string, args ...any) error {
	return &tagged{msg: fmt.Sprintf(format, args...), kind: dataplane.ErrNotFound}
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// --- lifecycle ---

func (f *Fake) Version(context.Context) (string, error) {
	done, err := f.read("Version")
	if err != nil {
		return "", err
	}
	defer done()
	return "linux 7.3.4-planetest", nil
}

func (f *Fake) Endpoint() string { return "kernel (test)" }

// Settle assembles the arrays on claimed disks that are complete (see the package comment).
func (f *Fake) Settle(_ context.Context, claimed []string) error {
	done, err := f.read("Settle")
	if err != nil {
		return err
	}
	defer done()
	for _, a := range f.arrays {
		if a.running {
			continue
		}
		if f.assemble(a, claimed, false) {
			a.running = true
		}
	}
	return nil
}

// assemble puts the present, claimed, current members into the array's slots and says whether it
// may run: every member the newest metadata counts as active is there, or degraded is true.
func (f *Fake) assemble(a *array, claimed []string, degraded bool) bool {
	var newest uint64
	cur := map[int]*disk{}
	for _, d := range f.disks {
		if d.present && d.member != nil && d.member.array == a.uuid && slices.Contains(claimed, d.handle) {
			newest = max(newest, d.member.events)
		}
	}
	for _, d := range f.disks {
		if d.present && d.member != nil && d.member.array == a.uuid && slices.Contains(claimed, d.handle) && d.member.events == newest {
			cur[d.member.slot] = d
		}
	}
	if newest < a.events { // only members that had left it
		return false
	}
	found, need := 0, 0
	for i, s := range a.slots {
		if s.disk != nil && !s.rebuild { // a rebuild target isn't active yet
			need++
			if cur[i] == s.disk {
				found++
			}
		}
	}
	if found == 0 || found < need && !degraded {
		return false
	}
	for i, s := range a.slots {
		if cur[i] != s.disk {
			s.disk, s.rebuild = nil, false
		}
	}
	if found < need {
		f.stamp(a)
	}
	return true
}

// --- disks ---

func (f *Fake) diskOf(d *store.Disk) *disk {
	key := d.DevicePath
	if d.Type == store.DiskTypeNVMe && d.PCIAddress != "" {
		key = d.PCIAddress
	}
	for _, x := range f.disks {
		if x.key == key || d.Handle != "" && x.handle == d.Handle {
			return x
		}
	}
	return nil
}

func (f *Fake) byHandle(h string) *disk {
	for _, d := range f.disks {
		if d.handle == h {
			return d
		}
	}
	return nil
}

func (f *Fake) AttachDisk(_ context.Context, d *store.Disk) (string, store.DiskDriverState, error) {
	done, err := f.begin("AttachDisk")
	if err != nil {
		return "", "", err
	}
	x := f.diskOf(d)
	switch {
	case d.IsBootDevice:
		err = store.ErrBootDeviceProtected
	case d.Type != store.DiskTypeNVMe && d.Type != store.DiskTypeSSD:
		err = fmt.Errorf("%w: unsupported disk type %q", store.ErrInvalidArgument, d.Type)
	case x == nil || !x.present:
		err = fmt.Errorf("disk %s has no block device", d.ID)
	}
	if err = done(err); err != nil {
		return "", "", err
	}
	return x.handle, store.DiskDriverStateKernel, nil
}

func (f *Fake) DetachDisk(context.Context, *store.Disk) error {
	done, err := f.begin("DetachDisk")
	if err != nil {
		return err
	}
	return done(nil)
}

func (f *Fake) NVMeHealth(_ context.Context, d *store.Disk) (*store.SmartHealth, error) {
	done, err := f.read("NVMeHealth")
	if err != nil {
		return nil, err
	}
	defer done()
	return &store.SmartHealth{}, nil
}

// SetNVMeCache sets a drive's write cache; NVMeCacheOf reads it.
func (f *Fake) SetNVMeCache(pci string, c NVMeCache) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.disks {
		if d.key == pci {
			d.cache = c
			return
		}
	}
	f.disks = append(f.disks, &disk{key: pci, handle: Handle(pci), cache: c})
}

func (f *Fake) NVMeCacheOf(pci string) NVMeCache {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byKey(pci).cache
}

func cacheState(c NVMeCache) store.WriteCacheState {
	if c.Present && c.Enabled {
		return store.WriteCacheWriteBack
	}
	return store.WriteCacheWriteThrough
}

func (f *Fake) NVMeWriteCache(_ context.Context, d *store.Disk) (store.WriteCacheState, error) {
	done, err := f.read("NVMeWriteCache")
	if err != nil {
		return "", err
	}
	defer done()
	x := f.diskOf(d)
	if x == nil || !x.present {
		return "", fmt.Errorf("disk %s is not there", d.ID)
	}
	return cacheState(x.cache), nil
}

func (f *Fake) SetNVMeWriteCache(_ context.Context, d *store.Disk, enabled bool) (store.WriteCacheState, error) {
	done, err := f.begin("SetNVMeWriteCache")
	if err != nil {
		return "", err
	}
	x := f.diskOf(d)
	if x == nil || !x.present {
		return "", done(fmt.Errorf("disk %s is not there", d.ID))
	}
	if x.cache.Refuse {
		return cacheState(x.cache), done(fmt.Errorf("the drive refused the write cache setting"))
	}
	x.cache.Enabled, x.cache.Saved = enabled, x.cache.Saveable
	return cacheState(x.cache), done(nil)
}

func (f *Fake) ExamineDisks(_ context.Context, disks []*store.Disk) ([]dataplane.DiskArray, error) {
	done, err := f.read("ExamineDisks")
	if err != nil {
		return nil, err
	}
	defer done()
	var out []dataplane.DiskArray
	for _, a := range f.arrays {
		da := dataplane.DiskArray{UUID: a.uuid, Name: a.name, Level: store.RaidLevel(strings.ToUpper(a.level)), Slots: len(a.slots), SizeBytes: 1 << 40}
		for _, s := range a.slots {
			if s.disk != nil {
				da.Current++
			}
		}
		var newest uint64
		for _, d := range disks {
			if x := f.diskOf(d); x != nil && x.present && x.member != nil && x.member.array == a.uuid {
				newest = max(newest, x.member.events)
			}
		}
		for _, d := range disks {
			if x := f.diskOf(d); x != nil && x.present && x.member != nil && x.member.array == a.uuid {
				da.Members = append(da.Members, dataplane.DiskMember{Disk: d, Slot: x.member.slot, Stale: x.member.events < newest})
			}
		}
		da.Generation = newest
		if len(da.Members) > 0 {
			out = append(out, da)
		}
	}
	return out, nil
}

// Devices lists present disks, running arrays, and the volumes of their thin pools.
func (f *Fake) Devices(context.Context) ([]dataplane.Device, error) {
	done, err := f.read("Devices")
	if err != nil {
		return nil, err
	}
	defer done()
	var out []dataplane.Device
	for _, a := range f.arrays {
		if !a.running {
			continue
		}
		out = append(out, dataplane.Device{Name: a.name, Product: "md array", SizeBytes: 1 << 40})
		if a.pool == nil {
			continue
		}
		for _, v := range a.pool.sorted() {
			out = append(out, dataplane.Device{Name: a.pool.name + "/" + v.name, UUID: v.uuid, Aliases: []string{v.uuid}, Product: "LVM thin",
				SizeBytes: v.size, Origin: v.parent})
		}
	}
	for _, d := range f.disks {
		if d.present {
			out = append(out, dataplane.Device{Name: d.handle, Product: "disk", SizeBytes: 1 << 40})
		}
	}
	return out, nil
}

// --- arrays ---

func (f *Fake) arrayNamed(name string) *array {
	for _, a := range f.arrays {
		if a.name == name {
			return a
		}
	}
	return nil
}

// Arrays lists running arrays, and the stopped ones that have a claimed, current member waiting
// for the others (any that has a present member, here).
func (f *Fake) Arrays(context.Context) ([]dataplane.Array, error) {
	done, err := f.read("Arrays")
	if err != nil {
		return nil, err
	}
	defer done()
	var out []dataplane.Array
	for _, a := range f.arrays {
		if !a.running {
			if w, ok := f.waiting(a); ok {
				out = append(out, w)
			}
			continue
		}
		if a.percent > 0 || a.rebuilding() {
			f.advance(a)
		}
		x := dataplane.Array{Name: a.name, UUID: a.uuid, State: dataplane.ArrayOnline, Level: a.level, StripKB: a.stripKB,
			Slots: uint32(len(a.slots)), Members: make([]dataplane.ArrayMember, len(a.slots))}
		for i, s := range a.slots {
			if s.disk == nil {
				continue
			}
			// md's rebuild target sits in its slot, not in sync.
			x.Members[i] = dataplane.ArrayMember{Name: s.disk.handle, Configured: !s.rebuild}
			x.Discovered++
			if s.rebuild {
				x.Rebuild = &dataplane.Rebuild{Target: s.disk.handle, Percent: a.percent}
				continue
			}
			x.Operational++
		}
		if a.level == "raid0" {
			x.Operational = uint32(len(a.slots))
		}
		if a.scrub != nil {
			f.advanceScrub(a)
			sc := *a.scrub
			x.Scrub = &sc
		}
		out = append(out, x)
	}
	return out, nil
}

// waiting describes a stopped array's present members, its empty slots marked as awaited.
func (f *Fake) waiting(a *array) (dataplane.Array, bool) {
	x := dataplane.Array{Name: a.name, UUID: a.uuid, State: dataplane.ArrayConfiguring, Level: a.level, StripKB: a.stripKB,
		Slots: uint32(len(a.slots)), Members: make([]dataplane.ArrayMember, len(a.slots))}
	for i := range x.Members {
		x.Members[i].UUID = "missing"
	}
	var newest uint64
	for _, d := range f.disks {
		if d.present && d.member != nil && d.member.array == a.uuid {
			newest = max(newest, d.member.events)
		}
	}
	for _, d := range f.disks {
		if d.present && d.member != nil && d.member.array == a.uuid && d.member.events == newest {
			x.Members[d.member.slot] = dataplane.ArrayMember{Name: d.handle, UUID: a.uuid, Configured: true}
			x.Discovered++
		}
	}
	x.Operational = x.Discovered
	return x, x.Discovered > 0
}

func (a *array) rebuilding() bool {
	return slices.ContainsFunc(a.slots, func(s *slot) bool { return s.rebuild })
}

// advance finishes a rebuild that reached 100%.
func (f *Fake) advance(a *array) {
	if a.percent < 100 {
		return
	}
	for _, s := range a.slots {
		s.rebuild = false
	}
	a.percent = 0
	f.stamp(a)
}

// SetRebuildProgress sets how far an array's rebuild is; 100 finishes it.
func (f *Fake) SetRebuildProgress(name string, percent uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.arrayNamed(name)
	if a == nil || !a.rebuilding() {
		panic("planetest: " + name + " isn't rebuilding")
	}
	a.percent = percent
}

func (f *Fake) CheckLevel(level store.RaidLevel, disks int) error {
	minDisks := map[store.RaidLevel]int{store.RaidLevel0: 1, store.RaidLevel1: 2, store.RaidLevel10: 4}
	n, ok := minDisks[level]
	if !ok {
		return fmt.Errorf("%w: unsupported raid_level %q (the kernel data plane builds RAID0, RAID1 and RAID10)", store.ErrInvalidArgument, level)
	}
	if disks < n {
		return fmt.Errorf("%w: %s requires at least %d disks", store.ErrInvalidArgument, level, n)
	}
	return nil
}

func (f *Fake) CreateArray(_ context.Context, name string, level store.RaidLevel, stripKB uint32, members []string) error {
	done, err := f.begin("CreateArray")
	if err != nil {
		return err
	}
	if f.arrayNamed(name) != nil {
		return done(fmt.Errorf("array %s exists", name))
	}
	a := &array{name: name, uuid: newUUID(), level: strings.ToLower(string(level)), stripKB: stripKB, running: true}
	for _, m := range members {
		d := f.byHandle(m)
		if d == nil || !d.present {
			return done(notFound("disk %s not found", m))
		}
		a.slots = append(a.slots, &slot{disk: d})
	}
	f.arrays = append(f.arrays, a)
	f.stamp(a)
	return done(nil)
}

func (f *Fake) DeleteArray(_ context.Context, name string) error {
	done, err := f.begin("DeleteArray")
	if err != nil {
		return err
	}
	i := slices.IndexFunc(f.arrays, func(a *array) bool { return a.name == name })
	if i < 0 {
		return done(notFound("array %s not found", name))
	}
	a := f.arrays[i]
	if a.pool != nil {
		return done(fmt.Errorf("array %s is in use by thin pool %s", name, a.pool.name))
	}
	for _, s := range a.slots {
		if s.disk != nil {
			s.disk.member = nil
		}
	}
	f.arrays = slices.Delete(f.arrays, i, i+1)
	return done(nil)
}

// AddMember adds a disk to an array's first empty slot, where it rebuilds.
func (f *Fake) AddMember(_ context.Context, name, handle string) error {
	done, err := f.begin("AddMember")
	if err != nil {
		return err
	}
	a, d := f.arrayNamed(name), f.byHandle(handle)
	if a == nil || !a.running {
		return done(notFound("array %s not found", name))
	}
	if d == nil || !d.present {
		return done(notFound("disk %s not found", handle))
	}
	for i, s := range a.slots {
		if s.disk == nil {
			a.slots[i] = &slot{disk: d, rebuild: true}
			a.percent = 0
			f.stamp(a)
			return done(nil)
		}
	}
	return done(fmt.Errorf("array %s has no empty slot", name))
}

func (f *Fake) RemoveMember(_ context.Context, handle string) error {
	done, err := f.begin("RemoveMember")
	if err != nil {
		return err
	}
	for _, a := range f.arrays {
		for _, s := range a.slots {
			if a.running && s.disk != nil && s.disk.handle == handle {
				s.disk, s.rebuild = nil, false
				a.percent = 0
				f.stamp(a)
				return done(nil)
			}
		}
	}
	return done(notFound("%s is not in an array", handle))
}

func (f *Fake) StartDegraded(_ context.Context, name string) error {
	done, err := f.begin("StartDegraded")
	if err != nil {
		return err
	}
	a := f.arrayNamed(name)
	if a == nil || a.running {
		return done(notFound("array %s is not waiting for members", name))
	}
	if _, ok := f.waiting(a); !ok {
		return done(notFound("array %s is not waiting for members", name))
	}
	var all []string
	for _, d := range f.disks {
		all = append(all, d.handle)
	}
	if !f.assemble(a, all, true) {
		return done(fmt.Errorf("array %s has no current member", name))
	}
	a.running = true
	return done(nil)
}

func (f *Fake) RebuildLimit(context.Context) (uint32, error) {
	done, err := f.read("RebuildLimit")
	if err != nil {
		return 0, err
	}
	defer done()
	return f.rebuildLimit, nil
}

func (f *Fake) SetRebuildLimit(_ context.Context, mbps uint32) error {
	done, err := f.begin("SetRebuildLimit")
	if err != nil {
		return err
	}
	f.rebuildLimit = mbps
	return done(nil)
}

// --- scrubs ---

const scrubBlocks = 1 << 20

// SetScrubStep makes every listing of the arrays move running scrubs on by blocks (0: not at all).
func (f *Fake) SetScrubStep(blocks uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scrubStep = blocks
}

// SetScrubFindings sets what a running scrub of the array has found.
func (f *Fake) SetScrubFindings(name string, repaired, mismatched, unrecoverable uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.arrayNamed(name).scrub
	s.RepairedBlocks, s.MismatchedBlocks, s.MismatchRepairedBlocks, s.UnrecoverableBlocks = repaired, mismatched, mismatched, unrecoverable
}

// ForgetScrubs makes the arrays forget their scrubs, as a reboot does.
func (f *Fake) ForgetScrubs() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.arrays {
		a.scrub = nil
	}
}

// ScrubStarts lists the offsets an array's scrubs started at.
func (f *Fake) ScrubStarts(name string) []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.arrayNamed(name).scrubStarts)
}

func (f *Fake) advanceScrub(a *array) {
	s := a.scrub
	if s.State != dataplane.ScrubRunning || f.scrubStep == 0 {
		return
	}
	s.OffsetBlocks = min(s.OffsetBlocks+f.scrubStep, s.TotalBlocks)
	if s.OffsetBlocks == s.TotalBlocks {
		s.State = dataplane.ScrubCompleted
	}
}

func (f *Fake) StartScrub(_ context.Context, name string, repair bool, _ uint32, offset uint64) error {
	done, err := f.begin("StartScrub")
	if err != nil {
		return err
	}
	a := f.arrayNamed(name)
	if a == nil || !a.running {
		return done(notFound("array %s not found", name))
	}
	if a.survivors() < len(a.slots) && a.level != "raid0" {
		return done(fmt.Errorf("array %s is degraded", name))
	}
	a.scrub = &dataplane.Scrub{State: dataplane.ScrubRunning, Repair: repair, StartOffsetBlocks: offset, OffsetBlocks: offset, TotalBlocks: scrubBlocks}
	a.scrubStarts = append(a.scrubStarts, offset)
	return done(nil)
}

func (f *Fake) StopScrub(_ context.Context, name string) error {
	done, err := f.begin("StopScrub")
	if err != nil {
		return err
	}
	a := f.arrayNamed(name)
	if a == nil {
		return done(notFound("array %s not found", name))
	}
	if a.scrub != nil && a.scrub.State == dataplane.ScrubRunning {
		a.scrub.State, a.scrub.Status = dataplane.ScrubStopped, dataplane.ScrubStatusCanceled
	}
	return done(nil)
}

// --- thin pools ---

func (p *pool) sorted() []*volume {
	var out []*volume
	for _, v := range p.volumes {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b *volume) int { return a.order - b.order })
	return out
}

// used is how many of the pool's chunks any volume maps.
func (p *pool) used() uint64 {
	seen := map[uint64]bool{}
	for _, v := range p.volumes {
		if !v.thin {
			continue
		}
		for _, c := range v.chunks {
			seen[c] = true
		}
	}
	n := uint64(len(seen))
	for _, v := range p.volumes {
		if !v.thin {
			n += v.size / ChunkBytes
		}
	}
	return n
}

func (f *Fake) ThinPools(_ context.Context, name, uuid string) ([]dataplane.ThinPool, error) {
	done, err := f.read("ThinPools")
	if err != nil {
		return nil, err
	}
	defer done()
	var out []dataplane.ThinPool
	for _, a := range f.arrays {
		if !a.running || a.pool == nil || name != "" && a.pool.name != name || uuid != "" && a.pool.uuid != uuid {
			continue
		}
		p := a.pool
		out = append(out, dataplane.ThinPool{UUID: p.uuid, Name: p.name, Array: a.name, ChunkBytes: ChunkBytes,
			TotalBytes: p.chunks * ChunkBytes, FreeBytes: (p.chunks - min(p.used(), p.chunks)) * ChunkBytes,
			MetadataBytes: p.metaBytes, MetadataUsedBytes: p.metaUsed})
	}
	return out, nil
}

func (f *Fake) CreateThinPool(_ context.Context, arrayName, name string) (string, error) {
	done, err := f.begin("CreateThinPool")
	if err != nil {
		return "", err
	}
	a := f.arrayNamed(arrayName)
	if a == nil || !a.running {
		return "", done(notFound("array %s not found", arrayName))
	}
	if a.pool != nil {
		return "", done(fmt.Errorf("array %s already holds thin pool %s", arrayName, a.pool.name))
	}
	f.pools[name]++
	a.pool = &pool{name: name, uuid: newUUID(), chunks: defaultChunks, generation: f.pools[name], volumes: map[string]*volume{},
		metaBytes: 64 << 20, metaUsed: 1 << 20}
	if err := done(nil); err != nil {
		return "", err
	}
	return a.pool.uuid, nil
}

// PoolGeneration is how often a thin pool was created under a name.
func (f *Fake) PoolGeneration(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pools[name]
}

// SetPoolChunks sets a thin pool's size in chunks.
func (f *Fake) SetPoolChunks(name string, n uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.poolNamed(name).chunks = n
}

func (f *Fake) poolNamed(name string) *pool {
	for _, a := range f.arrays {
		if a.pool != nil && a.pool.name == name {
			return a.pool
		}
	}
	panic("planetest: no thin pool " + name)
}

func (f *Fake) GrowMetadata(_ context.Context, uuid string) (uint64, error) {
	done, err := f.read("GrowMetadata")
	if err != nil {
		return 0, err
	}
	defer done()
	return 0, nil
}

func (f *Fake) LostData(context.Context, string) ([]dataplane.LostExtent, error) { return nil, nil }

func (f *Fake) DeleteThinPool(_ context.Context, uuid, name string) error {
	done, err := f.begin("DeleteThinPool")
	if err != nil {
		return err
	}
	for _, a := range f.arrays {
		if a.pool != nil && (uuid != "" && a.pool.uuid == uuid || uuid == "" && a.pool.name == name) {
			for nqn, t := range f.targets {
				for _, v := range a.pool.volumes {
					if t.Device == v.uuid {
						return done(fmt.Errorf("volume %s is exported as %s", v.name, nqn))
					}
				}
			}
			a.pool = nil
			return done(nil)
		}
	}
	return done(notFound("thin pool %s%s not found", uuid, name))
}

// --- volumes ---

// resolve finds a volume by UUID or "pool/name" in the running pools.
func (f *Fake) resolve(ref string) *volume {
	for _, a := range f.arrays {
		if !a.running || a.pool == nil {
			continue
		}
		for _, v := range a.pool.volumes {
			if v.uuid == ref || a.pool.name+"/"+v.name == ref {
				return v
			}
		}
	}
	return nil
}

func (f *Fake) Volumes(context.Context) ([]dataplane.Volume, error) {
	done, err := f.read("Volumes")
	if err != nil {
		return nil, err
	}
	defer done()
	var out []dataplane.Volume
	for _, a := range f.arrays {
		if !a.running || a.pool == nil {
			continue
		}
		for _, v := range a.pool.sorted() {
			alloc := uint64(len(v.chunks)) * ChunkBytes
			if !v.thin {
				alloc = v.size
			}
			out = append(out, dataplane.Volume{UUID: v.uuid, Name: v.name, Alias: a.pool.name + "/" + v.name, Pool: a.pool.uuid,
				Thin: v.thin, Snapshot: v.snapshot, AllocatedBytes: alloc, Seq: v.order})
		}
	}
	return out, nil
}

func (f *Fake) newVolume(p *pool, name string, size uint64, thin bool) (*volume, error) {
	if _, ok := p.volumes[name]; ok {
		return nil, fmt.Errorf("volume %s/%s exists", p.name, name)
	}
	f.volumes++
	v := &volume{uuid: newUUID(), name: name, pool: p, size: (size + extent - 1) / extent * extent, thin: thin, chunks: map[uint64]uint64{}, order: f.volumes}
	p.volumes[name] = v
	return v, nil
}

func (f *Fake) CreateVolume(_ context.Context, poolName, name string, size uint64, thin bool) (string, error) {
	done, err := f.begin("CreateVolume")
	if err != nil {
		return "", err
	}
	for _, a := range f.arrays {
		if a.running && a.pool != nil && a.pool.name == poolName {
			v, err := f.newVolume(a.pool, name, size, thin)
			if err = done(err); err != nil {
				return "", err
			}
			return v.uuid, nil
		}
	}
	return "", done(notFound("thin pool %s not found", poolName))
}

func (f *Fake) ResizeVolume(_ context.Context, ref string, size uint64) error {
	done, err := f.begin("ResizeVolume")
	if err != nil {
		return err
	}
	v := f.resolve(ref)
	if v == nil {
		return done(notFound("volume %s not found", ref))
	}
	v.size = (size + extent - 1) / extent * extent
	return done(nil)
}

func (f *Fake) DeleteVolume(_ context.Context, ref string) error {
	done, err := f.begin("DeleteVolume")
	if err != nil {
		return err
	}
	v := f.resolve(ref)
	if v == nil {
		return done(notFound("volume %s not found", ref))
	}
	for name, t := range f.targets {
		if t.Device == v.uuid {
			return done(fmt.Errorf("volume %s is exported as %s", v.name, name))
		}
	}
	delete(v.pool.volumes, v.name)
	for _, o := range v.pool.volumes { // as LVM does
		if o.parent == v.name {
			o.parent = ""
		}
	}
	return done(nil)
}

func copyChunks(m map[uint64]uint64) map[uint64]uint64 {
	out := make(map[uint64]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (f *Fake) Snapshot(_ context.Context, ref, name string) (string, error) {
	done, err := f.begin("Snapshot")
	if err != nil {
		return "", err
	}
	s, err := f.snapshot(ref, name)
	if err = done(err); err != nil {
		return "", err
	}
	return s.uuid, nil
}

func (f *Fake) snapshot(ref, name string) (*volume, error) {
	v := f.resolve(ref)
	if v == nil {
		return nil, notFound("volume %s not found", ref)
	}
	s, err := f.newVolume(v.pool, name, v.size, true)
	if err != nil {
		return nil, err
	}
	s.snapshot, s.parent, s.chunks = true, v.name, copyChunks(v.chunks)
	return s, nil
}

func (f *Fake) SnapshotGroup(_ context.Context, entries []dataplane.GroupEntry, _ time.Duration) ([]string, error) {
	done, err := f.begin("SnapshotGroup")
	if err != nil {
		return nil, err
	}
	var made []*volume
	for _, e := range entries {
		s, err := f.snapshot(e.Volume, e.Snapshot)
		if err != nil {
			for _, m := range made {
				delete(m.pool.volumes, m.name)
			}
			return nil, done(err)
		}
		made = append(made, s)
	}
	var out []string
	for _, m := range made {
		out = append(out, m.uuid)
	}
	if err := done(nil); err != nil {
		return nil, err
	}
	return out, nil
}

// MaxSnapshotGroup: small, so a test can exceed it.
func (f *Fake) MaxSnapshotGroup() int { return 64 }

func (f *Fake) Clone(_ context.Context, ref, name string) (string, error) {
	done, err := f.begin("Clone")
	if err != nil {
		return "", err
	}
	s := f.resolve(ref)
	if s == nil || !s.snapshot {
		return "", done(notFound("snapshot %s not found", ref))
	}
	v, err := f.newVolume(s.pool, name, s.size, true)
	if err == nil {
		v.parent, v.chunks = s.name, copyChunks(s.chunks)
	}
	if err = done(err); err != nil {
		return "", err
	}
	return v.uuid, nil
}

// WriteChunk writes a chunk of a volume, as a host does: a new pool chunk, unless the volume
// already has that chunk to itself.
func (f *Fake) WriteChunk(ref string, idx uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.resolve(ref)
	if v == nil {
		panic("planetest: no volume " + ref)
	}
	if c, ok := v.chunks[idx]; ok && !v.sharesChunk(c) {
		return
	}
	v.pool.nextChunk++
	v.chunks[idx] = v.pool.nextChunk
}

func (v *volume) sharesChunk(c uint64) bool {
	for _, o := range v.pool.volumes {
		if o == v {
			continue
		}
		for _, x := range o.chunks {
			if x == c {
				return true
			}
		}
	}
	return false
}

// VolumeExists reports whether a volume or snapshot exists (by UUID or "pool/name"), also in a
// pool that isn't running; VolumeCount counts them all; VolumeSize is a volume's size.
func (f *Fake) VolumeExists(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.find(ref) != nil
}

func (f *Fake) find(ref string) *volume {
	for _, a := range f.arrays {
		if a.pool == nil {
			continue
		}
		for _, v := range a.pool.volumes {
			if v.uuid == ref || a.pool.name+"/"+v.name == ref {
				return v
			}
		}
	}
	return nil
}

func (f *Fake) VolumeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, a := range f.arrays {
		if a.pool != nil {
			n += len(a.pool.volumes)
		}
	}
	return n
}

func (f *Fake) VolumeSize(ref string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v := f.find(ref); v != nil {
		return v.size
	}
	return 0
}

func merge(idx []uint64, size uint64) []dataplane.Extent {
	slices.Sort(idx)
	var out []dataplane.Extent
	for _, i := range idx {
		off := i * ChunkBytes
		if off >= size {
			continue
		}
		n := min(ChunkBytes, size-off)
		if k := len(out); k > 0 && out[k-1].OffsetBytes+out[k-1].SizeBytes == off {
			out[k-1].SizeBytes += n
			continue
		}
		out = append(out, dataplane.Extent{OffsetBytes: off, SizeBytes: n})
	}
	return out
}

func (f *Fake) AllocatedBlocks(_ context.Context, _ *store.Pool, target *store.Snapshot) ([]dataplane.Extent, error) {
	done, err := f.read("AllocatedBlocks")
	if err != nil {
		return nil, err
	}
	defer done()
	t := f.resolve(target.UUID)
	if t == nil {
		return nil, notFound("snapshot %s not found", target.UUID)
	}
	var idx []uint64
	for i := range t.chunks {
		idx = append(idx, i)
	}
	return merge(idx, t.size), nil
}

func (f *Fake) ChangedBlocks(_ context.Context, _ *store.Pool, base, target *store.Snapshot) ([]dataplane.Extent, error) {
	done, err := f.read("ChangedBlocks")
	if err != nil {
		return nil, err
	}
	defer done()
	b, t := f.resolve(base.UUID), f.resolve(target.UUID)
	if b == nil || t == nil {
		return nil, notFound("snapshot not found")
	}
	if b.order >= t.order {
		return nil, fmt.Errorf("%w: snapshot %s is not older than %s in its pool", store.ErrInvalidArgument, base.ID, target.ID)
	}
	var idx []uint64
	for i, c := range t.chunks {
		if b.chunks[i] != c {
			idx = append(idx, i)
		}
	}
	for i := range b.chunks {
		if _, ok := t.chunks[i]; !ok {
			idx = append(idx, i)
		}
	}
	return merge(idx, t.size), nil
}

// --- exports ---

// Target is an export's target, by NQN or IQN.
func (f *Fake) Target(name string) (Target, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.targets[name]
	if !ok {
		return Target{}, false
	}
	return *t, true
}

// Targets lists the targets' names.
func (f *Fake) Targets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.targets {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// AddTarget makes a target velod has no record of.
func (f *Fake) AddTarget(name string, t Target) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets[name] = &t
}

// Connect connects a host (by NQN, IQN or address) to a target.
func (f *Fake) Connect(target, host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns[target] = append(f.conns[target], dataplane.Connection{Host: host})
}

func targetName(exp *store.Export) string {
	if dataplane.IsNVMe(exp.Protocol) {
		return exp.SubsystemNQN
	}
	return exp.TargetIQN
}

// ApplyExport converges the target, unless nothing changes (then it isn't recorded as a call).
func (f *Fake) ApplyExport(_ context.Context, exp *store.Export, vol *store.Volume, acl dataplane.ACL, portals []*store.Portal) error {
	f.mu.Lock()
	if f.Down != nil {
		defer f.mu.Unlock()
		return f.Down
	}
	if f.failN["ApplyExport"] > 0 {
		f.failN["ApplyExport"]--
		err := f.failErr["ApplyExport"]
		f.mu.Unlock()
		return fmt.Errorf("ApplyExport: %w", err)
	}
	defer f.mu.Unlock()
	v := f.resolve(vol.UUID)
	if v == nil {
		if v = f.resolve(vol.Handle); v == nil {
			return notFound("volume %s not found", vol.UUID)
		}
	}
	if exp.Protocol == store.ExportProtocolNVMeoFRDMA {
		return fmt.Errorf("%w: NVMe over RDMA", dataplane.ErrUnsupported)
	}
	want := &Target{Device: v.uuid, AnyHost: exp.AllowAnyHost}
	if dataplane.IsNVMe(exp.Protocol) {
		want.Hosts = acl.HostNQNs
	} else {
		if !exp.AllowAnyHost && len(acl.IQNs) == 0 {
			return fmt.Errorf("%w: the kernel iSCSI target allows initiators by IQN only, and export %s has none (only addresses)",
				store.ErrInvalidArgument, exp.ID)
		}
		want.Hosts = acl.IQNs
	}
	var missing []error
	for _, pt := range portals {
		if !dataplane.AddressPresent(pt.IPAddress) {
			missing = append(missing, &dataplane.AddressMissingError{Portal: pt})
			continue
		}
		want.Listeners = append(want.Listeners, fmt.Sprintf("%s:%d", pt.IPAddress, pt.Port))
	}
	name := targetName(exp)
	cur := f.targets[name]
	if cur != nil && cur.Device != want.Device {
		return fmt.Errorf("target %s exports %s, not %s; refusing to repoint it", name, cur.Device, want.Device)
	}
	if cur == nil || !equalTarget(cur, want) {
		f.calls = append(f.calls, "ApplyExport")
		f.targets[name] = want
	}
	return errors.Join(missing...)
}

func equalTarget(a, b *Target) bool {
	sorted := func(s []string) []string { s = slices.Clone(s); slices.Sort(s); return s }
	return a.Device == b.Device && a.AnyHost == b.AnyHost && slices.Equal(sorted(a.Hosts), sorted(b.Hosts)) &&
		slices.Equal(sorted(a.Listeners), sorted(b.Listeners))
}

func (f *Fake) RemoveTarget(_ context.Context, exp *store.Export) error {
	done, err := f.begin("RemoveTarget")
	if err != nil {
		return err
	}
	delete(f.targets, targetName(exp))
	delete(f.conns, targetName(exp))
	return done(nil)
}

func (f *Fake) Connections(_ context.Context, exp *store.Export) ([]dataplane.Connection, error) {
	done, err := f.read("Connections")
	if err != nil {
		return nil, err
	}
	defer done()
	return append([]dataplane.Connection{}, f.conns[targetName(exp)]...), nil
}

func (f *Fake) IOStats(_ context.Context, device string) (*dataplane.IOStats, error) {
	done, err := f.read("IOStats")
	if err != nil {
		return nil, err
	}
	defer done()
	out := &dataplane.IOStats{TickRate: 1000, Devices: []dataplane.DeviceIO{}}
	add := func(name string) {
		if device == "" || device == name {
			out.Devices = append(out.Devices, dataplane.DeviceIO{Name: name})
		}
	}
	for _, a := range f.arrays {
		if !a.running {
			continue
		}
		add(a.name)
		if a.pool != nil {
			for _, v := range a.pool.sorted() {
				if !v.snapshot {
					add(a.pool.name + "/" + v.name)
				}
			}
		}
	}
	if device != "" && len(out.Devices) == 0 {
		return nil, notFound("device %s not found", device)
	}
	return out, nil
}

// Unmanaged reports arrays, thin pools, volumes and targets velod has no record of, with the
// kernel data plane's kinds.
func (f *Fake) Unmanaged(_ context.Context, known dataplane.Known) ([]dataplane.UnmanagedObject, error) {
	done, err := f.read("Unmanaged")
	if err != nil {
		return nil, err
	}
	defer done()
	var out []dataplane.UnmanagedObject
	for _, a := range f.arrays {
		if !a.running {
			continue
		}
		if !known.Devices[a.name] {
			out = append(out, dataplane.UnmanagedObject{Kind: "MD_ARRAY", Name: a.name})
		}
		if a.pool == nil {
			continue
		}
		if !known.ThinPools[a.pool.uuid] {
			out = append(out, dataplane.UnmanagedObject{Kind: "VOLUME_GROUP", Name: a.pool.name, UUID: a.pool.uuid})
			continue
		}
		for _, v := range a.pool.sorted() {
			if !known.Volumes[v.uuid] && !known.Skip[v.name] {
				out = append(out, dataplane.UnmanagedObject{Kind: "LV", Name: a.pool.name + "/" + v.name, UUID: v.uuid})
			}
		}
	}
	var names []string
	for n := range f.targets {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if known.Targets[n] {
			continue
		}
		kind := "ISCSI_TARGET"
		if strings.HasPrefix(n, "nqn.") {
			kind = "NVMET_SUBSYSTEM"
		}
		out = append(out, dataplane.UnmanagedObject{Kind: kind, Name: n})
	}
	return out, nil
}
