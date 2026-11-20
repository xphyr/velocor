package reconciler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

// purgeDeletedVolumes deletes the data of deleted volumes whose retention period has ended
// (docs/review-history.md P1-4). Only in pools that are online, so a pool that is still assembling never
// loses a volume to a lookup that failed; a failed delete is retried next pass.
func (r *Reconciler) purgeDeletedVolumes(ctx context.Context, p *pass, poolOnline map[string]bool) {
	volumes, err := r.store.ListVolumes("")
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list volumes: %v", err))
		return
	}
	now := r.clock()
	for _, v := range volumes {
		if !v.Deleted() || !poolOnline[v.PoolID] || (v.PurgeAt != nil && v.PurgeAt.After(now)) {
			continue
		}
		vol, err := store.StartPurge(r.store, v.ID, now)
		if errors.Is(err, store.ErrNotDue) || errors.Is(err, store.ErrNotFound) {
			continue // restored or purged since the listing
		}
		if err == nil {
			err = r.spdk.BdevLvolDelete(ctx, vol.UUID)
			if spdk.IsNotFound(err) {
				err = nil
			}
		}
		if err == nil {
			err = r.store.DeleteVolume(vol.ID)
		}
		if err != nil {
			p.errs = append(p.errs, fmt.Sprintf("purge deleted volume %s: %v", v.ID, err))
			continue
		}
		r.logger.Info("purged deleted volume: its retention period ended",
			"volume", v.ID, "name", v.Name, "deleted_at", v.DeletedAt, "deleted_by", v.DeletedBy)
	}
}

// purgeDeletedSnapshots deletes snapshots that were deleted while clones depended on them,
// once no volume (live or deleted) is cloned from them any more.
func (r *Reconciler) purgeDeletedSnapshots(ctx context.Context, p *pass, poolOnline map[string]bool) {
	snaps, err := r.store.ListSnapshots("", "")
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list snapshots: %v", err))
		return
	}
	for _, sn := range snaps {
		if !sn.Deleted() || !poolOnline[sn.PoolID] {
			continue
		}
		if ok, err := r.store.PurgeableSnapshot(sn.ID); err != nil || !ok {
			continue
		}
		err := r.spdk.BdevLvolDelete(ctx, sn.UUID)
		if spdk.IsNotFound(err) {
			err = nil
		}
		if err == nil {
			err = r.store.DeleteSnapshot(sn.ID)
		}
		if err != nil {
			p.errs = append(p.errs, fmt.Sprintf("purge deleted snapshot %s: %v", sn.ID, err))
			continue
		}
		r.logger.Info("deleted snapshot purged: no volume depends on it any more",
			"snapshot", sn.ID, "name", sn.Name, "deleted_at", sn.DeletedAt, "deleted_by", sn.DeletedBy)
	}
}

func (r *Reconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}
