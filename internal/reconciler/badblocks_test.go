package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/store"
)

// badBlockPlane reports bad blocks the way the kernel data plane does, over the SPDK fake.
type badBlockPlane struct {
	dataplane.Plane
	memberBad  map[int]uint64 // slot -> bytes
	unreadable uint64
	lost       []dataplane.LostExtent
	lostErr    error
}

func (b *badBlockPlane) LostData(context.Context, string) ([]dataplane.LostExtent, error) {
	return b.lost, b.lostErr
}

func (b *badBlockPlane) Arrays(ctx context.Context) ([]dataplane.Array, error) {
	as, err := b.Plane.Arrays(ctx)
	for i := range as {
		for slot := range as[i].Members {
			as[i].Members[slot].BadBlockBytes = b.memberBad[slot]
		}
		as[i].UnreadableBytes = b.unreadable
	}
	return as, err
}

func TestBadBlocksReported(t *testing.T) {
	e := newEnv(t)
	_, vol := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	id := e.pool().ID
	snap := &store.Snapshot{ID: "snap-1", Name: "nightly", VolumeID: vol.ID, PoolID: id, UUID: "5e5e5e5e-0000-4000-8000-000000000001"}
	if err := e.st.CreateSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	bp := &badBlockPlane{Plane: e.rec.dp}
	e.rec.dp = bp
	pass := func() *store.Pool {
		t.Helper()
		e.reconcile()
		p, err := e.st.GetPool(id)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := pass(); p.HealthStatus != store.HealthStatusOnline || p.UnreadableBytes != 0 || p.Members[0].BadBlockBytes != 0 {
		t.Fatalf("no bad blocks: %s %q %+v", p.HealthStatus, p.StatusMessage, p.Members)
	}
	// On one copy: still ONLINE, with a note naming the disk; recorded on the member.
	bp.memberBad = map[int]uint64{1: 4096}
	p := pass()
	if p.HealthStatus != store.HealthStatusOnline || p.Members[1].BadBlockBytes != 4096 || p.Members[0].BadBlockBytes != 0 ||
		!strings.Contains(p.StatusMessage, "disk-b has 4.0 KiB of bad blocks") {
		t.Fatalf("bad blocks on one copy: %s %q %+v", p.HealthStatus, p.StatusMessage, p.Members)
	}
	// On every copy, where the data plane can't say what it held: DEGRADED, saying so.
	bp.memberBad, bp.unreadable, bp.lostErr = map[int]uint64{0: 4096, 1: 4096}, 4096, errors.New("thin_dump failed")
	p = pass()
	if p.HealthStatus != store.HealthStatusDegraded || p.UnreadableBytes != 4096 || !strings.Contains(p.StatusMessage, "4.0 KiB of the pool can't be read from any disk") ||
		!strings.Contains(p.StatusMessage, "couldn't be worked out (thin_dump failed)") {
		t.Fatalf("bad blocks on every copy: %s %q %d", p.HealthStatus, p.StatusMessage, p.UnreadableBytes)
	}
	// Held by the volume and its snapshot (a shared block), and the configuration copy: all
	// named on the pool and recorded; the volume DEGRADED, still exported.
	bp.lostErr = nil
	bp.lost = []dataplane.LostExtent{
		{Kind: dataplane.LostVolume, UUID: vol.UUID, Offset: 3 << 20, Length: 4096},
		{Kind: dataplane.LostVolume, UUID: snap.UUID, Offset: 3 << 20, Length: 4096},
		{Kind: dataplane.LostPoolMetadata, Offset: 1 << 30, Length: 512},
	}
	p = pass()
	for _, want := range []string{"it held volume " + vol.Name + " (4.0 KiB from 3.0 MiB)", "snapshot nightly (4.0 KiB from 3.0 MiB)", "the on-pool configuration copy (512 B; rewritten on the next change)"} {
		if !strings.Contains(p.StatusMessage, want) {
			t.Errorf("pool message %q lacks %q", p.StatusMessage, want)
		}
	}
	if len(p.LostData) != 3 || p.LostData[0].VolumeID != vol.ID || p.LostData[1].SnapshotID != snap.ID || p.LostData[2].Kind != dataplane.LostPoolMetadata {
		t.Errorf("lost data recorded: %+v", p.LostData)
	}
	v, err := e.st.GetVolume(vol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.HealthStatus != store.HealthStatusDegraded || !strings.Contains(v.StatusMessage, "4.0 KiB of its data, from 3.0 MiB, can't be read") {
		t.Errorf("volume: %s %q", v.HealthStatus, v.StatusMessage)
	}
	// Gone (written again, or the disks replaced): back to ONLINE, nothing recorded.
	bp.memberBad, bp.unreadable, bp.lost = nil, 0, nil
	if p := pass(); p.HealthStatus != store.HealthStatusOnline || p.UnreadableBytes != 0 || p.LostData != nil || p.Members[1].BadBlockBytes != 0 || p.StatusMessage != "" {
		t.Fatalf("cleared: %s %q %+v", p.HealthStatus, p.StatusMessage, p.Members)
	}
	if v, _ := e.st.GetVolume(vol.ID); v.HealthStatus != store.HealthStatusOnline {
		t.Errorf("volume after: %s %q", v.HealthStatus, v.StatusMessage)
	}
}
