package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/store"
)

// metaPlane reports thin metadata the way the kernel data plane does, over the SPDK fake.
type metaPlane struct {
	dataplane.Plane
	total, used, grownTo uint64
	growErr              error
	grows                int
}

func (m *metaPlane) ThinPools(ctx context.Context, name, uuid string) ([]dataplane.ThinPool, error) {
	tps, err := m.Plane.ThinPools(ctx, name, uuid)
	for i := range tps {
		tps[i].MetadataBytes, tps[i].MetadataUsedBytes = m.total, m.used
	}
	return tps, err
}

func (m *metaPlane) GrowMetadata(context.Context, string) (uint64, error) {
	m.grows++
	return m.grownTo, m.growErr
}

func TestThinMetadataTracked(t *testing.T) {
	e := newEnv(t)
	e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")
	id := e.pool().ID
	mp := &metaPlane{Plane: e.rec.dp, total: 64 << 20, used: 16 << 20}
	e.rec.dp = mp

	pass := func() *store.Pool {
		t.Helper()
		e.reconcile()
		p, err := e.st.GetPool(id)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Recorded; nothing to grow.
	if p := pass(); p.ThinMetadataBytes != 64<<20 || p.ThinMetadataUsedBytes != 16<<20 || p.HealthStatus != store.HealthStatusOnline || mp.grows == 0 {
		t.Fatalf("pool %s %d/%d (grow asked %d times)", p.HealthStatus, p.ThinMetadataUsedBytes, p.ThinMetadataBytes, mp.grows)
	}
	// Grown: the new size is recorded.
	mp.grownTo = 128 << 20
	if p := pass(); p.ThinMetadataBytes != 128<<20 || p.HealthStatus != store.HealthStatusOnline {
		t.Fatalf("after growing: %s %d", p.HealthStatus, p.ThinMetadataBytes)
	}
	// Half full and no room: a note, still ONLINE.
	mp.grownTo, mp.growErr = 0, errors.New("no room to grow it")
	mp.used = 40 << 20
	if p := pass(); p.HealthStatus != store.HealthStatusOnline || !strings.Contains(p.StatusMessage, "no room") {
		t.Fatalf("half full, can't grow: %s %q", p.HealthStatus, p.StatusMessage)
	}
	// 95% full: DEGRADED, saying how full.
	mp.used = 61 << 20
	if p := pass(); p.HealthStatus != store.HealthStatusDegraded || !strings.Contains(p.StatusMessage, "95% full") {
		t.Fatalf("nearly full: %s %q", p.HealthStatus, p.StatusMessage)
	}
}
