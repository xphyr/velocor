package store

import (
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// What the reconciler repeats on every pass commits (and so fsyncs) only when something changed.
func TestUnchangedRecordsArentWritten(t *testing.T) {
	st, err := OpenBoltStore(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	commits := func(f func()) int {
		id := func() (n int) {
			must(st.db.View(func(tx *bolt.Tx) error { n = tx.ID(); return nil }))
			return n
		}
		before := id()
		f()
		return id() - before
	}
	scan := &Disk{ID: "d1", Name: "sdb", Type: DiskTypeSSD, SerialNumber: "SN1", SizeBytes: 1 << 40, WriteCache: WriteCacheWriteBack}
	must(st.SyncDiscoveredDisk(scan))
	_, err = st.ClaimDisk("d1", DiskDriverStateKernel, "/dev/disk/by-id/d1")
	must(err)
	must(st.SyncDiscoveredDisk(scan))
	if n := commits(func() { must(st.SyncDiscoveredDisk(scan)) }); n != 0 {
		t.Errorf("a scan that found nothing new: %d commits", n)
	}
	moved := *scan
	moved.Name = "sdc"
	if n := commits(func() { must(st.SyncDiscoveredDisk(&moved)) }); n != 1 {
		t.Errorf("a scan that found a change: %d commits", n)
	}
	if d, _ := st.GetDisk("d1"); d.Name != "sdc" || !d.Claimed || d.Handle == "" {
		t.Errorf("disk after the scan: %+v", d)
	}

	must(st.CreatePool(&Pool{ID: "p1", Name: "p1", DiskIDs: []string{"d1"}}))
	if n := commits(func() {
		_, err := st.MutatePool("p1", func(*Pool) error { return nil })
		must(err)
	}); n != 0 {
		t.Errorf("a mutation that changed nothing: %d commits", n)
	}
	for _, id := range []string{"v1", "v2", "v3"} {
		must(st.CreateVolume(&Volume{ID: id, Name: id, PoolID: "p1", SizeBytes: 1 << 30}))
	}
	must(st.CreateSnapshot(&Snapshot{ID: "s1", Name: "s1", VolumeID: "v1", PoolID: "p1"}))
	held := map[string]uint64{"v1": 1, "v2": 2, "v3": 3, "gone": 9}
	if n := commits(func() { must(st.UpdateSpaceUsage(held, map[string]uint64{"s1": 7})) }); n != 1 {
		t.Errorf("space of several volumes: %d commits", n)
	}
	if n := commits(func() { must(st.UpdateSpaceUsage(held, nil)) }); n != 0 {
		t.Errorf("unchanged space: %d commits", n)
	}
	v, _ := st.GetVolume("v3")
	s, _ := st.GetSnapshot("s1")
	if v.AllocatedBytes != 3 || v.Name != "v3" || s.AllocatedBytes != 7 || s.Name != "s1" {
		t.Errorf("after the update: %+v %+v", v, s)
	}
}
