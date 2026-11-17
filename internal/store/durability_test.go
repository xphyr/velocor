package store

import (
	"encoding/json"
	"testing"
)

func TestDiskDurability(t *testing.T) {
	for _, c := range []struct {
		cache WriteCacheState
		plp   bool
		want  Durability
	}{
		{WriteCacheWriteBack, false, DurabilityAtRisk},
		{WriteCacheWriteBack, true, DurabilityDurable},
		{WriteCacheWriteThrough, false, DurabilityDurable},
		{WriteCacheUnknown, false, DurabilityUnknown},
		{WriteCacheUnknown, true, DurabilityDurable},
	} {
		d := Disk{WriteCache: c.cache, PowerLossProtected: c.plp}
		if got := d.Durability(); got != c.want {
			t.Errorf("cache=%q plp=%v: %q, want %q", c.cache, c.plp, got, c.want)
		}
		b, _ := json.Marshal(d)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		if out["durability"] != string(c.want) || out["write_cache"] != string(c.cache) {
			t.Errorf("JSON %s lacks durability %q", b, c.want)
		}
	}
}

// A hardware scan must never undo an operator's declarations, and must not forget the cache
// state while SPDK owns the device (the kernel then reports nothing).
func TestScanKeepsWriteCacheSettings(t *testing.T) {
	s := newTestStore(t)
	if err := s.SyncDiscoveredDisk(&Disk{ID: "ssd-1", Name: "sdb", Type: DiskTypeSSD, WriteCache: WriteCacheWriteBack}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MutateDisk("ssd-1", func(d *Disk) error {
		d.PowerLossProtected, d.WriteCachePolicy = true, WriteCachePolicyDisabled
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncDiscoveredDisk(&Disk{ID: "ssd-1", Name: "sdb", Type: DiskTypeSSD}); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetDisk("ssd-1")
	if err != nil {
		t.Fatal(err)
	}
	if !d.PowerLossProtected || d.WriteCachePolicy != WriteCachePolicyDisabled || d.WriteCache != WriteCacheWriteBack {
		t.Fatalf("scan lost settings: plp=%v policy=%q cache=%q", d.PowerLossProtected, d.WriteCachePolicy, d.WriteCache)
	}
	// A scan that does see the device updates the hardware state.
	if err := s.SyncDiscoveredDisk(&Disk{ID: "ssd-1", Name: "sdb", Type: DiskTypeSSD, WriteCache: WriteCacheWriteThrough}); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.GetDisk("ssd-1"); d.WriteCache != WriteCacheWriteThrough {
		t.Fatalf("cache = %q, want WRITE_THROUGH", d.WriteCache)
	}
}
