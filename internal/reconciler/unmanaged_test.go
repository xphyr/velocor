package reconciler

import (
	"strings"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/dataplane/planetest"
	"github.com/velocor/velocor/internal/meta"
	"github.com/velocor/velocor/internal/store"
)

func TestUnmanagedObjectsAreReportedNotDeleted(t *testing.T) {
	e := newEnv(t)
	pool, _ := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	clock := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	e.rec.now = func() time.Time { return clock }

	// Leftovers: a volume with no record (a create whose store write failed), the metadata volume
	// (managed), and an NVMe subsystem with no export. The host's other disks aren't reported.
	leftover, err := e.fake.CreateVolume(e.ctx, pool.ThinPoolName, "vol-lost", 1<<30, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fake.CreateVolume(e.ctx, pool.ThinPoolName, meta.VolumeName, meta.VolumeBytes, false); err != nil {
		t.Fatal(err)
	}
	e.fake.AddDisk("0000:09:00.0")
	e.fake.AddTarget("nqn.2026-11.io.velocor:lost", planetest.Target{})

	if st := e.reconcile(); len(st.Unmanaged) != 0 {
		t.Fatalf("reported before the grace period (the API may be mid-create): %+v", st.Unmanaged)
	}
	clock = clock.Add(2 * time.Minute)
	st := e.reconcile()
	got := map[string]UnmanagedObject{}
	for _, o := range st.Unmanaged {
		got[o.Kind+" "+o.Name] = o
	}
	want := []string{"LV " + pool.ThinPoolName + "/vol-lost", "NVMET_SUBSYSTEM nqn.2026-11.io.velocor:lost"}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("not reported: %s (got %+v)", k, st.Unmanaged)
		}
	}
	if len(got) != len(want) {
		t.Errorf("reported %d objects, want %d (managed objects must not be reported): %+v", len(got), len(want), st.Unmanaged)
	}
	if o := got["LV "+pool.ThinPoolName+"/vol-lost"]; o.UUID != leftover || !o.FirstSeen.Equal(clock.Add(-2*time.Minute)) {
		t.Errorf("volume report: %+v", o)
	}
	if s := UnmanagedSummary(st.Unmanaged); s != "1 LV, 1 NVMET_SUBSYSTEM" {
		t.Errorf("summary %q", s)
	}
	if !e.fake.VolumeExists(pool.ThinPoolName + "/vol-lost") {
		t.Fatal("the reconciler deleted an unmanaged volume")
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
		if o.Kind == "LV" {
			t.Fatalf("recorded volume still reported: %+v", st.Unmanaged)
		}
	}
}

// A lost database leaves whole pools behind: the volume group is reported once, not every volume.
func TestUnmanagedVolumeGroupAfterDatabaseLoss(t *testing.T) {
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
	if kinds["VOLUME_GROUP"] != 1 || kinds["LV"] != 0 {
		t.Fatalf("want the volume group alone: %+v", st.Unmanaged)
	}
}
