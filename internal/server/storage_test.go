package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/dataplane/planetest"
	"github.com/velocor/velocor/internal/reconciler"
	"github.com/velocor/velocor/internal/store"
)

type harness struct {
	t     *testing.T
	st    *store.BoltStore
	fake  *planetest.Fake
	ts    *httptest.Server
	srv   *Server
	rec   *reconciler.Reconciler
	admin string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "srv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := planetest.New(t)
	authMgr, err := auth.NewManager(st, "admin-pass-123")
	if err != nil {
		t.Fatal(err)
	}
	clearMustChange(t, st)
	rec := reconciler.New(st, f, nil, nil)
	srv := New(st, f, nil, authMgr, rec, nil)
	srv.os.reboot = func() { t.Log("reboot requested (fake)") } // never the real one
	h := &harness{t: t, st: st, fake: f, srv: srv, rec: rec, ts: httptest.NewServer(srv.Handler())}
	t.Cleanup(h.ts.Close)
	code, resp := h.do("POST", "/api/v1/auth/login", "", map[string]any{"username": "admin", "password": "admin-pass-123"})
	if code != http.StatusOK {
		t.Fatalf("login: %d %v", code, resp)
	}
	h.admin = resp["access_token"].(string)
	return h
}

func (h *harness) do(method, path, token string, body any) (int, map[string]any) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.ts.URL+path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// expect asserts the HTTP status of a h.do call: h.expect(h.do(...))(http.StatusOK).
func (h *harness) expect(code int, resp map[string]any) func(int) map[string]any {
	return func(want int) map[string]any {
		h.t.Helper()
		if code != want {
			h.t.Fatalf("got HTTP %d, want %d: %v", code, want, resp)
		}
		return resp
	}
}

func field(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		cur = cur.(map[string]any)[p]
	}
	return cur
}

// addDisk seeds a data NVMe disk known to both the store and the fake.
func (h *harness) addDisk(id, pci string) {
	h.t.Helper()
	h.fake.AddDisk(pci)
	if err := h.st.UpsertDisk(&store.Disk{ID: id, Name: id, Type: store.DiskTypeNVMe, PCIAddress: pci, SizeBytes: 1 << 40}); err != nil {
		h.t.Fatal(err)
	}
}

// pool claims the given disks and creates a pool from them.
func (h *harness) pool(name, level string, diskIDs ...string) string {
	h.t.Helper()
	for _, id := range diskIDs {
		h.expect(h.do("POST", "/api/v1/disks/"+id+":claim", h.admin, nil))(http.StatusOK)
	}
	resp := h.expect(h.do("POST", "/api/v1/pools", h.admin, map[string]any{"name": name, "raid_level": level, "disk_ids": diskIDs}))(http.StatusCreated)
	return field(resp, "pool", "id").(string)
}

func (h *harness) volume(poolID, name string, size uint64) map[string]any {
	h.t.Helper()
	resp := h.expect(h.do("POST", "/api/v1/volumes", h.admin, map[string]any{"name": name, "pool_id": poolID, "size_bytes": size}))(http.StatusCreated)
	return resp["volume"].(map[string]any)
}

func TestStorageEndToEndAndCBT(t *testing.T) {
	h := newHarness(t)
	if err := h.st.UpsertDisk(&store.Disk{ID: "nvme-boot", Name: "nvme0n1", Type: store.DiskTypeNVMe, PCIAddress: "0000:00:04.0", IsBootDevice: true}); err != nil {
		t.Fatal(err)
	}
	h.addDisk("nvme-data1", "0000:01:00.0")

	if code, _ := h.do("POST", "/api/v1/disks/nvme-boot:claim", h.admin, nil); code != http.StatusConflict {
		t.Fatalf("claiming the boot disk must fail with 409, got %d", code)
	}
	// The extents below are checked at the fake's chunk size (1 MiB).
	h.expect(h.do("POST", "/api/v1/disks/nvme-data1:claim", h.admin, nil))(http.StatusOK)
	poolID := field(h.expect(h.do("POST", "/api/v1/pools", h.admin, map[string]any{
		"name": "pool0", "raid_level": "RAID0", "disk_ids": []string{"nvme-data1"}, "chunk_size_bytes": 65536,
	}))(http.StatusCreated), "pool", "id").(string)
	pool, _ := h.st.GetPool(poolID)
	if pool.ThinPoolUUID == "" || pool.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("pool not fully initialised: %+v", pool)
	}

	tok := h.expect(h.do("POST", "/api/v1/auth/tokens", h.admin, map[string]any{"name": "csi", "role": "CSI_PROVISIONER"}))(http.StatusCreated)
	csi := tok["secret_token"].(string)
	if code, _ := h.do("DELETE", "/api/v1/pools/"+poolID, csi, nil); code != http.StatusForbidden {
		t.Fatalf("CSI token must not delete pools, got %d", code)
	}

	volReq := map[string]any{"name": "pvc-01", "pool_id": poolID, "size_bytes": 1 << 30, "thin_provision": true}
	vol := h.expect(h.do("POST", "/api/v1/volumes", csi, volReq))(http.StatusCreated)["volume"].(map[string]any)
	again := h.expect(h.do("POST", "/api/v1/volumes", csi, volReq))(http.StatusOK)["volume"].(map[string]any)
	if again["id"] != vol["id"] {
		t.Fatalf("CreateVolume retry must be idempotent")
	}
	volID, volUUID := vol["id"].(string), vol["uuid"].(string)

	var snaps []string
	for i, name := range []string{"s1", "s2", "s3"} {
		h.fake.WriteChunk(volUUID, uint64(i))
		resp := h.expect(h.do("POST", "/api/v1/volumes/"+volID+"/snapshots", csi, map[string]any{"name": name}))(http.StatusCreated)
		snaps = append(snaps, field(resp, "snapshot", "id").(string))
	}

	for _, path := range []string{
		"/api/v1/snapshots/" + snaps[2] + "/blocks:changed?base_snapshot_id=" + snaps[0],
	} {
		resp := h.expect(h.do("GET", path, csi, nil))(http.StatusOK)
		ext := resp["extents"].([]any)
		if len(ext) != 1 {
			t.Fatalf("%s: want 1 coalesced extent, got %v", path, ext)
		}
		e := ext[0].(map[string]any)
		if e["offset_bytes"].(float64) != planetest.ChunkBytes || e["length_bytes"].(float64) != 2*planetest.ChunkBytes {
			t.Fatalf("%s: want offset 1 MiB length 2 MiB, got %v", path, e)
		}
	}
	resp := h.expect(h.do("GET", "/api/v1/snapshots/"+snaps[0]+"/blocks:allocated", csi, nil))(http.StatusOK)
	if ext := resp["extents"].([]any); len(ext) != 1 || ext[0].(map[string]any)["length_bytes"].(float64) != planetest.ChunkBytes {
		t.Fatalf("allocated blocks of s1 wrong: %v", resp)
	}
	// A later snapshot's allocated blocks include those it shares with older ones.
	extent := func(path string) (float64, float64) {
		t.Helper()
		resp := h.expect(h.do("GET", path, csi, nil))(http.StatusOK)
		ext := resp["extents"].([]any)
		if len(ext) != 1 {
			t.Fatalf("%s: want 1 extent, got %v", path, ext)
		}
		e := ext[0].(map[string]any)
		return e["offset_bytes"].(float64), e["length_bytes"].(float64)
	}
	if off, n := extent("/api/v1/snapshots/" + snaps[2] + "/blocks:allocated"); off != 0 || n != 3*planetest.ChunkBytes {
		t.Fatalf("allocated blocks of s3: %v+%v, want 0+3 MiB", off, n)
	}
	// The base must be older than the target in its chain.
	h.expect(h.do("GET", "/api/v1/snapshots/"+snaps[0]+"/blocks:changed?base_snapshot_id="+snaps[2], csi, nil))(http.StatusBadRequest)
	h.expect(h.do("GET", "/api/v1/snapshots/"+snaps[0]+"/blocks:changed?base_snapshot_id="+snaps[0], csi, nil))(http.StatusBadRequest)
	// Deleting the middle snapshot leaves s3's changes since s1 the same.
	h.expect(h.do("DELETE", "/api/v1/snapshots/"+snaps[1], csi, nil))(http.StatusOK)
	if off, n := extent("/api/v1/snapshots/" + snaps[2] + "/blocks:changed?base_snapshot_id=" + snaps[0]); off != planetest.ChunkBytes || n != 2*planetest.ChunkBytes {
		t.Fatalf("changes s1->s3 after deleting s2: %v+%v", off, n)
	}
	// A snapshot of a clone holds its origin's data too: allocated reaches into the origin's chain.
	clone := h.expect(h.do("POST", "/api/v1/snapshots/"+snaps[0]+":clone", csi, map[string]any{"clone_name": "restored"}))(http.StatusCreated)["volume"].(map[string]any)
	h.fake.WriteChunk(clone["uuid"].(string), 5)
	cs := h.expect(h.do("POST", "/api/v1/volumes/"+clone["id"].(string)+"/snapshots", csi, map[string]any{"name": "c1"}))(http.StatusCreated)
	resp = h.expect(h.do("GET", "/api/v1/snapshots/"+field(cs, "snapshot", "id").(string)+"/blocks:allocated", csi, nil))(http.StatusOK)
	if ext := resp["extents"].([]any); len(ext) != 2 || ext[0].(map[string]any)["offset_bytes"].(float64) != 0 || ext[1].(map[string]any)["offset_bytes"].(float64) != 5*planetest.ChunkBytes {
		t.Fatalf("allocated blocks of a clone's snapshot: %v", ext)
	}
	// A data plane that can't tell: 501, never made-up extents.
	h.fake.FailWith("AllocatedBlocks", 1, dataplane.ErrCBTUnsupported)
	h.expect(h.do("GET", "/api/v1/snapshots/"+snaps[2]+"/blocks:allocated", csi, nil))(http.StatusNotImplemented)
}

func TestSnapshotNamesDoNotCollideAcrossVolumes(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	a := h.volume(poolID, "vol-a", 1<<30)
	b := h.volume(poolID, "daily", 1<<30) // same name as a snapshot below

	sa := h.expect(h.do("POST", "/api/v1/volumes/"+a["id"].(string)+"/snapshots", h.admin, map[string]any{"name": "daily"}))(http.StatusCreated)["snapshot"].(map[string]any)
	sb := h.expect(h.do("POST", "/api/v1/volumes/"+b["id"].(string)+"/snapshots", h.admin, map[string]any{"name": "daily"}))(http.StatusCreated)["snapshot"].(map[string]any)
	if sa["uuid"] == "" || sb["uuid"] == "" || sa["uuid"] == sb["uuid"] || sa["handle"] == sb["handle"] {
		t.Fatalf("snapshots must map to distinct volumes: %v / %v", sa, sb)
	}
	h.expect(h.do("DELETE", "/api/v1/snapshots/"+sa["id"].(string), h.admin, nil))(http.StatusOK)
	if !h.fake.VolumeExists(sb["handle"].(string)) || !h.fake.VolumeExists(b["handle"].(string)) {
		t.Fatalf("deleting one volume's snapshot destroyed another volume")
	}
}

func TestDataPlaneFailuresAreNotPersisted(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	h.addDisk("d2", "0000:02:00.0")

	// Pool: RAID creation fails -> no pool record, disks released for reuse.
	h.expect(h.do("POST", "/api/v1/disks/d1:claim", h.admin, nil))(http.StatusOK)
	h.fake.Fail("CreateArray", 1)
	if code, _ := h.do("POST", "/api/v1/pools", h.admin, map[string]any{"name": "p", "raid_level": "RAID0", "disk_ids": []string{"d1"}}); code != http.StatusInternalServerError {
		t.Fatalf("pool create with a failing data plane returned %d", code)
	}
	if pools, _ := h.st.ListPools(); len(pools) != 0 {
		t.Fatalf("failed pool was persisted: %+v", pools[0])
	}
	if d, _ := h.st.GetDisk("d1"); d.PoolID != "" {
		t.Fatalf("disk still reserved by failed pool")
	}
	// Thin pool creation fails -> array rolled back too.
	h.fake.Fail("CreateThinPool", 1)
	if code, _ := h.do("POST", "/api/v1/pools", h.admin, map[string]any{"name": "p", "raid_level": "RAID0", "disk_ids": []string{"d1"}}); code != http.StatusInternalServerError {
		t.Fatalf("pool create with a failing thin pool returned %d", code)
	}
	if h.fake.CallCount("DeleteArray") != 1 {
		t.Fatalf("array not rolled back after the thin pool failed")
	}

	poolID := h.pool("p", "RAID0", "d2")

	// Volume create fails -> no record.
	h.fake.Fail("CreateVolume", 1)
	if code, _ := h.do("POST", "/api/v1/volumes", h.admin, map[string]any{"name": "v", "pool_id": poolID, "size_bytes": 1 << 30}); code != http.StatusInternalServerError {
		t.Fatalf("volume create returned %d", code)
	}
	if v, _ := h.st.ListVolumes(""); len(v) != 0 {
		t.Fatalf("failed volume persisted")
	}
	vol := h.volume(poolID, "v", 1<<30)
	volID := vol["id"].(string)

	// Resize failure -> size unchanged.
	h.fake.Fail("ResizeVolume", 1)
	if code, _ := h.do("POST", "/api/v1/volumes/"+volID+":resize", h.admin, map[string]any{"new_size_bytes": 2 << 30}); code != http.StatusInternalServerError {
		t.Fatalf("resize returned %d", code)
	}
	if v, _ := h.st.GetVolume(volID); v.SizeBytes != 1<<30 {
		t.Fatalf("failed resize persisted size %d", v.SizeBytes)
	}

	// Snapshot failure -> no record.
	h.fake.Fail("Snapshot", 1)
	if code, _ := h.do("POST", "/api/v1/volumes/"+volID+"/snapshots", h.admin, map[string]any{"name": "s"}); code != http.StatusInternalServerError {
		t.Fatalf("snapshot returned %d", code)
	}
	if s, _ := h.st.ListSnapshots("", ""); len(s) != 0 {
		t.Fatalf("failed snapshot persisted")
	}

	// Purge failure -> record retained (no leaked volume).
	h.expect(h.do("DELETE", "/api/v1/volumes/"+volID, h.admin, nil))(http.StatusOK)
	h.fake.Fail("DeleteVolume", 1)
	if code, _ := h.do("DELETE", "/api/v1/deleted-volumes/"+volID, h.admin, nil); code != http.StatusInternalServerError {
		t.Fatalf("purge returned %d", code)
	}
	if _, err := h.st.GetVolume(volID); err != nil {
		t.Fatalf("volume record dropped although the delete failed")
	}
	h.expect(h.do("DELETE", "/api/v1/deleted-volumes/"+volID, h.admin, nil))(http.StatusOK)
	if h.fake.VolumeExists(vol["handle"].(string)) {
		t.Fatalf("volume leaked")
	}
}

// The data plane makes volumes in whole 4 MiB extents: the record says what hosts see.
func TestVolumeSizeRoundsUp(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	vol := h.volume(poolID, "v", 3<<19) // 1.5 MiB
	if got := h.fake.VolumeSize(vol["uuid"].(string)); got != 4<<20 || vol["size_bytes"] != float64(4<<20) {
		t.Fatalf("1.5 MiB volume: %d bytes in the data plane, %v recorded, want 4 MiB", got, vol["size_bytes"])
	}
	// Asking again (a CSI retry) finds it; growing rounds up too.
	h.expect(h.do("POST", "/api/v1/volumes", h.admin, map[string]any{"name": "v", "pool_id": poolID, "size_bytes": 3 << 19}))(http.StatusOK)
	h.expect(h.do("POST", "/api/v1/volumes/"+vol["id"].(string)+":resize", h.admin, map[string]any{"new_size_bytes": 5 << 20}))(http.StatusOK)
	if v, _ := h.st.GetVolume(vol["id"].(string)); v.SizeBytes != 8<<20 || h.fake.VolumeSize(v.UUID) != 8<<20 {
		t.Fatalf("grown to 5 MiB: recorded %d, in the data plane %d, want 8 MiB", v.SizeBytes, h.fake.VolumeSize(v.UUID))
	}
}

// thin_provision is accepted and ignored: every volume is thin, so each can be snapshotted.
func TestThickVolumesAreThin(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	vol := h.expect(h.do("POST", "/api/v1/volumes", h.admin, map[string]any{"name": "v", "pool_id": poolID, "size_bytes": 1 << 30, "thin_provision": false}))(http.StatusCreated)["volume"].(map[string]any)
	if vol["thin_provision"] != true {
		t.Fatalf("volume: %v", vol)
	}
	h.expect(h.do("POST", "/api/v1/volumes/"+vol["id"].(string)+"/snapshots", h.admin, map[string]any{"name": "s"}))(http.StatusCreated)
}

func TestSnapshotsOutliveSourceVolume(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	vol := h.volume(poolID, "src", 1<<30)
	snap := h.expect(h.do("POST", "/api/v1/volumes/"+vol["id"].(string)+"/snapshots", h.admin, map[string]any{"name": "s"}))(http.StatusCreated)["snapshot"].(map[string]any)
	h.expect(h.do("DELETE", "/api/v1/volumes/"+vol["id"].(string), h.admin, nil))(http.StatusOK)
	if !h.fake.VolumeExists(snap["handle"].(string)) {
		t.Fatalf("snapshot deleted with its source volume")
	}
	h.expect(h.do("POST", "/api/v1/snapshots/"+snap["id"].(string)+":clone", h.admin, map[string]any{"clone_name": "restored"}))(http.StatusCreated)
}

func TestGroupSnapshotWithCSIStyleName(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	v1, v2 := h.volume(poolID, "a", 1<<30), h.volume(poolID, "b", 1<<30)
	name := "groupsnapshot-6a1f3c2e-9b7d-4c1e-8f2a-1234567890ab"
	resp := h.expect(h.do("POST", "/api/v1/volume-group-snapshots", h.admin, map[string]any{"name": name, "volume_ids": []string{v1["id"].(string), v2["id"].(string)}}))(http.StatusCreated)
	snaps := resp["snapshots"].([]any)
	if len(snaps) != 2 {
		t.Fatalf("want 2 member snapshots, got %v", resp)
	}
	for _, s := range snaps {
		if !h.fake.VolumeExists(s.(map[string]any)["handle"].(string)) {
			t.Fatalf("member snapshot missing: %v", s)
		}
	}
	// All members are taken in one call, never one by one.
	if calls := h.fake.Calls(); slices.Contains(calls, "Snapshot") || !slices.Contains(calls, "SnapshotGroup") {
		t.Fatalf("group snapshot used %v", calls)
	}
	ids := []string{v1["id"].(string), v2["id"].(string)}
	volumes := h.fake.VolumeCount
	before := volumes()

	// A failed group leaves nothing behind.
	h.fake.Fail("SnapshotGroup", 1)
	if code, _ := h.do("POST", "/api/v1/volume-group-snapshots", h.admin, map[string]any{"name": "g2", "volume_ids": ids}); code != http.StatusInternalServerError {
		t.Fatalf("failing group snapshot returned %d", code)
	}
	// It can fail after taking the snapshots: velod deletes them by name.
	h.fake.FailAfterApplying("SnapshotGroup", 1)
	if code, _ := h.do("POST", "/api/v1/volume-group-snapshots", h.admin, map[string]any{"name": "g3", "volume_ids": ids}); code != http.StatusInternalServerError {
		t.Fatalf("group snapshot failing after it was taken returned %d", code)
	}
	if n := volumes(); n != before {
		t.Fatalf("failed group snapshots left %d volumes", n-before)
	}
	// I/O that doesn't drain in time is worth retrying: 503.
	h.fake.FailWith("SnapshotGroup", 1, dataplane.ErrQuiesceTimeout)
	if code, out := h.do("POST", "/api/v1/volume-group-snapshots", h.admin, map[string]any{"name": "g4", "volume_ids": ids}); code != http.StatusServiceUnavailable || !strings.Contains(fmt.Sprint(out["error"]), "no snapshot was taken") {
		t.Fatalf("timed-out group snapshot: HTTP %d %v, want 503", code, out)
	}
	if all, _ := h.st.ListSnapshots("", ""); len(all) != 2 {
		t.Fatalf("failed group snapshots left %d snapshot records", len(all))
	}
	// The same volume twice counts once.
	h.expect(h.do("POST", "/api/v1/volume-group-snapshots", h.admin, map[string]any{"name": "g5", "volume_ids": []string{ids[0], ids[0]}}))(http.StatusCreated)
}

func TestIscsiExportACLs(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	v1, v2 := h.volume(poolID, "a", 1<<30), h.volume(poolID, "b", 1<<30)
	portal := h.expect(h.do("POST", "/api/v1/portals", h.admin, map[string]any{"name": "i", "protocol": "ISCSI", "ip_address": "10.0.0.10"}))(http.StatusCreated)
	pid := field(portal, "portal", "id").(string)
	ig1 := field(h.expect(h.do("POST", "/api/v1/initiator-groups", h.admin, map[string]any{"name": "g1", "initiator_iqns": []string{"iqn.a"}}))(http.StatusCreated), "initiator_group", "id").(string)
	ig2 := field(h.expect(h.do("POST", "/api/v1/initiator-groups", h.admin, map[string]any{"name": "g2", "initiator_iqns": []string{"iqn.b"}}))(http.StatusCreated), "initiator_group", "id").(string)

	if code, _ := h.do("POST", "/api/v1/exports", h.admin, map[string]any{"name": "open", "volume_id": v1["id"], "protocol": "ISCSI", "portal_ids": []string{pid}}); code != http.StatusBadRequest {
		t.Fatalf("iSCSI export with no initiators and no allow_any_host must be rejected, got %d", code)
	}
	e1 := h.expect(h.do("POST", "/api/v1/exports", h.admin, map[string]any{"name": "e1", "volume_id": v1["id"], "protocol": "ISCSI", "portal_ids": []string{pid}, "initiator_group_ids": []string{ig1}}))(http.StatusCreated)["export"].(map[string]any)
	e2 := h.expect(h.do("POST", "/api/v1/exports", h.admin, map[string]any{"name": "e2", "volume_id": v2["id"], "protocol": "ISCSI", "portal_ids": []string{pid}, "initiator_group_ids": []string{ig2}}))(http.StatusCreated)["export"].(map[string]any)

	t1, _ := h.fake.Target(e1["target_iqn"].(string))
	t2, _ := h.fake.Target(e2["target_iqn"].(string))
	if !slices.Equal(t1.Hosts, []string{"iqn.a"}) || !slices.Equal(t2.Hosts, []string{"iqn.b"}) {
		t.Fatalf("each export must have its own ACL: %+v / %+v", t1, t2)
	}

	h.expect(h.do("DELETE", "/api/v1/exports/"+e1["id"].(string), h.admin, nil))(http.StatusOK)
	if _, ok := h.fake.Target(e1["target_iqn"].(string)); ok {
		t.Fatal("the deleted export's target is still there")
	}
}

func TestFilterPoliciesRejected(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	code, resp := h.do("POST", "/api/v1/volumes", h.admin, map[string]any{"name": "v", "pool_id": poolID, "size_bytes": 1 << 30, "filter_policy": map[string]any{"encryption_enabled": true}})
	if code != http.StatusBadRequest || !strings.Contains(resp["error"].(string), "not implemented") {
		t.Fatalf("encryption request must be rejected rather than silently ignored: %d %v", code, resp)
	}
}
