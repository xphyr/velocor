package reconciler

import (
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/store"
)

// spareDisk claims an extra NVMe disk that belongs to no pool.
func (e *env) spareDisk(id, pci string) *store.Disk {
	e.t.Helper()
	return e.addDisk(&store.Disk{ID: id, Name: "nvme", Type: store.DiskTypeNVMe, PCIAddress: pci, SizeBytes: 1 << 40})
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

	e.fake.FailDisk("0000:02:00.0")
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
	e.fake.FailDisk("0000:02:00.0")
	e.reconcile()

	spare := e.spareDisk("disk-c", "0000:03:00.0")
	if _, err := e.st.ReplacePoolMember(pool.ID, 1, spare.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.fake.AddMember(e.ctx, pool.ArrayName, spare.Handle); err != nil {
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

// A member that failed (an I/O error took it out) while its drive is still there must not be
// put back: that would rebuild onto a disk that just failed. It stays out until an operator acts.
func TestFailedMemberIsNotPutBack(t *testing.T) {
	e := newEnv(t)
	e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	e.reconcile()
	e.fake.FailDisk("0000:02:00.0")
	before := len(e.fake.Calls())
	for range 3 {
		e.reconcile()
	}
	p := e.pool()
	if p.HealthStatus != store.HealthStatusDegraded || p.Members[1].State != store.MemberStateFailed {
		t.Fatalf("after the member failed: %s %q %+v", p.HealthStatus, p.StatusMessage, p.Members)
	}
	for _, c := range e.fake.Calls()[before:] {
		if c == "AddMember" || c == "AttachDisk" {
			t.Fatalf("%s on the failed disk: %v", c, e.fake.Calls()[before:])
		}
	}
}
