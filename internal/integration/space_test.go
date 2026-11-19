//go:build integration

package integration

import (
	"testing"

	"github.com/velocor/velocor/internal/store"
)

// Pool space on real SPDK (P0-2): admission at the default policy, then a deliberately
// overcommitted pool filled through an export: WARNING, CRITICAL (growth refused), FULL (the
// write to new space fails), and recovery once space is freed. Data written stays intact.
func TestPoolSpaceLifecycle(t *testing.T) {
	tg := newTarget(t)
	a := newAppliance(t, tg)
	for _, id := range []string{"diska", "diskb"} {
		a.addDisk(id, diskSize) // 128 MiB mirror
	}
	pool := a.must("POST", "/pools", map[string]any{"name": "p", "raid_level": "RAID1", "disk_ids": []string{"diska", "diskb"}}, "pool")
	id := pool["id"].(string)
	total := uint64(pool["total_bytes"].(float64))
	limit := total * 95 / 100
	t.Logf("pool: %d MiB, limit %d MiB at 1.0x with 5%% reserve", total>>20, limit>>20)

	// Default policy: more than fits is refused (HTTP 507), up to the limit is accepted.
	if code, out := a.do("POST", "/volumes", map[string]any{"name": "big", "pool_id": id, "size_bytes": limit + 1<<20, "thin_provision": true}); code != 507 {
		t.Fatalf("a volume larger than the limit: HTTP %d %v, want 507", code, out)
	}
	v1 := a.must("POST", "/volumes", map[string]any{"name": "v1", "pool_id": id, "size_bytes": 96 << 20, "thin_provision": true}, "volume")

	// Overcommit on purpose, then fill the pool from two volumes.
	a.must("PATCH", "/pools/"+id, map[string]any{"space_policy": map[string]any{"max_overcommit": 3}}, "")
	v2 := a.must("POST", "/volumes", map[string]any{"name": "v2", "pool_id": id, "size_bytes": 96 << 20, "thin_provision": true}, "volume")
	in1 := a.initiator(a.exportTo(v1["id"].(string), "v1", hostA), hostA)
	in2 := a.initiator(a.exportTo(v2["id"].(string), "v2", hostB), hostB)

	chunk := randomData(t, 8<<20)
	state := func() (store.SpaceState, float64) {
		a.reconcile()
		p, _ := a.st.GetPool(id)
		return p.SpaceState, store.UsedPercent(p)
	}
	seen := map[store.SpaceState]bool{}
	var full bool
	written := map[*initiator][]int{}
	for _, in := range []*initiator{in1, in2} {
		for off := 0; off < 96<<20/4096; off += len(chunk) / 4096 {
			if err := in.write(off, chunk); err != nil {
				t.Logf("write failed at %d MiB: %v", off*4096>>20, err)
				full = true
				break
			}
			written[in] = append(written[in], off)
			st, used := state()
			if !seen[st] {
				t.Logf("%.0f%% used: %s", used, st)
				seen[st] = true
			}
			if st == store.SpaceStateCritical && !seen["refused"] {
				if code, _ := a.do("POST", "/volumes", map[string]any{"name": "tiny", "pool_id": id, "size_bytes": 1 << 20, "thin_provision": true}); code != 507 {
					t.Fatalf("CRITICAL pool accepted a new volume (HTTP %d)", code)
				}
				if code, _ := a.do("POST", "/volumes/"+v1["id"].(string)+"/snapshots", map[string]any{"name": "s"}); code != 507 {
					t.Fatalf("CRITICAL pool accepted a snapshot (HTTP %d)", code)
				}
				seen["refused"] = true
			}
		}
		if full {
			break
		}
	}
	if st, _ := state(); !full || st != store.SpaceStateFull || !seen[store.SpaceStateWarning] || !seen[store.SpaceStateCritical] || !seen["refused"] {
		t.Fatalf("went through %v, ended %s (write failed: %v); want WARNING, CRITICAL (growth refused), FULL", seen, st, full)
	}
	// Everything written before the pool filled is intact.
	for in, offs := range written {
		for _, off := range offs {
			in.mustRead(off, chunk)
		}
	}

	// Free space: v2's export and volume go; the pool recovers and v1 can be written again.
	exps := a.must("GET", "/exports", nil, "")
	for _, e := range exps["exports"].([]any) {
		if e.(map[string]any)["volume_id"] == v2["id"] {
			a.must("DELETE", "/exports/"+e.(map[string]any)["id"].(string), nil, "")
		}
	}
	a.must("DELETE", "/volumes/"+v2["id"].(string), nil, "")
	a.must("DELETE", "/deleted-volumes/"+v2["id"].(string), nil, "") // kept until purged (P1-4)
	if st, used := state(); st == store.SpaceStateFull || st == store.SpaceStateCritical {
		t.Fatalf("after deleting v2: %s at %.0f%%", st, used)
	}
	for off := 0; off < 96<<20/4096; off += len(chunk) / 4096 {
		if err := in1.write(off, chunk); err != nil {
			t.Fatalf("v1 write after freeing space: %v", err)
		}
	}
	for _, off := range written[in1] {
		in1.mustRead(off, chunk)
	}
}
