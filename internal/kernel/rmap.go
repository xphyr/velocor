package kernel

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// What a range of an array holds, as LVM and the thin pool lay it out (RangeOwner.Kind).
const (
	OwnerThin         = "thin"          // a thin volume or snapshot (ThinID, Offset within it)
	OwnerThinMetadata = "thin-metadata" // the thin pool's own metadata
	OwnerLV           = "lv"            // another LV (LV, Offset within it): e.g. velod's metadata copy
	OwnerSpare        = "spare"         // LVM's spare copy of the thin metadata, unused until a repair
	OwnerLVMMetadata  = "lvm-metadata"  // the PV's label and LVM's own metadata, before the first extent
	OwnerFree         = "free"          // nothing: unallocated extents or thin data blocks no volume maps
)

// RangeOwner is what holds part of an array: a range of an LV or thin volume, in bytes.
type RangeOwner struct {
	Kind   string
	LV     string // the LV's name (OwnerLV, OwnerThinMetadata)
	ThinID int    // OwnerThin: the thin device; LVM's thin_id
	Offset uint64 // within the LV or thin volume
	Length uint64
}

// RangeOwners maps byte ranges of the array under volume group vg (its only PV) to what holds
// them: LVM's extents, and inside the thin pool's data, the thin volumes mapping those blocks (a
// block shared by snapshots has an owner each). It reads the pool's mappings from a metadata
// snapshot (thin_dump), so the pool keeps serving I/O meanwhile.
func (p *Plane) RangeOwners(ctx context.Context, vg, pool string, ranges []Extent) ([]RangeOwner, error) {
	out, err := p.lvm(ctx, "pvs", "--reportformat", "json", "--units", "b", "--nosuffix",
		"-o", "pv_name,pe_start,vg_extent_size", "-S", "vg_name="+vg)
	if err != nil {
		return nil, err
	}
	peStart, extent, err := parsePVStart(out)
	if err != nil {
		return nil, err
	}
	out, err = p.lvm(ctx, "lvs", "-a", "--reportformat", "json", "--units", "b", "--nosuffix",
		"-o", "lv_name,segtype,seg_start,seg_pe_ranges", vg)
	if err != nil {
		return nil, err
	}
	segs, err := parseSegments(out, extent)
	if err != nil {
		return nil, err
	}
	owners, tdata := lvmOwners(ranges, peStart, segs, pool)
	if len(tdata) == 0 {
		return owners, nil
	}
	var dump []byte
	if err := p.withMetadataSnap(ctx, vg, pool, func(meta string) error {
		dump, err = p.Run.Run(ctx, "thin_dump", "--metadata-snap", meta)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read the thin pool's mappings: %w", err)
	}
	thin, err := thinOwners(dump, tdata)
	if err != nil {
		return nil, err
	}
	return append(owners, thin...), nil
}

// segment is one LV segment's physical extents, as bytes on the PV's data area.
type segment struct {
	lv      string // without LVM's brackets for hidden LVs
	offset  uint64 // where the segment starts in the LV
	extents []Extent
}

func parsePVStart(out []byte) (peStart, extent uint64, err error) {
	var rep struct {
		Report []struct {
			PV []map[string]string `json:"pv"`
		} `json:"report"`
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		return 0, 0, fmt.Errorf("pvs report: %w", err)
	}
	for _, r := range rep.Report {
		if len(r.PV) != 1 {
			return 0, 0, fmt.Errorf("pvs report: %d physical volumes, expected one", len(r.PV))
		}
		peStart, _ = strconv.ParseUint(r.PV[0]["pe_start"], 10, 64)
		extent, _ = strconv.ParseUint(r.PV[0]["vg_extent_size"], 10, 64)
	}
	if extent == 0 {
		return 0, 0, errors.New("pvs report: no extent size")
	}
	return peStart, extent, nil
}

// parseSegments reads the LVs' segments on the PV ("seg_pe_ranges": "/dev/md126:448-57121",
// several separated by spaces; LVs stacked on other LVs, such as the thin pool on its data LV,
// name an LV instead of the PV, and thin volumes none: both skipped).
func parseSegments(out []byte, extent uint64) ([]segment, error) {
	var rep struct {
		Report []struct {
			LV []map[string]string `json:"lv"`
		} `json:"report"`
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		return nil, fmt.Errorf("lvs report: %w", err)
	}
	var segs []segment
	for _, r := range rep.Report {
		for _, m := range r.LV {
			s := segment{lv: strings.Trim(m["lv_name"], "[]")}
			s.offset, _ = strconv.ParseUint(m["seg_start"], 10, 64)
			for _, pr := range strings.Fields(m["seg_pe_ranges"]) {
				dev, rng, ok := strings.Cut(pr, ":")
				if !ok || !strings.HasPrefix(dev, "/dev/") {
					continue
				}
				a, b, ok := strings.Cut(rng, "-")
				first, err1 := strconv.ParseUint(a, 10, 64)
				last, err2 := strconv.ParseUint(b, 10, 64)
				if !ok || err1 != nil || err2 != nil || last < first {
					return nil, fmt.Errorf("lvs report: physical extents %q", pr)
				}
				s.extents = append(s.extents, Extent{Offset: first * extent, Length: (last - first + 1) * extent})
			}
			if len(s.extents) > 0 {
				segs = append(segs, s)
			}
		}
	}
	return segs, nil
}

// lvmOwners maps array ranges onto the LVs' extents. Ranges in the thin pool's data LV are
// returned apart, as offsets within it, for the thin mappings to resolve.
func lvmOwners(ranges []Extent, peStart uint64, segs []segment, pool string) (owners []RangeOwner, tdata []Extent) {
	for _, r := range ranges {
		if r.Offset < peStart {
			n := min(r.Length, peStart-r.Offset)
			owners = append(owners, RangeOwner{Kind: OwnerLVMMetadata, Offset: r.Offset, Length: n})
			r = Extent{Offset: peStart, Length: r.Length - n}
			if r.Length == 0 {
				continue
			}
		}
		// In the PV's data area from here; whatever no segment covers is unallocated.
		todo := []Extent{{Offset: r.Offset - peStart, Length: r.Length}}
		for _, s := range segs {
			lvOff := s.offset
			for _, e := range s.extents {
				var rest []Extent
				for _, t := range todo {
					lo, hi := max(t.Offset, e.Offset), min(t.Offset+t.Length, e.Offset+e.Length)
					if lo >= hi {
						rest = append(rest, t)
						continue
					}
					if t.Offset < lo {
						rest = append(rest, Extent{Offset: t.Offset, Length: lo - t.Offset})
					}
					if hi < t.Offset+t.Length {
						rest = append(rest, Extent{Offset: hi, Length: t.Offset + t.Length - hi})
					}
					in := Extent{Offset: lvOff + lo - e.Offset, Length: hi - lo}
					switch s.lv {
					case pool + "_tdata":
						tdata = append(tdata, in)
					case pool + "_tmeta":
						owners = append(owners, RangeOwner{Kind: OwnerThinMetadata, LV: s.lv, Offset: in.Offset, Length: in.Length})
					case "lvol0_pmspare":
						owners = append(owners, RangeOwner{Kind: OwnerSpare, LV: s.lv, Offset: in.Offset, Length: in.Length})
					default:
						owners = append(owners, RangeOwner{Kind: OwnerLV, LV: s.lv, Offset: in.Offset, Length: in.Length})
					}
				}
				todo = rest
				lvOff += e.Length
			}
		}
		for _, t := range todo {
			owners = append(owners, RangeOwner{Kind: OwnerFree, Offset: t.Offset + peStart, Length: t.Length})
		}
	}
	return owners, tdata
}

// thinMappings is a <device> or <def> of thin_dump's XML: mappings, and references to defs.
// thin_dump writes subtrees devices share (a volume and its snapshots) once, as a named <def>
// the devices (and other defs) refer to with <ref name>.
type thinMappings struct {
	Ranges []struct {
		Origin uint64 `xml:"origin_begin,attr"`
		Data   uint64 `xml:"data_begin,attr"`
		Length uint64 `xml:"length,attr"`
	} `xml:"range_mapping"`
	Singles []struct {
		Origin uint64 `xml:"origin_block,attr"`
		Data   uint64 `xml:"data_block,attr"`
	} `xml:"single_mapping"`
	Refs []struct {
		Name string `xml:"name,attr"`
	} `xml:"ref"`
}

// thinOwners maps ranges of the thin pool's data LV to the thin volumes whose blocks they are,
// from thin_dump's XML of every device; blocks no device maps hold nothing.
func thinOwners(dump []byte, tdata []Extent) ([]RangeOwner, error) {
	var doc struct {
		BlockSize uint64 `xml:"data_block_size,attr"`
		Defs      []struct {
			Name string `xml:"name,attr"`
			thinMappings
		} `xml:"def"`
		Devices []struct {
			ID int `xml:"dev_id,attr"`
			thinMappings
		} `xml:"device"`
	}
	if err := xml.Unmarshal(dump, &doc); err != nil {
		return nil, fmt.Errorf("thin_dump output: %w", err)
	}
	bs := doc.BlockSize * 512
	if bs == 0 {
		return nil, errors.New("thin_dump output has no data_block_size")
	}
	defs := map[string]*thinMappings{}
	for i := range doc.Defs {
		defs[doc.Defs[i].Name] = &doc.Defs[i].thinMappings
	}
	type mapping struct {
		dev                int
		origin, data, size uint64 // bytes
	}
	var maps []mapping
	var walk func(dev int, m *thinMappings, depth int) error
	walk = func(dev int, m *thinMappings, depth int) error {
		if depth > 64 {
			return errors.New("thin_dump output: references nest too deep")
		}
		for _, r := range m.Ranges {
			maps = append(maps, mapping{dev, r.Origin * bs, r.Data * bs, r.Length * bs})
		}
		for _, s := range m.Singles {
			maps = append(maps, mapping{dev, s.Origin * bs, s.Data * bs, bs})
		}
		for _, ref := range m.Refs {
			d := defs[ref.Name]
			if d == nil {
				return fmt.Errorf("thin_dump output: reference to an unknown def %q", ref.Name)
			}
			if err := walk(dev, d, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	for i := range doc.Devices {
		if err := walk(doc.Devices[i].ID, &doc.Devices[i].thinMappings, 0); err != nil {
			return nil, err
		}
	}
	var owners []RangeOwner
	for _, t := range tdata {
		mapped := []Extent{}
		for _, m := range maps {
			lo, hi := max(t.Offset, m.data), min(t.Offset+t.Length, m.data+m.size)
			if lo >= hi {
				continue
			}
			owners = append(owners, RangeOwner{Kind: OwnerThin, ThinID: m.dev, Offset: m.origin + lo - m.data, Length: hi - lo})
			mapped = append(mapped, Extent{Offset: lo, Length: hi - lo})
		}
		for _, f := range uncovered(t, mapped) {
			owners = append(owners, RangeOwner{Kind: OwnerFree, Offset: f.Offset, Length: f.Length})
		}
	}
	return owners, nil
}

// uncovered returns the parts of r none of covered overlaps.
func uncovered(r Extent, covered []Extent) []Extent {
	sort.Slice(covered, func(i, j int) bool { return covered[i].Offset < covered[j].Offset })
	var out []Extent
	at, end := r.Offset, r.Offset+r.Length
	for _, c := range covered {
		if c.Offset > at {
			out = append(out, Extent{Offset: at, Length: min(c.Offset, end) - at})
		}
		at = max(at, c.Offset+c.Length)
	}
	if at < end {
		out = append(out, Extent{Offset: at, Length: end - at})
	}
	return out
}
