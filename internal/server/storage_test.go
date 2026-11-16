package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/reconciler"
	"github.com/velocor/velocor/internal/spdk/spdktest"
	"github.com/velocor/velocor/internal/store"
)

type harness struct {
	t     *testing.T
	st    *store.BoltStore
	fake  *spdktest.Fake
	ts    *httptest.Server
	admin string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "srv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := spdktest.New(t)
	authMgr, err := auth.NewManager(st, "admin-pass-123")
	if err != nil {
		t.Fatal(err)
	}
	clearMustChange(t, st)
	srv := New(st, f.Client(), nil, authMgr, reconciler.New(st, f.Client(), nil, nil), nil)
	h := &harness{t: t, st: st, fake: f, ts: httptest.NewServer(srv.Handler())}
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
	h.fake.AddNVMe(pci)
	if err := h.st.UpsertDisk(&store.Disk{ID: id, Name: id, Type: store.DiskTypeNVMe, PCIAddress: pci, SizeBytes: 1 << 40, DriverState: store.DiskDriverStateVfioPCI}); err != nil {
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
	poolID := h.pool("pool0", "RAID0", "nvme-data1")
	pool, _ := h.st.GetPool(poolID)
	if pool.LVSUUID == "" || pool.HealthStatus != store.HealthStatusOnline {
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
		h.fake.WriteCluster(volUUID, uint64(i))
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
		if e["offset_bytes"].(float64) != 65536 || e["length_bytes"].(float64) != 131072 {
			t.Fatalf("%s: want offset 64KiB length 128KiB, got %v", path, e)
		}
	}
	resp := h.expect(h.do("GET", "/api/v1/snapshots/"+snaps[0]+"/blocks:allocated", csi, nil))(http.StatusOK)
	if ext := resp["extents"].([]any); len(ext) != 1 || ext[0].(map[string]any)["length_bytes"].(float64) != 65536 {
		t.Fatalf("allocated blocks of s1 wrong: %v", resp)
	}
}

func TestSnapshotNamesDoNotCollideAcrossVolumes(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	a := h.volume(poolID, "vol-a", 1<<30)
	b := h.volume(poolID, "daily", 1<<30) // same name as a snapshot below

	sa := h.expect(h.do("POST", "/api/v1/volumes/"+a["id"].(string)+"/snapshots", h.admin, map[string]any{"name": "daily"}))(http.StatusCreated)["snapshot"].(map[string]any)
	sb := h.expect(h.do("POST", "/api/v1/volumes/"+b["id"].(string)+"/snapshots", h.admin, map[string]any{"name": "daily"}))(http.StatusCreated)["snapshot"].(map[string]any)
	if sa["uuid"] == "" || sb["uuid"] == "" || sa["uuid"] == sb["uuid"] || sa["lvol_bdev_name"] == sb["lvol_bdev_name"] {
		t.Fatalf("snapshots must map to distinct SPDK lvols: %v / %v", sa, sb)
	}
	h.expect(h.do("DELETE", "/api/v1/snapshots/"+sa["id"].(string), h.admin, nil))(http.StatusOK)
	if !h.fake.LvolExists(sb["lvol_bdev_name"].(string)) || !h.fake.LvolExists(b["lvol_bdev_name"].(string)) {
		t.Fatalf("deleting one volume's snapshot destroyed another lvol")
	}
}

func TestSPDKFailuresAreNotPersisted(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	h.addDisk("d2", "0000:02:00.0")

	// Pool: RAID creation fails -> no pool record, disks released for reuse.
	h.expect(h.do("POST", "/api/v1/disks/d1:claim", h.admin, nil))(http.StatusOK)
	h.fake.Fail("bdev_raid_create", 1)
	if code, _ := h.do("POST", "/api/v1/pools", h.admin, map[string]any{"name": "p", "raid_level": "RAID0", "disk_ids": []string{"d1"}}); code != http.StatusInternalServerError {
		t.Fatalf("pool create with failing SPDK returned %d", code)
	}
	if pools, _ := h.st.ListPools(); len(pools) != 0 {
		t.Fatalf("failed pool was persisted: %+v", pools[0])
	}
	if d, _ := h.st.GetDisk("d1"); d.PoolID != "" {
		t.Fatalf("disk still reserved by failed pool")
	}
	// lvstore creation fails -> RAID rolled back too.
	h.fake.Fail("bdev_lvol_create_lvstore", 1)
	if code, _ := h.do("POST", "/api/v1/pools", h.admin, map[string]any{"name": "p", "raid_level": "RAID0", "disk_ids": []string{"d1"}}); code != http.StatusInternalServerError {
		t.Fatalf("pool create with failing lvstore returned %d", code)
	}
	if h.fake.CallCount("bdev_raid_delete") != 1 {
		t.Fatalf("RAID not rolled back after lvstore failure")
	}

	poolID := h.pool("p", "RAID0", "d2")

	// Volume create fails -> no record.
	h.fake.Fail("bdev_lvol_create", 1)
	if code, _ := h.do("POST", "/api/v1/volumes", h.admin, map[string]any{"name": "v", "pool_id": poolID, "size_bytes": 1 << 30}); code != http.StatusInternalServerError {
		t.Fatalf("volume create returned %d", code)
	}
	if v, _ := h.st.ListVolumes(""); len(v) != 0 {
		t.Fatalf("failed volume persisted")
	}
	// QoS failure on create -> lvol rolled back.
	h.fake.Fail("bdev_set_qos_limit", 1)
	if code, _ := h.do("POST", "/api/v1/volumes", h.admin, map[string]any{"name": "v", "pool_id": poolID, "size_bytes": 1 << 30, "qos_policy": map[string]any{"rw_ios_per_sec": 100}}); code != http.StatusInternalServerError {
		t.Fatalf("volume create with failing QoS returned %d", code)
	}

	vol := h.volume(poolID, "v", 1<<30)
	volID := vol["id"].(string)

	// Resize failure -> size unchanged.
	h.fake.Fail("bdev_lvol_resize", 1)
	if code, _ := h.do("POST", "/api/v1/volumes/"+volID+":resize", h.admin, map[string]any{"new_size_bytes": 2 << 30}); code != http.StatusInternalServerError {
		t.Fatalf("resize returned %d", code)
	}
	if v, _ := h.st.GetVolume(volID); v.SizeBytes != 1<<30 {
		t.Fatalf("failed resize persisted size %d", v.SizeBytes)
	}

	// Snapshot failure -> no record.
	h.fake.Fail("bdev_lvol_snapshot", 1)
	if code, _ := h.do("POST", "/api/v1/volumes/"+volID+"/snapshots", h.admin, map[string]any{"name": "s"}); code != http.StatusInternalServerError {
		t.Fatalf("snapshot returned %d", code)
	}
	if s, _ := h.st.ListSnapshots("", ""); len(s) != 0 {
		t.Fatalf("failed snapshot persisted")
	}

	// Delete failure -> record retained (no leaked lvol).
	h.fake.Fail("bdev_lvol_delete", 1)
	if code, _ := h.do("DELETE", "/api/v1/volumes/"+volID, h.admin, nil); code != http.StatusInternalServerError {
		t.Fatalf("delete returned %d", code)
	}
	if _, err := h.st.GetVolume(volID); err != nil {
		t.Fatalf("volume record dropped although SPDK delete failed")
	}
	h.expect(h.do("DELETE", "/api/v1/volumes/"+volID, h.admin, nil))(http.StatusOK)
	if h.fake.LvolExists(vol["lvol_bdev_name"].(string)) {
		t.Fatalf("lvol leaked")
	}
}

func TestVolumeSizeRoundsUp(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	vol := h.volume(poolID, "v", 3<<19) // 1.5 MiB
	if got := h.fake.LvolSizeMiB(vol["uuid"].(string)); got != 2 {
		t.Fatalf("1.5 MiB volume allocated %d MiB, want 2", got)
	}
}

func TestSnapshotsOutliveSourceVolume(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	poolID := h.pool("p", "RAID0", "d1")
	vol := h.volume(poolID, "src", 1<<30)
	snap := h.expect(h.do("POST", "/api/v1/volumes/"+vol["id"].(string)+"/snapshots", h.admin, map[string]any{"name": "s"}))(http.StatusCreated)["snapshot"].(map[string]any)
	h.expect(h.do("DELETE", "/api/v1/volumes/"+vol["id"].(string), h.admin, nil))(http.StatusOK)
	if !h.fake.LvolExists(snap["lvol_bdev_name"].(string)) {
		t.Fatalf("snapshot lvol deleted with its source volume")
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
		if !h.fake.LvolExists(s.(map[string]any)["lvol_bdev_name"].(string)) {
			t.Fatalf("member snapshot missing in SPDK: %v", s)
		}
	}
	// A failing member rolls the whole group back.
	h.fake.Fail("bdev_lvol_snapshot", -1)
	if code, _ := h.do("POST", "/api/v1/volume-group-snapshots", h.admin, map[string]any{"name": "g2", "volume_ids": []string{v1["id"].(string), v2["id"].(string)}}); code != http.StatusInternalServerError {
		t.Fatalf("failing group snapshot returned %d", code)
	}
	h.fake.Fail("bdev_lvol_snapshot", 0)
	if all, _ := h.st.ListSnapshots("", ""); len(all) != 2 {
		t.Fatalf("failed group snapshot left %d snapshots", len(all))
	}
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

	tag1, tag2 := int(e1["iscsi_ig_tag"].(float64)), int(e2["iscsi_ig_tag"].(float64))
	if tag1 == tag2 {
		t.Fatalf("exports must own distinct initiator groups")
	}
	g1, _ := h.fake.InitiatorGroup(tag1)
	g2, _ := h.fake.InitiatorGroup(tag2)
	if !slices.Equal(g1.Initiators, []string{"iqn.a"}) || !slices.Equal(g2.Initiators, []string{"iqn.b"}) {
		t.Fatalf("per-export ACLs wrong: %+v / %+v", g1, g2)
	}
	n2, _ := h.fake.TargetNode(e2["target_iqn"].(string))
	if len(n2.PgIgMaps) != 1 || n2.PgIgMaps[0].IgTag != tag2 {
		t.Fatalf("second export must use its own ACL, got %+v", n2.PgIgMaps)
	}

	h.expect(h.do("DELETE", "/api/v1/exports/"+e1["id"].(string), h.admin, nil))(http.StatusOK)
	if _, ok := h.fake.InitiatorGroup(tag1); ok {
		t.Fatalf("export's initiator group not cleaned up")
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
