package kernel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Plane drives the kernel data plane: md, LVM, device-mapper and nvmet.
type Plane struct {
	Run Runner
	// SysRoot is where sysfs is mounted ("/sys"); configfs is expected at SysRoot/kernel/config.
	SysRoot string
	// DevRoot is "/dev"; md arrays appear as DevRoot/md/<name>.
	DevRoot string
}

// New returns a Plane on this host's /sys and /dev.
func New(r Runner) *Plane {
	return &Plane{Run: r, SysRoot: "/sys", DevRoot: "/dev"}
}

// ArraySpec describes an md array to create.
type ArraySpec struct {
	Name    string   // array name: DevRoot/md/<Name>
	Level   string   // "raid1" or "raid10"
	Devices []string // member block devices
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
	if s.Level != "raid1" && s.Level != "raid10" {
		return "", fmt.Errorf("unsupported level %q", s.Level)
	}
	if len(s.Devices) < 2 {
		return "", fmt.Errorf("an array needs at least two devices, got %d", len(s.Devices))
	}
	args := []string{"--create", p.ArrayPath(s.Name), "--name=" + s.Name, "--homehost=any",
		"--level=" + s.Level, "--raid-devices=" + strconv.Itoa(len(s.Devices)),
		"--metadata=1.2", "--bitmap=internal", "--run", "--quiet"}
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
	args := []string{"--assemble", p.ArrayPath(name), "--uuid=" + uuid, "--quiet"}
	if allowDegraded {
		args = append(args, "--run")
	}
	_, err := p.Run.Run(ctx, "mdadm", append(args, devices...)...)
	return err
}

// StopArray stops an array (its volume group must be deactivated first).
func (p *Plane) StopArray(ctx context.Context, name string) error {
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

// SetSyncSpeedMax limits resync, rebuild and scrub to kbps KiB/s ("system" restores the default).
func (p *Plane) SetSyncSpeedMax(name, kbps string) error {
	return p.writeMD(name, "sync_speed_max", kbps)
}

// ArrayStatus is an array's state from sysfs.
type ArrayStatus struct {
	Kernel      string // md127
	UUID        string // as mdadm writes it: 8:8:8:8 hex
	Level       string
	RaidDisks   int
	Degraded    int    // members missing or failed
	ArrayState  string // clean, active, active-idle, ... ; "inactive" when not running
	SyncAction  string // idle, resync, recover, check, repair, reshape, frozen
	SyncDone    uint64 // sectors done of SyncTotal while SyncAction isn't idle
	SyncTotal   uint64
	MismatchCnt uint64 // sectors found different by the last check/repair
	Members     []MemberStatus
}

// MemberStatus is one member device's state.
type MemberStatus struct {
	Device    string   // kernel name: loop0, sdb, dm-3
	Slot      int      // -1 when not in a slot (spare, faulty)
	State     []string // in_sync, faulty, blocked, spare, write_mostly, want_replacement, ...
	BadBlocks int      // entries in the bad-block log
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
	}
	st.RaidDisks, _ = strconv.Atoi(rd("raid_disks"))
	st.Degraded, _ = strconv.Atoi(rd("degraded"))
	st.MismatchCnt, _ = strconv.ParseUint(rd("mismatch_cnt"), 10, 64)
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
		b, _ = os.ReadFile(filepath.Join(d, "bad_blocks"))
		for _, l := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(l) != "" {
				m.BadBlocks++
			}
		}
		st.Members = append(st.Members, m)
	}
	return st, nil
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
