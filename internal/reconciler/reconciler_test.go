package reconciler

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/dataplane/spdkplane"
	"github.com/velocor/velocor/internal/spdk/spdktest"
	"github.com/velocor/velocor/internal/store"
)

type env struct {
	t    *testing.T
	ctx  context.Context
	fake *spdktest.Fake
	st   *store.BoltStore
	rec  *Reconciler
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "rec.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := spdktest.New(t)
	return &env{t: t, ctx: context.Background(), fake: f, st: st, rec: New(st, spdkplane.New(f.Client(), nil), nil, nil)}
}

// provision builds a pool + volume through the real SPDK client calls, exactly the way the
// API does, and records them in the store. It returns the pool and volume.
func (e *env) provision(level store.RaidLevel, pcis ...string) (*store.Pool, *store.Volume) {
	e.t.Helper()
	c := e.fake.Client()
	var diskIDs, bdevs []string
	for i, pci := range pcis {
		e.fake.AddNVMe(pci)
		d := &store.Disk{ID: "disk-" + string(rune('a'+i)), Name: "nvme", Type: store.DiskTypeNVMe, PCIAddress: pci, SizeBytes: 1 << 40, DriverState: store.DiskDriverStateVfioPCI}
		if err := e.st.UpsertDisk(d); err != nil {
			e.t.Fatal(err)
		}
		bdev, drv, err := spdkplane.AttachDisk(e.ctx, c, nil, d)
		if err != nil {
			e.t.Fatal(err)
		}
		if _, err := e.st.ClaimDisk(d.ID, drv, bdev); err != nil {
			e.t.Fatal(err)
		}
		diskIDs = append(diskIDs, d.ID)
		bdevs = append(bdevs, bdev)
	}
	pool := &store.Pool{ID: "pool-1", Name: "p1", RaidLevel: level, DiskIDs: diskIDs, StripSizeKB: 64}
	if err := e.st.CreatePool(pool); err != nil {
		e.t.Fatal(err)
	}
	if err := c.BdevRaidCreate(e.ctx, pool.ArrayName, string(level), 64, bdevs, true); err != nil {
		e.t.Fatal(err)
	}
	lvsUUID, err := c.BdevLvolCreateLvstore(e.ctx, pool.Device, pool.ThinPoolName, 0, "")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.MutatePool(pool.ID, func(p *store.Pool) error { p.ThinPoolUUID = lvsUUID; return nil }); err != nil {
		e.t.Fatal(err)
	}
	vol := &store.Volume{ID: "vol-1", Name: "vol-1", PoolID: pool.ID, SizeBytes: 1 << 30, QosPolicy: store.QosPolicy{RwIosPerSec: 5000}}
	if err := e.st.CreateVolume(vol); err != nil {
		e.t.Fatal(err)
	}
	uuid, err := c.BdevLvolCreate(e.ctx, pool.ThinPoolName, vol.Name, vol.SizeBytes, false, "")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.MutateVolume(vol.ID, func(v *store.Volume) error { v.UUID = uuid; return nil }); err != nil {
		e.t.Fatal(err)
	}
	p, _ := e.st.GetPool(pool.ID)
	v, _ := e.st.GetVolume(vol.ID)
	return p, v
}

func (e *env) reconcile() State {
	e.t.Helper()
	st, _ := e.rec.ReconcileOnce(e.ctx)
	return st
}

func TestRestartWithLazyExamineNeverWipesLvstore(t *testing.T) {
	e := newEnv(t)
	pool, vol := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")

	e.fake.LazyExamine = true
	e.fake.Restart()
	st := e.reconcile()

	if gen := e.fake.LvstoreGeneration(pool.ThinPoolName); gen != 1 {
		t.Fatalf("lvstore was re-initialised (generation %d): pool data destroyed", gen)
	}
	if e.fake.CallCount("bdev_lvol_create_lvstore") != 1 || e.fake.CallCount("bdev_raid_create") != 1 {
		t.Fatalf("reconciler created storage instead of letting examine assemble it: %v", e.fake.Calls())
	}
	if !e.fake.LvolExists(vol.Handle) {
		t.Fatalf("volume lvol lost")
	}
	p, _ := e.st.GetPool(pool.ID)
	v, _ := e.st.GetVolume(vol.ID)
	if p.HealthStatus != store.HealthStatusOnline || v.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("pool=%s (%s) vol=%s (%s); reconciler state %+v", p.HealthStatus, p.StatusMessage, v.HealthStatus, v.StatusMessage, st)
	}
	if got := e.fake.QoS(vol.UUID).RwIosPerSec; got != 5000 {
		t.Fatalf("QoS not restored: %d", got)
	}
}

func TestMissingLvstoreIsFaultedNotReinitialised(t *testing.T) {
	e := newEnv(t)
	pool, _ := e.provision(store.RaidLevel0, "0000:01:00.0")
	// Simulate the lvstore being unreadable: pretend the recorded UUID belongs to something else.
	if _, err := e.st.MutatePool(pool.ID, func(p *store.Pool) error { p.ThinPoolUUID = "deadbeef"; return nil }); err != nil {
		t.Fatal(err)
	}
	e.fake.Restart()
	e.reconcile()
	if n := e.fake.CallCount("bdev_lvol_create_lvstore"); n != 1 {
		t.Fatalf("reconciler initialised an lvstore over a pool with recorded data (%d calls)", n)
	}
	p, _ := e.st.GetPool(pool.ID)
	if p.HealthStatus != store.HealthStatusFaulted {
		t.Fatalf("pool health %s, want FAULTED", p.HealthStatus)
	}
}

func TestMissingLvolIsFaultedNotRecreated(t *testing.T) {
	e := newEnv(t)
	_, vol := e.provision(store.RaidLevel0, "0000:01:00.0")
	if err := e.fake.Client().BdevLvolDelete(e.ctx, vol.UUID); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if e.fake.CallCount("bdev_lvol_create") != 1 {
		t.Fatalf("reconciler re-created a blank lvol for a lost volume")
	}
	v, _ := e.st.GetVolume(vol.ID)
	if v.HealthStatus != store.HealthStatusFaulted || v.StatusMessage == "" {
		t.Fatalf("volume %s %q, want FAULTED with message", v.HealthStatus, v.StatusMessage)
	}
}

func TestDegradedRaidStaysUntouched(t *testing.T) {
	e := newEnv(t)
	pool, _ := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	e.fake.RemoveMedia("0000:02:00.0")
	e.fake.Restart()
	e.reconcile()
	if e.fake.CallCount("bdev_raid_create") != 1 || e.fake.CallCount("bdev_lvol_create_lvstore") != 1 {
		t.Fatalf("reconciler mutated an incomplete array: %v", e.fake.Calls())
	}
	// The array serves nothing while it waits for the missing disk: UNAVAILABLE, naming it.
	p, _ := e.st.GetPool(pool.ID)
	if p.HealthStatus != store.HealthStatusUnavailable || !strings.Contains(p.StatusMessage, "disk-b") {
		t.Fatalf("pool %s %q, want UNAVAILABLE naming disk-b", p.HealthStatus, p.StatusMessage)
	}
	if m := p.Members[1]; m.State != store.MemberStateMissing || m.DiskID != "disk-b" {
		t.Fatalf("slot 1: %+v, want MISSING disk-b", m)
	}
}

func TestExportsRestoredWithACLsAfterRestart(t *testing.T) {
	e := newEnv(t)
	_, vol := e.provision(store.RaidLevel0, "0000:01:00.0")
	const hostNQN = "nqn.2014-08.org.nvmexpress:uuid:host-a"
	const hostIQN = "iqn.1994-05.com.redhat:host-a"
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.st.CreatePortal(&store.Portal{ID: "pt-n", Name: "n", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "fd00::10", Port: 4420}))
	must(e.st.CreatePortal(&store.Portal{ID: "pt-i", Name: "i", Protocol: store.ExportProtocolISCSI, IPAddress: "10.0.0.10", Port: 3260}))
	must(e.st.CreateInitiatorGroup(&store.InitiatorGroup{ID: "ig-1", Name: "g", HostNQNs: []string{hostNQN}, InitiatorIQNs: []string{hostIQN}}))
	must(e.st.CreateExport(&store.Export{ID: "exp-n", Name: "n", VolumeID: vol.ID, Protocol: store.ExportProtocolNVMeoFTCP,
		SubsystemNQN: "nqn.2026-11.io.velocor:n", NamespaceID: 1, PortalIDs: []string{"pt-n"}, InitiatorGroupIDs: []string{"ig-1"}}))
	must(e.st.CreateExport(&store.Export{ID: "exp-i", Name: "i", VolumeID: vol.ID, Protocol: store.ExportProtocolISCSI,
		TargetIQN: "iqn.2026-11.io.velocor:i", PortalIDs: []string{"pt-i"}, InitiatorGroupIDs: []string{"ig-1"}}))

	e.fake.Restart()
	st := e.reconcile()

	sub, ok := e.fake.Subsystem("nqn.2026-11.io.velocor:n")
	if !ok {
		t.Fatalf("nvmf subsystem not restored: %+v", st)
	}
	if sub.AllowAnyHost || len(sub.Hosts) != 1 || sub.Hosts[0].Nqn != hostNQN {
		t.Fatalf("nvmf ACL wrong: %+v", sub)
	}
	if len(sub.Namespaces) != 1 || sub.Namespaces[0].UUID != vol.UUID {
		t.Fatalf("namespace must carry the lvol UUID: %+v", sub.Namespaces)
	}
	if len(sub.ListenAddresses) != 1 || sub.ListenAddresses[0].Adrfam != "IPv6" {
		t.Fatalf("IPv6 listener wrong: %+v", sub.ListenAddresses)
	}

	exp, _ := e.st.GetExport("exp-i")
	node, ok := e.fake.TargetNode("iqn.2026-11.io.velocor:i")
	if !ok {
		t.Fatalf("iscsi target not restored: %+v", st)
	}
	if len(node.PgIgMaps) != 1 || node.PgIgMaps[0].IgTag != exp.IscsiIgTag {
		t.Fatalf("iscsi pg/ig maps wrong: %+v (export ig tag %d)", node.PgIgMaps, exp.IscsiIgTag)
	}
	ig, _ := e.fake.InitiatorGroup(exp.IscsiIgTag)
	if slices.Contains(ig.Initiators, "ANY") || !slices.Contains(ig.Initiators, hostIQN) {
		t.Fatalf("iscsi initiator group must be restricted to %s: %+v", hostIQN, ig)
	}

	// A second pass is a no-op: no creates and no store writes needed.
	before := len(e.fake.Calls())
	e.reconcile()
	for _, c := range e.fake.Calls()[before:] {
		if c != "spdk_get_version" && c != "bdev_wait_for_examine" && !slices.Contains([]string{
			"bdev_get_bdevs", "bdev_raid_get_bdevs", "bdev_lvol_get_lvstores", "bdev_nvme_get_controller_health_info", "framework_get_config", "bdev_lvol_get_lvols",
			"nvmf_get_transports", "nvmf_get_subsystems", "iscsi_get_portal_groups", "iscsi_get_initiator_groups", "iscsi_get_target_nodes",
			"bdev_nvme_send_cmd", // NVMe write cache reads (Identify, Get Features); the drives here have no cache to set
		}, c) {
			t.Fatalf("steady-state pass issued mutating call %s", c)
		}
	}
}

func TestIscsiExportRefusesImplicitAnyAccess(t *testing.T) {
	e := newEnv(t)
	_, vol := e.provision(store.RaidLevel0, "0000:01:00.0")
	if err := e.st.CreatePortal(&store.Portal{ID: "pt-i", Name: "i", Protocol: store.ExportProtocolISCSI, IPAddress: "10.0.0.10", Port: 3260}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.CreateExport(&store.Export{ID: "exp-i", Name: "i", VolumeID: vol.ID, Protocol: store.ExportProtocolISCSI,
		TargetIQN: "iqn.2026-11.io.velocor:i", PortalIDs: []string{"pt-i"}}); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if _, ok := e.fake.TargetNode("iqn.2026-11.io.velocor:i"); ok {
		t.Fatalf("iscsi target with no initiators must not be exposed")
	}
	exp, _ := e.st.GetExport("exp-i")
	if exp.HealthStatus != store.HealthStatusDegraded {
		t.Fatalf("export health %s, want DEGRADED", exp.HealthStatus)
	}
}

// SSD (io_uring/AIO) members must get a stable bdev UUID, or SPDK won't re-assemble their
// superblock RAID after a restart (found against real SPDK by internal/integration).
func TestSSDPoolReassemblesAfterRestart(t *testing.T) {
	e := newEnv(t)
	c := e.fake.Client()
	var ids, bdevs []string
	for _, f := range []string{"/dev/disk/by-id/wwn-a", "/dev/disk/by-id/wwn-b"} {
		e.fake.AddFile(f)
		d := &store.Disk{ID: "ssd-" + f[len(f)-1:], Name: f, Type: store.DiskTypeSSD, DevicePath: f, SizeBytes: 1 << 40}
		if err := e.st.UpsertDisk(d); err != nil {
			t.Fatal(err)
		}
		bdev, drv, err := spdkplane.AttachDisk(e.ctx, c, nil, d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.ClaimDisk(d.ID, drv, bdev); err != nil {
			t.Fatal(err)
		}
		ids, bdevs = append(ids, d.ID), append(bdevs, bdev)
	}
	pool := &store.Pool{ID: "pool-ssd", Name: "ssd", RaidLevel: store.RaidLevel1, DiskIDs: ids}
	if err := e.st.CreatePool(pool); err != nil {
		t.Fatal(err)
	}
	if err := c.BdevRaidCreate(e.ctx, pool.ArrayName, "raid1", 0, bdevs, true); err != nil {
		t.Fatal(err)
	}
	uuid, err := c.BdevLvolCreateLvstore(e.ctx, pool.Device, pool.ThinPoolName, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.MutatePool(pool.ID, func(p *store.Pool) error { p.ThinPoolUUID = uuid; return nil }); err != nil {
		t.Fatal(err)
	}

	e.fake.Restart()
	e.reconcile()
	p, _ := e.st.GetPool(pool.ID)
	if p.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("SSD pool did not come back after restart: %s %s", p.HealthStatus, p.StatusMessage)
	}
}
