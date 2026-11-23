package dataplane

import (
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/store"
)

func movedSB(level uint32, seq uint64, members ...string) *hardware.RaidSuperblock {
	sb := &hardware.RaidSuperblock{UUID: "raid-uuid", Name: "raid_pool-1", Level: level, BlockSize: 512, SeqNumber: seq}
	for i, u := range members {
		st := uint32(hardware.RaidSBMemberConfigured)
		if strings.HasPrefix(u, "-") {
			u, st = u[1:], hardware.RaidSBMemberFailed
		}
		sb.Members = append(sb.Members, hardware.RaidSBMember{Slot: i, BdevUUID: u, State: st, DataOffset: 2048, DataSize: 1 << 20})
	}
	return sb
}

func movedDisk(id string, sb *hardware.RaidSuperblock) MovedDisk {
	return MovedDisk{Disk: &store.Disk{ID: id, Type: store.DiskTypeSSD, SizeBytes: 1 << 30, BlockSize: 512}, SB: sb}
}

func TestMatchMovedMembers(t *testing.T) {
	cur := movedSB(1, 7, "u-a", "u-b")
	old := movedSB(1, 5, "u-a", "u-b")
	stripe := movedSB(0, 7, "u-a", "u-b")
	for _, tc := range []struct {
		name    string
		newest  *hardware.RaidSuperblock
		want    []string
		found   []MovedDisk
		match   string // "disk=uuid,..." sorted by disk
		errPart string
	}{
		{name: "one member, one disk", newest: stripe, want: []string{"u-b"}, found: []MovedDisk{movedDisk("x", stripe)}, match: "x=u-b"},
		{name: "mirror, both members", newest: cur, want: []string{"u-a", "u-b"}, found: []MovedDisk{movedDisk("y", cur), movedDisk("x", cur)}, match: "x=u-a,y=u-b"},
		{name: "mirror, one back: wait", newest: cur, want: []string{"u-a", "u-b"}, found: []MovedDisk{movedDisk("x", cur)}},
		{name: "older copy doesn't count", newest: cur, want: []string{"u-b"}, found: []MovedDisk{movedDisk("x", old)}},
		{name: "member that left is not wanted", newest: movedSB(1, 7, "u-a", "-u-b"), want: []string{"u-b"}, found: []MovedDisk{movedDisk("x", old)}},
		{name: "stripe, both members", newest: stripe, want: []string{"u-a", "u-b"}, found: []MovedDisk{movedDisk("x", stripe), movedDisk("y", stripe)}, errPart: "can't tell which is which"},
		{name: "more disks than members", newest: cur, want: []string{"u-b"}, found: []MovedDisk{movedDisk("x", cur), movedDisk("y", cur)}, errPart: "only 1 of its members"},
		{name: "disk too small", newest: cur, want: []string{"u-b"}, found: []MovedDisk{{Disk: &store.Disk{ID: "x", SizeBytes: 1 << 20, BlockSize: 512}, SB: cur}}, errPart: "smaller than member"},
		{name: "other block size", newest: cur, want: []string{"u-b"}, found: []MovedDisk{{Disk: &store.Disk{ID: "x", SizeBytes: 1 << 30, BlockSize: 4096}, SB: cur}}, errPart: "4096-byte blocks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := MatchMovedMembers(tc.newest, tc.want, tc.found)
			if tc.errPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errPart) {
					t.Fatalf("err %v, want one containing %q", err, tc.errPart)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, id := range []string{"x", "y"} {
				if u, ok := m[id]; ok {
					got = append(got, id+"="+u)
				}
			}
			if strings.Join(got, ",") != tc.match {
				t.Fatalf("match %v, want %q", m, tc.match)
			}
		})
	}
	t.Run("different layouts", func(t *testing.T) {
		sb := movedSB(1, 7, "u-a", "u-b")
		sb.Members[1].DataOffset = 4096
		if _, err := MatchMovedMembers(sb, []string{"u-a", "u-b"}, []MovedDisk{movedDisk("x", sb), movedDisk("y", sb)}); err == nil || !strings.Contains(err.Error(), "different data layouts") {
			t.Fatalf("err %v", err)
		}
	})
}

func TestDiskBdevUUIDKeepsAMovedMembersUUID(t *testing.T) {
	d := &store.Disk{ID: "ssd-new"}
	derived := DiskBdevUUID(d)
	d.MemberUUID = "11111111-2222-5333-8444-555555555555"
	if got := DiskBdevUUID(d); got != d.MemberUUID || got == derived {
		t.Fatalf("DiskBdevUUID = %s, want the kept %s", got, d.MemberUUID)
	}
}
