package kernelplane

import (
	"slices"
	"testing"

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
