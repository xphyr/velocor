package reconciler

import (
	"context"
	"testing"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/store"
)

// recoveringPlane reports a member being rebuilt the way the kernel data plane does: it is in its
// slot and the array's Rebuild names it, but it isn't in sync (Configured) until it finishes.
type recoveringPlane struct {
	dataplane.Plane
	slot    int
	percent uint32
	on      bool
}

func (r *recoveringPlane) Arrays(ctx context.Context) ([]dataplane.Array, error) {
	as, err := r.Plane.Arrays(ctx)
	if r.on {
		for i := range as {
			as[i].Members[r.slot].Configured = false
			as[i].Operational--
			as[i].Rebuild = &dataplane.Rebuild{Target: as[i].Members[r.slot].Name, Percent: r.percent}
		}
	}
	return as, err
}

func TestRebuildOfAMemberNotInSyncYet(t *testing.T) {
	e := newEnv(t)
	e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	rp := &recoveringPlane{Plane: e.rec.dp, slot: 1}
	e.rec.dp = rp
	e.reconcile()
	if p := e.pool(); p.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("before: %s %q", p.HealthStatus, p.StatusMessage)
	}
	for _, percent := range []uint32{0, 54, 99} {
		rp.on, rp.percent = true, percent
		e.reconcile()
		p := e.pool()
		m := p.Members[1]
		if p.HealthStatus != store.HealthStatusRebuilding || m.State != store.MemberStateRebuilding || m.RebuildPercent != percent || m.DiskID != "disk-b" {
			t.Fatalf("at %d%%: %s %q %+v, want REBUILDING disk-b", percent, p.HealthStatus, p.StatusMessage, m)
		}
		if len(p.DiskIDs) != 2 || len(p.DetachedDiskIDs) != 0 {
			t.Fatalf("at %d%%: disks %v detached %v: the disk being rebuilt left the pool", percent, p.DiskIDs, p.DetachedDiskIDs)
		}
	}
	rp.on = false
	e.reconcile()
	if p := e.pool(); p.HealthStatus != store.HealthStatusOnline || p.Members[1].State != store.MemberStateActive {
		t.Fatalf("after: %s %q %+v", p.HealthStatus, p.StatusMessage, p.Members[1])
	}
}

// A disk that fails while it is being rebuilt leaves its slot: it stays FAILED, and the
// reconciler never adds it back (that would rebuild onto a failing disk over and over).
func TestFailedRebuildTargetIsNotReAdded(t *testing.T) {
	e := newEnv(t)
	e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	rp := &recoveringPlane{Plane: e.rec.dp, slot: 1, on: true, percent: 40}
	e.rec.dp = rp
	e.reconcile()
	if p := e.pool(); p.Members[1].State != store.MemberStateRebuilding {
		t.Fatalf("rebuilding: %+v", p.Members[1])
	}
	lost := &lostTargetPlane{Plane: rp.Plane, slot: 1}
	e.rec.dp = lost
	for range 3 {
		e.reconcile()
	}
	p := e.pool()
	if p.HealthStatus != store.HealthStatusDegraded || p.Members[1].State != store.MemberStateFailed {
		t.Fatalf("%s %q %+v, want DEGRADED with the target FAILED", p.HealthStatus, p.StatusMessage, p.Members[1])
	}
	if lost.adds != 0 {
		t.Fatalf("the failed rebuild target was added back %d times", lost.adds)
	}
}

// lostTargetPlane reports an array whose slot is empty (its member failed), and counts AddMember.
type lostTargetPlane struct {
	dataplane.Plane
	slot, adds int
}

func (l *lostTargetPlane) Arrays(ctx context.Context) ([]dataplane.Array, error) {
	as, err := l.Plane.Arrays(ctx)
	for i := range as {
		as[i].Members[l.slot] = dataplane.ArrayMember{}
		as[i].Operational--
	}
	return as, err
}

func (l *lostTargetPlane) AddMember(context.Context, string, string) error {
	l.adds++
	return nil
}
