package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func (h *harness) getPool(id string) map[string]any {
	h.t.Helper()
	return h.expect(h.do("GET", "/api/v1/pools/"+id, h.admin, nil))(http.StatusOK)["pool"].(map[string]any)
}

func (h *harness) reconcile() {
	h.t.Helper()
	h.expect(h.do("POST", "/api/v1/system/reconcile", h.admin, nil))(http.StatusOK)
}

func member(p map[string]any, slot int) map[string]any {
	return p["members"].([]any)[slot].(map[string]any)
}

func TestReplaceFailedDiskRebuildAndRelease(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	h.addDisk("d2", "0000:02:00.0")
	h.addDisk("d3", "0000:03:00.0")
	id := h.pool("p1", "RAID1", "d1", "d2")
	raid := h.getPool(id)["base_bdev_name"].(string)

	h.fake.FailMedia("0000:02:00.0")
	h.reconcile()
	if p := h.getPool(id); p["health_status"] != "DEGRADED" || member(p, 1)["state"] != "FAILED" {
		t.Fatalf("after a failure: %v %v", p["health_status"], p["members"])
	}

	// d3 is unclaimed: replace claims it and starts the rebuild into the empty slot.
	p := h.expect(h.do("POST", "/api/v1/pools/"+id+"/replace", h.admin, map[string]any{"new_disk_id": "d3"}))(http.StatusAccepted)["pool"].(map[string]any)
	if p["health_status"] != "REBUILDING" || member(p, 1)["disk_id"] != "d3" || member(p, 1)["state"] != "REBUILDING" {
		t.Fatalf("after replace: %v %v", p["health_status"], p["members"])
	}
	// Only one rebuild at a time.
	h.addDisk("d4", "0000:04:00.0")
	h.expect(h.do("POST", "/api/v1/pools/"+id+"/replace", h.admin, map[string]any{"new_disk_id": "d4", "old_disk_id": "d1", "allow_degraded": true}))(http.StatusConflict)

	h.fake.SetRebuildProgress(raid, 100)
	h.reconcile()
	p = h.getPool(id)
	if p["health_status"] != "ONLINE" || member(p, 1)["state"] != "ACTIVE" {
		t.Fatalf("after the rebuild: %v %v", p["health_status"], p["members"])
	}
	if det := p["detached_disk_ids"].([]any); len(det) != 1 || det[0] != "d2" {
		t.Fatalf("detached: %v", det)
	}

	// Release: only detached disks.
	h.expect(h.do("DELETE", "/api/v1/pools/"+id+"/disks/d1", h.admin, nil))(http.StatusConflict)
	resp := h.expect(h.do("DELETE", "/api/v1/pools/"+id+"/disks/d2", h.admin, nil))(http.StatusOK)
	if d := resp["disk"].(map[string]any); d["pool_id"] != nil && d["pool_id"] != "" || d["claimed"] == true {
		t.Fatalf("released disk: %v", d)
	}
	if p = h.getPool(id); p["detached_disk_ids"] != nil {
		t.Fatalf("still detached: %v", p["detached_disk_ids"])
	}
}

func TestReplaceWorkingDiskNeedsAllowDegraded(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	h.addDisk("d2", "0000:02:00.0")
	h.addDisk("d3", "0000:03:00.0")
	id := h.pool("p1", "RAID1", "d1", "d2")
	h.reconcile()

	resp := h.expect(h.do("POST", "/api/v1/pools/"+id+"/replace", h.admin, map[string]any{"new_disk_id": "d3", "old_disk_id": "d1"}))(http.StatusBadRequest)
	if !strings.Contains(resp["error"].(string), "allow_degraded") {
		t.Fatalf("refusal should explain allow_degraded: %v", resp)
	}
	p := h.expect(h.do("POST", "/api/v1/pools/"+id+"/replace", h.admin, map[string]any{"new_disk_id": "d3", "old_disk_id": "d1", "allow_degraded": true}))(http.StatusAccepted)["pool"].(map[string]any)
	if member(p, 0)["disk_id"] != "d3" || member(p, 0)["state"] != "REBUILDING" || member(p, 1)["state"] != "ACTIVE" {
		t.Fatalf("after proactive replace: %v", p["members"])
	}
	if det := p["detached_disk_ids"].([]any); len(det) != 1 || det[0] != "d1" {
		t.Fatalf("detached: %v", det)
	}
}

func TestReplaceRefusals(t *testing.T) {
	h := newHarness(t)
	for i, id := range []string{"d1", "d2", "d3", "d4", "d5", "d6", "d7"} {
		h.addDisk(id, "0000:0"+string(rune('1'+i))+":00.0")
	}
	mirror := h.pool("m", "RAID1", "d1", "d2")
	stripe := h.pool("s", "RAID0", "d3")
	h.reconcile()

	h.expect(h.do("POST", "/api/v1/pools/"+stripe+"/replace", h.admin, map[string]any{"new_disk_id": "d4"}))(http.StatusBadRequest)
	// Nothing failed: without old_disk_id there is nothing to replace.
	h.expect(h.do("POST", "/api/v1/pools/"+mirror+"/replace", h.admin, map[string]any{"new_disk_id": "d4"}))(http.StatusBadRequest)
	// A disk of another pool.
	h.fake.FailMedia("0000:02:00.0")
	h.reconcile()
	h.expect(h.do("POST", "/api/v1/pools/"+mirror+"/replace", h.admin, map[string]any{"new_disk_id": "d3"}))(http.StatusConflict)

	// A disk carrying an old array's superblock: SPDK assembles it into that array when it
	// is claimed (velod wipes unclaimed disks first on real hardware; the fake has no wipe).
	c := h.fake.Client()
	ctx := context.Background()
	for _, id := range []string{"d5", "d6"} {
		h.expect(h.do("POST", "/api/v1/disks/"+id+":claim", h.admin, nil))(http.StatusOK)
	}
	d5, _ := h.st.GetDisk("d5")
	d6, _ := h.st.GetDisk("d6")
	if err := c.BdevRaidCreate(ctx, "stale", "raid1", 0, []string{d5.SPDKBdevName, d6.SPDKBdevName}, true); err != nil {
		t.Fatal(err)
	}
	if err := c.BdevRaidDelete(ctx, "stale", false); err != nil {
		t.Fatal(err)
	}
	h.expect(h.do("POST", "/api/v1/disks/d5:release", h.admin, nil))(http.StatusOK)
	resp := h.expect(h.do("POST", "/api/v1/pools/"+mirror+"/replace", h.admin, map[string]any{"new_disk_id": "d5"}))(http.StatusConflict)
	if !strings.Contains(resp["error"].(string), "stale") {
		t.Fatalf("refusal should name the old array: %v", resp)
	}
	// A clean disk works.
	h.expect(h.do("POST", "/api/v1/pools/"+mirror+"/replace", h.admin, map[string]any{"new_disk_id": "d7"}))(http.StatusAccepted)
}

func TestStartPoolDegraded(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	h.addDisk("d2", "0000:02:00.0")
	h.addDisk("d3", "0000:03:00.0")
	id := h.pool("p1", "RAID1", "d1", "d2")

	// d2 dies while spdk_tgt is down.
	h.fake.Restart()
	h.fake.RemoveMedia("0000:02:00.0")
	h.reconcile()
	if p := h.getPool(id); p["health_status"] != "UNAVAILABLE" || member(p, 1)["state"] != "MISSING" {
		t.Fatalf("missing at boot: %v %v", p["health_status"], p["members"])
	}
	// No volumes on an unavailable pool.
	h.expect(h.do("POST", "/api/v1/volumes", h.admin, map[string]any{"name": "v", "pool_id": id, "size_bytes": 1 << 30}))(http.StatusBadRequest)
	// Replace needs a running array.
	h.expect(h.do("POST", "/api/v1/pools/"+id+"/replace", h.admin, map[string]any{"new_disk_id": "d3"}))(http.StatusBadRequest)

	h.expect(h.do("POST", "/api/v1/pools/"+id+"/start-degraded", h.admin, map[string]any{}))(http.StatusBadRequest)
	h.expect(h.do("POST", "/api/v1/pools/"+id+"/start-degraded", h.admin, map[string]any{"confirm": "wrong"}))(http.StatusBadRequest)
	p := h.expect(h.do("POST", "/api/v1/pools/"+id+"/start-degraded", h.admin, map[string]any{"confirm": "p1"}))(http.StatusOK)["pool"].(map[string]any)
	if p["health_status"] != "DEGRADED" || member(p, 1)["state"] != "MISSING" {
		t.Fatalf("after start-degraded: %v %v", p["health_status"], p["members"])
	}
	h.expect(h.do("POST", "/api/v1/pools/"+id+"/start-degraded", h.admin, map[string]any{"confirm": "p1"}))(http.StatusBadRequest)

	// Volumes work again, and the missing disk can be replaced.
	h.volume(id, "v1", 1<<30)
	h.expect(h.do("POST", "/api/v1/pools/"+id+"/replace", h.admin, map[string]any{"new_disk_id": "d3"}))(http.StatusAccepted)
}

// A missing member's own disk comes back: replace with it puts it back and rebuilds onto it
// (neither release nor replace with another disk is needed).
func TestPutMissingMemberBack(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	h.addDisk("d2", "0000:02:00.0")
	h.addDisk("d3", "0000:03:00.0")
	id := h.pool("p1", "RAID1", "d1", "d2")
	h.fake.Restart()
	h.fake.RemoveMedia("0000:02:00.0")
	h.reconcile()
	h.expect(h.do("POST", "/api/v1/pools/"+id+"/start-degraded", h.admin, map[string]any{"confirm": "p1"}))(http.StatusOK)

	// d2 is back; the running array doesn't take it by itself.
	h.fake.AddNVMe("0000:02:00.0")
	h.reconcile()
	if p := h.getPool(id); member(p, 1)["state"] == "ACTIVE" || member(p, 1)["disk_id"] != "d2" {
		t.Fatalf("returned member taken back implicitly: %v", p["members"])
	}
	// Not d3 for d2's slot by naming d3's slot; not d1, which is working.
	h.expect(h.do("POST", "/api/v1/pools/"+id+"/replace", h.admin, map[string]any{"new_disk_id": "d2", "old_disk_id": "d1"}))(http.StatusBadRequest)
	h.expect(h.do("POST", "/api/v1/pools/"+id+"/replace", h.admin, map[string]any{"new_disk_id": "d1"}))(http.StatusBadRequest)

	p := h.expect(h.do("POST", "/api/v1/pools/"+id+"/replace", h.admin, map[string]any{"new_disk_id": "d2"}))(http.StatusAccepted)["pool"].(map[string]any)
	if s := member(p, 1)["state"]; s != "REBUILDING" && s != "ACTIVE" {
		t.Fatalf("after putting d2 back: %v", p["members"])
	}
	if d := h.expect(h.do("GET", "/api/v1/disks/d2", h.admin, nil))(http.StatusOK)["disk"].(map[string]any); d["pool_id"] != id || d["claimed"] != true {
		t.Errorf("d2 after putting it back: %v", d)
	}
}

func TestRebuildSpeedSetting(t *testing.T) {
	h := newHarness(t)
	if v := h.expect(h.do("GET", "/api/v1/system/rebuild", h.admin, nil))(http.StatusOK)["max_mb_per_sec"]; v != float64(0) {
		t.Fatalf("default: %v", v)
	}
	h.expect(h.do("PUT", "/api/v1/system/rebuild", h.admin, map[string]any{"max_mb_per_sec": 300}))(http.StatusOK)
	if h.fake.RebuildMaxMBps() != 300 {
		t.Fatalf("not applied to SPDK: %d", h.fake.RebuildMaxMBps())
	}
	h.expect(h.do("PUT", "/api/v1/system/rebuild", h.admin, map[string]any{}))(http.StatusBadRequest)
	h.expect(h.do("PUT", "/api/v1/system/rebuild", h.admin, map[string]any{"max_mb_per_sec": maxRebuildMBps + 1}))(http.StatusBadRequest)

	// spdk_tgt forgets it on restart; the reconciler puts it back.
	h.fake.Restart()
	h.reconcile()
	if h.fake.RebuildMaxMBps() != 300 {
		t.Fatalf("not re-applied after restart: %d", h.fake.RebuildMaxMBps())
	}
	if s, _ := h.st.GetSettings(); s.RebuildMaxMBps != 300 {
		t.Fatalf("store: %+v", s)
	}
}
