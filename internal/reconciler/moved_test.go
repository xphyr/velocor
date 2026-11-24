package reconciler

import (
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/store"
)

// ssdPool builds a pool of SSDs at the given device paths, the way the API does, with a volume.
func (e *env) ssdPool(level store.RaidLevel, paths ...string) *store.Pool {
	e.t.Helper()
	var ids, handles []string
	for _, path := range paths {
		d := e.addDisk(&store.Disk{ID: "ssd-" + path[strings.LastIndex(path, "/")+1:], Name: "sd", Type: store.DiskTypeSSD,
			DevicePath: path, SizeBytes: 1 << 40, BlockSize: 512, WriteCachePolicy: store.WriteCachePolicyDisabled})
		ids, handles = append(ids, d.ID), append(handles, d.Handle)
	}
	pool, _ := e.provisionOn("pool-ssd", "ssd", "vol-ssd", level, ids, handles)
	e.reconcile()
	if p, _ := e.st.GetPool(pool.ID); p.HealthStatus != store.HealthStatusOnline {
		e.t.Fatalf("new pool: %s %q", p.HealthStatus, p.StatusMessage)
	}
	return pool
}

// scanFinds makes the reconciler's hardware scan report SSDs at these paths (ID ssd-<base>).
func (e *env) scanFinds(paths ...string) {
	e.rec.discover = func() ([]*store.Disk, error) {
		var out []*store.Disk
		for _, path := range paths {
			out = append(out, &store.Disk{ID: "ssd-" + path[strings.LastIndex(path, "/")+1:], Name: "sd", Type: store.DiskTypeSSD,
				DevicePath: path, SizeBytes: 1 << 40, BlockSize: 512, HealthStatus: store.HealthStatusOnline})
		}
		return out, nil
	}
}

func TestMirrorUnderNewDiskIDsComesBack(t *testing.T) {
	e := newEnv(t)
	pool := e.ssdPool(store.RaidLevel1, "/dev/disk/by-id/ata-a", "/dev/disk/by-id/ata-b")
	old := append([]string{}, pool.DiskIDs...)

	// Both disks move to another controller: new paths, new IDs, same contents.
	e.fake.Reboot()
	e.fake.MoveDisk("/dev/disk/by-id/ata-a", "/dev/disk/by-id/wwn-x")
	e.fake.MoveDisk("/dev/disk/by-id/ata-b", "/dev/disk/by-id/wwn-y")
	e.scanFinds("/dev/disk/by-id/wwn-x", "/dev/disk/by-id/wwn-y")
	e.reconcile()
	e.reconcile() // assembles once both are claimed

	p, _ := e.st.GetPool(pool.ID)
	if p.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("pool after the move: %s %q", p.HealthStatus, p.StatusMessage)
	}
	if strings.Join(p.DiskIDs, ",") != "ssd-wwn-x,ssd-wwn-y" {
		t.Fatalf("members %v, want the new IDs", p.DiskIDs)
	}
	for _, id := range p.DiskIDs {
		d, err := e.st.GetDisk(id)
		if err != nil {
			t.Fatal(err)
		}
		if !d.Claimed || d.PoolID != pool.ID || d.WriteCachePolicy != store.WriteCachePolicyDisabled {
			t.Fatalf("new disk %s: %+v", id, d)
		}
	}
	for _, id := range old {
		if _, err := e.st.GetDisk(id); err == nil {
			t.Fatalf("old record %s still there", id)
		}
	}
	if v, _ := e.st.GetVolume("vol-ssd"); v.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("volume: %s %q", v.HealthStatus, v.StatusMessage)
	}

	// And again after another reboot and scan.
	e.fake.Reboot()
	e.reconcile()
	e.reconcile()
	if p, _ := e.st.GetPool(pool.ID); p.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("after another reboot: %s %q", p.HealthStatus, p.StatusMessage)
	}
}

func TestOneMovedMirrorMemberComesBack(t *testing.T) {
	e := newEnv(t)
	pool := e.ssdPool(store.RaidLevel1, "/dev/disk/by-id/ata-a", "/dev/disk/by-id/ata-b")
	e.fake.Reboot()
	e.fake.MoveDisk("/dev/disk/by-id/ata-b", "/dev/disk/by-id/wwn-y")
	e.scanFinds("/dev/disk/by-id/ata-a", "/dev/disk/by-id/wwn-y")
	e.reconcile()
	e.reconcile()
	p, _ := e.st.GetPool(pool.ID)
	if p.HealthStatus != store.HealthStatusOnline || strings.Join(p.DiskIDs, ",") != "ssd-ata-a,ssd-wwn-y" {
		t.Fatalf("pool: %s %q %v", p.HealthStatus, p.StatusMessage, p.DiskIDs)
	}
}

// md's metadata names each member's slot, so a stripe whose disks all moved comes back too.
func TestMovedStripeComesBack(t *testing.T) {
	e := newEnv(t)
	pool := e.ssdPool(store.RaidLevel0, "/dev/disk/by-id/ata-a", "/dev/disk/by-id/ata-b")
	e.fake.Reboot()
	e.fake.MoveDisk("/dev/disk/by-id/ata-a", "/dev/disk/by-id/wwn-x")
	e.fake.MoveDisk("/dev/disk/by-id/ata-b", "/dev/disk/by-id/wwn-y")
	e.scanFinds("/dev/disk/by-id/wwn-x", "/dev/disk/by-id/wwn-y")
	e.reconcile()
	e.reconcile()
	p, _ := e.st.GetPool(pool.ID)
	if p.HealthStatus != store.HealthStatusOnline || strings.Join(p.DiskIDs, ",") != "ssd-wwn-x,ssd-wwn-y" {
		t.Fatalf("pool: %s %q %v", p.HealthStatus, p.StatusMessage, p.DiskIDs)
	}
}

// One of two moved members back: relinked (its metadata says which it is), but the array waits for
// the other, which may hold the newer copy.
func TestMovedMirrorWaitsForEveryMember(t *testing.T) {
	e := newEnv(t)
	pool := e.ssdPool(store.RaidLevel1, "/dev/disk/by-id/ata-a", "/dev/disk/by-id/ata-b")
	e.fake.Reboot()
	e.fake.MoveDisk("/dev/disk/by-id/ata-a", "/dev/disk/by-id/wwn-x")
	e.fake.RemoveDisk("/dev/disk/by-id/ata-b")
	e.scanFinds("/dev/disk/by-id/wwn-x")
	e.reconcile()
	e.reconcile()
	if p, _ := e.st.GetPool(pool.ID); p.HealthStatus != store.HealthStatusUnavailable {
		t.Fatalf("pool: %s %q", p.HealthStatus, p.StatusMessage)
	}
	if n := e.fake.CallCount("StartDegraded"); n != 0 {
		t.Fatal("started without the missing member")
	}
}

// A disk that left the array earlier and turns up under a new ID isn't taken back in: the member
// still in place has newer metadata.
func TestMovedStaleMemberIsNotRelinked(t *testing.T) {
	e := newEnv(t)
	pool := e.ssdPool(store.RaidLevel1, "/dev/disk/by-id/ata-a", "/dev/disk/by-id/ata-b")
	e.fake.RemoveDisk("/dev/disk/by-id/ata-b") // pulled while running: the array goes on without it
	e.reconcile()
	e.fake.Reboot()
	e.fake.MoveDisk("/dev/disk/by-id/ata-b", "/dev/disk/by-id/wwn-y")
	e.fake.AddDisk("/dev/disk/by-id/wwn-y")
	e.scanFinds("/dev/disk/by-id/ata-a", "/dev/disk/by-id/wwn-y")
	e.reconcile()
	e.reconcile()
	p, _ := e.st.GetPool(pool.ID)
	if p.HealthStatus != store.HealthStatusDegraded {
		t.Fatalf("pool: %s %q", p.HealthStatus, p.StatusMessage)
	}
	if d, err := e.st.GetDisk("ssd-wwn-y"); err != nil || d.Claimed || d.PoolID != "" {
		t.Fatalf("the stale disk was taken back: %+v %v", d, err)
	}
}
