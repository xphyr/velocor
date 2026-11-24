package kernel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// probeSection returns one "##### name" section of testdata/probe.txt: real output of mdadm, LVM,
// sysfs and the thin tools, captured on the lab VM (Fedora 45, kernel 7.3, lvm2 2.03.43,
// mdadm 4.6, thin-provisioning-tools 1.3.4).
func probeSection(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/probe.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), "##### "+name+"\n")
	if !ok {
		t.Fatalf("no section %q", name)
	}
	if i := strings.Index(rest, "##### "); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func TestParseLVs(t *testing.T) {
	lvs, err := parseLVs([]byte(probeSection(t, "lvs-json")))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]LV{}
	for _, lv := range lvs {
		byName[lv.Name] = lv
	}
	pool, vol, s1 := byName["tpool"], byName["vol"], byName["s1"]
	if pool.SegType != "thin-pool" || pool.ChunkSize != 65536 || !pool.Active || pool.MetadataPercent != 10.28 {
		t.Errorf("pool %+v", pool)
	}
	if vol.ThinID != 1 || vol.Size != 268435456 || vol.Pool != "tpool" || !vol.Active || vol.SkipActivation {
		t.Errorf("volume %+v", vol)
	}
	if s1.ThinID != 2 || s1.Origin != "vol" || s1.Active || !s1.SkipActivation || s1.DMPath != "/dev/mapper/k1vg-s1" {
		t.Errorf("snapshot %+v", s1)
	}
}

// The captured delta: snapshot 1 had 4 chunks written; then 4 KiB at 400 KiB (chunk 6) and
// 128 KiB at 640 KiB (chunks 10-11). Both forms of thin_delta's output must give the same.
func TestParseThinDelta(t *testing.T) {
	want := []Extent{{6 << 16, 1 << 16}, {10 << 16, 2 << 16}}
	for _, sec := range []string{"thin_delta", "thin_delta-verbose"} {
		got, err := parseThinDelta([]byte(probeSection(t, sec)), 256<<20)
		if err != nil {
			t.Fatalf("%s: %v", sec, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", sec, got, want)
		}
	}
	// Cut at the volume's size, and every kind of change counted.
	x := `<superblock data_block_size="128"><diff left="1" right="2"><left_only begin="0" length="1"/>` +
		`<different begin="1" length="2"/><same begin="3" length="1"/><right_only begin="4" length="10"/></diff></superblock>`
	got, err := parseThinDelta([]byte(x), 5<<16+100)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []Extent{{0, 3 << 16}, {4 << 16, 1<<16 + 100}}) {
		t.Errorf("merged/cut %v", got)
	}
	if _, err := parseThinDelta([]byte(`<superblock data_block_size="128"><diff><odd begin="0" length="1"/></diff></superblock>`), 0); err == nil {
		t.Error("an unknown entry must be an error, not silently skipped")
	}
}

func TestParseThinDump(t *testing.T) {
	got, err := parseThinDump([]byte(probeSection(t, "thin_dump-s1")), 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []Extent{{0, 4 << 16}}) {
		t.Errorf("allocated %v", got)
	}
	x := `<superblock data_block_size="128"><device dev_id="1"><single_mapping origin_block="7" data_block="1" time="0"/>` +
		`<range_mapping origin_begin="0" data_begin="2" length="2" time="0"/><single_mapping origin_block="2" data_block="9" time="0"/></device></superblock>`
	got, err = parseThinDump([]byte(x), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []Extent{{0, 3 << 16}, {7 << 16, 1 << 16}}) {
		t.Errorf("single and range mappings %v", got)
	}
}

// fakeArray writes an md array's sysfs files and /dev/md/<name> link under a temp root.
func fakeArray(t *testing.T, files map[string]string, members map[string][3]string) *Plane {
	t.Helper()
	root := t.TempDir()
	p := &Plane{SysRoot: filepath.Join(root, "sys"), DevRoot: filepath.Join(root, "dev")}
	md := filepath.Join(p.SysRoot, "block", "md127", "md")
	for f, v := range files {
		mustWrite(t, filepath.Join(md, f), v+"\n")
	}
	for dev, v := range members {
		mustWrite(t, filepath.Join(md, "dev-"+dev, "state"), v[0]+"\n")
		mustWrite(t, filepath.Join(md, "dev-"+dev, "slot"), v[1]+"\n")
		mustWrite(t, filepath.Join(md, "dev-"+dev, "bad_blocks"), v[2])
	}
	if err := os.MkdirAll(filepath.Join(p.DevRoot, "md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p.DevRoot, "md127"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../md127", filepath.Join(p.DevRoot, "md", "pool1")); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustWrite(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The states seen while the probe rebuilt a mirror onto a new member.
func TestArrayStatusDuringRecovery(t *testing.T) {
	p := fakeArray(t, map[string]string{
		"uuid": "2249bf85-011f-2b38-e0c6-74cc21efb06e", "level": "raid1", "raid_disks": "2", "degraded": "1",
		"array_state": "clean", "sync_action": "recover", "sync_completed": "261888 / 4188160", "mismatch_cnt": "0",
	}, map[string][3]string{
		"loop0": {"in_sync", "0", "8192 8\n"},
		"loop2": {"spare", "1", ""},
	})
	st, err := p.ArrayStatus("pool1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Kernel != "md127" || st.UUID != "2249bf85:011f2b38:e0c674cc:21efb06e" || st.Degraded != 1 ||
		st.SyncAction != "recover" || st.SyncDone != 261888 || st.SyncTotal != 4188160 {
		t.Fatalf("status %+v", st)
	}
	m := map[string]MemberStatus{}
	for _, x := range st.Members {
		m[x.Device] = x
	}
	if !m["loop0"].Has("in_sync") || len(m["loop0"].BadBlocks) != 1 || m["loop0"].BadSectors() != 8 || m["loop0"].Slot != 0 {
		t.Errorf("good member %+v", m["loop0"])
	}
	if !m["loop2"].Has("spare") || m["loop2"].Slot != 1 {
		t.Errorf("rebuilding member %+v", m["loop2"])
	}
}

func TestParseBadBlocks(t *testing.T) {
	// Device sectors; the data starts at 2048. Duplicates (both logs) once; a range in the
	// metadata area dropped, one straddling it cut.
	got := parseBadBlocks("4196352 8\n100 8\n2040 16\n4196352 8\n\nbad line\n5000 0\n4196356 8\n", 2048)
	want := []SectorRange{{0, 8}, {4194304, 12}} // the logs' overlap once, too
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestArrayStatusFailedMember(t *testing.T) {
	p := fakeArray(t, map[string]string{"uuid": "x", "degraded": "1", "sync_action": "idle", "sync_completed": "none"},
		map[string][3]string{"loop1": {"faulty,blocked", "none", ""}})
	st, err := p.ArrayStatus("pool1")
	if err != nil {
		t.Fatal(err)
	}
	if f := st.Members[0]; !f.Has("faulty") || !f.Has("blocked") || f.Slot != -1 || st.SyncTotal != 0 {
		t.Errorf("failed member %+v, status %+v", f, st)
	}
	if _, err := p.ArrayStatus("nope"); err == nil {
		t.Error("a missing array must be an error")
	}
}

func TestSmallHelpers(t *testing.T) {
	if got := dmName("vg-a", "vol-1"); got != "vg--a-vol--1" {
		t.Errorf("dmName %q", got)
	}
	if !sameDevNum("253:4", "fd:4") || sameDevNum("253:4", "fd:5") || sameDevNum("8:0", "bogus") {
		t.Error("sameDevNum")
	}
	const gib = 1 << 30
	for _, c := range []struct{ data, want uint64 }{{1 * gib, 64 << 20}, {100 * gib, 800 << 20}, {10000 * gib, 16188 << 20}} {
		if got := MetadataSize(c.data); got != c.want {
			t.Errorf("MetadataSize(%d GiB) = %d MiB, want %d", c.data/gib, got>>20, c.want>>20)
		}
	}
}

// A group snapshot suspends every wrapper before taking any snapshot and resumes them all after.
func TestGroupSnapshotOrder(t *testing.T) {
	s := &Script{Steps: []Step{
		{Prefix: "dmsetup suspend --nolockfs velocor-a"},
		{Prefix: "dmsetup suspend --nolockfs velocor-b"},
		{Prefix: "lvm lvcreate --config"},
		{Prefix: "lvm lvcreate --config"},
		{Prefix: "dmsetup resume velocor-a"},
		{Prefix: "dmsetup resume velocor-b"},
	}}
	p := &Plane{Run: s}
	err := p.GroupSnapshot(context.Background(), []GroupMember{
		{Volume: "a", VG: "vg", LV: "a", Snap: "sa"}, {Volume: "b", VG: "vg", LV: "b", Snap: "sb"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Steps) != 0 {
		t.Fatalf("steps left: %v (calls %v)", s.Steps, s.Calls)
	}
	if !strings.HasSuffix(s.Calls[2], "--snapshot --permission r --name sa vg/a") || !strings.HasSuffix(s.Calls[3], "--snapshot --permission r --name sb vg/b") {
		t.Errorf("snapshot calls %v", s.Calls[2:4])
	}
}

// A failed snapshot leaves nothing behind: the snapshots taken are removed and every wrapper is
// resumed, even one the failure came after.
func TestGroupSnapshotFailureCleansUp(t *testing.T) {
	boom := errors.New("boom")
	s := &Script{Steps: []Step{
		{Prefix: "dmsetup suspend --nolockfs velocor-a"},
		{Prefix: "dmsetup suspend --nolockfs velocor-b"},
		{Prefix: "lvm lvcreate"},
		{Prefix: "lvm lvcreate", Err: boom},
		{Prefix: "dmsetup resume velocor-a"},
		{Prefix: "dmsetup resume velocor-b"},
		{Prefix: "lvm lvremove"},
	}}
	p := &Plane{Run: s}
	err := p.GroupSnapshot(context.Background(), []GroupMember{
		{Volume: "a", VG: "vg", LV: "a", Snap: "sa"}, {Volume: "b", VG: "vg", LV: "b", Snap: "sb"}})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "snapshot vg/b") {
		t.Fatalf("error %v", err)
	}
	if len(s.Steps) != 0 {
		t.Fatalf("cleanup incomplete, steps left %v (calls %v)", s.Steps, s.Calls)
	}
	if !strings.HasSuffix(s.Calls[6], "vg/sa") {
		t.Errorf("removed %q, want the snapshot taken (vg/sa)", s.Calls[6])
	}

	// A failed suspend: that volume is resumed too (dmsetup killed at the deadline may have
	// suspended it), nothing is snapshotted.
	s = &Script{Steps: []Step{
		{Prefix: "dmsetup suspend --nolockfs velocor-a"},
		{Prefix: "dmsetup suspend --nolockfs velocor-b", Err: boom},
		{Prefix: "dmsetup resume velocor-a"},
		{Prefix: "dmsetup resume velocor-b"},
	}}
	p.Run = s
	if err := p.GroupSnapshot(context.Background(), []GroupMember{{Volume: "a"}, {Volume: "b"}}); !errors.Is(err, boom) || len(s.Steps) != 0 {
		t.Fatalf("suspend failure: %v, steps left %v", err, s.Steps)
	}
}

// Resuming happens even after the context ran out.
func TestGroupSnapshotResumesAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := runnerFunc(func(c context.Context, name string, args ...string) ([]byte, error) {
		line := name + " " + strings.Join(args, " ")
		if strings.HasPrefix(line, "lvm lvcreate") {
			cancel()
			return nil, context.Canceled
		}
		if strings.HasPrefix(line, "dmsetup resume") && c.Err() != nil {
			return nil, errors.New("resume ran with a cancelled context")
		}
		return nil, nil
	})
	p := &Plane{Run: r}
	err := p.GroupSnapshot(ctx, []GroupMember{{Volume: "a", VG: "vg", LV: "a", Snap: "sa"}})
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "cancelled context") {
		t.Fatalf("error %v", err)
	}
}

type runnerFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

// Every LVM command carries velod's own device filter, so the host's devices file can't decide.
func TestLVMConfigOnEveryCall(t *testing.T) {
	var calls []string
	p := &Plane{Run: runnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "lvm" && len(args) > 0 && args[0] == "vgs" {
			return []byte("  10737418240 4194304\n"), nil // a 10 GiB VG, 4 MiB extents
		}
		return []byte(`{"report":[{"lv":[]}]}`), nil
	}), DevRoot: "/dev"}
	ctx := context.Background()
	_ = p.CreatePool(ctx, PoolSpec{VG: "vg", Device: "/dev/md/p", Thin: "thin", DataPercent: 90})
	_ = p.CreateVolume(ctx, "vg", "thin", "v", 1<<30, true)
	_, _ = p.ListVolumes(ctx, "vg")
	for _, c := range calls {
		if strings.HasPrefix(c, "lvm ") && !strings.Contains(c, "use_devicesfile = 0") {
			t.Errorf("no velod config: %s", c)
		}
	}
	pool := calls[len(calls)-4]
	if c := calls[len(calls)-3]; !strings.Contains(c, "lvchange") || !strings.Contains(c, "--errorwhenfull y vg/thin") {
		t.Errorf("full pool not set to fail writes at once: %s", c)
	}
	for _, want := range []string{"--type thin-pool", "--chunksize 65536b", "--zero y", "--size 9512681472b", "--poolmetadatasize 75497472b"} { // 90%: 9 GiB, less two 72 MiB metadata copies
		if !strings.Contains(pool, want) {
			t.Errorf("thin pool command lacks %q: %s", want, pool)
		}
	}
}

func TestParseExamineScan(t *testing.T) {
	// mdadm 4.6, captured on the lab VM.
	out := "ARRAY /dev/md/raid_pool-k2probe  level=raid1 metadata=1.2 num-devices=2 UUID=18b475c2:edd58647:45bb9801:365ca52d\n" +
		"   devices=/dev/loop1,/dev/loop0\n" +
		"ARRAY /dev/md/other  level=raid1 metadata=1.2 num-devices=2 UUID=1314364f:36a60865:1e5f4e62:1e0af79f name=any:raid_pool-x\n" +
		"   devices=/dev/loop2\n"
	a := parseExamineScan(out + "ARRAY /dev/md/spared  level=raid1 metadata=1.2 num-devices=2 UUID=2314364f:36a60865:1e5f4e62:1e0af79f\n" +
		"   spares=1   devices=/dev/sda,/dev/sdb,/dev/sdc\n")
	if len(a) != 3 {
		t.Fatalf("arrays %+v", a)
	}
	if !slices.Equal(a[2].Found, []string{"/dev/sda", "/dev/sdb", "/dev/sdc"}) {
		t.Errorf("an array with a spare: %+v", a[2])
	}
	if a[0].Name != "raid_pool-k2probe" || a[0].UUID != "18b475c2:edd58647:45bb9801:365ca52d" || a[0].Level != "raid1" ||
		a[0].Devices != 2 || !slices.Equal(a[0].Found, []string{"/dev/loop1", "/dev/loop0"}) {
		t.Errorf("first %+v", a[0])
	}
	if a[1].Name != "raid_pool-x" || len(a[1].Found) != 1 {
		t.Errorf("second %+v", a[1])
	}
}

func TestParseVGsAndTags(t *testing.T) {
	vgs, err := parseVGs([]byte(`{"report":[{"vg":[{"vg_name":"lvs_pool-k2probe","vg_uuid":"PpWRHu-d9k3-IyXe-nLaa-Wb3D-vdR9-9qS1Ne","vg_size":"310378496","vg_free":"197132288","vg_extent_size":"4194304","pv_name":"/dev/md127"}]}],"log":[]}`))
	if err != nil || len(vgs) != 1 || vgs[0] != (VG{Name: "lvs_pool-k2probe", UUID: "PpWRHu-d9k3-IyXe-nLaa-Wb3D-vdR9-9qS1Ne", Size: 310378496, Free: 197132288, ExtentSize: 4194304, PV: "/dev/md127"}) {
		t.Fatalf("vgs %+v %v", vgs, err)
	}
	lvs, err := parseLVs([]byte(`{"report":[{"lv":[{"lv_name":"snap-1","vg_name":"v","lv_uuid":"u","lv_attr":"Vwi---tz-k","lv_size":"67108864","pool_lv":"thin","origin":"vol-1","thin_id":"2","segtype":"thin","lv_skip_activation":"skip activation","lv_tags":"velocor.uuid.1111,velocor.snapshot"}]}]}`))
	if err != nil || len(lvs) != 1 || !slices.Equal(lvs[0].Tags, []string{"velocor.uuid.1111", "velocor.snapshot"}) || lvs[0].ThinID != 2 || !lvs[0].SkipActivation {
		t.Fatalf("lvs %+v %v", lvs, err)
	}
}

func TestParseExamine(t *testing.T) {
	a, err := parseExamine(probeSection(t, "mdadm-examine-active"))
	if err != nil {
		t.Fatal(err)
	}
	want := MemberInfo{ArrayUUID: "4009d12d:f41d8d3a:ac2d1e26:a4757ed6", Name: "raid_pool-mdex", Level: "raid1", RaidDevices: 2,
		ArrayKiB: 203776, DeviceUUID: "abc0d854:f6cd4475:57c73c73:8b94c9ad", Events: 0, Role: 0, ArrayState: "AA"}
	if *a != want {
		t.Errorf("active member %+v, want %+v", *a, want)
	}
	r, err := parseExamine(probeSection(t, "mdadm-examine-removed"))
	if err != nil || r.Role != 1 || r.ArrayUUID != want.ArrayUUID {
		t.Errorf("removed member %+v %v", r, err)
	}
	if _, err := parseExamine("garbage"); err == nil {
		t.Error("garbage parsed")
	}
	// A foreign disk's odd metadata: empty values, not a crash.
	if m, err := parseExamine("Array UUID : 1:2:3:4\nName : host:\nArray Size :  \nArray State :  \nDevice Role : \n"); err != nil || m.Name != "" || m.ArrayKiB != 0 {
		t.Errorf("empty values: %+v %v", m, err)
	}
	s := &Script{Steps: []Step{{Prefix: "mdadm --examine /dev/vda1", Err: &CommandError{Code: 1, Stderr: "mdadm: No md superblock detected on /dev/vda1.\n"}}}}
	if m, err := New(s).ExamineMember(context.Background(), "/dev/vda1"); m != nil || err != nil {
		t.Errorf("no metadata: %v %v", m, err)
	}
}

func TestSuspendedWrappers(t *testing.T) {
	s := &Script{Steps: []Step{{Prefix: "dmsetup info", Out: "velocor-vol-a Suspended\nvelocor-vol-b Active\nvg0-thin Suspended\n"}}}
	got, err := (&Plane{Run: s}).SuspendedWrappers(context.Background())
	if err != nil || len(got) != 1 || got[0] != "vol-a" {
		t.Fatalf("suspended wrappers %v %v", got, err)
	}
}

// Names become configfs directories: one that isn't a single path component is refused before
// anything is created, and removing one does nothing.
func TestTargetNamesStayInConfigfs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "kernel", "config", "nvmet", "subsystems"), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "kernel", "config", "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Plane{SysRoot: root}
	const up = "nqn.2024-01.x:../../../victim"
	for name, err := range map[string]error{
		"subsystem":  p.EnsureNVMeExport(NVMeExport{NQN: up}),
		"host":       p.EnsureNVMeExport(NVMeExport{NQN: "nqn.2024-01.x:a", Hosts: []string{"../../victim/h"}}),
		"target":     p.EnsureISCSIExport(ISCSIExport{IQN: "iqn.2024-01.x:../../../victim", Backstore: "v"}),
		"backstore":  p.EnsureISCSIExport(ISCSIExport{IQN: "iqn.2024-01.x:a", Backstore: "../v"}),
		"initiator":  p.EnsureISCSIExport(ISCSIExport{IQN: "iqn.2024-01.x:a", Backstore: "v", Initiators: []string{".."}}),
		"portal":     p.EnsureISCSIExport(ISCSIExport{IQN: "iqn.2024-01.x:a", Backstore: "v", Portals: []string{"../x:3260"}}),
		"empty name": p.EnsureNVMeExport(NVMeExport{}),
	} {
		if err == nil || !strings.Contains(err.Error(), "invalid name") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := errors.Join(p.RemoveNVMeExport("nqn.2024-01.x:a/../../../victim"), p.RemoveNVMeExport("../../victim"), p.RemoveISCSIExport("../../victim", "../victim")); err != nil {
		t.Error(err)
	}
	if es, err := os.ReadDir(victim); err != nil || len(es) != 0 {
		t.Errorf("outside directory: %v %v", es, err)
	}
	if es := dirNames(filepath.Join(root, "kernel", "config", "nvmet", "subsystems")); len(es) != 0 {
		t.Errorf("created %v", es)
	}
}

// A metadata snapshot a crashed velod left reserved is released and reserved again; one that
// can't be had is reported; the snapshot is released even if reading it panics.
func TestMetadataSnapRecovery(t *testing.T) {
	busy := &CommandError{Code: 1, Stderr: "device-mapper: message ioctl failed: Invalid argument"}
	run := func(steps ...Step) (*Script, error) {
		s := &Script{Steps: steps}
		return s, (&Plane{Run: s}).withMetadataSnap(context.Background(), "vg", "thin", func(string) error { return nil })
	}
	const msg = "dmsetup message vg-thin-tpool 0 "
	if s, err := run(Step{Prefix: msg + "reserve", Err: busy}, Step{Prefix: msg + "release"}, Step{Prefix: msg + "reserve"}, Step{Prefix: msg + "release"}); err != nil || len(s.Steps) != 0 {
		t.Errorf("left over: %v, steps left %v", err, s.Steps)
	}
	if s, err := run(Step{Prefix: msg + "reserve", Err: busy}, Step{Prefix: msg + "release", Err: errors.New("none held")}); !errors.Is(err, busy) || len(s.Steps) != 0 {
		t.Errorf("not left over: %v, steps left %v", err, s.Steps)
	}
	s := &Script{Steps: []Step{{Prefix: msg + "reserve"}, {Prefix: msg + "release"}}}
	func() {
		defer func() { _ = recover() }()
		_ = (&Plane{Run: s}).withMetadataSnap(context.Background(), "vg", "thin", func(string) error { panic("boom") })
	}()
	if len(s.Steps) != 0 {
		t.Errorf("after a panic, steps left %v", s.Steps)
	}
}

// A pool whose creation fails half way leaves nothing behind.
func TestCreatePoolCleansUp(t *testing.T) {
	boom := errors.New("boom")
	s := &Script{Steps: []Step{{Prefix: "lvm pvcreate"}, {Prefix: "lvm vgcreate"}, {Prefix: "lvm vgs", Out: "10737418240 4194304\n"},
		{Prefix: "lvm lvcreate"}, {Prefix: "lvm lvchange", Err: boom}, {Prefix: "lvm vgremove"}, {Prefix: "lvm pvremove"}}}
	if err := (&Plane{Run: s}).CreatePool(context.Background(), PoolSpec{VG: "vg", Device: "/dev/md/a", Thin: "thin", DataPercent: 90}); !errors.Is(err, boom) || len(s.Steps) != 0 {
		t.Fatalf("%v, steps left %v (calls %v)", err, s.Steps, s.Calls)
	}
}
