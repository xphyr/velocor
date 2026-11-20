package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/meta"
	"github.com/velocor/velocor/internal/reconciler"
	"github.com/velocor/velocor/internal/spdk/spdktest"
	"github.com/velocor/velocor/internal/store"
)

// appliance builds a velod (store, reconciler, API) on fake, with dev as its metadata device
// (nil: copies disabled). A new appliance on the same fake is a velod that lost its database.
func appliance(t *testing.T, f *spdktest.Fake, dev meta.Device) *harness {
	t.Helper()
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "velocor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	authMgr, err := auth.NewManager(st, "admin-pass-123")
	if err != nil {
		t.Fatal(err)
	}
	clearMustChange(t, st)
	rec := reconciler.New(st, f.Client(), nil, nil)
	if dev != nil {
		rec.SetMetaDevice(dev, "node-1", "test")
	}
	srv := New(st, f.Client(), nil, authMgr, rec, nil, WithMetaDevice(dev))
	srv.sbReader = func(d *store.Disk) (*hardware.RaidSuperblock, error) {
		if d.PCIAddress == "" {
			return f.SuperblockOf(d.DevicePath), nil
		}
		return f.SuperblockOf(d.PCIAddress), nil
	}
	h := &harness{t: t, st: st, fake: f, ts: httptest.NewServer(srv.Handler())}
	t.Cleanup(h.ts.Close)
	_, resp := h.do("POST", "/api/v1/auth/login", "", map[string]any{"username": "admin", "password": "admin-pass-123"})
	h.admin = resp["access_token"].(string)
	return h
}

// discover records the disks as a hardware scan of a fresh appliance would: unclaimed.
func (h *harness) discover(pcis map[string]string) {
	for id, pci := range pcis {
		if err := h.st.UpsertDisk(&store.Disk{ID: id, Name: id, Type: store.DiskTypeNVMe, PCIAddress: pci, SizeBytes: 1 << 40, DriverState: store.DiskDriverStateVfioPCI}); err != nil {
			h.t.Fatal(err)
		}
	}
}

// buildPool makes a RAID1 pool with one volume, a served export and one whose portal
// address isn't local, and writes its metadata copy.
func buildPool(t *testing.T, f *spdktest.Fake, dev meta.Device) (poolID, volID string) {
	defer dataplane.SetAddressCheck(func(string) bool { return true })() // the old appliance had 203.0.113.9
	h := appliance(t, f, dev)
	h.addDisk("d1", "0000:01:00.0")
	h.addDisk("d2", "0000:02:00.0")
	poolID = h.pool("tank", "RAID1", "d1", "d2")
	vol := h.volume(poolID, "db", 1<<30)
	volID = vol["id"].(string)
	ig := h.expect(h.do("POST", "/api/v1/initiator-groups", h.admin, map[string]any{"name": "g", "host_nqns": []string{"nqn.2026-01.io.example:h"}}))(http.StatusCreated)
	igID := field(ig, "initiator_group", "id").(string)
	for i, ip := range []string{"127.0.0.1", "203.0.113.9"} {
		pt := h.expect(h.do("POST", "/api/v1/portals", h.admin, map[string]any{"name": "pt" + ip, "protocol": "NVMEOF_TCP", "ip_address": ip}))(http.StatusCreated)
		h.expect(h.do("POST", "/api/v1/exports", h.admin, map[string]any{
			"name": []string{"local", "remote"}[i], "volume_id": volID, "protocol": "NVMEOF_TCP",
			"portal_ids": []string{field(pt, "portal", "id").(string)}, "initiator_group_ids": []string{igID},
		}))(http.StatusCreated)
	}
	if _, err := h.st.MutateDisk("d1", func(d *store.Disk) error {
		d.PowerLossProtected, d.WriteCachePolicy = true, store.WriteCachePolicyDisabled
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	return poolID, volID
}

var labDisks = map[string]string{"d1": "0000:01:00.0", "d2": "0000:02:00.0"}

func TestImportFromManifestAfterLosingTheDatabase(t *testing.T) {
	f := spdktest.New(t)
	dev := &meta.MemDevice{}
	poolID, volID := buildPool(t, f, dev)

	f.Restart() // reboot: SPDK starts empty
	h := appliance(t, f, dev)
	h.discover(labDisks)

	list := h.expect(h.do("GET", "/api/v1/pool-imports", h.admin, nil))(http.StatusOK)["candidates"].([]any)
	if len(list) != 1 {
		t.Fatalf("candidates: %v", list)
	}
	c := list[0].(map[string]any)
	if c["pool_id"] != poolID || c["complete"] != true || c["importable"] != true || c["raid_level"] != "RAID1" {
		t.Fatalf("candidate: %v", c)
	}

	res := h.expect(h.do("POST", "/api/v1/pool-imports", h.admin, map[string]any{"pool_id": poolID}))(http.StatusCreated)
	if res["source"] != "manifest" || res["volumes"] != float64(1) || res["exports_enabled"] != float64(1) || res["exports_disabled"] != float64(1) {
		t.Fatalf("import result: %v", res)
	}
	h.reconcile()
	p := h.getPool(poolID)
	if p["name"] != "tank" || p["health_status"] != "ONLINE" {
		t.Fatalf("imported pool: %v %v", p["name"], p["health_status"])
	}
	if d, _ := h.st.GetDisk("d1"); !d.PowerLossProtected || d.WriteCachePolicy != store.WriteCachePolicyDisabled {
		t.Fatalf("disk settings not restored: %+v", d)
	}
	v, err := h.st.GetVolume(volID)
	if err != nil || v.Name != "db" {
		t.Fatalf("imported volume: %+v %v", v, err)
	}
	exps, _ := h.st.ListExports("", "")
	for _, e := range exps {
		local := e.Name == "local"
		if e.Disabled == local {
			t.Fatalf("export %s: disabled %v (%s)", e.Name, e.Disabled, e.DisabledReason)
		}
		if !local && !strings.Contains(e.DisabledReason, "isn't on this appliance") {
			t.Fatalf("reason: %q", e.DisabledReason)
		}
	}
	// The operator moves the disabled export to a portal on this appliance and enables it.
	var remote, local *store.Export
	for _, e := range exps {
		if e.Name == "remote" {
			remote = e
		} else {
			local = e
		}
	}
	path := "/api/v1/exports/" + remote.ID
	h.expect(h.do("PATCH", path, h.admin, map[string]any{"portal_ids": []string{}}))(http.StatusBadRequest)
	upd := h.expect(h.do("PATCH", path, h.admin, map[string]any{"portal_ids": local.PortalIDs, "enabled": true}))(http.StatusOK)["export"].(map[string]any)
	if upd["disabled"] == true || upd["portal_ids"].([]any)[0] != local.PortalIDs[0] {
		t.Fatalf("export after the portal change: %v", upd)
	}
	subs, _ := h.fake.Client().NvmfGetSubsystems(t.Context())
	served := false
	for _, sub := range subs {
		if sub.Nqn == remote.SubsystemNQN {
			served = len(sub.ListenAddresses) == 1 && sub.ListenAddresses[0].Traddr == "127.0.0.1"
		}
	}
	if !served {
		t.Fatalf("%s is not served on the local portal only: %+v", remote.SubsystemNQN, subs)
	}

	// The next copy continues the pool's generations.
	if p := h.getPool(poolID); p["meta_generation"].(float64) < 2 {
		t.Fatalf("meta generation after import: %v", p["meta_generation"])
	}
	// Nothing left to import.
	if list := h.expect(h.do("GET", "/api/v1/pool-imports", h.admin, nil))(http.StatusOK)["candidates"].([]any); len(list) != 0 {
		t.Fatalf("candidates after import: %v", list)
	}
}

// ssd records an unclaimed SSD at path, as a hardware scan would.
func (h *harness) ssd(id, path string) {
	h.t.Helper()
	h.fake.AddFile(path)
	if err := h.st.UpsertDisk(&store.Disk{ID: id, Name: id, Type: store.DiskTypeSSD, DevicePath: path, SizeBytes: 1 << 40, BlockSize: 512, DriverState: store.DiskDriverStateKernel}); err != nil {
		h.t.Fatal(err)
	}
}

// docs/review-history.md P1-14: disks moved to another appliance and to another controller on the way (new
// IDs) are imported, keeping the member UUIDs their RAID knows them by.
func TestImportMirrorUnderNewDiskIDs(t *testing.T) {
	f := spdktest.New(t)
	dev := &meta.MemDevice{}
	old := appliance(t, f, dev)
	old.ssd("ssd-ata-1", "/dev/disk/by-id/ata-1")
	old.ssd("ssd-ata-2", "/dev/disk/by-id/ata-2")
	poolID := old.pool("tank", "RAID1", "ssd-ata-1", "ssd-ata-2")
	old.volume(poolID, "db", 1<<30)
	old.reconcile()

	f.Restart()
	f.MoveMedia("/dev/disk/by-id/ata-1", "/dev/disk/by-id/wwn-1")
	f.MoveMedia("/dev/disk/by-id/ata-2", "/dev/disk/by-id/wwn-2")
	h := appliance(t, f, dev)
	for _, n := range []string{"1", "2"} {
		if err := h.st.UpsertDisk(&store.Disk{ID: "ssd-wwn-" + n, Name: "sd", Type: store.DiskTypeSSD, DevicePath: "/dev/disk/by-id/wwn-" + n, SizeBytes: 1 << 40, BlockSize: 512, DriverState: store.DiskDriverStateKernel}); err != nil {
			t.Fatal(err)
		}
	}
	c := h.expect(h.do("GET", "/api/v1/pool-imports", h.admin, nil))(http.StatusOK)["candidates"].([]any)[0].(map[string]any)
	if c["complete"] != true || c["importable"] != true || c["present"] != float64(2) {
		t.Fatalf("candidate: %v", c)
	}
	h.expect(h.do("POST", "/api/v1/pool-imports", h.admin, map[string]any{"pool_id": poolID}))(http.StatusCreated)
	h.reconcile()
	if p := h.getPool(poolID); p["health_status"] != "ONLINE" {
		t.Fatalf("imported pool: %v %v", p["health_status"], p["status_message"])
	}
	// The kept UUIDs survive a restart.
	f.Restart()
	h.reconcile()
	h.reconcile()
	if p := h.getPool(poolID); p["health_status"] != "ONLINE" {
		t.Fatalf("after a restart: %v %v", p["health_status"], p["status_message"])
	}
}

func TestImportSkeletonWithoutACopy(t *testing.T) {
	f := spdktest.New(t)
	poolID, volID := buildPool(t, f, nil)
	f.Restart()
	h := appliance(t, f, nil)
	h.discover(labDisks)

	res := h.expect(h.do("POST", "/api/v1/pool-imports", h.admin, map[string]any{"pool_id": poolID}))(http.StatusCreated)
	if res["source"] != "skeleton" || res["volumes"] != float64(1) || res["exports_enabled"] != float64(0) {
		t.Fatalf("skeleton import: %v", res)
	}
	v, err := h.st.GetVolume(volID)
	if err != nil || v.Name != volID || v.SizeBytes != 1<<30 {
		t.Fatalf("skeleton volume: %+v %v", v, err)
	}
	if p := h.getPool(poolID); p["name"] != poolID {
		t.Fatalf("skeleton pool name: %v", p["name"])
	}
}

func TestImportConflictRollsBack(t *testing.T) {
	f := spdktest.New(t)
	dev := &meta.MemDevice{}
	poolID, _ := buildPool(t, f, dev)
	f.Restart()
	h := appliance(t, f, dev)
	h.discover(labDisks)
	h.addDisk("d3", "0000:03:00.0")
	h.pool("tank", "RAID0", "d3") // the name is taken here

	code, out := h.do("POST", "/api/v1/pool-imports", h.admin, map[string]any{"pool_id": poolID})
	if code != http.StatusConflict || !strings.Contains(out["error"].(string), "pool name") {
		t.Fatalf("conflict: %d %v", code, out)
	}
	// Nothing recorded; the array stays assembled and the import can be resumed.
	if _, err := h.st.GetPool(poolID); err == nil {
		t.Fatal("pool recorded by a failed import")
	}
	for id := range labDisks {
		if d, _ := h.st.GetDisk(id); d.PoolID != "" {
			t.Fatalf("disk %s in a pool after a failed import", id)
		}
	}
	list := h.expect(h.do("GET", "/api/v1/pool-imports", h.admin, nil))(http.StatusOK)["candidates"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["importable"] != true || list[0].(map[string]any)["complete"] != true {
		t.Fatalf("candidates after a failed import: %v", list)
	}
	// Retry under another name.
	res := h.expect(h.do("POST", "/api/v1/pool-imports", h.admin, map[string]any{"pool_id": poolID, "name": "tank-old"}))(http.StatusCreated)
	if res["pool"].(map[string]any)["name"] != "tank-old" || res["source"] != "manifest" {
		t.Fatalf("renamed import: %v", res)
	}
	h.reconcile()
	if p := h.getPool(poolID); p["health_status"] != "ONLINE" || len(p["disk_ids"].([]any)) != 2 {
		t.Fatalf("resumed import: %v %v", p["health_status"], p["disk_ids"])
	}
}
