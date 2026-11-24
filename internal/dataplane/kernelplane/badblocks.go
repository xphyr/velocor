package kernelplane

import (
	"cmp"
	"slices"

	"github.com/velocor/velocor/internal/kernel"
)

// md's RAID10 layout number for near-2 (two copies, adjacent): the only one velod creates.
const raid10Near2 = 0x102

// unreadableSectors is how much of an array no in-sync member can supply: the ranges md's
// bad-block logs hold on every copy (on the only copy, for RAID0). md fails reads there, and
// writes there too, so whatever data was there is lost. Members that aren't in sync (being
// rebuilt, spares, failed) hold no usable copy.
func unreadableSectors(st *kernel.ArrayStatus) uint64 {
	bad := map[int][]kernel.SectorRange{}
	inSync := map[int]bool{}
	for _, m := range st.Members {
		if m.Slot >= 0 && m.Has("in_sync") && !m.Has("faulty") {
			inSync[m.Slot] = true
			bad[m.Slot] = m.BadBlocks
		}
	}
	switch st.Level {
	case "raid0":
		var n uint64
		for _, rs := range bad {
			n += sum(rs)
		}
		return n
	case "raid1":
		var lost []kernel.SectorRange
		first := true
		for slot := range st.RaidDisks {
			if !inSync[slot] {
				continue
			}
			if first {
				lost, first = bad[slot], false
			} else {
				lost = intersect(lost, bad[slot])
			}
		}
		return sum(lost)
	case "raid10":
		if st.Layout != raid10Near2 || st.ChunkSectors == 0 || st.RaidDisks < 2 {
			return 0 // not a layout velod makes: not worked out
		}
		return raid10Near2Unreadable(uint64(st.RaidDisks), st.ChunkSectors, inSync, bad)
	}
	return 0
}

// raid10Near2Unreadable: near-2 lays the chunks out two by two along the members, row after
// row: chunk positions p = row*n + slot, with positions 2k and 2k+1 holding the two copies of
// one chunk (with an odd number of members a pair spans the end of one row and the start of
// the next). A bad range is lost where its other copy is bad too, or not in sync. Each lost
// piece is counted once, from the copy at the even position (or the only in-sync one).
func raid10Near2Unreadable(n, chunk uint64, inSync map[int]bool, bad map[int][]kernel.SectorRange) uint64 {
	var lost uint64
	for slot, rs := range bad {
		for _, r := range rs {
			for s, end := r.Start, r.Start+r.Sectors; s < end; {
				row, in := s/chunk, s%chunk
				piece := min(end-s, chunk-in) // within one chunk
				p := row*n + uint64(slot)
				q := p ^ 1 // the other copy's position
				qslot, qstart := int(q%n), (q/n)*chunk+in
				switch {
				case !inSync[qslot]:
					lost += piece
				case p%2 == 0:
					lost += sum(intersect([]kernel.SectorRange{{Start: qstart, Sectors: piece}}, bad[qslot]))
				}
				s += piece
			}
		}
	}
	return lost
}

// intersect returns the parts of a that are also in b.
func intersect(a, b []kernel.SectorRange) []kernel.SectorRange {
	var out []kernel.SectorRange
	for _, x := range a {
		for _, y := range b {
			lo, hi := max(x.Start, y.Start), min(x.Start+x.Sectors, y.Start+y.Sectors)
			if lo < hi {
				out = append(out, kernel.SectorRange{Start: lo, Sectors: hi - lo})
			}
		}
	}
	slices.SortFunc(out, func(a, b kernel.SectorRange) int { return cmp.Compare(a.Start, b.Start) })
	return out
}

func sum(rs []kernel.SectorRange) uint64 {
	var n uint64
	for _, r := range rs {
		n += r.Sectors
	}
	return n
}
