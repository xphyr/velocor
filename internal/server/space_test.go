package server

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

const mib = uint64(1) << 20

// spacePool makes a RAID0 pool whose volume store has 1000 clusters of 1 MiB (limit 950 MiB at
// the default 1.0x overcommit and 5% reserve) and returns its ID and lvstore name.
func spacePool(t *testing.T, h *harness) (string, string) {
	t.Helper()
	h.addDisk("d1", "0000:01:00.0")
	id := h.pool("p1", "RAID0", "d1")
	lvs := h.getPool(id)["lvs_name"].(string)
	h.fake.SetLvstoreClusters(lvs, 1000)
	return id, lvs
}

func TestVolumeAdmissionAtTheLimit(t *testing.T) {
	h := newHarness(t)
	pool, _ := spacePool(t, h)
	v1 := h.volume(pool, "v1", 600*mib)
	resp := h.expect(h.do("POST", "/api/v1/volumes", h.admin, map[string]any{"name": "v2", "pool_id": pool, "size_bytes": 400 * mib}))(http.StatusInsufficientStorage)
	if !strings.Contains(resp["error"].(string), "committed") {
		t.Fatalf("refusal should explain the numbers: %v", resp)
	}
	h.volume(pool, "v2", 350*mib) // exactly 950 MiB committed
	// Expanding beyond the limit is refused too.
	h.expect(h.do("POST", "/api/v1/volumes/"+v1["id"].(string)+":resize", h.admin, map[string]any{"new_size_bytes": 610 * mib}))(http.StatusInsufficientStorage)
	if p := h.getPool(pool); p["committed_bytes"] != float64(950*mib) || p["available_bytes"] != float64(0) {
		t.Fatalf("pool after creates: committed %v available %v", p["committed_bytes"], p["available_bytes"])
	}

	// Overcommit on purpose: PATCH the policy.
	h.expect(h.do("PATCH", "/api/v1/pools/"+pool, h.admin, map[string]any{"space_policy": map[string]any{"max_overcommit": 0.01}}))(http.StatusBadRequest)
	h.expect(h.do("PATCH", "/api/v1/pools/"+pool, h.admin, map[string]any{"space_policy": map[string]any{"warning_percent": 95, "critical_percent": 90}}))(http.StatusBadRequest)
	h.expect(h.do("PATCH", "/api/v1/pools/"+pool, h.admin, map[string]any{"space_policy": map[string]any{"warning_percent": 70}}))(http.StatusOK)
	sp := h.expect(h.do("PATCH", "/api/v1/pools/"+pool, h.admin, map[string]any{"space_policy": map[string]any{"max_overcommit": 2}}))(http.StatusOK)["pool"].(map[string]any)["space_policy"].(map[string]any)
	if sp["warning_percent"] != float64(70) || sp["max_overcommit"] != float64(2) {
		t.Fatalf("PATCH must merge: %v", sp)
	}
	h.volume(pool, "v3", 900*mib)
	h.expect(h.do("POST", "/api/v1/volumes/"+v1["id"].(string)+":resize", h.admin, map[string]any{"new_size_bytes": 610 * mib}))(http.StatusOK)
}

func TestSnapshotAndCloneAdmission(t *testing.T) {
	h := newHarness(t)
	pool, _ := spacePool(t, h)
	v1 := h.volume(pool, "v1", 600*mib)
	v2 := h.volume(pool, "v2", 300*mib) // 900 of 950 committed
	for i := range uint64(100) {
		h.fake.WriteCluster(v1["uuid"].(string), i) // v1 holds 100 MiB
	}
	// A snapshot would hold v1's 100 MiB: over the limit.
	h.expect(h.do("POST", "/api/v1/volumes/"+v1["id"].(string)+"/snapshots", h.admin, map[string]any{"name": "s1"}))(http.StatusInsufficientStorage)
	h.expect(h.do("DELETE", "/api/v1/volumes/"+v2["id"].(string), h.admin, nil))(http.StatusOK)
	snap := h.expect(h.do("POST", "/api/v1/volumes/"+v1["id"].(string)+"/snapshots", h.admin, map[string]any{"name": "s1"}))(http.StatusCreated)["snapshot"].(map[string]any)
	if snap["allocated_bytes"] != float64(100*mib) {
		t.Fatalf("snapshot holds %v, want 100 MiB", snap["allocated_bytes"])
	}
	if p := h.getPool(pool); p["committed_bytes"] != float64(700*mib) {
		t.Fatalf("committed %v, want 700 MiB (600 volume + 100 snapshot)", p["committed_bytes"])
	}
	// A clone of the 600 MiB snapshot doesn't fit (700 + 600 > 950).
	h.expect(h.do("POST", "/api/v1/snapshots/"+snap["id"].(string)+":clone", h.admin, map[string]any{"clone_name": "c1"}))(http.StatusInsufficientStorage)
}

func TestCriticalPoolRefusesAllGrowthButDeletes(t *testing.T) {
	h := newHarness(t)
	pool, _ := spacePool(t, h)
	h.expect(h.do("PATCH", "/api/v1/pools/"+pool, h.admin, map[string]any{"space_policy": map[string]any{"max_overcommit": 3}}))(http.StatusOK)
	v1 := h.volume(pool, "v1", 2000*mib)
	for i := range uint64(920) {
		h.fake.WriteCluster(v1["uuid"].(string), i) // 92% used: CRITICAL
	}
	h.reconcile()
	p := h.getPool(pool)
	if p["space_state"] != "CRITICAL" || p["available_bytes"] != float64(0) {
		t.Fatalf("pool at 92%%: %v available %v", p["space_state"], p["available_bytes"])
	}
	metrics := h.metrics()
	for _, want := range []string{`velocor_pool_space_state{pool="p1",state="CRITICAL"} 2`, `velocor_pool_used_ratio{pool="p1"} 0.9200`,
		`velocor_pool_available_bytes{pool="p1"} 0`, `velocor_volume_allocated_bytes{volume="v1",pool="p1"} 964689920`} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	resp := h.expect(h.do("POST", "/api/v1/volumes", h.admin, map[string]any{"name": "tiny", "pool_id": pool, "size_bytes": 1 * mib}))(http.StatusInsufficientStorage)
	if !strings.Contains(resp["error"].(string), "CRITICAL") {
		t.Fatalf("refusal should name the state: %v", resp)
	}
	h.expect(h.do("POST", "/api/v1/volumes/"+v1["id"].(string)+"/snapshots", h.admin, map[string]any{"name": "s"}))(http.StatusInsufficientStorage)
	h.expect(h.do("DELETE", "/api/v1/volumes/"+v1["id"].(string), h.admin, nil))(http.StatusOK)
	h.reconcile()
	if p = h.getPool(pool); p["space_state"] != "CRITICAL" {
		t.Fatalf("a deleted volume keeps its data until purged, so the pool stays full: %v", p["space_state"])
	}
	resp = h.expect(h.do("POST", "/api/v1/volumes", h.admin, map[string]any{"name": "tiny", "pool_id": pool, "size_bytes": 1 * mib}))(http.StatusInsufficientStorage)
	if !strings.Contains(resp["error"].(string), "1 deleted volumes still hold") {
		t.Fatalf("refusal should point at the deleted volume: %v", resp)
	}
	h.expect(h.do("DELETE", "/api/v1/deleted-volumes/"+v1["id"].(string), h.admin, nil))(http.StatusOK)
	h.reconcile()
	if p = h.getPool(pool); p["space_state"] != "OK" {
		t.Fatalf("after freeing space: %v", p["space_state"])
	}
	h.volume(pool, "v2", 100*mib)
}

func TestConcurrentCreatesCannotOvercommitTogether(t *testing.T) {
	h := newHarness(t)
	pool, _ := spacePool(t, h)
	var wg sync.WaitGroup
	codes := make(chan int, 12)
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := h.do("POST", "/api/v1/volumes", h.admin, map[string]any{"name": "c" + string(rune('a'+i)), "pool_id": pool, "size_bytes": 100 * mib})
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	created, refused := 0, 0
	for c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusInsufficientStorage:
			refused++
		default:
			t.Errorf("unexpected HTTP %d", c)
		}
	}
	if created != 9 || refused != 3 {
		t.Fatalf("created %d, refused %d; want 9 and 3 (950 MiB limit, 100 MiB each)", created, refused)
	}
}

func (h *harness) metrics() string {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+h.admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
