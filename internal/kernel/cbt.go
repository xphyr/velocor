package kernel

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
)

// Extent is a range of a volume, in bytes.
type Extent struct {
	Offset, Length uint64
}

// cbtLocks serializes CBT per pool: a pool can hold one metadata snapshot at a time.
var cbtLocks sync.Map // vg/pool -> *sync.Mutex

func poolLock(vg, pool string) *sync.Mutex {
	m, _ := cbtLocks.LoadOrStore(vg+"/"+pool, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// withMetadataSnap runs fn with a metadata snapshot of the live pool reserved, so the thin tools
// can read the pool's mappings while it serves I/O, and releases it afterwards.
func (p *Plane) withMetadataSnap(ctx context.Context, vg, pool string, fn func(meta string) error) error {
	l := poolLock(vg, pool)
	l.Lock()
	defer l.Unlock()
	target := dmName(vg, pool) + "-tpool"
	if _, err := p.Run.Run(ctx, "dmsetup", "message", target, "0", "reserve_metadata_snap"); err != nil {
		return err
	}
	err := fn(p.DevRoot + "/mapper/" + dmName(vg, pool+"_tmeta"))
	if _, rerr := p.Run.Run(context.WithoutCancel(ctx), "dmsetup", "message", target, "0", "release_metadata_snap"); rerr != nil {
		err = errors.Join(err, fmt.Errorf("release metadata snapshot: %w", rerr))
	}
	return err
}

// ChangedBlocks returns the ranges where two thin volumes of a pool (snapshots of one volume,
// older and newer) differ, in whole chunks, cut at sizeBytes: written on one side only,
// written on both to different blocks, or discarded on one side.
func (p *Plane) ChangedBlocks(ctx context.Context, vg, pool string, oldThinID, newThinID int, sizeBytes uint64) ([]Extent, error) {
	var ext []Extent
	err := p.withMetadataSnap(ctx, vg, pool, func(meta string) error {
		out, err := p.Run.Run(ctx, "thin_delta", "--metadata-snap", "--snap1", strconv.Itoa(oldThinID),
			"--snap2", strconv.Itoa(newThinID), meta)
		if err != nil {
			return err
		}
		ext, err = parseThinDelta(out, sizeBytes)
		return err
	})
	return ext, err
}

// AllocatedBlocks returns the ranges a thin volume maps (its own and those it shares), in whole
// chunks, cut at sizeBytes: everything a full backup must read.
func (p *Plane) AllocatedBlocks(ctx context.Context, vg, pool string, thinID int, sizeBytes uint64) ([]Extent, error) {
	var ext []Extent
	err := p.withMetadataSnap(ctx, vg, pool, func(meta string) error {
		out, err := p.Run.Run(ctx, "thin_dump", "--metadata-snap", "--dev-id", strconv.Itoa(thinID), meta)
		if err != nil {
			return err
		}
		ext, err = parseThinDump(out, sizeBytes)
		return err
	})
	return ext, err
}

type xmlRange struct {
	Begin  uint64 `xml:"begin,attr"`
	Length uint64 `xml:"length,attr"`
}

// thin_delta writes either the short form (<right_only begin length/>) or, with --verbose, a
// list of <range> children; both are accepted.
type xmlDiffEntry struct {
	XMLName xml.Name
	xmlRange
	Ranges []xmlRange `xml:"range"`
}

func parseThinDelta(out []byte, sizeBytes uint64) ([]Extent, error) {
	var doc struct {
		BlockSize uint64 `xml:"data_block_size,attr"`
		Diff      struct {
			Entries []xmlDiffEntry `xml:",any"`
		} `xml:"diff"`
	}
	if err := xml.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("thin_delta output: %w", err)
	}
	if doc.BlockSize == 0 {
		return nil, errors.New("thin_delta output has no data_block_size")
	}
	var rs []xmlRange
	for _, e := range doc.Diff.Entries {
		switch e.XMLName.Local {
		case "same":
			continue
		case "left_only", "right_only", "different":
		default:
			return nil, fmt.Errorf("thin_delta output: unknown entry %q", e.XMLName.Local)
		}
		if len(e.Ranges) > 0 {
			rs = append(rs, e.Ranges...)
		} else {
			rs = append(rs, e.xmlRange)
		}
	}
	return toExtents(rs, doc.BlockSize*512, sizeBytes), nil
}

func parseThinDump(out []byte, sizeBytes uint64) ([]Extent, error) {
	var doc struct {
		BlockSize uint64 `xml:"data_block_size,attr"`
		Device    struct {
			Ranges []struct {
				Origin uint64 `xml:"origin_begin,attr"`
				Length uint64 `xml:"length,attr"`
			} `xml:"range_mapping"`
			Singles []struct {
				Origin uint64 `xml:"origin_block,attr"`
			} `xml:"single_mapping"`
		} `xml:"device"`
	}
	if err := xml.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("thin_dump output: %w", err)
	}
	if doc.BlockSize == 0 {
		return nil, errors.New("thin_dump output has no data_block_size")
	}
	var rs []xmlRange
	for _, r := range doc.Device.Ranges {
		rs = append(rs, xmlRange{Begin: r.Origin, Length: r.Length})
	}
	for _, s := range doc.Device.Singles {
		rs = append(rs, xmlRange{Begin: s.Origin, Length: 1})
	}
	return toExtents(rs, doc.BlockSize*512, sizeBytes), nil
}

// toExtents converts block ranges to byte extents, sorted, merged where they touch, cut at size.
func toExtents(rs []xmlRange, block, size uint64) []Extent {
	sort.Slice(rs, func(i, j int) bool { return rs[i].Begin < rs[j].Begin })
	var out []Extent
	for _, r := range rs {
		off, end := r.Begin*block, (r.Begin+r.Length)*block
		if size > 0 && end > size {
			end = size
		}
		if off >= end {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Offset+out[n-1].Length >= off {
			if e := out[n-1].Offset + out[n-1].Length; end > e {
				out[n-1].Length = end - out[n-1].Offset
			}
			continue
		}
		out = append(out, Extent{Offset: off, Length: end - off})
	}
	return out
}
