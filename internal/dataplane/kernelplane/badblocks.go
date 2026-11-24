package kernelplane

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/kernel"
)

// md's RAID10 layout number for near-2 (two copies, adjacent): the only one velod creates.
const raid10Near2 = 0x102

// unreadable is the part of an array no in-sync member can supply: the ranges md's bad-block
// logs hold on every copy (on the only copy, for RAID0). md fails reads there until the data is
// written again, so whatever was there is lost. Members that aren't in sync (being rebuilt,
// spares, failed) hold no usable copy. Ranges are in array sectors, sorted and merged; located
// is false where the layout isn't worked out (RAID0 over members of different sizes, which md
// lays out in zones), with sectors still the total.
type unreadable struct {
	ranges  []kernel.SectorRange
	sectors uint64
	located bool
}

func unreadableOf(st *kernel.ArrayStatus) unreadable {
	bad := map[int][]kernel.SectorRange{}
	inSync := map[int]bool{}
	var sizes []uint64
	for _, m := range st.Members {
		if m.Slot >= 0 && m.Has("in_sync") && !m.Has("faulty") {
			inSync[m.Slot] = true
			bad[m.Slot] = m.BadBlocks
			sizes = append(sizes, m.Size)
		}
	}
	var lost []kernel.SectorRange
	switch st.Level {
	case "raid0":
		n, chunk := uint64(st.RaidDisks), st.ChunkSectors
		equal := len(sizes) > 0 && !slices.ContainsFunc(sizes, func(s uint64) bool { return s != sizes[0] })
		if !equal || chunk == 0 {
			var total uint64
			for _, rs := range bad {
				total += sum(rs)
			}
			return unreadable{sectors: total}
		}
		// One zone: chunk c of the array is chunk c/n of member c%n.
		for slot, rs := range bad {
			for _, r := range rs {
				eachChunk(r, chunk, func(row, in, n2 uint64) {
					lost = append(lost, kernel.SectorRange{Start: (row*n+uint64(slot))*chunk + in, Sectors: n2})
				})
			}
		}
	case "raid1":
		first := true
		for slot := range st.RaidDisks {
			if !inSync[slot] {
				continue
			}
			if first {
				lost, first = slices.Clone(bad[slot]), false
			} else {
				lost = intersect(lost, bad[slot])
			}
		}
	case "raid10":
		if st.Layout != raid10Near2 || st.ChunkSectors == 0 || st.RaidDisks < 2 {
			return unreadable{located: true} // not a layout velod makes: not worked out
		}
		lost = raid10Near2Lost(uint64(st.RaidDisks), st.ChunkSectors, inSync, bad)
	default:
		return unreadable{located: true}
	}
	lost = merge(lost)
	return unreadable{ranges: lost, sectors: sum(lost), located: true}
}

// raid10Near2Lost: near-2 lays the chunks out two by two along the members, row after row:
// chunk positions p = row*n + slot, with positions 2k and 2k+1 holding the two copies of array
// chunk k (with an odd number of members a pair spans the end of one row and the start of the
// next). A bad range is lost where its other copy is bad too, or not in sync.
func raid10Near2Lost(n, chunk uint64, inSync map[int]bool, bad map[int][]kernel.SectorRange) []kernel.SectorRange {
	var lost []kernel.SectorRange
	for slot, rs := range bad {
		for _, r := range rs {
			eachChunk(r, chunk, func(row, in, size uint64) {
				p := row*n + uint64(slot)
				q := p ^ 1 // the other copy's position
				qslot, qstart := int(q%n), (q/n)*chunk+in
				arrayStart := (p/2)*chunk + in
				if !inSync[qslot] {
					lost = append(lost, kernel.SectorRange{Start: arrayStart, Sectors: size})
					return
				}
				for _, x := range intersect([]kernel.SectorRange{{Start: qstart, Sectors: size}}, bad[qslot]) {
					lost = append(lost, kernel.SectorRange{Start: arrayStart + x.Start - qstart, Sectors: x.Sectors})
				}
			})
		}
	}
	return lost // each piece found from both copies: merge takes it once
}

// eachChunk calls fn for the parts of r within one chunk each: the chunk's row on the member,
// the offset in it, and the part's size.
func eachChunk(r kernel.SectorRange, chunk uint64, fn func(row, in, size uint64)) {
	for s, end := r.Start, r.Start+r.Sectors; s < end; {
		in := s % chunk
		size := min(end-s, chunk-in)
		fn(s/chunk, in, size)
		s += size
	}
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
	return merge(out)
}

// merge sorts ranges and joins those that overlap or touch.
func merge(rs []kernel.SectorRange) []kernel.SectorRange {
	slices.SortFunc(rs, func(a, b kernel.SectorRange) int { return cmp.Compare(a.Start, b.Start) })
	var out []kernel.SectorRange
	for _, r := range rs {
		if n := len(out); n > 0 && out[n-1].Start+out[n-1].Sectors >= r.Start {
			out[n-1].Sectors = max(out[n-1].Start+out[n-1].Sectors, r.Start+r.Sectors) - out[n-1].Start
			continue
		}
		out = append(out, r)
	}
	return out
}

func sum(rs []kernel.SectorRange) uint64 {
	var n uint64
	for _, r := range rs {
		n += r.Sectors
	}
	return n
}

// lostDataTTL is how long LostData's answer is kept while the lost ranges stay the same: working
// it out dumps the thin pool's mappings, and what they point at changes only as the volumes
// over the lost range are written, discarded or deleted.
const lostDataTTL = 10 * time.Minute

type lostData struct {
	key string
	at  time.Time
	out []dataplane.LostExtent
}

// LostData maps the array's unreadable ranges to what held them: LVM's extents, then within
// the thin pool's data the volumes and snapshots mapping those blocks (kernel.RangeOwners).
func (p *Plane) LostData(ctx context.Context, array string) ([]dataplane.LostExtent, error) {
	st, err := p.k.ArrayStatus(array)
	if err != nil {
		return nil, err
	}
	u := unreadableOf(st)
	if u.sectors == 0 {
		return nil, nil
	}
	if !u.located {
		return []dataplane.LostExtent{{Kind: dataplane.LostUnlocated, Length: u.sectors * 512}}, nil
	}
	key := fmt.Sprint(u.ranges)
	p.mu.Lock()
	c, ok := p.lost[array]
	p.mu.Unlock()
	if ok && c.key == key && time.Since(c.at) < lostDataTTL {
		return c.out, nil
	}
	out, err := p.lostData(ctx, array, u)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.lost == nil {
		p.lost = map[string]lostData{}
	}
	p.lost[array] = lostData{key: key, at: time.Now(), out: out}
	p.mu.Unlock()
	return out, nil
}

func (p *Plane) lostData(ctx context.Context, array string, u unreadable) ([]dataplane.LostExtent, error) {
	vgs, err := p.k.ListVGs(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(vgs, func(vg kernel.VG) bool { return p.arrayName(vg.PV) == array })
	if i < 0 { // no volume group on it (yet): nothing to map onto
		return []dataplane.LostExtent{{Kind: dataplane.LostUnlocated, Length: u.sectors * 512}}, nil
	}
	vg := vgs[i].Name
	ranges := make([]kernel.Extent, len(u.ranges))
	for i, r := range u.ranges {
		ranges[i] = kernel.Extent{Offset: r.Start * 512, Length: r.Sectors * 512}
	}
	owners, err := p.k.RangeOwners(ctx, vg, thinLV, ranges)
	if err != nil {
		return nil, err
	}
	lvs, err := p.k.ListVolumes(ctx, vg)
	if err != nil {
		return nil, err
	}
	var out []dataplane.LostExtent
	for _, o := range owners {
		e := dataplane.LostExtent{Offset: o.Offset, Length: o.Length}
		switch o.Kind {
		case kernel.OwnerThin:
			j := slices.IndexFunc(lvs, func(lv kernel.LV) bool { return lv.SegType == "thin" && lv.ThinID == o.ThinID })
			if j < 0 {
				e.Kind, e.Name = dataplane.LostOther, fmt.Sprintf("thin device %d", o.ThinID)
			} else {
				e.Kind, e.UUID = dataplane.LostVolume, volumeUUID(lvs[j])
			}
		case kernel.OwnerThinMetadata:
			e.Kind = dataplane.LostThinMetadata
		case kernel.OwnerLVMMetadata:
			e.Kind = dataplane.LostLVMMetadata
		case kernel.OwnerLV:
			j := slices.IndexFunc(lvs, func(lv kernel.LV) bool { return lv.Name == o.LV })
			switch {
			case o.LV == metaLV:
				e.Kind = dataplane.LostPoolMetadata
			case j >= 0 && isVolume(lvs[j]): // a thick volume
				e.Kind, e.UUID = dataplane.LostVolume, volumeUUID(lvs[j])
			default:
				e.Kind, e.Name = dataplane.LostOther, o.LV
			}
		default: // free space, the spare metadata copy: nothing lost
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
