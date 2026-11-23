package reconciler

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/store"
)

// relinkMovedMembers finds pool members that came back under a new disk ID (docs/review-history.md P1-14)
// and moves them to their new disk records, keeping the identity their array knows them by;
// reconcileDisks then attaches them and the data plane assembles the array. It looks only at
// pools whose array isn't online and members whose disk the last scan didn't find, and
// identifies the disks by the array metadata on them (Plane.ExamineDisks). It reports whether
// it relinked anything.
func (r *Reconciler) relinkMovedMembers(ctx context.Context, p *pass, discovered []*store.Disk) bool {
	if discovered == nil {
		return false // no scan this pass: every disk would look gone
	}
	present := map[string]bool{}
	for _, d := range discovered {
		present[d.ID] = true
	}
	pools, err := r.store.ListPools()
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list pools: %v", err))
		return false
	}
	disks, err := r.store.ListDisks()
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("list disks: %v", err))
		return false
	}
	diskByID := map[string]*store.Disk{}
	for _, d := range disks {
		diskByID[d.ID] = d
	}

	// Pools with a member whose SSD is gone, and no running array.
	type gone struct {
		pool    *store.Pool
		members []*store.Disk // the gone disks' records
		staying []*store.Disk // members still there
	}
	var todo []gone
	var raids []dataplane.Array
	for _, pool := range pools {
		if pool.HealthStatus == store.HealthStatusReconciling && pool.LVSUUID == "" {
			continue // being created
		}
		var g gone
		for _, m := range store.PoolMembers(pool) {
			d := diskByID[m.DiskID]
			if d == nil || m.State == store.MemberStateRebuilding {
				continue
			}
			if present[d.ID] {
				g.staying = append(g.staying, d)
			} else if d.Type == store.DiskTypeSSD {
				g.members = append(g.members, d)
			}
		}
		if len(g.members) == 0 {
			continue
		}
		if raids == nil {
			if raids, err = r.dp.Arrays(ctx); err != nil {
				p.errs = append(p.errs, fmt.Sprintf("list raid bdevs: %v", err))
				return false
			}
		}
		if i := slices.IndexFunc(raids, func(rb dataplane.Array) bool { return rb.Name == pool.BaseBdevName }); i >= 0 && strings.EqualFold(raids[i].State, "online") {
			continue // running: a member that left it is a failure, handled elsewhere
		}
		g.pool = pool
		todo = append(todo, g)
	}
	if len(todo) == 0 {
		return false
	}

	// The array metadata on the unclaimed SSDs and on the members still in place (a member in
	// place may hold a newer copy: then the found disks left the array earlier and must not
	// come back as current members), read once.
	var examine []*store.Disk
	seen := map[string]bool{}
	for _, d := range discovered {
		cur := diskByID[d.ID]
		if cur == nil || cur.Claimed || cur.PoolID != "" || cur.IsBootDevice || cur.Type != store.DiskTypeSSD {
			continue
		}
		examine, seen[cur.ID] = append(examine, cur), true
	}
	if len(examine) == 0 {
		return false
	}
	for _, g := range todo {
		for _, d := range g.staying {
			if !seen[d.ID] {
				examine, seen[d.ID] = append(examine, d), true
			}
		}
	}
	arrays, err := r.dp.ExamineDisks(ctx, examine)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("read array metadata: %v", err))
		return false
	}

	relinked := false
	for _, g := range todo {
		i := slices.IndexFunc(arrays, func(a dataplane.DiskArray) bool { return a.Name == g.pool.BaseBdevName })
		if i < 0 {
			continue // nothing came back (yet): the pool stays UNAVAILABLE
		}
		a := arrays[i]
		res := "pool:" + g.pool.ID
		if a.Unplaced != "" {
			p.notePool(g.pool.ID, "disks came back under new IDs but can't be matched to their slots: "+a.Unplaced)
			continue
		}
		// A slot's gone disk is replaced by the unclaimed disk that now holds the slot.
		goneBySlot := map[int]*store.Disk{}
		for _, m := range store.PoolMembers(g.pool) {
			if d := diskByID[m.DiskID]; d != nil && slices.Contains(g.members, d) {
				goneBySlot[m.Slot] = d
			}
		}
		for _, m := range a.Members {
			old := goneBySlot[m.Slot]
			if m.Stale || m.Slot < 0 || m.Disk.Claimed || old == nil {
				continue
			}
			if _, err := r.store.RelinkPoolMember(g.pool.ID, old.ID, m.Disk.ID, m.Identity); err != nil {
				p.errs = append(p.errs, fmt.Sprintf("%s: relink disk %s as %s: %v", res, old.ID, m.Disk.ID, err))
				continue
			}
			delete(goneBySlot, m.Slot)
			relinked = true
			p.repaired++
			r.logger.Warn("pool member came back under a new disk ID; relinked",
				"pool", g.pool.Name, "old_disk", old.ID, "new_disk", m.Disk.ID, "slot", m.Slot)
		}
	}
	return relinked
}
