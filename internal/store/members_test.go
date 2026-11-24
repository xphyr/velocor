package store

import (
	"errors"
	"slices"
	"testing"
)

func memberTestPool(t *testing.T) *BoltStore {
	t.Helper()
	st := newTestStore(t)
	for _, id := range []string{"d1", "d2", "d3", "boot"} {
		if err := st.UpsertDisk(&Disk{ID: id, Type: DiskTypeSSD, IsBootDevice: id == "boot"}); err != nil {
			t.Fatal(err)
		}
		if id != "boot" {
			if _, err := st.ClaimDisk(id, DiskDriverStateKernel, "/dev/disk/by-id/"+id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.CreatePool(&Pool{ID: "p1", Name: "p1", RaidLevel: "RAID1", DiskIDs: []string{"d1", "d2"}}); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestPoolMembersInitialisedAtCreate(t *testing.T) {
	st := memberTestPool(t)
	p, _ := st.GetPool("p1")
	want := []PoolMember{{Slot: 0, DiskID: "d1", State: MemberStateActive}, {Slot: 1, DiskID: "d2", State: MemberStateActive}}
	if !slices.Equal(p.Members, want) {
		t.Fatalf("members %+v, want %+v", p.Members, want)
	}
	// Pools from before member tracking derive their slots from DiskIDs.
	legacy := &Pool{DiskIDs: []string{"a", "b"}}
	if m := PoolMembers(legacy); len(m) != 2 || m[1].DiskID != "b" || m[1].State != MemberStateActive {
		t.Fatalf("legacy members %+v", m)
	}
}

func TestReplacePoolMemberAndRelease(t *testing.T) {
	st := memberTestPool(t)

	// d2 failed: the reconciler records it.
	if _, err := st.MutatePool("p1", func(p *Pool) error {
		m := PoolMembers(p)
		m[1].State = MemberStateFailed
		SetPoolMembers(p, m)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p, _ := st.GetPool("p1")
	if !slices.Equal(p.DiskIDs, []string{"d1"}) {
		t.Fatalf("DiskIDs with a failed member: %v", p.DiskIDs)
	}

	// Refusals: boot disk, a disk of another pool, a slot that doesn't exist, unclaimed disk.
	for _, tc := range []struct {
		disk string
		slot int
		want error
	}{
		{"boot", 1, ErrBootDeviceProtected},
		{"d1", 1, ErrInUse},
		{"d3", 5, ErrInvalidArgument},
		{"nope", 1, ErrNotFound},
	} {
		if _, err := st.ReplacePoolMember("p1", tc.slot, tc.disk); !errors.Is(err, tc.want) {
			t.Errorf("replace slot %d with %s: %v, want %v", tc.slot, tc.disk, err, tc.want)
		}
	}

	p, err := st.ReplacePoolMember("p1", 1, "d3")
	if err != nil {
		t.Fatal(err)
	}
	if p.Members[1] != (PoolMember{Slot: 1, DiskID: "d3", State: MemberStateRebuilding}) ||
		!slices.Equal(p.DiskIDs, []string{"d1", "d3"}) || !slices.Equal(p.DetachedDiskIDs, []string{"d2"}) {
		t.Fatalf("after replace: %+v", p)
	}
	if d, _ := st.GetDisk("d3"); d.PoolID != "p1" {
		t.Fatalf("new disk not bound to the pool: %+v", d)
	}
	if d, _ := st.GetDisk("d2"); d.PoolID != "p1" {
		t.Fatalf("detached disk must stay bound until released: %+v", d)
	}

	// Releasing: only detached disks.
	if _, err := st.ReleasePoolDisk("p1", "d1"); !errors.Is(err, ErrInUse) {
		t.Fatalf("release of an active member: %v", err)
	}
	if p, err = st.ReleasePoolDisk("p1", "d2"); err != nil {
		t.Fatal(err)
	}
	if len(p.DetachedDiskIDs) != 0 {
		t.Fatalf("still detached: %v", p.DetachedDiskIDs)
	}
	if d, _ := st.GetDisk("d2"); d.PoolID != "" {
		t.Fatalf("released disk still bound: %+v", d)
	}
}

func TestDeletePoolReleasesDetachedDisks(t *testing.T) {
	st := memberTestPool(t)
	if _, err := st.ReplacePoolMember("p1", 1, "d3"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePool("p1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"d1", "d2", "d3"} {
		if d, _ := st.GetDisk(id); d.PoolID != "" {
			t.Errorf("disk %s still bound after pool delete", id)
		}
	}
}

// A failed or missing member's disk is in neither DiskIDs nor DetachedDiskIDs (it moves to the
// detached list only when replaced), yet still belongs to the pool: deleting the pool must free
// it too, or it can never be released or claimed again (found in the lab, 2026-11-20).
func TestDeletePoolReleasesFailedAndMissingMembers(t *testing.T) {
	st := memberTestPool(t)
	if _, err := st.MutatePool("p1", func(p *Pool) error {
		m := PoolMembers(p)
		m[0].State = MemberStateFailed
		m[1].State = MemberStateMissing
		SetPoolMembers(p, m)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePool("p1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"d1", "d2"} {
		if d, _ := st.GetDisk(id); d.PoolID != "" {
			t.Errorf("disk %s still bound after pool delete", id)
		}
	}
}

func TestSettingsDefaultsAndSave(t *testing.T) {
	st := newTestStore(t)
	if s, err := st.GetSettings(); err != nil || s.RebuildMaxMBps != 0 {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	if err := st.SaveSettings(&Settings{RebuildMaxMBps: 200}); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.GetSettings(); s.RebuildMaxMBps != 200 {
		t.Fatalf("saved: %+v", s)
	}
}
