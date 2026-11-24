package kernelplane

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/kernel"
)

func TestFreshMembers(t *testing.T) {
	mi := func(role int, events uint64, state string) *kernel.MemberInfo {
		return &kernel.MemberInfo{RaidDevices: len(state), Role: role, Events: events, ArrayState: state}
	}
	for _, tc := range []struct {
		name     string
		infos    map[string]*kernel.MemberInfo
		fresh    []string
		complete bool
	}{
		{"healthy", map[string]*kernel.MemberInfo{"a": mi(0, 10, "AA"), "b": mi(1, 10, "AA")}, []string{"a", "b"}, true},
		{"one behind after an unclean stop", map[string]*kernel.MemberInfo{"a": mi(0, 11, "AA"), "b": mi(1, 10, "AA")}, []string{"a", "b"}, true},
		{"a member missing since it was stopped", map[string]*kernel.MemberInfo{"a": mi(0, 10, "AA")}, []string{"a"}, false},
		{"degraded before it was stopped", map[string]*kernel.MemberInfo{"a": mi(0, 20, "A.")}, []string{"a"}, true},
		{"degraded, the failed member back, stale", map[string]*kernel.MemberInfo{"a": mi(0, 20, "A."), "b": mi(1, 10, "AA")}, []string{"a"}, true},
		{"only the stale member", map[string]*kernel.MemberInfo{"b": mi(1, 10, "AA")}, []string{"b"}, false},
		{"the current member missing, an old one present", map[string]*kernel.MemberInfo{"a": mi(0, 30, "AA"), "old": mi(1, 12, "AA")}, []string{"a"}, false},
		{"rebuilding: the target is a fresh spare", map[string]*kernel.MemberInfo{"a": mi(0, 20, "A."), "c": mi(-1, 20, "A.")}, []string{"a", "c"}, true},
		{"raid10 without one member", map[string]*kernel.MemberInfo{"a": mi(0, 5, "A.AA"), "c": mi(2, 5, "A.AA"), "d": mi(3, 5, "A.AA")}, []string{"a", "c", "d"}, true},
		{"raid10, another one missing", map[string]*kernel.MemberInfo{"a": mi(0, 5, "A.AA"), "c": mi(2, 5, "A.AA")}, []string{"a", "c"}, false},
		{"unreadable metadata", map[string]*kernel.MemberInfo{"a": nil}, nil, false},
	} {
		var devs []string
		for d := range tc.infos {
			devs = append(devs, d)
		}
		slices.Sort(devs)
		fresh, complete := freshMembers(devs, tc.infos)
		if !slices.Equal(fresh, tc.fresh) || complete != tc.complete {
			t.Errorf("%s: %v %v, want %v %v", tc.name, fresh, complete, tc.fresh, tc.complete)
		}
	}
}

type runnerFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

// groupPlane is a plane with two volumes, vol-a and vol-b, recording what it runs; on is called
// before each command.
func groupPlane(t *testing.T, on func(line string)) (p *Plane, calls func() []string) {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	suspended := map[string]bool{}
	run := runnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		line := name + " " + strings.Join(args, " ")
		if on != nil {
			on(line)
		}
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, line)
		last := args[len(args)-1]
		switch {
		case strings.HasPrefix(line, "dmsetup info"):
			out := ""
			for v, s := range suspended {
				out += v + map[bool]string{true: " Suspended\n", false: " Active\n"}[s]
			}
			return []byte(out), nil
		case strings.HasPrefix(line, "dmsetup suspend"):
			suspended[last] = true
		case strings.HasPrefix(line, "dmsetup resume"):
			suspended[last] = false
		case strings.HasPrefix(line, "dmsetup table"):
			return []byte("0 2048 linear 253:4 0"), nil
		case strings.HasPrefix(line, "blockdev --getsz"):
			return []byte("2048\n"), nil
		case strings.HasPrefix(line, "stat -L"):
			return []byte("fd:4\n"), nil
		case strings.HasPrefix(line, "lvm lvs"):
			return []byte(`{"report":[{"lv":[
				{"lv_name":"vol-a","vg_name":"vg","lv_uuid":"ua","segtype":"thin","lv_size":"1048576"},
				{"lv_name":"vol-b","vg_name":"vg","lv_uuid":"ub","segtype":"thin","lv_size":"1048576"}]}]}`), nil
		case strings.HasPrefix(line, "lvm vgs"):
			return []byte(`{"report":[{"vg":[]}]}`), nil
		}
		return nil, nil
	})
	dev := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dev, "mapper"), 0o755); err != nil {
		t.Fatal(err)
	}
	p = New(&kernel.Plane{Run: run, DevRoot: dev, SysRoot: t.TempDir()}, nil)
	return p, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(lines) }
}

// A reconciler pass in the middle of a group snapshot doesn't resume the volumes it holds: every
// snapshot is taken before the first resume.
func TestSettleLeavesAGroupSnapshotAlone(t *testing.T) {
	var p *Plane
	var once sync.Once
	settled := make(chan error, 1)
	p, calls := groupPlane(t, func(line string) {
		if strings.HasPrefix(line, "lvm lvcreate") {
			once.Do(func() {
				go func() { settled <- p.Settle(context.Background(), nil) }()
				time.Sleep(200 * time.Millisecond) // the pass gets as far as it can
			})
		}
	})
	if _, err := p.SnapshotGroup(context.Background(), []dataplane.GroupEntry{{Volume: "ua", Snapshot: "sa"}, {Volume: "ub", Snapshot: "sb"}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := <-settled; err != nil {
		t.Fatal(err)
	}
	snaps := 0
	for _, l := range calls() {
		if strings.HasPrefix(l, "lvm lvcreate") {
			snaps++
		}
		if strings.HasPrefix(l, "dmsetup resume") && snaps != 2 {
			t.Fatalf("resumed after %d of 2 snapshots: %v", snaps, calls())
		}
	}
	if snaps != 2 {
		t.Fatalf("%d snapshots", snaps)
	}
}

// A wrapper left suspended by a crash is resumed before mdadm reads every block device.
func TestSettleResumesLeftoversFirst(t *testing.T) {
	p, calls := groupPlane(t, nil)
	if err := p.k.SuspendWrapper(context.Background(), "vol-a"); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(p.k.DevRoot, "sdz")
	if err := os.WriteFile(disk, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.Settle(context.Background(), []string{disk}); err != nil {
		t.Fatal(err)
	}
	c := calls()
	resume := slices.Index(c, "dmsetup resume velocor-vol-a")
	mdadm := slices.IndexFunc(c, func(l string) bool { return strings.HasPrefix(l, "mdadm ") })
	if resume < 0 || mdadm < 0 || resume > mdadm {
		t.Fatalf("resume at %d, mdadm at %d: %v", resume, mdadm, c)
	}
}

// Looking for arrays to assemble reads every block device, so Settle does it only when a claimed
// disk is in none, and not again while nothing changed.
func TestSettleExaminesOnlyWhenNeeded(t *testing.T) {
	p, calls := groupPlane(t, nil)
	disk := filepath.Join(p.k.DevRoot, "sdz")
	if err := os.WriteFile(disk, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sys := filepath.Join(p.k.SysRoot, "block", "sdz")
	holder := filepath.Join(sys, "holders", "md127")
	if err := os.MkdirAll(holder, 0o755); err != nil {
		t.Fatal(err)
	}
	examines := func() int {
		n := len(calls())
		if err := p.Settle(context.Background(), []string{disk}); err != nil {
			t.Fatal(err)
		}
		got := 0
		for _, c := range calls()[n:] {
			if strings.HasPrefix(c, "mdadm ") {
				got++
			}
		}
		return got
	}
	if n := examines(); n != 0 {
		t.Fatalf("every disk is in an array: %d looks", n)
	}
	if err := os.Remove(holder); err != nil {
		t.Fatal(err)
	}
	if a, b := examines(), examines(); a != 1 || b != 0 {
		t.Fatalf("a disk in no array: %d looks, then %d", a, b)
	}
	if err := os.WriteFile(filepath.Join(sys, "diskseq"), []byte("7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := examines(); n != 1 {
		t.Fatalf("the disk was plugged in again: %d looks", n)
	}
	p.examinedAt = time.Now().Add(-2 * examineEvery)
	if n := examines(); n != 1 {
		t.Fatalf("a minute later: %d looks", n)
	}
}
