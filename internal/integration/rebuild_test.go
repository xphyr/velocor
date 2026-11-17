//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/store"
)

// spareDisk registers a file-backed SSD with the store without claiming it (replace claims it).
func (a *appliance) spareDisk(id string) {
	a.t.Helper()
	path := a.tg.disk(id+".img", diskSize)
	if err := a.st.UpsertDisk(&store.Disk{ID: id, Name: id, Type: store.DiskTypeSSD, DevicePath: path, SizeBytes: diskSize, BlockSize: 512, SerialNumber: id}); err != nil {
		a.t.Fatal(err)
	}
}

// failDisk makes a member disappear while spdk_tgt runs (hot removal of its bdev): SPDK fails
// it out of its RAID.
func (a *appliance) failDisk(id string) {
	a.t.Helper()
	d, err := a.st.GetDisk(id)
	if err != nil {
		a.t.Fatal(err)
	}
	if err := a.tg.client().BdevAioDelete(context.Background(), d.SPDKBdevName); err != nil {
		a.t.Fatalf("hot-remove %s: %v", id, err)
	}
}

// waitPool reconciles until the pool reaches health (or times out) and returns it.
func (a *appliance) waitPool(id string, health store.HealthStatus, timeout time.Duration) *store.Pool {
	a.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		a.reconcile()
		p, err := a.st.GetPool(id)
		if err != nil {
			a.t.Fatal(err)
		}
		if p.HealthStatus == health {
			return p
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("pool %s is %s (%s) %+v, want %s", id, p.HealthStatus, p.StatusMessage, p.Members, health)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func memberState(p *store.Pool, slot int) store.MemberState { return p.Members[slot].State }

// A member that fails while the pool runs is replaced and rebuilt; the rebuilt disk alone
// then serves everything written before and during the degraded period.
func TestReplaceFailedMemberAndRebuild(t *testing.T) {
	a, pool, _, in := setup(t, "RAID1", 2)
	id := pool["id"].(string)
	before := randomData(t, 1<<20)
	if err := in.write(0, before); err != nil {
		t.Fatal(err)
	}

	a.failDisk("diskb")
	p := a.waitPool(id, store.HealthStatusDegraded, 30*time.Second)
	if memberState(p, 1) != store.MemberStateFailed {
		t.Fatalf("slot 1 after the failure: %+v", p.Members)
	}
	during := randomData(t, 1<<20)
	if err := in.write(512, during); err != nil { // 2 MiB in (4 KiB blocks), written while degraded
		t.Fatal(err)
	}

	a.spareDisk("diskc")
	a.must("POST", "/pools/"+id+"/replace", map[string]any{"new_disk_id": "diskc"}, "pool")
	p = a.waitPool(id, store.HealthStatusOnline, 2*time.Minute)
	if p.Members[1].DiskID != "diskc" || memberState(p, 1) != store.MemberStateActive {
		t.Fatalf("after the rebuild: %+v", p.Members)
	}
	in.mustRead(0, before)
	in.mustRead(512, during)

	// The failed disk's file is still there, but velod doesn't re-attach a disk that left its
	// pool (SPDK would re-add it and rebuild onto it): the array stays diska + diskc.
	a.reconcile()
	if p, _ = a.st.GetPool(id); p.Members[0].DiskID != "diska" || p.Members[1].DiskID != "diskc" || p.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("after the failed disk's return: %s %+v", p.HealthStatus, p.Members)
	}
	if bdevs, err := a.tg.client().BdevGetBdevs(context.Background(), "uring_diskb"); err == nil && len(bdevs) > 0 {
		t.Fatalf("the failed disk was re-attached: %+v", bdevs)
	}

	// Prove the rebuilt copy: lose the original member, serve everything from diskc alone.
	a.failDisk("diska")
	in.mustRead(0, before)
	in.mustRead(512, during)
}

// A member missing at boot leaves the pool UNAVAILABLE until an operator starts it degraded;
// it can then be replaced; the old disk coming back later is left out (velod leaves disks that left the pool detached, and SPDK refuses it because its slot is filled).
func TestMissingAtBootStartDegradedAndReplace(t *testing.T) {
	a, pool, _, in := setup(t, "RAID1", 2)
	id := pool["id"].(string)
	data := randomData(t, 1<<20)
	if err := in.write(0, data); err != nil {
		t.Fatal(err)
	}
	a.tg.stop()
	diskb, _ := a.st.GetDisk("diskb")
	if err := os.Rename(diskb.DevicePath, diskb.DevicePath+".pulled"); err != nil {
		t.Fatal(err)
	}
	a.tg.start()
	p := a.waitPool(id, store.HealthStatusUnavailable, 30*time.Second)
	if memberState(p, 1) != store.MemberStateMissing {
		t.Fatalf("slot 1 at boot: %+v", p.Members)
	}

	if code, out := a.do("POST", "/pools/"+id+"/start-degraded", map[string]any{"confirm": "nope"}); code != 400 {
		t.Fatalf("start-degraded without the right confirmation: HTTP %d %v", code, out)
	}
	a.must("POST", "/pools/"+id+"/start-degraded", map[string]any{"confirm": "p"}, "pool")
	a.waitPool(id, store.HealthStatusDegraded, 30*time.Second)
	in.mustRead(0, data)

	a.spareDisk("diskc")
	a.must("POST", "/pools/"+id+"/replace", map[string]any{"new_disk_id": "diskc"}, "pool")
	a.waitPool(id, store.HealthStatusOnline, 2*time.Minute)
	in.mustRead(0, data)

	// The old disk comes back and the appliance restarts: the array is diska + diskc.
	a.tg.stop()
	if err := os.Rename(diskb.DevicePath+".pulled", diskb.DevicePath); err != nil {
		t.Fatal(err)
	}
	a.tg.start()
	p = a.waitPool(id, store.HealthStatusOnline, 30*time.Second)
	if p.Members[0].DiskID != "diska" || p.Members[1].DiskID != "diskc" {
		t.Fatalf("after the old disk returned: %+v", p.Members)
	}
	in.mustRead(0, data)
}

// spdk_tgt dying mid-rebuild: the array comes back degraded, the reconciler re-adds the
// target and the rebuild completes.
func TestInterruptedRebuildRestarts(t *testing.T) {
	a, pool, _, in := setup(t, "RAID1", 2)
	id := pool["id"].(string)
	data := randomData(t, 1<<20)
	if err := in.write(0, data); err != nil {
		t.Fatal(err)
	}
	a.failDisk("diskb")
	a.waitPool(id, store.HealthStatusDegraded, 30*time.Second)

	// 1 MB/s: the 128 MiB rebuild takes minutes, long enough to interrupt.
	a.must("PUT", "/system/rebuild", map[string]any{"max_mb_per_sec": 1}, "")
	a.spareDisk("diskc")
	a.must("POST", "/pools/"+id+"/replace", map[string]any{"new_disk_id": "diskc"}, "pool")
	a.waitPool(id, store.HealthStatusRebuilding, 30*time.Second)
	time.Sleep(3 * time.Second)
	a.reconcile()
	p, _ := a.st.GetPool(id)
	t.Logf("before the crash: %+v", p.Members)

	a.tg.crash()
	// SPDK applies the limit only to rebuilds that start after it is set, so lift it before the
	// restarted rebuild begins (re-applying the limit after a restart is unit-tested).
	if err := a.st.SaveSettings(&store.Settings{}); err != nil {
		t.Fatal(err)
	}
	a.tg.start()
	p = a.waitPool(id, store.HealthStatusRebuilding, 60*time.Second)
	if p.Members[1].DiskID != "diskc" || !strings.Contains(p.StatusMessage, "restarted") {
		t.Fatalf("after the restart: %q %+v", p.StatusMessage, p.Members)
	}
	a.waitPool(id, store.HealthStatusOnline, 2*time.Minute)
	in.mustRead(0, data)
}
