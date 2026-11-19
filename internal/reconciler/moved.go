package reconciler

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

// relinkMovedMembers finds pool members that came back under a new disk ID (TODO.md P1-14)
// and moves them to their new disk records, keeping the bdev UUID their RAID superblock knows
// them by; reconcileDisks then attaches them and SPDK examine assembles the array. It looks
// only at pools whose RAID isn't online and members whose disk the last scan didn't find, and
// identifies the disks by the superblock on them (dataplane.MatchMovedMembers). It reports
// whether it relinked anything.
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
	var raids []spdk.RaidBdevInfo
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
			if raids, err = r.spdk.BdevRaidGetBdevs(ctx, "all"); err != nil {
				p.errs = append(p.errs, fmt.Sprintf("list raid bdevs: %v", err))
				return false
			}
		}
		if i := slices.IndexFunc(raids, func(rb spdk.RaidBdevInfo) bool { return rb.Name == pool.BaseBdevName }); i >= 0 && strings.EqualFold(raids[i].State, "online") {
			continue // running: a member that left it is a failure, handled elsewhere
		}
		g.pool = pool
		todo = append(todo, g)
	}
	if len(todo) == 0 {
		return false
	}

	// Superblocks of the unclaimed SSDs, read once.
	var unclaimed []dataplane.MovedDisk
	for _, d := range discovered {
		cur := diskByID[d.ID]
		if cur == nil || cur.Claimed || cur.PoolID != "" || cur.IsBootDevice || cur.Type != store.DiskTypeSSD {
			continue
		}
		sb, err := r.readSuperblock(cur)
		if err != nil {
			r.logger.Warn("cannot read the RAID superblock", "disk", cur.ID, "err", err)
			continue
		}
		if sb != nil {
			unclaimed = append(unclaimed, dataplane.MovedDisk{Disk: cur, SB: sb})
		}
	}

	relinked := false
	for _, g := range todo {
		var found []dataplane.MovedDisk
		var newest *hardware.RaidSuperblock
		for _, f := range unclaimed {
			if f.SB.Name == g.pool.BaseBdevName && (newest == nil || f.SB.SeqNumber > newest.SeqNumber) {
				newest = f.SB
			}
			if f.SB.Name == g.pool.BaseBdevName {
				found = append(found, f)
			}
		}
		if newest == nil {
			continue // nothing came back (yet): the pool stays UNAVAILABLE
		}
		// A member still in place may hold a newer superblock (the found disks left the
		// array earlier): then they must not come back as current members.
		for _, d := range g.staying {
			if sb, err := r.readSuperblock(d); err == nil && sb != nil && sb.UUID == newest.UUID && sb.SeqNumber > newest.SeqNumber {
				newest = sb
			}
		}
		res := "pool:" + g.pool.ID
		want := map[string]*store.Disk{} // bdev UUID -> gone record
		var uuids []string
		for _, d := range g.members {
			u := dataplane.DiskBdevUUID(d)
			want[u] = d
			uuids = append(uuids, u)
		}
		match, err := dataplane.MatchMovedMembers(newest, uuids, found)
		if err != nil {
			p.notePool(g.pool.ID, "disks came back under new IDs but can't be matched to their slots: "+err.Error())
			continue
		}
		for newID, u := range match {
			old := want[u]
			if _, err := r.store.RelinkPoolMember(g.pool.ID, old.ID, newID, u); err != nil {
				p.errs = append(p.errs, fmt.Sprintf("%s: relink disk %s as %s: %v", res, old.ID, newID, err))
				continue
			}
			relinked = true
			p.repaired++
			r.logger.Warn("pool member came back under a new disk ID; relinked",
				"pool", g.pool.Name, "old_disk", old.ID, "new_disk", newID, "bdev_uuid", u)
		}
	}
	return relinked
}

// readSuperblock reads a disk's RAID superblock through the kernel (nil: none).
func (r *Reconciler) readSuperblock(d *store.Disk) (*hardware.RaidSuperblock, error) {
	if r.sbReader != nil {
		return r.sbReader(d)
	}
	if d.DevicePath == "" {
		return nil, nil
	}
	return hardware.ReadRaidSuperblock(d.DevicePath)
}
