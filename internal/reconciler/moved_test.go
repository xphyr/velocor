package reconciler

import (
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/store"
)

// ssdPool builds a pool of SSDs at the given device paths, the way the API does, with a volume.
func (e *env) ssdPool(level store.RaidLevel, paths ...string) *store.Pool {
	e.t.Helper()
	c := e.fake.Client()
	var ids, bdevs []string
	for _, path := range paths {
		e.fake.AddFile(path)
		d := &store.Disk{ID: "ssd-" + path[strings.LastIndex(path, "/")+1:], Name: "sd", Type: store.DiskTypeSSD,
			DevicePath: path, SizeBytes: 1 << 40, BlockSize: 512, WriteCachePolicy: store.WriteCachePolicyDisabled}
		if err := e.st.UpsertDisk(d); err != nil {
			e.t.Fatal(err)
		}
		bdev, drv, err := dataplane.AttachDisk(e.ctx, c, nil, d)
		if err != nil {
			e.t.Fatal(err)
		}
		if _, err := e.st.ClaimDisk(d.ID, drv, bdev); err != nil {
			e.t.Fatal(err)
		}
		ids, bdevs = append(ids, d.ID), append(bdevs, bdev)
	}
	pool := &store.Pool{ID: "pool-ssd", Name: "ssd", RaidLevel: level, DiskIDs: ids, StripSizeKB: 64}
	if err := e.st.CreatePool(pool); err != nil {
		e.t.Fatal(err)
	}
	if err := c.BdevRaidCreate(e.ctx, pool.BaseBdevName, strings.ToLower(string(level)), 64, bdevs, true); err != nil {
		e.t.Fatal(err)
	}
	lvs, err := c.BdevLvolCreateLvstore(e.ctx, pool.EffectiveBdevName, pool.LVSName, 0, "")
	if err != nil {
		e.t.Fatal(err)
	}
	vol := &store.Volume{ID: "vol-ssd", Name: "vol-ssd", PoolID: pool.ID, SizeBytes: 1 << 30}
	if err := e.st.CreateVolume(vol); err != nil {
		e.t.Fatal(err)
	}
	uuid, err := c.BdevLvolCreate(e.ctx, pool.LVSName, vol.Name, vol.SizeBytes, false, "")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.MutateVolume(vol.ID, func(v *store.Volume) error { v.UUID = uuid; return nil }); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.MutatePool(pool.ID, func(p *store.Pool) error { p.LVSUUID = lvs; return nil }); err != nil {
		e.t.Fatal(err)
	}
	e.reconcile()
	if p, _ := e.st.GetPool(pool.ID); p.HealthStatus != store.HealthStatusOnline {
		e.t.Fatalf("new pool: %s %q", p.HealthStatus, p.StatusMessage)
	}
	return pool
}

// scanFinds makes the reconciler's hardware scan report SSDs at these paths (ID ssd-<base>)
// and read their superblocks from the fake's media.
func (e *env) scanFinds(paths ...string) {
	e.rec.discover = func() ([]*store.Disk, error) {
		var out []*store.Disk
		for _, path := range paths {
			out = append(out, &store.Disk{ID: "ssd-" + path[strings.LastIndex(path, "/")+1:], Name: "sd", Type: store.DiskTypeSSD,
				DevicePath: path, SizeBytes: 1 << 40, BlockSize: 512, HealthStatus: store.HealthStatusOnline})
		}
		return out, nil
	}
	e.rec.sbReader = func(d *store.Disk) (*hardware.RaidSuperblock, error) { return e.fake.SuperblockOf(d.DevicePath), nil }
}

func TestMirrorUnderNewDiskIDsComesBack(t *testing.T) {
	e := newEnv(t)
	pool := e.ssdPool(store.RaidLevel1, "/dev/disk/by-id/ata-a", "/dev/disk/by-id/ata-b")
	old := map[string]string{}
	for _, id := range pool.DiskIDs {
		d, _ := e.st.GetDisk(id)
		old[id] = dataplane.DiskBdevUUID(d)
	}

	// Both disks move to another controller: new paths, new IDs, same contents.
	e.fake.Restart()
	e.fake.MoveMedia("/dev/disk/by-id/ata-a", "/dev/disk/by-id/wwn-x")
	e.fake.MoveMedia("/dev/disk/by-id/ata-b", "/dev/disk/by-id/wwn-y")
	e.scanFinds("/dev/disk/by-id/wwn-x", "/dev/disk/by-id/wwn-y")
	e.reconcile()
	e.reconcile() // SPDK examine assembles once both are attached

	p, _ := e.st.GetPool(pool.ID)
	if p.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("pool after the move: %s %q", p.HealthStatus, p.StatusMessage)
	}
	if strings.Join(p.DiskIDs, ",") != "ssd-wwn-x,ssd-wwn-y" {
		t.Fatalf("members %v, want the new IDs", p.DiskIDs)
	}
	got := map[string]bool{}
	for _, id := range p.DiskIDs {
		d, err := e.st.GetDisk(id)
		if err != nil {
			t.Fatal(err)
		}
		if !d.Claimed || d.PoolID != pool.ID || d.WriteCachePolicy != store.WriteCachePolicyDisabled {
			t.Fatalf("new disk %s: %+v", id, d)
		}
		got[d.BdevUUID] = true
	}
	for id, u := range old {
		if !got[u] {
			t.Fatalf("no new disk kept member UUID %s", u)
		}
		if _, err := e.st.GetDisk(id); err == nil {
			t.Fatalf("old record %s still there", id)
		}
	}
	if v, _ := e.st.GetVolume("vol-ssd"); v.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("volume: %s %q", v.HealthStatus, v.StatusMessage)
	}

	// The kept UUID survives the next restart and scan.
	e.fake.Restart()
	e.reconcile()
	e.reconcile()
	if p, _ := e.st.GetPool(pool.ID); p.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("after another restart: %s %q", p.HealthStatus, p.StatusMessage)
	}
}

func TestOneMovedMirrorMemberComesBack(t *testing.T) {
	e := newEnv(t)
	pool := e.ssdPool(store.RaidLevel1, "/dev/disk/by-id/ata-a", "/dev/disk/by-id/ata-b")
	e.fake.Restart()
	e.fake.MoveMedia("/dev/disk/by-id/ata-b", "/dev/disk/by-id/wwn-y")
	e.scanFinds("/dev/disk/by-id/ata-a", "/dev/disk/by-id/wwn-y")
	e.reconcile()
	e.reconcile()
	p, _ := e.st.GetPool(pool.ID)
	if p.HealthStatus != store.HealthStatusOnline || strings.Join(p.DiskIDs, ",") != "ssd-ata-a,ssd-wwn-y" {
		t.Fatalf("pool: %s %q %v", p.HealthStatus, p.StatusMessage, p.DiskIDs)
	}
}

func TestMovedStripeIsNotGuessed(t *testing.T) {
	e := newEnv(t)
	pool := e.ssdPool(store.RaidLevel0, "/dev/disk/by-id/ata-a", "/dev/disk/by-id/ata-b")
	e.fake.Restart()
	e.fake.MoveMedia("/dev/disk/by-id/ata-a", "/dev/disk/by-id/wwn-x")
	e.fake.MoveMedia("/dev/disk/by-id/ata-b", "/dev/disk/by-id/wwn-y")
	e.scanFinds("/dev/disk/by-id/wwn-x", "/dev/disk/by-id/wwn-y")
	e.reconcile()
	p, _ := e.st.GetPool(pool.ID)
	if p.HealthStatus != store.HealthStatusUnavailable || !strings.Contains(p.StatusMessage, "can't tell which is which") {
		t.Fatalf("pool: %s %q", p.HealthStatus, p.StatusMessage)
	}
	for _, id := range []string{"ssd-wwn-x", "ssd-wwn-y"} {
		if d, _ := e.st.GetDisk(id); d.Claimed || d.PoolID != "" {
			t.Fatalf("disk %s was taken: %+v", id, d)
		}
	}
}

func TestMovedMirrorWaitsForEveryMember(t *testing.T) {
	e := newEnv(t)
	pool := e.ssdPool(store.RaidLevel1, "/dev/disk/by-id/ata-a", "/dev/disk/by-id/ata-b")
	e.fake.Restart()
	e.fake.MoveMedia("/dev/disk/by-id/ata-a", "/dev/disk/by-id/wwn-x")
	e.fake.RemoveMedia("/dev/disk/by-id/ata-b")
	e.scanFinds("/dev/disk/by-id/wwn-x")
	e.reconcile()
	if d, _ := e.st.GetDisk("ssd-wwn-x"); d.Claimed {
		t.Fatal("one of two moved members was relinked before the other came back")
	}
	if p, _ := e.st.GetPool(pool.ID); p.HealthStatus != store.HealthStatusUnavailable {
		t.Fatalf("pool: %s %q", p.HealthStatus, p.StatusMessage)
	}
}
