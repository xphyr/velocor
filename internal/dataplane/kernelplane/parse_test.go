package kernelplane

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/kernel"
	"github.com/velocor/velocor/internal/store"
)

// Output captured on the lab VM (Fedora 45, nvme-cli 2.16, iproute2).

func TestParseSmartLog(t *testing.T) {
	h, err := parseSmartLog([]byte(`{"critical_warning":0,"temperature":323,"avail_spare":100,"spare_thresh":10,"percent_used":3,
		"data_units_read":7,"data_units_written":9,"power_on_hours":66,"unsafe_shutdowns":2,"media_errors":1}`))
	if err != nil {
		t.Fatal(err)
	}
	want := store.SmartHealth{TemperatureCelsius: 50, AvailableSparePercent: 100, AvailableSpareThresholdPercent: 10, PercentageUsed: 3,
		DataUnitsRead: 7, DataUnitsWritten: 9, MediaErrors: 1, PowerOnHours: 66, UnsafeShutdowns: 2}
	if *h != want {
		t.Errorf("smart %+v, want %+v", *h, want)
	}
	if h, _ := parseSmartLog([]byte(`{"critical_warning":4}`)); !h.CriticalWarning {
		t.Error("critical warning lost")
	}
}

func TestParseFeatureValue(t *testing.T) {
	for out, want := range map[string]uint64{
		"get-feature:0x06 (Volatile Write Cache), Current value:0x00000001\n": 1,
		"get-feature:0x06 (Volatile Write Cache), Current value:00000000\n":   0,
	} {
		if v, err := parseFeatureValue(out); err != nil || v != want {
			t.Errorf("%q: %d %v", out, v, err)
		}
	}
	if _, err := parseFeatureValue("NVMe status: INVALID_FIELD"); err == nil {
		t.Error("garbage parsed")
	}
}

func TestParseSSPeers(t *testing.T) {
	out := "0      0      127.0.0.1:14421    127.0.0.1:53210\n" +
		"0      0      172.16.20.115:4420 172.16.20.80:41000\n" +
		"0      0      [::ffff:172.16.20.115]:4420 [::ffff:172.16.20.81]:41001\n" +
		"0      0      10.0.0.1:4420      10.0.0.9:1\n"
	if got := parseSSPeers(out, "172.16.20.115", 4420); !slices.Equal(got, []string{"172.16.20.80", "172.16.20.81"}) {
		t.Errorf("peers %v", got)
	}
	if got := parseSSPeers(out, "0.0.0.0", 4420); len(got) != 3 {
		t.Errorf("wildcard listener peers %v", got)
	}
	if got := parseSSPeers(out, "127.0.0.1", 14421); !slices.Equal(got, []string{"127.0.0.1"}) {
		t.Errorf("loopback peers %v", got)
	}
}

func TestSerial(t *testing.T) {
	if s := serial(&store.Export{ID: "exp-6f7b3acbf52b"}); s != "VELOEXP6F7B3ACBF52B" {
		t.Errorf("serial %q", s)
	}
}

func TestScrubStateKept(t *testing.T) {
	dir := t.TempDir()
	p := New(nil, nil)
	p.StateDir = dir
	p.mu.Lock()
	p.scrubs["raid_pool-x"] = &scrubRun{Repair: true, Start: 2048}
	p.saveScrubsLocked()
	p.mu.Unlock()

	q := New(nil, nil)
	q.StateDir = dir
	if err := q.LoadState(); err != nil {
		t.Fatal(err)
	}
	if r := q.scrubs["raid_pool-x"]; r == nil || !r.Repair || r.Start != 2048 || r.Stopped {
		t.Fatalf("scrub after reload: %+v", r)
	}
	if err := New(nil, nil).LoadState(); err != nil { // no StateDir: nothing to do
		t.Fatal(err)
	}
}

// How a scrub ended, from what md shows: completed, stopped by an operator (with how far it got),
// cut short by a member that failed or a rebuild, and nothing that happens later changes it.
func TestScrubOutcome(t *testing.T) {
	sys := t.TempDir()
	md := filepath.Join(sys, "block", "md127")
	if err := os.MkdirAll(filepath.Join(md, "md"), 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{"size": "2000000", "md/component_size": "500000", "md/sync_speed_max": ""} {
		if err := os.WriteFile(filepath.Join(md, f), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const total = 1000000 // component_size in sectors
	status := func(action, last string, degraded int, done uint64) *kernel.ArrayStatus {
		return &kernel.ArrayStatus{Kernel: "md127", Level: "raid1", RaidDisks: 2, Degraded: degraded, SyncAction: action, LastSyncAction: last, SyncDone: done, SyncTotal: total}
	}
	plane := func(run *scrubRun) *Plane {
		p := New(&kernel.Plane{SysRoot: sys, DevRoot: sys}, nil)
		p.scrubs["a"] = run
		return p
	}

	p := plane(&scrubRun{Start: 64, Last: 64})
	if sc := p.scrubOf("a", status("check", "check", 0, 400000)); sc.State != dataplane.ScrubRunning || sc.OffsetBlocks != 400000 || sc.StartOffsetBlocks != 64 {
		t.Fatalf("running: %+v", sc)
	}
	if sc := p.scrubOf("a", status("idle", "check", 0, 0)); sc.State != dataplane.ScrubCompleted || sc.OffsetBlocks != total || sc.Status != 0 {
		t.Fatalf("completed: %+v", sc)
	}
	// A member failing after the scrub completed doesn't make it interrupted.
	if sc := p.scrubOf("a", status("idle", "check", 1, 0)); sc.State != dataplane.ScrubCompleted {
		t.Fatalf("completed, then degraded: %+v", sc)
	}

	p = plane(&scrubRun{Start: 0, Stopped: true, Last: 300000})
	if sc := p.scrubOf("a", status("idle", "check", 0, 0)); sc.State != dataplane.ScrubStopped || sc.Status != dataplane.ScrubStatusCanceled || sc.OffsetBlocks != 300000 {
		t.Fatalf("stopped by an operator: %+v", sc)
	}

	p = plane(&scrubRun{})
	p.scrubOf("a", status("check", "check", 0, 250000))
	if sc := p.scrubOf("a", status("idle", "check", 1, 0)); sc.State != dataplane.ScrubStopped || sc.Status != dataplane.ScrubStatusMemberRemoved || sc.OffsetBlocks != 250000 {
		t.Fatalf("cut short by a member: %+v", sc)
	}
	p = plane(&scrubRun{})
	p.scrubOf("a", status("check", "check", 0, 250000))
	if sc := p.scrubOf("a", status("recover", "recover", 1, 10)); sc.State != dataplane.ScrubStopped || sc.OffsetBlocks != 250000 {
		t.Fatalf("a rebuild right after a member failed: %+v", sc)
	}
	p = plane(&scrubRun{Degraded: 1})
	p.scrubOf("a", status("check", "check", 1, 250000))
	if sc := p.scrubOf("a", status("recover", "recover", 1, 10)); sc.Status != dataplane.ScrubStatusRebuild || sc.OffsetBlocks != 250000 {
		t.Fatalf("a rebuild started: %+v", sc)
	}
	if sc := plane(nil).scrubOf("a", status("idle", "check", 0, 0)); sc != nil {
		t.Fatalf("a check velod didn't start, finished: %+v", sc)
	}
}
