package kernel

import (
	"bytes"
	"slices"
	"testing"
)

// Captured on the test appliance (lvm 2.03, one md PV, 4 MiB extents, 1 MiB pe_start).
const segmentsJSON = `{"report": [{"lv": [
  {"lv_name":"[lvol0_pmspare]", "segtype":"linear", "seg_start":"0", "seg_pe_ranges":"/dev/md126:0-447"},
  {"lv_name":"thin", "segtype":"thin-pool", "seg_start":"0", "seg_pe_ranges":"thin_tdata:0-56673"},
  {"lv_name":"[thin_tdata]", "segtype":"linear", "seg_start":"0", "seg_pe_ranges":"/dev/md126:448-57121"},
  {"lv_name":"[thin_tmeta]", "segtype":"linear", "seg_start":"0", "seg_pe_ranges":"/dev/md126:57122-57569"},
  {"lv_name":"velocor-meta", "segtype":"linear", "seg_start":"0", "seg_pe_ranges":"/dev/md126:57570-57571"},
  {"lv_name":"vol-9709c22ae1ca", "segtype":"thin", "seg_start":"0", "seg_pe_ranges":""}
]}], "log": []}`

const pvJSON = `{"report": [{"pv": [{"pv_name":"/dev/md126", "pe_start":"1048576", "vg_extent_size":"4194304"}]}], "log": []}`

// Device 1 maps volume blocks 10-11 to data blocks 3-4; device 2 (its snapshot) shares data
// block 3 for its block 10 and has its own block 11 at data block 9. 64 KiB blocks.
const dumpXML = `<superblock uuid="" time="1" transaction="2" flags="0" version="2" data_block_size="128" nr_data_blocks="1000">
  <device dev_id="1" mapped_blocks="2" transaction="0" creation_time="0" snap_time="1">
    <range_mapping origin_begin="10" data_begin="3" length="2" time="0"/>
  </device>
  <device dev_id="2" mapped_blocks="2" transaction="1" creation_time="1" snap_time="1">
    <single_mapping origin_block="10" data_block="3" time="0"/>
    <single_mapping origin_block="11" data_block="9" time="1"/>
  </device>
</superblock>`

func TestRangeOwners(t *testing.T) {
	const mib, kib = 1 << 20, 1 << 10
	peStart, extent, err := parsePVStart([]byte(pvJSON))
	if err != nil || peStart != mib || extent != 4*mib {
		t.Fatalf("pvs: %d %d %v", peStart, extent, err)
	}
	segs, err := parseSegments([]byte(segmentsJSON), extent)
	if err != nil || len(segs) != 4 {
		t.Fatalf("segments %+v %v", segs, err)
	}
	tdataStart := peStart + 448*4*mib
	ranges := []Extent{
		{Offset: 4 * kib, Length: 4 * kib},                               // the PV label area
		{Offset: tdataStart + 3*64*kib + 512, Length: 4 * kib},           // data block 3: both devices
		{Offset: tdataStart + 5*64*kib, Length: 4 * kib},                 // data block 5: nobody's
		{Offset: peStart + 57122*4*mib + 8*kib, Length: 4 * kib},         // the thin metadata
		{Offset: peStart + 57571*4*mib + 4*mib - 2*kib, Length: 4 * kib}, // the end of velocor-meta, and past it
		{Offset: peStart + 100*4*mib, Length: 512},                       // the spare
	}
	owners, tdata := lvmOwners(ranges, peStart, segs, "thin")
	if !slices.Equal(tdata, []Extent{{Offset: 3*64*kib + 512, Length: 4 * kib}, {Offset: 5 * 64 * kib, Length: 4 * kib}}) {
		t.Fatalf("thin data ranges %+v", tdata)
	}
	thin, err := thinOwners(bytes.NewReader([]byte(dumpXML)), tdata)
	if err != nil {
		t.Fatal(err)
	}
	got := append(owners, thin...)
	want := []RangeOwner{
		{Kind: OwnerLVMMetadata, Offset: 4 * kib, Length: 4 * kib},
		{Kind: OwnerThinMetadata, LV: "thin_tmeta", Offset: 8 * kib, Length: 4 * kib},
		{Kind: OwnerLV, LV: "velocor-meta", Offset: 8*mib - 2*kib, Length: 2 * kib},
		{Kind: OwnerFree, Offset: peStart + 57572*4*mib, Length: 2 * kib},
		{Kind: OwnerSpare, LV: "lvol0_pmspare", Offset: 100 * 4 * mib, Length: 512},
		{Kind: OwnerThin, ThinID: 1, Offset: 10*64*kib + 512, Length: 4 * kib},
		{Kind: OwnerThin, ThinID: 2, Offset: 10*64*kib + 512, Length: 4 * kib},
		{Kind: OwnerFree, Offset: 5 * 64 * kib, Length: 4 * kib},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("owners:\n got  %+v\n want %+v", got, want)
	}
}

// thin_dump writes subtrees devices share once, as <def>s they refer to (captured on the lab VM:
// a 2.5 GiB volume, written in full, and its snapshot).
const dumpSharedXML = `<superblock uuid="" time="1" transaction="2" version="2" data_block_size="128" nr_data_blocks="0">
  <def name="26">
    <range_mapping origin_begin="0" data_begin="0" length="40960" time="0"/>
  </def>
  <def name="27">
    <ref name="26"/>
    <single_mapping origin_block="50000" data_block="50000" time="1"/>
  </def>
  <device dev_id="1" mapped_blocks="40960" transaction="0" creation_time="0" snap_time="1">
    <ref name="26"/>
  </device>
  <device dev_id="2" mapped_blocks="40961" transaction="1" creation_time="1" snap_time="1">
    <ref name="27"/>
  </device>
</superblock>`

func TestThinOwnersSharedDefs(t *testing.T) {
	const kib = 1 << 10
	got, err := thinOwners(bytes.NewReader([]byte(dumpSharedXML)), []Extent{{Offset: 31587*64*kib + 4*kib, Length: 4 * kib}, {Offset: 50000 * 64 * kib, Length: 512}})
	if err != nil {
		t.Fatal(err)
	}
	want := []RangeOwner{
		{Kind: OwnerThin, ThinID: 1, Offset: 31587*64*kib + 4*kib, Length: 4 * kib},
		{Kind: OwnerThin, ThinID: 2, Offset: 31587*64*kib + 4*kib, Length: 4 * kib},
		{Kind: OwnerThin, ThinID: 2, Offset: 50000 * 64 * kib, Length: 512},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("owners:\n got  %+v\n want %+v", got, want)
	}
	if _, err := thinOwners(bytes.NewReader([]byte(`<superblock data_block_size="128"><device dev_id="1"><ref name="9"/></device></superblock>`)), []Extent{{Length: 1}}); err == nil {
		t.Error("a reference to a missing def accepted")
	}
}
