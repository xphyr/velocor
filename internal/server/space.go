package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/velocor/velocor/internal/store"
)

// Admission control for pool space (TODO P0-2; docs/design/p0-2-capacity.md). Every operation
// that commits space reserves it first. A reservation counts on top of the store until the
// object it is for appears there (recorded), so concurrent requests can't overcommit together,
// and a finished request isn't counted twice while its handler is still returning.

type reservation struct {
	pool     string
	delta    uint64
	recorded func(volumes []*store.Volume, snapshots []*store.Snapshot) bool
}

type spaceReservations struct {
	mu      sync.Mutex
	pending map[*reservation]struct{}
}

// volumeRecorded: the volume with this ID is in the store with at least this size.
func volumeRecorded(id string, size uint64) func([]*store.Volume, []*store.Snapshot) bool {
	return func(vols []*store.Volume, _ []*store.Snapshot) bool {
		for _, v := range vols {
			if v.ID == id && v.SizeBytes >= size {
				return true
			}
		}
		return false
	}
}

// snapshotRecorded: the snapshot with this ID is in the store.
func snapshotRecorded(id string) func([]*store.Volume, []*store.Snapshot) bool {
	return func(_ []*store.Volume, snaps []*store.Snapshot) bool {
		for _, s := range snaps {
			if s.ID == id {
				return true
			}
		}
		return false
	}
}

// reserveSpace admits delta more committed bytes in the pool for the object recorded checks
// for, or returns ErrInsufficientCapacity. Call release when the request is done.
func (s *Server) reserveSpace(ctx context.Context, poolID string, delta uint64,
	recorded func([]*store.Volume, []*store.Snapshot) bool) (release func(), err error) {
	s.space.mu.Lock()
	defer s.space.mu.Unlock()
	pool, err := s.store.GetPool(poolID)
	if err != nil {
		return nil, err
	}
	s.refreshFree(ctx, pool)
	volumes, err := s.store.ListVolumes(poolID)
	if err != nil {
		return nil, err
	}
	snapshots, err := s.store.ListSnapshots(poolID, "")
	if err != nil {
		return nil, err
	}
	committed := store.CommittedBytes(volumes, snapshots)
	for r := range s.space.pending {
		if r.pool == poolID && !r.recorded(volumes, snapshots) {
			committed += r.delta
		}
	}
	if err := store.CheckGrowth(pool, committed, delta); err != nil {
		return nil, withDeletedVolumes(err, volumes)
	}
	if s.space.pending == nil {
		s.space.pending = map[*reservation]struct{}{}
	}
	res := &reservation{pool: poolID, delta: delta, recorded: recorded}
	s.space.pending[res] = struct{}{}
	return func() {
		s.space.mu.Lock()
		delete(s.space.pending, res)
		s.space.mu.Unlock()
		s.updatePoolSpace(poolID)
	}, nil
}

// refreshFree updates the pool copy with the thin pool's current size and free space from the
// data plane (the stored values are as old as the last reconciler pass). Best effort.
func (s *Server) refreshFree(ctx context.Context, pool *store.Pool) {
	if s.dp == nil || pool.ThinPoolUUID == "" {
		return
	}
	stores, err := s.dp.ThinPools(ctx, "", pool.ThinPoolUUID)
	if err != nil || len(stores) != 1 {
		return
	}
	pool.TotalBytes, pool.FreeBytes = stores[0].TotalBytes, stores[0].FreeBytes
}

// updatePoolSpace recomputes a pool's size, committed and available space (from the data plane and
// the store), so the API shows the effect of a create or delete before the next reconciler pass.
func (s *Server) updatePoolSpace(poolID string) {
	pool, err := s.store.GetPool(poolID)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.refreshFree(ctx, pool)
	volumes, err := s.store.ListVolumes(poolID)
	if err != nil {
		return
	}
	snapshots, err := s.store.ListSnapshots(poolID, "")
	if err != nil {
		return
	}
	committed := store.CommittedBytes(volumes, snapshots)
	_, _ = s.store.MutatePool(poolID, func(p *store.Pool) error {
		if pool.TotalBytes != 0 {
			p.TotalBytes, p.FreeBytes = pool.TotalBytes, pool.FreeBytes
		}
		avail := store.AvailableBytes(p, committed)
		state := store.SpaceStateOf(p)
		if p.CommittedBytes == committed && p.AvailableBytes == avail && p.SpaceState == state && p.TotalBytes == pool.TotalBytes {
			return store.ErrSkipWrite
		}
		p.CommittedBytes, p.AvailableBytes, p.SpaceState = committed, avail, state
		return nil
	})
}

// volumeHeldBytes returns how many bytes a volume holds now (its allocated space).
func (s *Server) volumeHeldBytes(ctx context.Context, uuid string) (uint64, error) {
	dpVols, err := s.dp.Volumes(ctx)
	if err != nil {
		return 0, fmt.Errorf("list volumes: %w", err)
	}
	for _, l := range dpVols {
		if l.UUID == uuid {
			return l.AllocatedBytes, nil
		}
	}
	return 0, fmt.Errorf("%w: volume %s", store.ErrNotFound, uuid)
}

// handleUpdatePool changes a pool's space policy: PATCH /api/v1/pools/{id} {space_policy}; only
// the fields given change.
func (s *Server) handleUpdatePool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SpacePolicy *store.SpacePolicy `json:"space_policy"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.SpacePolicy == nil {
		writeError(w, fmt.Errorf("%w: nothing to change (space_policy)", store.ErrInvalidArgument))
		return
	}
	id := r.PathValue("id")
	// Merge: only the fields given change; the others keep their current values.
	if _, err := s.store.MutatePool(id, func(p *store.Pool) error {
		next := p.SpacePolicy
		in := req.SpacePolicy
		if in.MaxOvercommit != 0 {
			next.MaxOvercommit = in.MaxOvercommit
		}
		if in.ReservePercent != nil {
			next.ReservePercent = in.ReservePercent
		}
		if in.WarningPercent != 0 {
			next.WarningPercent = in.WarningPercent
		}
		if in.CriticalPercent != 0 {
			next.CriticalPercent = in.CriticalPercent
		}
		if err := next.Validate(); err != nil {
			return err
		}
		p.SpacePolicy = next
		p.SpaceState = store.SpaceStateOf(p)
		return nil
	}); err != nil {
		writeError(w, err)
		return
	}
	s.updatePoolSpace(id)
	pool, err := s.store.GetPool(id)
	if err != nil {
		writeError(w, err)
		return
	}
	s.logger.Info("pool space policy changed", "pool", id, "policy", fmt.Sprintf("%+v", pool.SpacePolicy.Effective()))
	writeJSON(w, http.StatusOK, map[string]any{"pool": pool})
}

// withDeletedVolumes adds to a capacity refusal how much of the pool deleted volumes still
// hold (they count until purged), so the operator knows purging them frees the space.
func withDeletedVolumes(err error, volumes []*store.Volume) error {
	var n int
	var held uint64
	for _, v := range volumes {
		if v.Deleted() {
			n++
			held += v.SizeBytes
		}
	}
	if n == 0 {
		return err
	}
	return fmt.Errorf("%w (%d deleted volumes still hold %.1f GiB until they are purged; an administrator can purge them now)",
		err, n, float64(held)/(1<<30))
}
