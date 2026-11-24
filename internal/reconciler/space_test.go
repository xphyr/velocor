package reconciler

import (
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/store"
)

func TestSpaceAccountingAndStates(t *testing.T) {
	e := newEnv(t)
	pool, vol := e.provision(store.RaidLevel0, "0000:01:00.0")
	e.fake.SetPoolChunks(pool.ThinPoolName, 4000)
	write := func(from, to uint64) {
		for i := from; i < to; i++ {
			e.fake.WriteChunk(vol.UUID, i)
		}
	}

	e.reconcile()
	p := e.pool()
	cs := p.TotalBytes / 4000
	if p.SpaceState != store.SpaceStateOK || p.CommittedBytes != vol.SizeBytes || p.AvailableBytes == 0 {
		t.Fatalf("empty pool: state %s committed %d available %d", p.SpaceState, p.CommittedBytes, p.AvailableBytes)
	}

	for _, step := range []struct {
		upTo  uint64
		state store.SpaceState
	}{{2000, store.SpaceStateOK}, {3400, store.SpaceStateWarning}, {3680, store.SpaceStateCritical}, {4000, store.SpaceStateFull}} {
		write(0, step.upTo)
		st := e.reconcile()
		p = e.pool()
		v, _ := e.st.GetVolume(vol.ID)
		if v.AllocatedBytes != step.upTo*cs {
			t.Fatalf("%d chunks written: volume allocated %d, want %d", step.upTo, v.AllocatedBytes, step.upTo*cs)
		}
		if p.SpaceState != step.state {
			t.Fatalf("%d/4000 chunks used: space state %s, want %s", step.upTo, p.SpaceState, step.state)
		}
		flagged := strings.Contains(strings.Join(st.DegradedResources, " "), "pool:"+pool.ID)
		if flagged != (step.state != store.SpaceStateOK) {
			t.Fatalf("%s: pool flagged as needing attention: %v (%v)", step.state, flagged, st.DegradedResources)
		}
		if (step.state == store.SpaceStateCritical || step.state == store.SpaceStateFull) && p.AvailableBytes != 0 {
			t.Fatalf("%s: available %d, want 0", step.state, p.AvailableBytes)
		}
		// Space never changes the RAID health.
		if p.HealthStatus != store.HealthStatusOnline {
			t.Fatalf("%s: health %s, want ONLINE", step.state, p.HealthStatus)
		}
	}
}
