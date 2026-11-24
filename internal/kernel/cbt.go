package kernel

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
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
func (p *Plane) withMetadataSnap(ctx context.Context, vg, pool string, fn func(meta string) error) (err error) {
	l := poolLock(vg, pool)
	l.Lock()
	defer l.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	target := dmName(vg, pool) + "-tpool"
	// Neither is cut short by ctx: the kernel may have acted on a message whose dmsetup was killed.
	message := func(msg string) error {
		_, err := p.Run.Run(context.WithoutCancel(ctx), "dmsetup", "message", target, "0", msg)
		return err
	}
	if err := message("reserve_metadata_snap"); err != nil {
		// A velod that died in the middle left its snapshot reserved, and a pool has room for one.
		// Nothing else takes one, and the lock says velod isn't using it: release it, try again.
		if message("release_metadata_snap") != nil {
			return err
		}
		if err := message("reserve_metadata_snap"); err != nil {
			return err
		}
	}
	defer func() { // also if fn panics
		if rerr := message("release_metadata_snap"); rerr != nil {
			err = errors.Join(err, fmt.Errorf("release metadata snapshot: %w", rerr))
		}
	}()
	return fn(p.DevRoot + "/mapper/" + dmName(vg, pool+"_tmeta"))
}

// ChangedBlocks returns the ranges where two thin volumes of a pool (snapshots of one volume,
// older and newer) differ, in whole chunks, cut at sizeBytes: written on one side only,
// written on both to different blocks, or discarded on one side.
func (p *Plane) ChangedBlocks(ctx context.Context, vg, pool string, oldThinID, newThinID int, sizeBytes uint64) ([]Extent, error) {
	var ext []Extent
	err := p.withMetadataSnap(ctx, vg, pool, func(meta string) error {
		return p.stream(ctx, func(r io.Reader) (err error) {
			ext, err = parseThinDelta(r, sizeBytes)
			return err
		}, "thin_delta", "--metadata-snap", "--snap1", strconv.Itoa(oldThinID), "--snap2", strconv.Itoa(newThinID), meta)
	})
	return ext, err
}

// AllocatedBlocks returns the ranges a thin volume maps (its own and those it shares), in whole
// chunks, cut at sizeBytes: everything a full backup must read.
func (p *Plane) AllocatedBlocks(ctx context.Context, vg, pool string, thinID int, sizeBytes uint64) ([]Extent, error) {
	var ext []Extent
	err := p.withMetadataSnap(ctx, vg, pool, func(meta string) error {
		return p.stream(ctx, func(r io.Reader) (err error) {
			ext, err = parseThinDump(r, sizeBytes)
			return err
		}, "thin_dump", "--metadata-snap", "--dev-id", strconv.Itoa(thinID), meta)
	})
	return ext, err
}

type blockRange struct {
	Begin, Length uint64
}

// addRange appends a range, extending the last one if it follows on: the tools list ranges in
// order, so a volume's millions of mappings come down to its few contiguous ranges.
func addRange(rs []blockRange, r blockRange) []blockRange {
	if n := len(rs); n > 0 && rs[n-1].Begin+rs[n-1].Length == r.Begin {
		rs[n-1].Length += r.Length
		return rs
	}
	return append(rs, r)
}

// uintAttr reads an element's numeric attribute; ok is false if it has none.
func uintAttr(e xml.StartElement, name string) (v uint64, ok bool, err error) {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			v, err := strconv.ParseUint(a.Value, 10, 64)
			if err != nil {
				return 0, true, fmt.Errorf("%s %s=%q", e.Name.Local, name, a.Value)
			}
			return v, true, nil
		}
	}
	return 0, false, nil
}

// uintAttrs reads numeric attributes an element must have.
func uintAttrs(e xml.StartElement, names ...string) ([]uint64, error) {
	out := make([]uint64, len(names))
	for i, n := range names {
		v, ok, err := uintAttr(e, n)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%s without %s", e.Name.Local, n)
		}
		out[i] = v
	}
	return out, nil
}

// elements calls fn for every element's start and end (attributes only at the start), reading the
// document as it comes; it must be complete.
func elements(r io.Reader, start func(xml.StartElement) error, end func(name string) error) error {
	d := xml.NewDecoder(r)
	seen := false
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			if !seen {
				return errors.New("no output")
			}
			return nil // Token checks that elements are closed
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			seen = true
			if err := start(t); err != nil {
				return err
			}
		case xml.EndElement:
			if err := end(t.Name.Local); err != nil {
				return err
			}
		}
	}
}

// parseThinDelta reads thin_delta's XML: the short form (<right_only begin length/>) or, with
// --verbose, the same with a list of <range> children; both are accepted.
func parseThinDelta(r io.Reader, sizeBytes uint64) ([]Extent, error) {
	var bs uint64
	var rs []blockRange
	depth, diffAt, entry := 0, 0, ""
	err := elements(r, func(e xml.StartElement) (err error) {
		depth++
		name := e.Name.Local
		if bs == 0 {
			if v, ok, err := uintAttr(e, "data_block_size"); err != nil {
				return err
			} else if ok {
				bs = v
			}
		}
		switch {
		case name == "diff":
			diffAt = depth
			return nil
		case diffAt > 0 && depth == diffAt+1:
			entry = name
			switch name {
			case "same", "left_only", "right_only", "different":
			default:
				return fmt.Errorf("unknown entry %q", name)
			}
		case diffAt > 0 && depth == diffAt+2 && name == "range":
		default:
			return nil
		}
		if entry == "same" {
			return nil
		}
		if _, ok, _ := uintAttr(e, "begin"); !ok && name != "range" {
			return nil // the verbose form: its children say
		}
		v, err := uintAttrs(e, "begin", "length")
		if err == nil {
			rs = addRange(rs, blockRange{v[0], v[1]})
		}
		return err
	}, func(string) error {
		if depth == diffAt {
			diffAt = 0
		}
		depth--
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("thin_delta output: %w", err)
	}
	if bs == 0 {
		return nil, errors.New("thin_delta output has no data_block_size")
	}
	return toExtents(rs, bs*512, sizeBytes), nil
}

// thinMap is a mapping of thin_dump's: blocks of a thin device to blocks of the pool's data.
type thinMap struct {
	origin, data, length uint64
}

// thinNode is a <device> or <def> of thin_dump's: mappings, and references to defs. thin_dump
// writes subtrees devices share (a volume and its snapshots) once, as a named <def> the devices
// (and other defs) refer to with <ref name>.
type thinNode struct {
	maps []thinMap
	refs []string
}

// parseMappings reads thin_dump's XML as it comes. add puts a mapping into its node (or drops it).
func parseMappings(r io.Reader, add func(bs uint64, n *thinNode, m thinMap)) (blockSize uint64, devices map[int]*thinNode, defs map[string]*thinNode, err error) {
	devices, defs = map[int]*thinNode{}, map[string]*thinNode{}
	var cur *thinNode
	err = elements(r, func(e xml.StartElement) error {
		switch e.Name.Local {
		case "superblock":
			v, err := uintAttrs(e, "data_block_size")
			if err != nil {
				return err
			}
			blockSize = v[0]
		case "device":
			v, err := uintAttrs(e, "dev_id")
			if err != nil {
				return err
			}
			cur = &thinNode{}
			devices[int(v[0])] = cur
		case "def":
			cur = &thinNode{}
			for _, a := range e.Attr {
				if a.Name.Local == "name" {
					defs[a.Value] = cur
				}
			}
		case "range_mapping":
			v, err := uintAttrs(e, "origin_begin", "data_begin", "length")
			if err != nil {
				return err
			}
			if cur == nil {
				return errors.New("a mapping outside a device")
			}
			add(blockSize, cur, thinMap{v[0], v[1], v[2]})
		case "single_mapping":
			v, err := uintAttrs(e, "origin_block", "data_block")
			if err != nil {
				return err
			}
			if cur == nil {
				return errors.New("a mapping outside a device")
			}
			add(blockSize, cur, thinMap{v[0], v[1], 1})
		case "ref":
			if cur == nil {
				return errors.New("a reference outside a device")
			}
			for _, a := range e.Attr {
				if a.Name.Local == "name" {
					cur.refs = append(cur.refs, a.Value)
				}
			}
		}
		return nil
	}, func(name string) error {
		if name == "device" || name == "def" {
			cur = nil
		}
		return nil
	})
	if err != nil {
		return 0, nil, nil, fmt.Errorf("thin_dump output: %w", err)
	}
	if blockSize == 0 {
		return 0, nil, nil, errors.New("thin_dump output has no data_block_size")
	}
	return blockSize, devices, defs, nil
}

// eachMapping calls fn for a node's mappings, its references' included.
func eachMapping(n *thinNode, defs map[string]*thinNode, depth int, fn func(thinMap)) error {
	if depth > 64 {
		return errors.New("thin_dump output: references nest too deep")
	}
	for _, m := range n.maps {
		fn(m)
	}
	for _, name := range n.refs {
		d := defs[name]
		if d == nil {
			return fmt.Errorf("thin_dump output: reference to an unknown def %q", name)
		}
		if err := eachMapping(d, defs, depth+1, fn); err != nil {
			return err
		}
	}
	return nil
}

// parseThinDump reads the ranges one thin device maps, from thin_dump --dev-id's XML.
func parseThinDump(r io.Reader, sizeBytes uint64) ([]Extent, error) {
	bs, devices, defs, err := parseMappings(r, func(_ uint64, n *thinNode, m thinMap) {
		// Only where on the device matters here: contiguous mappings are one range.
		if l := len(n.maps); l > 0 && n.maps[l-1].origin+n.maps[l-1].length == m.origin {
			n.maps[l-1].length += m.length
			return
		}
		n.maps = append(n.maps, m)
	})
	if err != nil {
		return nil, err
	}
	var rs []blockRange
	for _, d := range devices {
		if err := eachMapping(d, defs, 0, func(m thinMap) { rs = addRange(rs, blockRange{m.origin, m.length}) }); err != nil {
			return nil, err
		}
	}
	return toExtents(rs, bs*512, sizeBytes), nil
}

// toExtents converts block ranges to byte extents, sorted, merged where they touch, cut at size.
func toExtents(rs []blockRange, block, size uint64) []Extent {
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
