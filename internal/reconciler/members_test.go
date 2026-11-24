package reconciler

import (
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/dataplane/spdkplane"
	"github.com/velocor/velocor/internal/store"
)

// spareDisk claims an extra NVMe disk that belongs to no pool.
func (e *env) spareDisk(id, pci string) *store.Disk {
	e.t.Helper()
	e.fake.AddNVMe(pci)
	d := &store.Disk{ID: id, Name: "nvme", Type: store.DiskTypeNVMe, PCIAddress: pci, SizeBytes: 1 << 40, DriverState: store.DiskDriverStateVfioPCI}
	if err := e.st.UpsertDisk(d); err != nil {
		e.t.Fatal(err)
	}
	bdev, drv, err := spdkplane.AttachDisk(e.ctx, e.fake.Client(), nil, d)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := e.st.ClaimDisk(d.ID, drv, bdev)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) pool() *store.Pool {
	e.t.Helper()
	p, err := e.st.GetPool("pool-1")
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestOnlineMemberFailureIsDegraded(t *testing.T) {
	e := newEnv(t)
	e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	e.reconcile()
	if p := e.pool(); p.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("healthy mirror: %s %q", p.HealthStatus, p.StatusMessage)
	}

	e.fake.FailMedia("0000:02:00.0")
	e.reconcile()
	p := e.pool()
	if p.HealthStatus != store.HealthStatusDegraded || !strings.Contains(p.StatusMessage, "disk-b") {
		t.Fatalf("after a member failed: %s %q, want DEGRADED naming disk-b", p.HealthStatus, p.StatusMessage)
	}
	if m := p.Members[1]; m.State != store.MemberStateFailed || m.DiskID != "disk-b" {
		t.Fatalf("slot 1: %+v, want FAILED disk-b", m)
	}
	if len(p.DiskIDs) != 1 || p.DiskIDs[0] != "disk-a" {
		t.Fatalf("DiskIDs %v, want [disk-a]", p.DiskIDs)
	}
}

func TestRebuildProgressAndCompletion(t *testing.T) {
	e := newEnv(t)
	pool, _ := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	e.fake.FailMedia("0000:02:00.0")
	e.reconcile()

	spare := e.spareDisk("disk-c", "0000:03:00.0")
	if _, err := e.st.ReplacePoolMember(pool.ID, 1, spare.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.fake.Client().BdevRaidAddBaseBdev(e.ctx, pool.ArrayName, spare.Handle); err != nil {
		t.Fatal(err)
	}
	e.fake.SetRebuildProgress(pool.ArrayName, 37)
	e.reconcile()
	p := e.pool()
	if p.HealthStatus != store.HealthStatusRebuilding || p.Members[1].RebuildPercent != 37 || p.Members[1].DiskID != "disk-c" {
		t.Fatalf("during rebuild: %s %q %+v", p.HealthStatus, p.StatusMessage, p.Members)
	}

	e.fake.SetRebuildProgress(pool.ArrayName, 100)
	e.reconcile()
	p = e.pool()
	if p.HealthStatus != store.HealthStatusOnline || p.Members[1].State != store.MemberStateActive {
		t.Fatalf("after rebuild: %s %q %+v", p.HealthStatus, p.StatusMessage, p.Members)
	}
	if len(p.DetachedDiskIDs) != 1 || p.DetachedDiskIDs[0] != "disk-b" {
		t.Fatalf("detached %v, want [disk-b]", p.DetachedDiskIDs)
	}
}

// A member whose bdev disappears while its drive is still there (an I/O error took it out,
// a transient disconnect) must not be re-attached: SPDK would re-add it and rebuild onto a
// disk that just failed. It stays out until an operator acts.
func TestFailedMemberIsNotReattached(t *testing.T) {
	e := newEnv(t)
	pool, _ := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	e.reconcile()
	if err := e.fake.Client().BdevNvmeDetachController(e.ctx, "Nvme_0000_02_00_0"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		e.reconcile()
	}
	p := e.pool()
	if p.HealthStatus != store.HealthStatusDegraded || p.Members[1].State != store.MemberStateFailed {
		t.Fatalf("after the member failed: %s %q %+v", p.HealthStatus, p.StatusMessage, p.Members)
	}
	if n := e.fake.CallCount("bdev_nvme_attach_controller"); n != 2 {
		t.Fatalf("the failed member was re-attached (%d attaches, want the 2 at provisioning)", n)
	}
	if n := e.fake.CallCount("bdev_raid_add_base_bdev"); n != 0 {
		t.Fatalf("a rebuild was started onto the failed disk")
	}
	_ = pool
}
