package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/spdk/spdktest"
	"github.com/velocor/velocor/internal/store"
)

func TestDiskDurabilityAPI(t *testing.T) {
	h := newHarness(t)
	if err := h.st.UpsertDisk(&store.Disk{ID: "ssd-1", Name: "sdb", Type: store.DiskTypeSSD, Claimed: true, WriteCache: store.WriteCacheWriteBack}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpsertDisk(&store.Disk{ID: "ssd-boot", Name: "sda", Type: store.DiskTypeSSD, IsBootDevice: true}); err != nil {
		t.Fatal(err)
	}
	warned := func() bool {
		_, st := h.do("GET", "/api/v1/system/status", h.admin, nil)
		for _, w := range st["warnings"].([]any) {
			if strings.Contains(w.(string), "Power loss can lose acknowledged writes on sdb") {
				return true
			}
		}
		return false
	}

	_, resp := h.do("GET", "/api/v1/disks/ssd-1", h.admin, nil)
	if got := resp["disk"].(map[string]any)["durability"]; got != string(store.DurabilityAtRisk) {
		t.Fatalf("durability = %v, want AT_RISK", got)
	}
	if !warned() {
		t.Fatal("no status warning for a claimed disk with its write cache on")
	}

	code, resp := h.do("PATCH", "/api/v1/disks/ssd-1", h.admin, map[string]any{"power_loss_protected": true})
	if code != http.StatusOK || resp["disk"].(map[string]any)["durability"] != string(store.DurabilityDurable) {
		t.Fatalf("declare PLP: %d %v", code, resp)
	}
	if warned() {
		t.Fatal("still warning after declaring the disk power-loss protected")
	}

	for _, c := range []struct {
		id   string
		body map[string]any
	}{
		{"ssd-1", map[string]any{"write_cache_policy": "SOMETIMES"}},
		{"ssd-boot", map[string]any{"power_loss_protected": true}},
	} {
		if code, resp := h.do("PATCH", "/api/v1/disks/"+c.id, h.admin, c.body); code != http.StatusBadRequest {
			t.Errorf("PATCH %s %v: %d %v, want 400", c.id, c.body, code, resp)
		}
	}
	if code, _ := h.do("PATCH", "/api/v1/disks/nope", h.admin, map[string]any{"power_loss_protected": true}); code != http.StatusNotFound {
		t.Errorf("unknown disk: %d, want 404", code)
	}
}

// A disk whose drive refused to turn its cache off gets one warning that says so.
func TestRefusedWriteCacheWarning(t *testing.T) {
	h := newHarness(t)
	for _, n := range []string{"sdb", "sdc"} {
		if err := h.st.UpsertDisk(&store.Disk{ID: "ssd-" + n, Name: n, Type: store.DiskTypeSSD, Claimed: true, WriteCache: store.WriteCacheWriteBack,
			WriteCachePolicy: store.WriteCachePolicyDisabled, WriteCacheError: n + ` refused "write through": invalid argument`}); err != nil {
			t.Fatal(err)
		}
	}
	_, st := h.do("GET", "/api/v1/system/status", h.admin, nil)
	var got []string
	for _, w := range st["warnings"].([]any) {
		if strings.Contains(w.(string), "sdb") {
			got = append(got, w.(string))
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "write cache of sdb, sdc can't be turned off") {
		t.Fatalf("warnings about sdb: %q", got)
	}
}

// NVMe drives: the policy is applied through SPDK once SPDK owns the drive, and at claim for a
// drive the policy was set on before (docs/review-history.md P0-4).
func TestNVMeWriteCachePolicyAPI(t *testing.T) {
	h := newHarness(t)
	h.addDisk("nv1", "0000:01:00.0")
	h.addDisk("nv2", "0000:02:00.0")
	h.addDisk("nv3", "0000:03:00.0")
	h.fake.SetNVMeCache("0000:01:00.0", spdktest.NVMeCache{Present: true, Enabled: true})
	h.fake.SetNVMeCache("0000:02:00.0", spdktest.NVMeCache{Present: true, Enabled: true})
	h.fake.SetNVMeCache("0000:03:00.0", spdktest.NVMeCache{Present: true, Enabled: true, Refuse: true})
	disabled := map[string]any{"write_cache_policy": "DISABLED"}

	// Claimed: switched off right away.
	h.expect(h.do("POST", "/api/v1/disks/nv1:claim", h.admin, nil))(http.StatusOK)
	d := h.expect(h.do("PATCH", "/api/v1/disks/nv1", h.admin, disabled))(http.StatusOK)["disk"].(map[string]any)
	if d["write_cache"] != "WRITE_THROUGH" || h.fake.NVMeCacheOf("0000:01:00.0").Enabled {
		t.Fatalf("claimed drive: %v, drive %+v", d["write_cache"], h.fake.NVMeCacheOf("0000:01:00.0"))
	}

	// Not claimed yet: recorded, applied by the claim.
	h.expect(h.do("PATCH", "/api/v1/disks/nv2", h.admin, disabled))(http.StatusOK)
	if !h.fake.NVMeCacheOf("0000:02:00.0").Enabled {
		t.Fatal("an unclaimed drive's cache changed before the claim")
	}
	claimed := h.expect(h.do("POST", "/api/v1/disks/nv2:claim", h.admin, nil))(http.StatusOK)["disk"].(map[string]any)
	if h.fake.NVMeCacheOf("0000:02:00.0").Enabled || claimed["write_cache"] != "WRITE_THROUGH" {
		t.Fatalf("the claim didn't switch the cache off (reported %v)", claimed["write_cache"])
	}

	// A drive that refuses: the change is refused, with the drive's reason.
	h.expect(h.do("POST", "/api/v1/disks/nv3:claim", h.admin, nil))(http.StatusOK)
	code, resp := h.do("PATCH", "/api/v1/disks/nv3", h.admin, disabled)
	if code != http.StatusBadRequest || !strings.Contains(resp["error"].(string), "refused") {
		t.Fatalf("refusing drive: %d %v", code, resp)
	}
}
