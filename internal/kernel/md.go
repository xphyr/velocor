package kernel

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Plane drives the kernel data plane: md, LVM, device-mapper and nvmet.
type Plane struct {
	Run Runner
	// SysRoot is where sysfs is mounted ("/sys"); configfs is expected at SysRoot/kernel/config.
	SysRoot string
	// DevRoot is "/dev"; md arrays appear as DevRoot/md/<name>.
	DevRoot string
	// InventoryTTL is how long LVM's reports may be reused (0: not at all); anything this Plane
	// changes drops them at once. They say how full volumes are, which changes with I/O.
	InventoryTTL time.Duration

	inv inventory
}

// New returns a Plane on this host's /sys and /dev.
func New(r Runner) *Plane {
	return &Plane{Run: r, SysRoot: "/sys", DevRoot: "/dev", InventoryTTL: 2 * time.Second}
}

// ArraySpec describes an md array to create.
type ArraySpec struct {
	Name    string   // array name: DevRoot/md/<Name>
	Level   string   // "raid0", "raid1" or "raid10"
	Devices []string // member block devices
	// ChunkKB is the chunk size of a striped level (raid0, raid10); 0 leaves mdadm's default.
	ChunkKB uint32
	// AssumeClean skips the initial resync. Only for tests: a production array resyncs so the
	// members agree before data is written (md's write-intent bitmap keeps later resyncs short).
	AssumeClean bool
}

// ArrayPath is where an array named name appears.
func (p *Plane) ArrayPath(name string) string { return filepath.Join(p.DevRoot, "md", name) }

// CreateArray creates and starts an md array with 1.2 metadata and an internal write-intent
// bitmap (resync after an unclean stop covers only dirty regions). It returns the array's UUID.
// A read error a good copy can't fix fails only that read; on the last in-sync member md returns
// the error to the reader and keeps the array running (TODO.md P0-7, checked by
// TestLastCopyReadErrorKeepsThePool).
func (p *Plane) CreateArray(ctx context.Context, s ArraySpec) (string, error) {
	defer p.DropInventory()
	if s.Level != "raid0" && s.Level != "raid1" && s.Level != "raid10" {
		return "", fmt.Errorf("unsupported level %q", s.Level)
	}
	if len(s.Devices) < 2 && s.Level != "raid0" {
		return "", fmt.Errorf("an array needs at least two devices, got %d", len(s.Devices))
	}
	args := []string{"--create", p.ArrayPath(s.Name), "--name=" + s.Name, "--homehost=any",
		"--level=" + s.Level, "--raid-devices=" + strconv.Itoa(len(s.Devices)),
		"--metadata=1.2", "--run", "--quiet"}
	if s.Level != "raid0" { // raid0 has no redundancy to resync, so no bitmap
		args = append(args, "--bitmap=internal")
	}
	if len(s.Devices) == 1 {
		args = append(args, "--force") // mdadm wants it for a one-device raid0
	}
	if s.ChunkKB > 0 && s.Level != "raid1" {
		args = append(args, "--chunk="+strconv.FormatUint(uint64(s.ChunkKB), 10))
	}
	if s.AssumeClean {
		args = append(args, "--assume-clean")
	}
	args = append(args, s.Devices...)
	if _, err := p.Run.Run(ctx, "mdadm", args...); err != nil {
		return "", err
	}
	st, err := p.ArrayStatus(s.Name)
	if err != nil {
		return "", err
	}
	return st.UUID, nil
}

// AssembleArray starts an existing array from its members. Without allowDegraded mdadm starts it
// only with every member present (an operator decides on a degraded start: TODO.md P1-8).
func (p *Plane) AssembleArray(ctx context.Context, name, uuid string, devices []string, allowDegraded bool) error {
	defer p.DropInventory()
	args := []string{"--assemble", p.ArrayPath(name), "--uuid=" + uuid, "--quiet"}
	if allowDegraded {
		args = append(args, "--run")
	}
	_, err := p.Run.Run(ctx, "mdadm", append(args, devices...)...)
	return err
}

// StopArray stops an array (its volume group must be deactivated first).
func (p *Plane) StopArray(ctx context.Context, name string) error {
	defer p.DropInventory()
	_, err := p.Run.Run(ctx, "mdadm", "--stop", p.ArrayPath(name))
	return err
}

// FailMember marks a member faulty; RemoveMember takes a faulty or spare member out of the array;
// AddMember adds a disk, which md rebuilds onto when a slot is free.
func (p *Plane) FailMember(ctx context.Context, name, dev string) error {
	_, err := p.Run.Run(ctx, "mdadm", p.ArrayPath(name), "--fail", dev)
	return err
}

func (p *Plane) RemoveMember(ctx context.Context, name, dev string) error {
	_, err := p.Run.Run(ctx, "mdadm", p.ArrayPath(name), "--remove", dev)
	return err
}

func (p *Plane) AddMember(ctx context.Context, name, dev string) error {
	_, err := p.Run.Run(ctx, "mdadm", p.ArrayPath(name), "--add", dev)
	return err
}

// StartScrub starts md's own scrub: "check" counts mismatches, "repair" also rewrites them; a
// read error on one member is repaired from another either way. StopScrub stops it.
func (p *Plane) StartScrub(name string, repair bool) error {
	action := "check"
	if repair {
		action = "repair"
	}
	return p.writeMD(name, "sync_action", action)
}

func (p *Plane) StopScrub(name string) error { return p.writeMD(name, "sync_action", "idle") }

// SetSyncMin makes the next sync (a scrub) start at sector: md resets it when the sync ends.
func (p *Plane) SetSyncMin(name string, sector uint64) error {
	return p.writeMD(name, "sync_min", strconv.FormatUint(sector, 10))
}

// SetSyncSpeedMax limits resync, rebuild and scrub to kbps KiB/s ("system" restores the default).
func (p *Plane) SetSyncSpeedMax(name, kbps string) error {
	return p.writeMD(name, "sync_speed_max", kbps)
}

// ArrayStatus is an array's state from sysfs.
type ArrayStatus struct {
	Kernel     string // md127
	UUID       string // as mdadm writes it: 8:8:8:8 hex
	Level      string
	RaidDisks  int
	Degraded   int    // members missing or failed
	ArrayState string // clean, active, active-idle, ... ; "inactive" when not running
	SyncAction string // idle, resync, recover, check, repair, reshape, frozen
	// LastSyncAction is the last sync action started; after a check or repair finishes,
	// SyncAction is idle and this stays check or repair.
	LastSyncAction string
	SyncDone       uint64 // sectors done of SyncTotal while SyncAction isn't idle
	SyncTotal      uint64
	MismatchCnt    uint64 // sectors found different by the last check/repair
	ChunkSectors   uint64 // chunk size of a striped level (raid0, raid10), in sectors
	Layout         int    // raid10: md's layout number (0x102: near-2)
	Members        []MemberStatus
}

// SectorRange is a range of 512-byte sectors.
type SectorRange struct {
	Start, Sectors uint64
}

// MemberStatus is one member device's state.
type MemberStatus struct {
	Device string   // kernel name: loop0, sdb, dm-3
	Slot   int      // -1 when not in a slot (spare, faulty)
	State  []string // in_sync, faulty, blocked, spare, write_mostly, want_replacement, ...
	// BadBlocks is md's bad-block log for the member: ranges it couldn't read and couldn't
	// rewrite. md fails reads there (and doesn't write there) and serves them from another
	// copy if one has them. Relative to the member's data, as Offset is subtracted.
	BadBlocks []SectorRange
	Offset    uint64 // where the member's data starts on the device (sectors)
	Size      uint64 // the member's data size (sectors)
}

// BadSectors is the total size of the member's bad blocks, in sectors.
func (m MemberStatus) BadSectors() uint64 {
	var n uint64
	for _, r := range m.BadBlocks {
		n += r.Sectors
	}
	return n
}

// Has reports whether the member's state has flag.
func (m MemberStatus) Has(flag string) bool {
	for _, s := range m.State {
		if s == flag {
			return true
		}
	}
	return false
}

// ArrayStatus reads an array's state.
func (p *Plane) ArrayStatus(name string) (*ArrayStatus, error) {
	kernel, err := p.arrayKernelName(name)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(p.SysRoot, "block", kernel, "md")
	rd := func(f string) string {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		return strings.TrimSpace(string(b))
	}
	st := &ArrayStatus{
		Kernel:     kernel,
		UUID:       mdadmUUID(rd("uuid")),
		Level:      rd("level"),
		ArrayState: rd("array_state"),
		SyncAction: rd("sync_action"),

		LastSyncAction: rd("last_sync_action"),
	}
	st.RaidDisks, _ = strconv.Atoi(rd("raid_disks"))
	st.Degraded, _ = strconv.Atoi(rd("degraded"))
	st.MismatchCnt, _ = strconv.ParseUint(rd("mismatch_cnt"), 10, 64)
	if b, err := strconv.ParseUint(rd("chunk_size"), 10, 64); err == nil {
		st.ChunkSectors = b / 512
	}
	st.Layout, _ = strconv.Atoi(rd("layout"))
	if done, total, ok := strings.Cut(rd("sync_completed"), "/"); ok {
		st.SyncDone, _ = strconv.ParseUint(strings.TrimSpace(done), 10, 64)
		st.SyncTotal, _ = strconv.ParseUint(strings.TrimSpace(total), 10, 64)
	}
	devs, _ := filepath.Glob(filepath.Join(dir, "dev-*"))
	for _, d := range devs {
		m := MemberStatus{Device: strings.TrimPrefix(filepath.Base(d), "dev-"), Slot: -1}
		b, _ := os.ReadFile(filepath.Join(d, "state"))
		if s := strings.TrimSpace(string(b)); s != "" {
			m.State = strings.Split(s, ",")
		}
		b, _ = os.ReadFile(filepath.Join(d, "slot"))
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			m.Slot = n
		}
		b, _ = os.ReadFile(filepath.Join(d, "offset"))
		m.Offset, _ = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		b, _ = os.ReadFile(filepath.Join(d, "size")) // KiB
		if kib, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
			m.Size = kib * 2
		}
		// md records a bad block at once (unacknowledged) and in the superblock's log after.
		var log string
		for _, f := range []string{"bad_blocks", "unacknowledged_bad_blocks"} {
			b, _ = os.ReadFile(filepath.Join(d, f))
			log += string(b) + "\n"
		}
		m.BadBlocks = parseBadBlocks(log, m.Offset)
		st.Members = append(st.Members, m)
	}
	return st, nil
}

// parseBadBlocks parses md's bad_blocks file ("<sector> <length>" a line, sectors on the
// device) into ranges relative to the member's data, which starts at offset.
func parseBadBlocks(s string, offset uint64) []SectorRange {
	var out []SectorRange
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		start, err1 := strconv.ParseUint(f[0], 10, 64)
		n, err2 := strconv.ParseUint(f[1], 10, 64)
		if err1 != nil || err2 != nil || n == 0 {
			continue
		}
		if start < offset { // in the metadata area, before the data: holds no copy of anything
			if start+n <= offset {
				continue
			}
			n -= offset - start
			start = offset
		}
		out = append(out, SectorRange{start - offset, n})
	}
	// The two logs can overlap: each sector once.
	slices.SortFunc(out, func(a, b SectorRange) int { return cmp.Compare(a.Start, b.Start) })
	merged := out[:0]
	for _, r := range out {
		if n := len(merged); n > 0 && r.Start < merged[n-1].Start+merged[n-1].Sectors {
			last := &merged[n-1]
			last.Sectors = max(last.Start+last.Sectors, r.Start+r.Sectors) - last.Start
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// arrayKernelName resolves DevRoot/md/<name> to the kernel's md device (md127).
func (p *Plane) arrayKernelName(name string) (string, error) {
	target, err := filepath.EvalSymlinks(p.ArrayPath(name))
	if err != nil {
		return "", fmt.Errorf("array %s: %w", name, err)
	}
	return filepath.Base(target), nil
}

func (p *Plane) writeMD(name, file, value string) error {
	kernel, err := p.arrayKernelName(name)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(p.SysRoot, "block", kernel, "md", file), []byte(value), 0o644)
}

// mdadmUUID turns sysfs's 8-4-4-4-12 form into mdadm's 8:8:8:8 (what --uuid takes).
func mdadmUUID(sysfs string) string {
	h := strings.ReplaceAll(sysfs, "-", "")
	if len(h) != 32 {
		return sysfs
	}
	return h[0:8] + ":" + h[8:16] + ":" + h[16:24] + ":" + h[24:32]
}

// ListArrays returns the names of the running arrays (DevRoot/md/<name>).
func (p *Plane) ListArrays() []string {
	es, _ := os.ReadDir(filepath.Join(p.DevRoot, "md"))
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// ExaminedArray is an array found in its members' metadata (mdadm --examine).
type ExaminedArray struct {
	Name    string // without mdadm's "homehost:" prefix
	UUID    string // mdadm form
	Level   string
	Devices int      // the array's member count
	Found   []string // members present, as device paths
}

// ExamineArrays reads every block device's md metadata and returns the arrays it describes,
// running or not.
func (p *Plane) ExamineArrays(ctx context.Context) ([]ExaminedArray, error) {
	out, err := p.Run.Run(ctx, "mdadm", "--examine", "--scan", "--verbose", "--config=partitions")
	if err != nil {
		return nil, err
	}
	return parseExamineScan(string(out)), nil
}

// parseExamineScan parses "mdadm --examine --scan --verbose": an ARRAY line per array, its
// members on the "devices=" line after it.
func parseExamineScan(out string) []ExaminedArray {
	var arrays []ExaminedArray
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) >= 2 && f[0] == "ARRAY":
			a := ExaminedArray{Name: filepath.Base(f[1])}
			for _, kv := range f[2:] {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "UUID":
					a.UUID = v
				case "level":
					a.Level = v
				case "num-devices":
					a.Devices, _ = strconv.Atoi(v)
				case "name":
					_, a.Name, _ = strings.Cut(v, ":")
					if a.Name == "" {
						a.Name = v
					}
				}
			}
			arrays = append(arrays, a)
		case len(arrays) > 0:
			// "devices=/dev/sda,/dev/sdb", after "spares=1" on the same line if there are spares.
			for _, kv := range f {
				v, ok := strings.CutPrefix(kv, "devices=")
				if !ok {
					continue
				}
				for _, d := range strings.Split(v, ",") {
					if d != "" {
						arrays[len(arrays)-1].Found = append(arrays[len(arrays)-1].Found, d)
					}
				}
			}
		}
	}
	return arrays
}

// RunArray starts an assembled but inactive array with the members it has.
func (p *Plane) RunArray(ctx context.Context, name string) error {
	defer p.DropInventory()
	_, err := p.Run.Run(ctx, "mdadm", "--run", p.ArrayPath(name))
	return err
}

// MemberRole returns a device's slot in its array from its md metadata, or -1 for a spare (or
// a device md doesn't place).
func (p *Plane) MemberRole(ctx context.Context, dev string) (int, error) {
	out, err := p.Run.Run(ctx, "mdadm", "--examine", dev)
	if err != nil {
		return -1, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != "Device Role" {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(v), "Active device ")); err == nil {
			return n, nil
		}
		return -1, nil
	}
	return -1, fmt.Errorf("mdadm --examine %s: no device role", dev)
}

// ZeroSuperblock erases md metadata from a device that left its array.
func (p *Plane) ZeroSuperblock(ctx context.Context, dev string) error {
	_, err := p.Run.Run(ctx, "mdadm", "--zero-superblock", dev)
	return err
}

// MemberInfo is what one device's md metadata says (mdadm --examine).
type MemberInfo struct {
	ArrayUUID   string // mdadm form
	Name        string // without the "homehost:" prefix
	Level       string
	RaidDevices int
	ArrayKiB    uint64
	DeviceUUID  string
	// Events counts the array's metadata updates: a member that left the array keeps a lower
	// count than the members still in it.
	Events uint64
	Role   int // slot; -1 for a spare
	// ArrayState is this copy's view of the slots: "A" active, "." missing, "R" replacing.
	ArrayState string
}

// ExamineMember reads a device's md metadata; nil (no error) when it has none.
func (p *Plane) ExamineMember(ctx context.Context, dev string) (*MemberInfo, error) {
	out, err := p.Run.Run(ctx, "mdadm", "--examine", dev)
	var ce *CommandError
	if errors.As(err, &ce) && strings.Contains(ce.Stderr, "No md superblock") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseExamine(string(out))
}

// firstField is the first word of s, "" if it has none.
func firstField(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), " ")
	return s
}

func parseExamine(out string) (*MemberInfo, error) {
	m := &MemberInfo{Role: -1}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, " : ")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "Array UUID":
			m.ArrayUUID = v
		case "Name":
			m.Name = v
			if _, n, ok := strings.Cut(v, ":"); ok {
				m.Name = n
			}
			m.Name = firstField(m.Name) // "name  (local to host x)"
		case "Raid Level":
			m.Level = v
		case "Raid Devices":
			m.RaidDevices, _ = strconv.Atoi(v)
		case "Array Size":
			m.ArrayKiB, _ = strconv.ParseUint(firstField(v), 10, 64)
		case "Device UUID":
			m.DeviceUUID = v
		case "Events":
			m.Events, _ = strconv.ParseUint(v, 10, 64)
		case "Device Role":
			if n, err := strconv.Atoi(strings.TrimPrefix(v, "Active device ")); err == nil {
				m.Role = n
			}
		case "Array State":
			m.ArrayState = firstField(v)
		}
	}
	if m.ArrayUUID == "" {
		return nil, fmt.Errorf("mdadm --examine: no array UUID in %q", strings.TrimSpace(out))
	}
	return m, nil
}
