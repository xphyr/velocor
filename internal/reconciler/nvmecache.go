package reconciler

import (
	"context"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/store"
)

// nvmeWriteCache reads the volatile write cache of the NVMe drives SPDK owns, which the kernel
// can no longer see, and keeps it off where the policy says so (docs/review-history.md P0-4). Drives can turn
// it back on after a power cycle or reset, so it is checked every pass. A refusal is handled
// like an SSD's (enforceWriteCache): recorded on the disk and tried again only after a restart
// or a new policy.
func (r *Reconciler) nvmeWriteCache(ctx context.Context, p *pass) {
	disks, err := r.store.ListDisks()
	if err != nil {
		return // reported by the other steps of the pass
	}
	for _, d := range disks {
		if d.Type != store.DiskTypeNVMe || !d.Claimed || d.IsBootDevice || d.SPDKBdevName == "" {
			continue
		}
		ctrl := dataplane.NVMeControllerFromBdev(d.SPDKBdevName)
		state, err := dataplane.NVMeWriteCache(ctx, r.spdk, ctrl)
		if err != nil {
			r.logger.Debug("cannot read the NVMe write cache", "disk", d.ID, "err", err)
			continue // keep the last known state
		}
		msg := d.WriteCacheError
		switch {
		case d.WriteCachePolicy != store.WriteCachePolicyDisabled || state == store.WriteCacheWriteThrough:
			msg = ""
		case d.WriteCacheError != "" && r.cacheRefused[d.ID]:
			// refused since this start: reported, not retried
		default:
			state, err = dataplane.SetNVMeWriteCache(ctx, r.spdk, ctrl, false)
			if err != nil {
				msg = err.Error()
				if r.cacheRefused == nil {
					r.cacheRefused = map[string]bool{}
				}
				r.cacheRefused[d.ID] = true
				r.logger.Warn("the drive refuses to turn its write cache off; it stays at risk on power loss", "disk", d.ID, "err", err)
			} else {
				msg = ""
				delete(r.cacheRefused, d.ID)
				p.repaired++
			}
		}
		r.recordWriteCache(p, d.ID, state, msg)
	}
}
