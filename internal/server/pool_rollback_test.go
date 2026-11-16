package server

import (
	"context"
	"net/http"
	"testing"
)

// Reproduces velocor-1 (2026-11-16): creating the lvstore on a large, slow device outlived
// the RPC timeout, spdk_tgt finished it anyway, and the rollback deleted the RAID from under
// the lvstore, leaving a RAID stuck half-deleted and both disks unusable. The rollback must
// remove the lvstore first and leave nothing behind.
func TestPoolCreateRollbackAfterLateLvstore(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	h.addDisk("d2", "0000:02:00.0")
	for _, id := range []string{"d1", "d2"} {
		h.expect(h.do("POST", "/api/v1/disks/"+id+":claim", h.admin, nil))(http.StatusOK)
	}
	h.fake.FailAfterApplying("bdev_lvol_create_lvstore", 1)
	if code, resp := h.do("POST", "/api/v1/pools", h.admin, map[string]any{"name": "p", "raid_level": "RAID1", "disk_ids": []string{"d1", "d2"}}); code < 400 {
		t.Fatalf("pool create should fail: %d %v", code, resp)
	}
	ctx := context.Background()
	c := h.fake.Client()
	if lvs, err := c.BdevLvolGetLvstores(ctx, "", ""); err != nil || len(lvs) != 0 {
		t.Fatalf("lvstores left behind: %v %v", lvs, err)
	}
	if raids, err := c.BdevRaidGetBdevs(ctx, "all"); err != nil || len(raids) != 0 {
		t.Fatalf("RAIDs left behind: %v %v", raids, err)
	}
	// The disks are free again: a retry succeeds.
	h.expect(h.do("POST", "/api/v1/pools", h.admin, map[string]any{"name": "p", "raid_level": "RAID1", "disk_ids": []string{"d1", "d2"}}))(http.StatusCreated)
}

func TestRAID10Refused(t *testing.T) {
	h := newHarness(t)
	for i, pci := range []string{"0000:01:00.0", "0000:02:00.0", "0000:03:00.0", "0000:04:00.0"} {
		id := []string{"a", "b", "c", "d"}[i]
		h.addDisk(id, pci)
		h.expect(h.do("POST", "/api/v1/disks/"+id+":claim", h.admin, nil))(http.StatusOK)
	}
	if code, resp := h.do("POST", "/api/v1/pools", h.admin, map[string]any{"name": "r10", "raid_level": "RAID10", "disk_ids": []string{"a", "b", "c", "d"}}); code != http.StatusBadRequest {
		t.Fatalf("RAID10: %d %v, want 400", code, resp)
	}
}
