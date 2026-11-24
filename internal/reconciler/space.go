package reconciler

import (
	"context"
	"fmt"

	"github.com/velocor/velocor/internal/store"
)

// reconcileSpace refreshes how much each volume and snapshot really holds, and each pool's
// committed and available space and space state (TODO P0-2). Reads the data plane only; writes the
// store only when a value changed.
func (r *Reconciler) reconcileSpace(ctx context.Context, p *pass, poolOnline map[string]bool) {
	pools, err := r.store.ListPools()
	if err != nil || len(pools) == 0 {
		return
	}
	dpVols, err := r.dp.Volumes(ctx)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list volumes: %v", err))
		return
	}
	// Bytes each volume and snapshot holds.
	held := make(map[string]uint64, len(dpVols))
	for _, l := range dpVols {
		held[l.UUID] = l.AllocatedBytes
	}

	for _, pool := range pools {
		if !poolOnline[pool.ID] {
			continue // nothing reliable to measure; keep the last values
		}
		volumes, err := r.store.ListVolumes(pool.ID)
		if err != nil {
			p.errs = append(p.errs, fmt.Sprintf("list volumes of %s: %v", pool.ID, err))
			continue
		}
		snapshots, err := r.store.ListSnapshots(pool.ID, "")
		if err != nil {
			p.errs = append(p.errs, fmt.Sprintf("list snapshots of %s: %v", pool.ID, err))
			continue
		}
		volHeld, snapHeld := map[string]uint64{}, map[string]uint64{}
		for _, v := range volumes {
			if b, ok := held[v.UUID]; ok && b != v.AllocatedBytes {
				v.AllocatedBytes, volHeld[v.ID] = b, b
			}
		}
		for _, s := range snapshots {
			if b, ok := held[s.UUID]; ok && b != s.AllocatedBytes {
				s.AllocatedBytes, snapHeld[s.ID] = b, b
			}
		}
		if err := r.store.UpdateSpaceUsage(volHeld, snapHeld); err != nil {
			p.errs = append(p.errs, fmt.Sprintf("update the space held in %s: %v", pool.ID, err))
		}

		committed := store.CommittedBytes(volumes, snapshots)
		var before, after store.SpaceState
		updated, err := r.store.MutatePool(pool.ID, func(cur *store.Pool) error {
			before = cur.SpaceState
			after = store.SpaceStateOf(cur)
			avail := store.AvailableBytes(cur, committed)
			if cur.CommittedBytes == committed && cur.AvailableBytes == avail && cur.SpaceState == after {
				return store.ErrSkipWrite
			}
			cur.CommittedBytes, cur.AvailableBytes, cur.SpaceState = committed, avail, after
			return nil
		})
		if err != nil {
			p.errs = append(p.errs, fmt.Sprintf("update pool space %s: %v", pool.ID, err))
			continue
		}
		if after != store.SpaceStateOK {
			e := updated.SpacePolicy.Effective()
			p.degrade("pool:"+pool.ID, fmt.Sprintf("space %s: %.0f%% used (warning at %.0f%%, critical at %.0f%%)%s",
				after, store.UsedPercent(updated), e.WarningPercent, e.CriticalPercent, spaceAdvice(after)))
		}
		if before != after && before != "" {
			r.logger.Warn("pool space state changed", "pool", pool.ID, "from", before, "to", after,
				"used_percent", fmt.Sprintf("%.1f", store.UsedPercent(updated)))
		}
	}
}

func spaceAdvice(st store.SpaceState) string {
	switch st {
	case store.SpaceStateCritical:
		return "; new volumes, snapshots, clones and expansions are refused: free space or add capacity"
	case store.SpaceStateFull:
		return "; writes to new space are failing now: free space immediately"
	}
	return ""
}
