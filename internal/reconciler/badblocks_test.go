package reconciler

import (
	"context"
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
	e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	id := e.pool().ID
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
	// On every copy: DEGRADED, saying data is lost.
	bp.memberBad, bp.unreadable = map[int]uint64{0: 4096, 1: 4096}, 4096
	p = pass()
	if p.HealthStatus != store.HealthStatusDegraded || p.UnreadableBytes != 4096 || !strings.Contains(p.StatusMessage, "4.0 KiB of the pool can't be read from any disk") {
		t.Fatalf("bad blocks on every copy: %s %q %d", p.HealthStatus, p.StatusMessage, p.UnreadableBytes)
	}
	// Gone (the pool was recreated or the disks replaced): back to ONLINE, nothing recorded.
	bp.memberBad, bp.unreadable = nil, 0
	if p := pass(); p.HealthStatus != store.HealthStatusOnline || p.UnreadableBytes != 0 || p.Members[1].BadBlockBytes != 0 || p.StatusMessage != "" {
		t.Fatalf("cleared: %s %q %+v", p.HealthStatus, p.StatusMessage, p.Members)
	}
}
