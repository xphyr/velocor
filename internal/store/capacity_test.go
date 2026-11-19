package store

import (
	"errors"
	"strings"
	"testing"
)

const gib = uint64(1) << 30

func TestSpacePolicyDefaultsAndValidation(t *testing.T) {
	e := SpacePolicy{}.Effective()
	if e.MaxOvercommit != 1 || *e.ReservePercent != 5 || e.WarningPercent != 80 || e.CriticalPercent != 90 {
		t.Fatalf("defaults: %+v reserve %v", e, *e.ReservePercent)
	}
	zero := 0.0
	if e := (SpacePolicy{ReservePercent: &zero}).Effective(); *e.ReservePercent != 0 {
		t.Fatalf("an explicit 0%% reserve must stay 0")
	}
	bad := []SpacePolicy{{MaxOvercommit: 0.01}, {MaxOvercommit: 1000}, {WarningPercent: 95, CriticalPercent: 90}, {CriticalPercent: 120}}
	r60 := 60.0
	bad = append(bad, SpacePolicy{ReservePercent: &r60})
	for _, sp := range bad {
		if err := sp.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%+v: %v, want invalid", sp, err)
		}
	}
	if err := (SpacePolicy{MaxOvercommit: 2, WarningPercent: 70, CriticalPercent: 85}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedCountsVolumesAndSnapshotHoldings(t *testing.T) {
	vols := []*Volume{{SizeBytes: 4 * gib}, {SizeBytes: 2 * gib, ParentSnapshotID: "s1"}} // a volume and a clone
	snaps := []*Snapshot{{SizeBytes: 4 * gib, AllocatedBytes: 1 * gib}}
	if k := CommittedBytes(vols, snaps); k != 7*gib {
		t.Fatalf("committed %d, want 7 GiB", k)
	}
}

func TestGrowthAdmission(t *testing.T) {
	p := &Pool{Name: "p", TotalBytes: 100 * gib, FreeBytes: 100 * gib} // defaults: 1.0x, 5% reserve
	if LimitBytes(p) != 95*gib {
		t.Fatalf("limit %d", LimitBytes(p))
	}
	if err := CheckGrowth(p, 90*gib, 5*gib); err != nil {
		t.Fatalf("exactly at the limit must be allowed: %v", err)
	}
	err := CheckGrowth(p, 90*gib, 6*gib)
	if !errors.Is(err, ErrInsufficientCapacity) || !strings.Contains(err.Error(), "committed") {
		t.Fatalf("over the limit: %v", err)
	}
	if AvailableBytes(p, 90*gib) != 5*gib {
		t.Fatalf("available %d", AvailableBytes(p, 90*gib))
	}

	// Overcommit allowed on purpose.
	p.SpacePolicy.MaxOvercommit = 2
	if err := CheckGrowth(p, 150*gib, 40*gib); err != nil {
		t.Fatalf("2x overcommit: %v", err)
	}

	// States by how much is really used; CRITICAL and FULL refuse any growth.
	for _, tc := range []struct {
		free  uint64
		state SpaceState
	}{{25 * gib, SpaceStateOK}, {20 * gib, SpaceStateWarning}, {10 * gib, SpaceStateCritical}, {0, SpaceStateFull}} {
		p.FreeBytes = tc.free
		if st := SpaceStateOf(p); st != tc.state {
			t.Errorf("free %d GiB: %s, want %s", tc.free/gib, st, tc.state)
		}
		growthErr := CheckGrowth(p, 0, 1)
		refused := tc.state == SpaceStateCritical || tc.state == SpaceStateFull
		if refused != (growthErr != nil) || (refused && AvailableBytes(p, 0) != 0) {
			t.Errorf("free %d GiB: growth error %v, available %d", tc.free/gib, growthErr, AvailableBytes(p, 0))
		}
	}

	// A pool without a volume store yet (creation in progress) has nothing to judge.
	if st := SpaceStateOf(&Pool{}); st != SpaceStateOK {
		t.Fatalf("empty pool: %s", st)
	}
}
