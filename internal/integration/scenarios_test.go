//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

const (
	diskSize = 128 << 20
	volSize  = 16 << 20
	hostA    = "nqn.2014-08.org.nvmexpress:uuid:4f6c2a1e-0000-4000-8000-00000000000a"
	hostB    = "nqn.2014-08.org.nvmexpress:uuid:4f6c2a1e-0000-4000-8000-00000000000b"
)

// setup returns an appliance with a pool of the given RAID level over n disks and one volume
// exported to hostA.
func setup(t *testing.T, level string, n int) (*appliance, map[string]any, map[string]any, *initiator) {
	t.Helper()
	tg := newTarget(t)
	a := newAppliance(t, tg)
	var ids []string
	for i := range n {
		id := "disk" + string(rune('a'+i))
		a.addDisk(id, diskSize)
		ids = append(ids, id)
	}
	pool := a.must("POST", "/pools", map[string]any{"name": "p", "raid_level": level, "disk_ids": ids}, "pool")
	vol := a.must("POST", "/volumes", map[string]any{"name": "v", "pool_id": pool["id"], "size_bytes": volSize, "thin_provision": true}, "volume")
	nqn := a.exportTo(vol["id"].(string), "v", hostA)
	return a, pool, vol, a.initiator(nqn, hostA)
}

// Data written through an export must read back identically after spdk_tgt is killed and
// restarted, and after a clean restart, with the reconciler rebuilding everything from bbolt.
func TestDataSurvivesCrashAndRestart(t *testing.T) {
	a, pool, vol, in := setup(t, "RAID1", 2)
	data := randomData(t, 1<<20)
	if err := in.write(0, data); err != nil {
		t.Fatal(err)
	}
	in.mustRead(0, data)

	for _, how := range []string{"crash", "clean"} {
		t.Run(how, func(t *testing.T) {
			if how == "crash" {
				a.tg.crash()
			} else {
				a.tg.stop()
			}
			a.tg.start()
			// Until a pass has restored the new spdk_tgt, status must not say ONLINE (P2-11).
			health := func() string { return a.must("GET", "/system/status", nil, "")["overall_health"].(string) }
			visible := a.rec.SPDKInstance() != ""
			if !visible {
				t.Log("spdk_tgt's process isn't visible from here: restart detection can't be checked")
			} else if h := health(); h != "RECONCILING" {
				t.Fatalf("right after the %s restart, status says %s, want RECONCILING", how, h)
			}
			st := a.reconcile()
			if h := health(); visible && h != "ONLINE" {
				t.Fatalf("after restoring, status says %s, want ONLINE (%+v)", h, st)
			}
			p, _ := a.st.GetPool(pool["id"].(string))
			v, _ := a.st.GetVolume(vol["id"].(string))
			if p.HealthStatus != store.HealthStatusOnline || v.HealthStatus != store.HealthStatusOnline || p.LVSUUID != pool["lvs_uuid"] {
				t.Fatalf("after %s restart: pool %s (%s) lvs %s, volume %s (%s); reconciler %+v",
					how, p.HealthStatus, p.StatusMessage, p.LVSUUID, v.HealthStatus, v.StatusMessage, st)
			}
			in.mustRead(0, data)
			if len(st.Unmanaged) != 0 {
				t.Fatalf("after %s restart, managed objects reported as unmanaged: %+v", how, st.Unmanaged)
			}
		})
	}

	// Something created behind Velocor's back is reported, and left alone (P1-5).
	stray, err := a.tg.client().BdevLvolCreate(context.Background(), pool["lvs_name"].(string), "stray", 4<<20, true, "")
	if err != nil {
		t.Fatal(err)
	}
	st := a.reconcile()
	if len(st.Unmanaged) != 1 || st.Unmanaged[0].Kind != "LVOL" || st.Unmanaged[0].UUID != stray {
		t.Fatalf("want the stray lvol reported: %+v", st.Unmanaged)
	}
	if lvols, _ := a.tg.client().BdevLvolGetLvols(context.Background()); !slices.ContainsFunc(lvols, func(l spdk.LvolInfo) bool { return l.UUID == stray }) {
		t.Fatal("the reconciler deleted the stray lvol")
	}
}

// Only hosts in the export's initiator groups may connect, and access changes apply live.
func TestExportACLEnforced(t *testing.T) {
	a, _, _, in := setup(t, "RAID0", 1)
	if err := in.write(0, randomData(t, blockSize)); err != nil {
		t.Fatalf("allowed host could not write: %v", err)
	}
	other := a.initiator(in.nqn, hostB)
	if _, err := other.read(0, blockSize); err == nil {
		t.Fatalf("a host that isn't in the export's initiator groups could read it")
	}
	// After a restart the reconciler must restore the same ACL, not an open one.
	a.tg.crash()
	a.tg.start()
	a.reconcile()
	if _, err := other.read(0, blockSize); err == nil {
		t.Fatalf("after restart, a host outside the ACL could read the export")
	}
	if _, err := in.read(0, blockSize); err != nil {
		t.Fatalf("after restart, the allowed host lost access: %v", err)
	}
}

// Snapshots freeze data and clones see the snapshot's data, through real exports.
func TestSnapshotsAndClones(t *testing.T) {
	a, pool, vol, in := setup(t, "RAID0", 1)
	cluster := int(pool["cluster_size_bytes"].(float64))
	clusterBlocks := cluster / blockSize
	first := randomData(t, cluster)
	if err := in.write(0, first); err != nil {
		t.Fatal(err)
	}
	s1 := a.must("POST", "/volumes/"+vol["id"].(string)+"/snapshots", map[string]any{"name": "s1"}, "snapshot")
	second := randomData(t, cluster)
	if err := in.write(3*clusterBlocks, second); err != nil {
		t.Fatal(err)
	}
	s2 := a.must("POST", "/volumes/"+vol["id"].(string)+"/snapshots", map[string]any{"name": "s2"}, "snapshot")

	clone := a.must("POST", "/snapshots/"+s1["id"].(string)+":clone", map[string]any{"clone_name": "c1"}, "volume")
	cin := a.initiator(a.exportTo(clone["id"].(string), "c1", hostA), hostA)
	cin.mustRead(0, first)
	cin.mustRead(3*clusterBlocks, make([]byte, cluster)) // written after s1, so not in the clone
	in.mustRead(3*clusterBlocks, second)                 // the source keeps its newer data

	// The source volume can be deleted while its snapshots and clones live on.
	for _, e := range a.must("GET", "/exports?volume_id="+vol["id"].(string), nil, "")["exports"].([]any) {
		a.must("DELETE", "/exports/"+e.(map[string]any)["id"].(string), nil, "")
	}
	a.must("DELETE", "/volumes/"+vol["id"].(string), nil, "")
	cin.mustRead(0, first)
	a.must("DELETE", "/deleted-volumes/"+vol["id"].(string), nil, "") // purge: the lvol goes from SPDK
	cin.mustRead(0, first)

	// Changed-block tracking from SPDK's cluster maps (patch 0005, docs/review-history.md L-2): cluster 0 was
	// written before s1, cluster 3 between s1 and s2. Still right with the source volume gone.
	extentsOf := func(path string) [][2]int {
		t.Helper()
		code, out := a.do("GET", path, nil)
		if code != 200 {
			t.Fatalf("%s: HTTP %d %v", path, code, out)
		}
		var res [][2]int
		for _, e := range out["extents"].([]any) {
			m := e.(map[string]any)
			res = append(res, [2]int{int(m["offset_bytes"].(float64)), int(m["length_bytes"].(float64))})
		}
		return res
	}
	if got := extentsOf("/snapshots/" + s2["id"].(string) + "/blocks:changed?base_snapshot_id=" + s1["id"].(string)); !slices.Equal(got, [][2]int{{3 * cluster, cluster}}) {
		t.Fatalf("changed blocks s1->s2: %v, want cluster 3", got)
	}
	if got := extentsOf("/snapshots/" + s1["id"].(string) + "/blocks:allocated"); !slices.Equal(got, [][2]int{{0, cluster}}) {
		t.Fatalf("allocated blocks of s1: %v, want cluster 0", got)
	}
	if got := extentsOf("/snapshots/" + s2["id"].(string) + "/blocks:allocated"); !slices.Equal(got, [][2]int{{0, cluster}, {3 * cluster, cluster}}) {
		t.Fatalf("allocated blocks of s2: %v, want clusters 0 and 3", got)
	}

	// A snapshot with a clone can be deleted: it disappears, and SPDK deletes it once the clone
	// is gone (P1-4).
	a.must("DELETE", "/snapshots/"+s1["id"].(string), nil, "")
	if code, _ := a.do("GET", "/snapshots/"+s1["id"].(string), nil); code != 404 {
		t.Fatalf("deleted snapshot still visible: HTTP %d", code)
	}
	cin.mustRead(0, first)

	for _, e := range a.must("GET", "/exports?volume_id="+clone["id"].(string), nil, "")["exports"].([]any) {
		a.must("DELETE", "/exports/"+e.(map[string]any)["id"].(string), nil, "")
	}
	a.must("DELETE", "/volumes/"+clone["id"].(string), nil, "")
	a.must("DELETE", "/deleted-volumes/"+clone["id"].(string), nil, "")
	a.must("POST", "/system/reconcile", nil, "")
	lvols, err := a.tg.client().BdevLvolGetLvols(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lvols {
		if l.UUID == s1["uuid"] || l.UUID == clone["uuid"] || l.UUID == vol["uuid"] {
			t.Fatalf("lvol %s (%s) still in SPDK after the purge", l.Name, l.UUID)
		}
	}
	if code, _ := a.do("GET", "/snapshots/"+s2["id"].(string), nil); code != 200 {
		t.Fatalf("snapshot s2 lost: HTTP %d", code)
	}
	if st := a.reconcile(); len(st.Unmanaged) != 0 {
		t.Fatalf("managed objects reported as unmanaged: %+v", st.Unmanaged)
	}
}

// Hosts must be told about volatile write caches, or they will treat a completed write as
// durable when it may still be in a disk's cache. This checks what each layer advertises.
func TestWriteCacheAndFlushAdvertised(t *testing.T) {
	a, pool, vol, _ := setup(t, "RAID1", 2)
	disk, _ := a.st.GetDisk("diska")
	for _, b := range []struct{ what, name string }{
		{"base disk (aio)", disk.SPDKBdevName},
		{"RAID", pool["base_bdev_name"].(string)},
		{"volume", vol["uuid"].(string)},
	} {
		raw := a.tg.rawBdev(b.name)
		types, _ := raw["supported_io_types"].(map[string]any)
		t.Logf("%-16s %-40s write_cache=%v flush=%v", b.what, b.name, raw["write_cache"], types["flush"])
		if types["flush"] != true {
			// Known issue TODO.md L-1 (P0-4): SPDK's lvol module doesn't support FLUSH and RAID
			// reports no write cache, so hosts are told writes are durable on completion.
			t.Skipf("KNOWN ISSUE (TODO.md L-1): %s bdev %s does not support FLUSH, so a host's flush cannot reach the disks", b.what, b.name)
		}
	}
}

// What happens when a RAID1 member is missing at startup (TODO F2).
func TestDegradedRAID1(t *testing.T) {
	a, pool, _, in := setup(t, "RAID1", 2)
	data := randomData(t, 256<<10)
	if err := in.write(0, data); err != nil {
		t.Fatal(err)
	}
	a.tg.stop()
	disk, _ := a.st.GetDisk("diskb")
	if err := os.Rename(disk.DevicePath, disk.DevicePath+".pulled"); err != nil {
		t.Fatal(err)
	}
	a.tg.start()
	a.reconcile()
	raids, err := a.tg.client().BdevRaidGetBdevs(context.Background(), "all")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range raids {
		t.Logf("raid %s state=%s discovered=%d/%d", r.Name, r.State, r.NumBaseBdevsDiscovered, r.NumBaseBdevs)
	}
	p, _ := a.st.GetPool(pool["id"].(string))
	t.Logf("pool health %s: %s", p.HealthStatus, p.StatusMessage)
	if _, err := in.read(0, len(data)); err == nil {
		in.mustRead(0, data)
		t.Logf("RESULT: SPDK assembled the RAID1 degraded and the data is readable")
	} else {
		t.Logf("RESULT: RAID1 with a missing member is not served: %v", err)
		if p.HealthStatus == store.HealthStatusOnline {
			t.Fatalf("the pool is reported ONLINE although its data can't be served")
		}
	}
	if p.LVSUUID != pool["lvs_uuid"] {
		t.Fatalf("the pool's volume store changed: %s -> %s", pool["lvs_uuid"], p.LVSUUID)
	}
}

// Deleting a pool, releasing its disks, and building a new pool on them must work, and the
// old array must not come back on its own (TODO F1).
func TestPoolDeleteThenReuseDisks(t *testing.T) {
	a, pool, vol, _ := setup(t, "RAID1", 2)
	exps := a.must("GET", "/exports", nil, "")
	for _, e := range exps["exports"].([]any) {
		a.must("DELETE", "/exports/"+e.(map[string]any)["id"].(string), nil, "")
	}
	a.must("DELETE", "/volumes/"+vol["id"].(string), nil, "")
	a.must("DELETE", "/deleted-volumes/"+vol["id"].(string), nil, "") // deleted volumes are kept until purged (P1-4)
	a.must("DELETE", "/pools/"+pool["id"].(string), nil, "")
	for _, d := range []string{"diska", "diskb"} {
		a.must("POST", "/disks/"+d+":release", nil, "")
	}
	a.tg.stop()
	a.tg.start()
	for _, d := range []string{"diska", "diskb"} {
		a.must("POST", "/disks/"+d+":claim", nil, "")
	}
	raids, _ := a.tg.client().BdevRaidGetBdevs(context.Background(), "all")
	for _, r := range raids {
		t.Logf("after re-claim: raid %s state=%s (%d/%d)", r.Name, r.State, r.NumBaseBdevsDiscovered, r.NumBaseBdevs)
	}
	code, out := a.do("POST", "/pools", map[string]any{"name": "p2", "raid_level": "RAID1", "disk_ids": []string{"diska", "diskb"}})
	if code >= 300 {
		t.Fatalf("a new pool on released disks failed: HTTP %d: %v (the deleted array came back: %v)", code, out["error"], len(raids) > 0)
	}
}

// SPDK raid5f only accepts full-stripe writes, which the lvol blobstore can't do, so Velocor
// must refuse RAID5F pools up front. This also checks SPDK still behaves that way.
func TestRAID5FRefused(t *testing.T) {
	tg := newTarget(t)
	a := newAppliance(t, tg)
	for _, id := range []string{"diska", "diskb", "diskc"} {
		a.addDisk(id, diskSize)
	}
	if code, out := a.do("POST", "/pools", map[string]any{"name": "p", "raid_level": "RAID5F", "disk_ids": []string{"diska", "diskb", "diskc"}}); code != 400 {
		t.Fatalf("RAID5F pool: want 400, got %d %v", code, out)
	}
	// Confirm the underlying SPDK limitation, so this refusal is revisited if SPDK changes.
	ctx := context.Background()
	var bdevs []string
	for _, id := range []string{"diska", "diskb", "diskc"} {
		d, _ := a.st.GetDisk(id)
		bdevs = append(bdevs, d.SPDKBdevName)
	}
	if err := tg.client().BdevRaidCreate(ctx, "r5", "raid5f", 64, bdevs, true); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.client().BdevLvolCreateLvstore(ctx, "r5", "lvs-r5", 0, ""); err == nil {
		t.Fatalf("SPDK can now create a volume store on raid5f: RAID5F support can be reconsidered")
	}
}

// Group snapshots on real SPDK (docs/review-history.md P1-15, SPDK patch 0003) of exported volumes, one of
// them being written: SPDK quiesces both, snapshots them and resumes I/O in one RPC. The writer
// runs at queue depth 1 with 4 KiB blocks, so its writes are strictly sequential and any point
// in time shows a prefix of new blocks followed by old ones; the snapshot must show exactly that
// (a torn or out-of-order image would not), and the writer must finish without errors.
// Cross-volume ordering (every member quiesced before the first snapshot) is covered by SPDK's
// unit test ut_lvol_snapshot_group.
func TestGroupSnapshots(t *testing.T) {
	const size = 16 << 20
	tg := newTarget(t)
	a := newAppliance(t, tg)
	a.addDisk("diska", diskSize)
	a.addDisk("diskb", diskSize)
	pool := a.must("POST", "/pools", map[string]any{"name": "p", "raid_level": "RAID0", "disk_ids": []string{"diska", "diskb"}}, "pool")
	vol := a.must("POST", "/volumes", map[string]any{"name": "v", "pool_id": pool["id"], "size_bytes": size, "thin_provision": true}, "volume")
	other := a.must("POST", "/volumes", map[string]any{"name": "w", "pool_id": pool["id"], "size_bytes": volSize, "thin_provision": true}, "volume")
	in := a.initiator(a.exportTo(vol["id"].(string), "v", hostA), hostA)
	otherIn := a.initiator(a.exportTo(other["id"].(string), "w", hostA), hostA)
	otherData := randomData(t, 1<<20)
	if err := otherIn.write(0, otherData); err != nil {
		t.Fatal(err)
	}
	oldData := randomData(t, size)
	if err := in.write(0, oldData); err != nil {
		t.Fatal(err)
	}
	ids := []string{vol["id"].(string), other["id"].(string)}

	writes := func() uint64 {
		st, err := tg.client().BdevGetIostat(context.Background(), vol["uuid"].(string))
		if err != nil || len(st.Bdevs) != 1 {
			t.Fatalf("iostat of %s: %v", vol["uuid"], err)
		}
		return st.Bdevs[0].NumWriteOps
	}
	// Retry if the writer happens to finish before the snapshot is taken.
	for attempt := 1; ; attempt++ {
		newData := randomData(t, size)
		in.qd = 1
		before := writes()
		done := make(chan error, 1)
		go func() { done <- in.write(0, newData) }()
		for writes() == before {
			time.Sleep(5 * time.Millisecond)
		}
		name := fmt.Sprintf("g%d", attempt)
		g := a.must("POST", "/volume-group-snapshots", map[string]any{"name": name, "volume_ids": ids}, "")
		atSnapshot := writes()
		if err := <-done; err != nil {
			t.Fatalf("writes during the group snapshot: %v", err)
		}
		in.qd = 0
		if writes() == atSnapshot {
			if attempt == 5 {
				t.Fatal("the writer finished before the group snapshot every time")
			}
			// Free the space the missed snapshots pin.
			a.must("DELETE", "/volume-group-snapshots/"+g["volume_group_snapshot"].(map[string]any)["id"].(string), nil, "")
			oldData = newData
			continue
		}

		members := g["snapshots"].([]any)
		if len(members) != 2 {
			t.Fatalf("group snapshot: %v", g)
		}
		snapOf := map[string]string{}
		for _, m := range members {
			snapOf[m.(map[string]any)["volume_id"].(string)] = m.(map[string]any)["id"].(string)
		}
		clone := a.must("POST", "/snapshots/"+snapOf[vol["id"].(string)]+":clone", map[string]any{"clone_name": "gc"}, "volume")
		got, err := a.initiator(a.exportTo(clone["id"].(string), "gc", hostB), hostB).read(0, size)
		if err != nil {
			t.Fatal(err)
		}
		blocks := size / blockSize
		k := 0
		for k < blocks && bytes.Equal(got[k*blockSize:(k+1)*blockSize], newData[k*blockSize:(k+1)*blockSize]) {
			k++
		}
		for i := k; i < blocks; i++ {
			if !bytes.Equal(got[i*blockSize:(i+1)*blockSize], oldData[i*blockSize:(i+1)*blockSize]) {
				t.Fatalf("snapshot isn't a point in time: blocks 0-%d are new, but block %d is neither the old nor the following new data", k-1, i)
			}
		}
		if k == 0 || k == blocks {
			t.Fatalf("snapshot taken during the writes holds %d of %d new blocks, want a part", k, blocks)
		}
		t.Logf("attempt %d: the snapshot holds the first %d of %d rewritten blocks", attempt, k, blocks)

		otherClone := a.must("POST", "/snapshots/"+snapOf[other["id"].(string)]+":clone", map[string]any{"clone_name": "wc"}, "volume")
		a.initiator(a.exportTo(otherClone["id"].(string), "wc", hostB), hostB).mustRead(0, otherData)
		in.mustRead(0, newData)
		return
	}
}

// Scrub on real SPDK (docs/review-history.md P2-2, SPDK patch 0004): a mirror copy changed behind SPDK's back
// (as after an unclean shutdown or silent corruption) is found by the scrub and rewritten from
// the first member, the volume's data still reads back, and a second, check-only pass finds
// nothing.
func TestScrubRepairsDifferingMirrorCopies(t *testing.T) {
	a, pool, _, in := setup(t, "RAID1", 2)
	data := randomData(t, 1<<20)
	if err := in.write(0, data); err != nil {
		t.Fatal(err)
	}

	// Change 4 KiB of the second member's data area (well past the RAID superblock).
	const off = 64 << 20
	b := filepath.Join(a.tg.dir, "diskb.img")
	f, err := os.OpenFile(b, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(randomData(t, 4096), off); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	id := pool["id"].(string)
	scrub := func(body map[string]any) map[string]any {
		a.t.Helper()
		a.must("POST", "/pools/"+id+"/scrub", body, "")
		deadline := time.Now().Add(2 * time.Minute)
		for {
			v := a.must("GET", "/pools/"+id+"/scrub", nil, "scrub")
			if v["state"] != "RUNNING" {
				return v
			}
			if time.Now().After(deadline) {
				t.Fatalf("scrub still running: %v", v)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	v := scrub(map[string]any{"repair": true})
	total := v["total"].(map[string]any)
	t.Logf("scrub: %v %v", v["state"], total)
	if v["state"] != "COMPLETED" || total["mismatched_blocks"] == float64(0) || total["mismatch_repaired_blocks"] != total["mismatched_blocks"] ||
		total["repaired_blocks"] != float64(0) || total["unrecoverable_blocks"] != float64(0) {
		t.Fatalf("scrub of a pool with one changed copy: %v", v)
	}
	readAt := func(path string) []byte {
		buf := make([]byte, 4096)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if _, err := f.ReadAt(buf, off); err != nil {
			t.Fatal(err)
		}
		return buf
	}
	if !bytes.Equal(readAt(filepath.Join(a.tg.dir, "diska.img")), readAt(b)) {
		t.Fatal("the differing copy wasn't rewritten from the first member")
	}
	in.mustRead(0, data)

	v = scrub(map[string]any{"repair": false})
	if total := v["total"].(map[string]any); v["state"] != "COMPLETED" || total["mismatched_blocks"] != float64(0) {
		t.Fatalf("second scrub: %v", v)
	}
}
