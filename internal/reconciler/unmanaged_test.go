package reconciler

import (
	"strings"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/meta"
	"github.com/velocor/velocor/internal/store"
)

func TestUnmanagedSPDKObjectsAreReportedNotDeleted(t *testing.T) {
	e := newEnv(t)
	pool, _ := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	clock := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	e.rec.now = func() time.Time { return clock }
	c := e.fake.Client()

	// Leftovers: an lvol with no record (a create whose store write failed), the metadata lvol
	// (managed), an attached NVMe drive nobody claimed, and an NVMe-oF subsystem with no export.
	leftover, err := c.BdevLvolCreate(e.ctx, pool.ThinPoolName, "vol-lost", 1<<30, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.BdevLvolCreate(e.ctx, pool.ThinPoolName, meta.LvolName, meta.LvolBytes, false, ""); err != nil {
		t.Fatal(err)
	}
	e.fake.AddNVMe("0000:09:00.0")
	if _, err := c.BdevNvmeAttachController(e.ctx, "nvme9", "PCIe", "0000:09:00.0"); err != nil {
		t.Fatal(err)
	}
	if err := c.NvmfCreateSubsystem(e.ctx, "nqn.2026-11.io.velocor:lost", "SN1", "", true, 1); err != nil {
		t.Fatal(err)
	}

	if st := e.reconcile(); len(st.Unmanaged) != 0 {
		t.Fatalf("reported before the grace period (the API may be mid-create): %+v", st.Unmanaged)
	}
	clock = clock.Add(2 * time.Minute)
	st := e.reconcile()
	got := map[string]UnmanagedObject{}
	for _, o := range st.Unmanaged {
		got[o.Kind+" "+o.Name] = o
	}
	want := []string{"LVOL " + pool.ThinPoolName + "/vol-lost", "BDEV nvme9n1", "NVMF_SUBSYSTEM nqn.2026-11.io.velocor:lost"}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("not reported: %s (got %+v)", k, st.Unmanaged)
		}
	}
	if len(got) != len(want) {
		t.Errorf("reported %d objects, want %d (managed objects must not be reported): %+v", len(got), len(want), st.Unmanaged)
	}
	if o := got["LVOL "+pool.ThinPoolName+"/vol-lost"]; o.UUID != leftover || !o.FirstSeen.Equal(clock.Add(-2*time.Minute)) {
		t.Errorf("lvol report: %+v", o)
	}
	if s := UnmanagedSummary(st.Unmanaged); s != "1 BDEV, 1 LVOL, 1 NVMF_SUBSYSTEM" {
		t.Errorf("summary %q", s)
	}
	if !e.fake.LvolExists(pool.ThinPoolName + "/vol-lost") {
		t.Fatal("the reconciler deleted an unmanaged lvol")
	}
	for _, d := range st.DegradedResources {
		if strings.Contains(d, "lost") {
			t.Errorf("an unmanaged object made something degraded: %v", st.DegradedResources)
		}
	}

	// Recording it (e.g. by importing) makes it managed.
	if err := e.st.CreateVolume(&store.Volume{ID: "vol-lost", Name: "lost", PoolID: pool.ID, SizeBytes: 1 << 30, UUID: leftover}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	st = e.reconcile()
	for _, o := range st.Unmanaged {
		if o.Kind == "LVOL" {
			t.Fatalf("recorded lvol still reported: %+v", st.Unmanaged)
		}
	}
}

// A lost database leaves whole pools behind: the lvstore is reported once, not every lvol.
func TestUnmanagedLvstoreAfterDatabaseLoss(t *testing.T) {
	e := newEnv(t)
	pool, _ := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	clock := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	e.rec.now = func() time.Time { return clock }
	if _, err := e.st.MutatePool(pool.ID, func(p *store.Pool) error { p.ThinPoolUUID = "lost"; return nil }); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	clock = clock.Add(2 * time.Minute)
	st := e.reconcile()
	kinds := map[string]int{}
	for _, o := range st.Unmanaged {
		kinds[o.Kind]++
	}
	if kinds["LVSTORE"] != 1 || kinds["LVOL"] != 0 {
		t.Fatalf("want the lvstore alone: %+v", st.Unmanaged)
	}
}
