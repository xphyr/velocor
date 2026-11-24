package reconciler

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/dataplane/planetest"
	"github.com/velocor/velocor/internal/store"
)

type env struct {
	t    *testing.T
	ctx  context.Context
	fake *planetest.Fake
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
	f := planetest.New(t)
	return &env{t: t, ctx: context.Background(), fake: f, st: st, rec: New(st, f, nil, nil)}
}

// addDisk adds a disk to the machine and claims it, the way the API does.
func (e *env) addDisk(d *store.Disk) *store.Disk {
	e.t.Helper()
	key := d.DevicePath
	if d.Type == store.DiskTypeNVMe {
		key = d.PCIAddress
	}
	e.fake.AddDisk(key)
	if err := e.st.UpsertDisk(d); err != nil {
		e.t.Fatal(err)
	}
	handle, drv, err := e.fake.AttachDisk(e.ctx, d)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := e.st.ClaimDisk(d.ID, drv, handle)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// provision builds a pool and a volume on NVMe drives through the data plane, exactly the way the
// API does, and records them in the store. It returns the pool and volume.
func (e *env) provision(level store.RaidLevel, pcis ...string) (*store.Pool, *store.Volume) {
	e.t.Helper()
	var ids []string
	var handles []string
	for i, pci := range pcis {
		d := e.addDisk(&store.Disk{ID: "disk-" + string(rune('a'+i)), Name: "nvme", Type: store.DiskTypeNVMe, PCIAddress: pci, SizeBytes: 1 << 40})
		ids, handles = append(ids, d.ID), append(handles, d.Handle)
	}
	return e.provisionOn("pool-1", "p1", "vol-1", level, ids, handles)
}

func (e *env) provisionOn(poolID, name, volID string, level store.RaidLevel, ids, handles []string) (*store.Pool, *store.Volume) {
	e.t.Helper()
	must := func(err error) {
		e.t.Helper()
		if err != nil {
			e.t.Fatal(err)
		}
	}
	pool := &store.Pool{ID: poolID, Name: name, RaidLevel: level, DiskIDs: ids, StripSizeKB: 64}
	must(e.st.CreatePool(pool))
	must(e.fake.CreateArray(e.ctx, pool.ArrayName, level, 64, handles))
	tp, err := e.fake.CreateThinPool(e.ctx, pool.ArrayName, pool.ThinPoolName)
	must(err)
	_, err = e.st.MutatePool(pool.ID, func(p *store.Pool) error { p.ThinPoolUUID = tp; return nil })
	must(err)
	vol := &store.Volume{ID: volID, Name: volID, PoolID: pool.ID, SizeBytes: 1 << 30}
	must(e.st.CreateVolume(vol))
	uuid, err := e.fake.CreateVolume(e.ctx, pool.ThinPoolName, vol.Name, vol.SizeBytes, true)
	must(err)
	_, err = e.st.MutateVolume(vol.ID, func(v *store.Volume) error { v.UUID = uuid; return nil })
	must(err)
	p, _ := e.st.GetPool(pool.ID)
	v, _ := e.st.GetVolume(vol.ID)
	return p, v
}

func (e *env) reconcile() State {
	e.t.Helper()
	st, _ := e.rec.ReconcileOnce(e.ctx)
	return st
}

func TestRebootBringsEverythingBackWithoutRecreatingIt(t *testing.T) {
	e := newEnv(t)
	pool, vol := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	before := len(e.fake.Calls())

	e.fake.Reboot()
	st := e.reconcile()

	if gen := e.fake.PoolGeneration(pool.ThinPoolName); gen != 1 {
		t.Fatalf("thin pool created again (generation %d): pool data destroyed", gen)
	}
	for _, c := range e.fake.Calls()[before:] {
		if slices.Contains([]string{"CreateArray", "CreateThinPool", "CreateVolume", "DeleteArray", "DeleteThinPool", "DeleteVolume"}, c) {
			t.Fatalf("reconciler issued %s instead of letting the data plane settle: %v", c, e.fake.Calls()[before:])
		}
	}
	if !e.fake.VolumeExists(vol.Handle) {
		t.Fatal("volume lost")
	}
	p, _ := e.st.GetPool(pool.ID)
	v, _ := e.st.GetVolume(vol.ID)
	if p.HealthStatus != store.HealthStatusOnline || v.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("pool=%s (%s) vol=%s (%s); reconciler state %+v", p.HealthStatus, p.StatusMessage, v.HealthStatus, v.StatusMessage, st)
	}
}

// Only disks velod has claimed take part: an array on an unclaimed disk is left alone.
func TestRebootAssemblesOnlyClaimedDisks(t *testing.T) {
	e := newEnv(t)
	pool, _ := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	e.fake.Reboot()
	if _, err := e.st.MutateDisk("disk-b", func(d *store.Disk) error { d.Claimed = false; return nil }); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if p, _ := e.st.GetPool(pool.ID); p.HealthStatus != store.HealthStatusUnavailable {
		t.Fatalf("pool %s %q, want UNAVAILABLE: half its members aren't velod's", p.HealthStatus, p.StatusMessage)
	}
}

func TestMissingThinPoolIsFaultedNotReinitialised(t *testing.T) {
	e := newEnv(t)
	pool, _ := e.provision(store.RaidLevel0, "0000:01:00.0")
	// Simulate the thin pool being unreadable: pretend the recorded UUID belongs to something else.
	if _, err := e.st.MutatePool(pool.ID, func(p *store.Pool) error { p.ThinPoolUUID = "deadbeef"; return nil }); err != nil {
		t.Fatal(err)
	}
	e.fake.Reboot()
	e.reconcile()
	if n := e.fake.CallCount("CreateThinPool"); n != 1 {
		t.Fatalf("reconciler initialised a thin pool over a pool with recorded data (%d calls)", n)
	}
	p, _ := e.st.GetPool(pool.ID)
	if p.HealthStatus != store.HealthStatusFaulted {
		t.Fatalf("pool health %s, want FAULTED", p.HealthStatus)
	}
}

func TestMissingVolumeIsFaultedNotRecreated(t *testing.T) {
	e := newEnv(t)
	_, vol := e.provision(store.RaidLevel0, "0000:01:00.0")
	if err := e.fake.DeleteVolume(e.ctx, vol.UUID); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if e.fake.CallCount("CreateVolume") != 1 {
		t.Fatal("reconciler re-created a blank volume for a lost one")
	}
	v, _ := e.st.GetVolume(vol.ID)
	if v.HealthStatus != store.HealthStatusFaulted || v.StatusMessage == "" {
		t.Fatalf("volume %s %q, want FAULTED with message", v.HealthStatus, v.StatusMessage)
	}
}

func TestIncompleteArrayStaysUntouched(t *testing.T) {
	e := newEnv(t)
	pool, _ := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	e.fake.Reboot()
	e.fake.RemoveDisk("0000:02:00.0")
	e.reconcile()
	if e.fake.CallCount("CreateArray") != 1 || e.fake.CallCount("CreateThinPool") != 1 || e.fake.CallCount("StartDegraded") != 0 {
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

// A pool that was degraded before the reboot comes back degraded, without an operator.
func TestDegradedPoolComesBackAfterReboot(t *testing.T) {
	e := newEnv(t)
	pool, vol := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	e.fake.RemoveDisk("0000:02:00.0")
	e.reconcile()
	e.fake.Reboot()
	e.reconcile()
	p, _ := e.st.GetPool(pool.ID)
	v, _ := e.st.GetVolume(vol.ID)
	if p.HealthStatus != store.HealthStatusDegraded || v.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("pool %s %q, volume %s, want DEGRADED and ONLINE", p.HealthStatus, p.StatusMessage, v.HealthStatus)
	}
}

// A rebuild a reboot interrupts goes on by itself.
func TestRebuildGoesOnAfterReboot(t *testing.T) {
	e := newEnv(t)
	pool, _ := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	e.fake.FailDisk("0000:02:00.0")
	e.reconcile()
	spare := e.spareDisk("disk-c", "0000:03:00.0")
	if _, err := e.st.ReplacePoolMember(pool.ID, 1, spare.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.fake.AddMember(e.ctx, pool.ArrayName, spare.Handle); err != nil {
		t.Fatal(err)
	}
	e.fake.SetRebuildProgress(pool.ArrayName, 60)
	e.reconcile()
	e.fake.Reboot()
	e.reconcile()
	p := e.pool()
	if p.HealthStatus != store.HealthStatusRebuilding || p.Members[1].State != store.MemberStateRebuilding {
		t.Fatalf("after the reboot: %s %q %+v", p.HealthStatus, p.StatusMessage, p.Members)
	}
	if n := e.fake.CallCount("AddMember"); n != 1 {
		t.Fatalf("AddMember called %d times, want once: md goes on by itself", n)
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

	e.fake.Reboot()
	st := e.reconcile()

	sub, ok := e.fake.Target("nqn.2026-11.io.velocor:n")
	if !ok {
		t.Fatalf("NVMe subsystem not restored: %+v", st)
	}
	if sub.AnyHost || !slices.Equal(sub.Hosts, []string{hostNQN}) {
		t.Fatalf("NVMe ACL wrong: %+v", sub)
	}
	if sub.Device != vol.UUID {
		t.Fatalf("subsystem exports %s, want the volume %s", sub.Device, vol.UUID)
	}
	if !slices.Equal(sub.Listeners, []string{"fd00::10:4420"}) {
		t.Fatalf("IPv6 listener wrong: %+v", sub.Listeners)
	}
	tgt, ok := e.fake.Target("iqn.2026-11.io.velocor:i")
	if !ok {
		t.Fatalf("iSCSI target not restored: %+v", st)
	}
	if tgt.AnyHost || !slices.Equal(tgt.Hosts, []string{hostIQN}) {
		t.Fatalf("iSCSI ACL must be restricted to %s: %+v", hostIQN, tgt)
	}

	// A second pass is a no-op: nothing changed in the data plane.
	before := len(e.fake.Calls())
	e.reconcile()
	if c := e.fake.Calls()[before:]; len(c) != 0 {
		t.Fatalf("a steady-state pass changed things: %v", c)
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
	if _, ok := e.fake.Target("iqn.2026-11.io.velocor:i"); ok {
		t.Fatalf("iscsi target with no initiators must not be exposed")
	}
	exp, _ := e.st.GetExport("exp-i")
	if exp.HealthStatus != store.HealthStatusDegraded {
		t.Fatalf("export health %s, want DEGRADED", exp.HealthStatus)
	}
}

func TestSSDPoolReassemblesAfterReboot(t *testing.T) {
	e := newEnv(t)
	pool := e.ssdPool(store.RaidLevel1, "/dev/disk/by-id/wwn-a", "/dev/disk/by-id/wwn-b")
	e.fake.Reboot()
	e.reconcile()
	p, _ := e.st.GetPool(pool.ID)
	if p.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("SSD pool did not come back after a reboot: %s %s", p.HealthStatus, p.StatusMessage)
	}
}
