package kernelplane

import (
	"testing"

	"github.com/velocor/velocor/internal/kernel"
)

func br(start, sectors uint64) kernel.SectorRange {
	return kernel.SectorRange{Start: start, Sectors: sectors}
}

func member(slot int, state string, bad ...kernel.SectorRange) kernel.MemberStatus {
	return kernel.MemberStatus{Device: "x", Slot: slot, State: []string{state}, BadBlocks: bad}
}

func TestUnreadableSectors(t *testing.T) {
	raid10 := func(n int, ms ...kernel.MemberStatus) *kernel.ArrayStatus {
		return &kernel.ArrayStatus{Level: "raid10", RaidDisks: n, Layout: raid10Near2, ChunkSectors: 128, Members: ms}
	}
	for _, tc := range []struct {
		name string
		st   *kernel.ArrayStatus
		want uint64
	}{
		{"raid1: bad on one copy only", &kernel.ArrayStatus{Level: "raid1", RaidDisks: 2, Members: []kernel.MemberStatus{
			member(0, "in_sync", br(1000, 8)), member(1, "in_sync")}}, 0},
		{"raid1: the same range on both copies (P0-7 after the rebuild)", &kernel.ArrayStatus{Level: "raid1", RaidDisks: 2, Members: []kernel.MemberStatus{
			member(0, "in_sync", br(4194304, 8)), member(1, "in_sync", br(4194304, 8))}}, 8},
		{"raid1: overlapping ranges: only the overlap", &kernel.ArrayStatus{Level: "raid1", RaidDisks: 2, Members: []kernel.MemberStatus{
			member(0, "in_sync", br(1000, 16), br(5000, 8)), member(1, "in_sync", br(1008, 16))}}, 8},
		{"raid1: degraded, bad on the last copy", &kernel.ArrayStatus{Level: "raid1", RaidDisks: 2, Members: []kernel.MemberStatus{
			member(0, "in_sync", br(1000, 8))}}, 8},
		{"raid1: the rebuild target holds no copy yet", &kernel.ArrayStatus{Level: "raid1", RaidDisks: 2, Members: []kernel.MemberStatus{
			member(0, "in_sync", br(1000, 8)), member(1, "spare")}}, 8},
		{"raid1: three copies, two bad", &kernel.ArrayStatus{Level: "raid1", RaidDisks: 3, Members: []kernel.MemberStatus{
			member(0, "in_sync", br(1000, 8)), member(1, "in_sync", br(1000, 8)), member(2, "in_sync")}}, 0},
		{"raid0: every bad block", &kernel.ArrayStatus{Level: "raid0", RaidDisks: 2, Members: []kernel.MemberStatus{
			member(0, "in_sync", br(1000, 8)), member(1, "in_sync", br(50, 4))}}, 12},
		// Four members: pairs (0,1) and (2,3), at the same offsets.
		{"raid10/4: bad on both copies of a chunk", raid10(4, member(0, "in_sync", br(10, 8)), member(1, "in_sync", br(12, 8)),
			member(2, "in_sync"), member(3, "in_sync")), 6},
		{"raid10/4: bad on two members of different pairs", raid10(4, member(0, "in_sync", br(10, 8)), member(1, "in_sync"),
			member(2, "in_sync", br(10, 8)), member(3, "in_sync")), 0},
		{"raid10/4: the other copy missing", raid10(4, member(0, "in_sync", br(10, 8)), member(2, "in_sync"), member(3, "in_sync")), 8},
		// Five members: position 4 (slot 4, row 0) pairs with position 5 (slot 0, row 1).
		{"raid10/5: a pair across rows", raid10(5, member(0, "in_sync", br(128+5, 4)), member(1, "in_sync"), member(2, "in_sync"),
			member(3, "in_sync"), member(4, "in_sync", br(5, 4))), 4},
		{"raid10/5: same offsets, but not copies of each other", raid10(5, member(0, "in_sync", br(5, 4)), member(1, "in_sync"), member(2, "in_sync"),
			member(3, "in_sync"), member(4, "in_sync", br(5, 4))), 0},
		{"raid10/4: a range across a chunk boundary", raid10(4, member(0, "in_sync", br(120, 16)), member(1, "in_sync", br(124, 4)),
			member(2, "in_sync"), member(3, "in_sync")), 4},
		{"raid10: another layout isn't worked out", &kernel.ArrayStatus{Level: "raid10", RaidDisks: 4, Layout: 0x201, ChunkSectors: 128,
			Members: []kernel.MemberStatus{member(0, "in_sync", br(10, 8)), member(1, "in_sync", br(10, 8))}}, 0},
	} {
		if got := unreadableSectors(tc.st); got != tc.want {
			t.Errorf("%s: %d sectors, want %d", tc.name, got, tc.want)
		}
	}
}
