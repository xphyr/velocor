package spdktest

import (
	"context"
	"testing"

	"github.com/velocor/velocor/internal/spdk"
)

// raidState returns the named RAID bdev as reported by bdev_raid_get_bdevs.
func raidState(t *testing.T, c *spdk.Client, name string) spdk.RaidBdevInfo {
	t.Helper()
	raids, err := c.BdevRaidGetBdevs(context.Background(), "all")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range raids {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("raid %s not found", name)
	return spdk.RaidBdevInfo{}
}

// mirror creates a 2-way raid1 with a superblock over /dev/a and /dev/b plus a spare /dev/c.
func mirror(t *testing.T) (*Fake, *spdk.Client) {
	t.Helper()
	f := New(t)
	c := f.Client()
	ctx := context.Background()
	for i, dev := range []string{"/dev/a", "/dev/b", "/dev/c"} {
		f.AddFile(dev)
		if _, err := c.BdevUringCreate(ctx, "d"+string(rune('a'+i)), dev, 512, "uuid-"+dev); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.BdevRaidCreate(ctx, "r1", "raid1", 0, []string{"da", "db"}, true); err != nil {
		t.Fatal(err)
	}
	return f, c
}

// attach re-creates the base bdevs after a restart, as velod's reconciler does.
func attach(t *testing.T, c *spdk.Client, devs map[string]string) {
	t.Helper()
	for name, dev := range devs {
		if _, err := c.BdevUringCreate(context.Background(), name, dev, 512, "uuid-"+dev); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRaidFailureRebuildAndRestart(t *testing.T) {
	f, c := mirror(t)
	ctx := context.Background()

	f.FailMedia("/dev/b")
	r := raidState(t, c, "r1")
	if r.State != "online" || r.NumBaseBdevsOperational != 1 || r.NumBaseBdevs != 2 {
		t.Fatalf("after a member failure: %+v, want online with 1/2 operational", r)
	}

	// Replace: the rebuild runs, then the new member is recorded.
	if err := c.BdevRaidAddBaseBdev(ctx, "r1", "dc"); err != nil {
		t.Fatal(err)
	}
	r = raidState(t, c, "r1")
	if r.Process == nil || r.Process.Target != "dc" || r.NumBaseBdevsOperational != 2 {
		t.Fatalf("rebuild not running: %+v", r)
	}
	if err := c.BdevRaidAddBaseBdev(ctx, "r1", "dc"); err == nil {
		t.Fatal("a second add during a rebuild must be refused")
	}

	// Restart mid-rebuild: back degraded on the old member; the target isn't a member yet.
	f.SetRebuildProgress("r1", 40)
	f.Restart()
	attach(t, c, map[string]string{"da": "/dev/a", "dc": "/dev/c"})
	r = raidState(t, c, "r1")
	if r.State != "online" || r.NumBaseBdevsOperational != 1 || r.Process != nil {
		t.Fatalf("after restart mid-rebuild: %+v, want online degraded, no process", r)
	}

	// Re-add and complete.
	if err := c.BdevRaidAddBaseBdev(ctx, "r1", "dc"); err != nil {
		t.Fatal(err)
	}
	f.SetRebuildProgress("r1", 100)
	r = raidState(t, c, "r1")
	if r.Process != nil || r.NumBaseBdevsOperational != 2 {
		t.Fatalf("after the rebuild: %+v", r)
	}

	// A full restart assembles both members.
	f.Restart()
	attach(t, c, map[string]string{"da": "/dev/a", "dc": "/dev/c"})
	if r = raidState(t, c, "r1"); r.State != "online" || r.NumBaseBdevsOperational != 2 {
		t.Fatalf("after restart: %+v", r)
	}
}

func TestRaidMissingAtBootAndStartDegraded(t *testing.T) {
	f, c := mirror(t)
	ctx := context.Background()

	// /dev/b dies while spdk_tgt is down: the array waits for it.
	f.Restart()
	f.RemoveMedia("/dev/b")
	attach(t, c, map[string]string{"da": "/dev/a"})
	r := raidState(t, c, "r1")
	if r.State != "configuring" || r.NumBaseBdevsDiscovered != 1 || r.NumBaseBdevsOperational != 2 {
		t.Fatalf("missing at boot: %+v, want configuring 1/2", r)
	}

	if err := c.BdevRaidStartDegraded(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if r = raidState(t, c, "r1"); r.State != "online" || r.NumBaseBdevsOperational != 1 {
		t.Fatalf("after start-degraded: %+v", r)
	}
	if err := c.BdevRaidStartDegraded(ctx, "r1"); err == nil {
		t.Fatal("start-degraded of an online array must be refused")
	}

	// The old disk comes back: older superblock, not taken back.
	f.AddFile("/dev/b")
	attach(t, c, map[string]string{"db": "/dev/b"})
	if r = raidState(t, c, "r1"); r.NumBaseBdevsOperational != 1 || r.BaseBdevsList[1].Name != "" {
		t.Fatalf("stale member was taken back: %+v", r)
	}

	// Its slot can be filled by a new disk.
	attach(t, c, map[string]string{"dc": "/dev/c"})
	if err := c.BdevRaidAddBaseBdev(ctx, "r1", "dc"); err != nil {
		t.Fatal(err)
	}
	f.SetRebuildProgress("r1", 100)
	if r = raidState(t, c, "r1"); r.NumBaseBdevsOperational != 2 {
		t.Fatalf("after replacing: %+v", r)
	}
}

func TestRaidStartDegradedNeedsEnoughMembers(t *testing.T) {
	f, c := mirror(t)
	f.Restart()
	if err := c.BdevRaidStartDegraded(context.Background(), "r1"); err == nil {
		t.Fatal("start-degraded with no array must fail")
	}
	attach(t, c, map[string]string{"da": "/dev/a", "db": "/dev/b"})
	if err := c.BdevRaidStartDegraded(context.Background(), "r1"); err == nil {
		t.Fatal("start-degraded of a complete (online) array must fail")
	}
}

func TestRaidSecondFailureTakesArrayOffline(t *testing.T) {
	f, c := mirror(t)
	f.FailMedia("/dev/a")
	f.FailMedia("/dev/b")
	if r := raidState(t, c, "r1"); r.State != "offline" || r.NumBaseBdevsOperational != 0 {
		t.Fatalf("after losing both members: %+v, want offline", r)
	}
}
