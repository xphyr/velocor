package server

import (
	"net/http"
	"testing"
)

// Creating the thin pool can fail after it was made (a slow device outlived a timeout on the test
// appliance, 2026-11-16): the rollback must remove the thin pool first and leave nothing behind.
func TestPoolCreateRollbackAfterLateThinPool(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	h.addDisk("d2", "0000:02:00.0")
	for _, id := range []string{"d1", "d2"} {
		h.expect(h.do("POST", "/api/v1/disks/"+id+":claim", h.admin, nil))(http.StatusOK)
	}
	h.fake.FailAfterApplying("CreateThinPool", 1)
	if code, resp := h.do("POST", "/api/v1/pools", h.admin, map[string]any{"name": "p", "raid_level": "RAID1", "disk_ids": []string{"d1", "d2"}}); code < 400 {
		t.Fatalf("pool create should fail: %d %v", code, resp)
	}
	if tps, err := h.fake.ThinPools(t.Context(), "", ""); err != nil || len(tps) != 0 {
		t.Fatalf("thin pools left behind: %v %v", tps, err)
	}
	if as, err := h.fake.Arrays(t.Context()); err != nil || len(as) != 0 {
		t.Fatalf("arrays left behind: %v %v", as, err)
	}
	// The disks are free again: a retry succeeds.
	h.expect(h.do("POST", "/api/v1/pools", h.admin, map[string]any{"name": "p", "raid_level": "RAID1", "disk_ids": []string{"d1", "d2"}}))(http.StatusCreated)
}

func TestRAID10NeedsFourDisks(t *testing.T) {
	h := newHarness(t)
	for i, pci := range []string{"0000:01:00.0", "0000:02:00.0", "0000:03:00.0", "0000:04:00.0"} {
		id := []string{"a", "b", "c", "d"}[i]
		h.addDisk(id, pci)
		h.expect(h.do("POST", "/api/v1/disks/"+id+":claim", h.admin, nil))(http.StatusOK)
	}
	if code, resp := h.do("POST", "/api/v1/pools", h.admin, map[string]any{"name": "r10", "raid_level": "RAID10", "disk_ids": []string{"a", "b", "c"}}); code != http.StatusBadRequest {
		t.Fatalf("RAID10 on three disks: %d %v, want 400", code, resp)
	}
	h.expect(h.do("POST", "/api/v1/pools", h.admin, map[string]any{"name": "r10", "raid_level": "RAID10", "disk_ids": []string{"a", "b", "c", "d"}}))(http.StatusCreated)
}
