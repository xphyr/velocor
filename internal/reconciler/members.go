package reconciler

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

// raidView is what the reconciler concluded about a pool's RAID in one pass.
type raidView struct {
	members []store.PoolMember
	health  store.HealthStatus
	reasons []string
	online  bool // the RAID serves I/O (possibly degraded or rebuilding)
}

// inRaid reports whether bdev is a member of any RAID bdev.
func inRaid(raids []spdk.RaidBdevInfo, bdev string) bool {
	for _, rb := range raids {
		for _, b := range rb.BaseBdevsList {
			if b.Name == bdev {
				return true
			}
		}
	}
	return false
}

func diskLabel(id string) string {
	if id == "" {
		return "unknown disk"
	}
	return "disk " + id
}

// viewRaid derives a pool's member states and health from SPDK's view of its RAID bdev.
// SPDK is the source of truth for slot membership; the store supplies which disk a now-empty
// slot held. An online array whose rebuild target dropped out (e.g. spdk_tgt restarted
// mid-rebuild, which records nothing) gets the disk re-added, restarting the rebuild.
func (r *Reconciler) viewRaid(ctx context.Context, pool *store.Pool, rb *spdk.RaidBdevInfo,
	raids []spdk.RaidBdevInfo, disks []*store.Disk, bdevs map[string]spdk.BdevInfo) raidView {
	prev := store.PoolMembers(pool)
	diskByBdev := map[string]*store.Disk{}
	diskByID := map[string]*store.Disk{}
	for _, d := range disks {
		diskByID[d.ID] = d
		if d.SPDKBdevName != "" {
			diskByBdev[d.SPDKBdevName] = d
		}
	}
	v := raidView{health: store.HealthStatusOnline}
	state := strings.ToLower(rb.State)
	v.online = state == "online"

	for i, b := range rb.BaseBdevsList {
		m := store.PoolMember{Slot: i}
		if i < len(prev) {
			m.DiskID = prev[i].DiskID
		}
		switch {
		case b.Name != "" && b.IsConfigured:
			if d := diskByBdev[b.Name]; d != nil {
				m.DiskID = d.ID
			}
			m.State = store.MemberStateActive
			if rb.Process != nil && rb.Process.Target == b.Name {
				m.State, m.RebuildPercent = store.MemberStateRebuilding, rb.Process.Progress.Percent
			}
		case v.online && i < len(prev) && prev[i].State == store.MemberStateRebuilding && rb.Process == nil:
			// The rebuild target isn't in the array any more and nothing is rebuilding: the
			// rebuild was interrupted (restart) or its disk failed. Re-add it if it's here.
			m.State = store.MemberStateFailed
			d := diskByID[m.DiskID]
			if _, present := bdevs[bdevName(d)]; d == nil || !present || inRaid(raids, d.SPDKBdevName) {
				break
			}
			if err := r.spdk.BdevRaidAddBaseBdev(ctx, rb.Name, d.SPDKBdevName); err != nil {
				v.reasons = append(v.reasons, fmt.Sprintf("restarting the rebuild of %s failed: %v", diskLabel(d.ID), err))
				break
			}
			m.State = store.MemberStateRebuilding
			msg := fmt.Sprintf("rebuild of %s restarted from the beginning (it was interrupted)", diskLabel(d.ID))
			v.reasons = append(v.reasons, msg)
			r.logger.Warn(msg, "pool", pool.ID, "raid", rb.Name)
		case !v.online && b.UUID != "":
			m.State = store.MemberStateMissing // awaited by a configuring array
		case i < len(prev) && prev[i].State == store.MemberStateMissing:
			m.State = store.MemberStateMissing
		default:
			m.State = store.MemberStateFailed
		}
		v.members = append(v.members, m)
	}

	var empty, rebuilding []string
	for _, m := range v.members {
		switch m.State {
		case store.MemberStateFailed, store.MemberStateMissing:
			empty = append(empty, fmt.Sprintf("slot %d (%s, %s)", m.Slot, diskLabel(m.DiskID), strings.ToLower(string(m.State))))
		case store.MemberStateRebuilding:
			rebuilding = append(rebuilding, fmt.Sprintf("%s %d%%", diskLabel(m.DiskID), m.RebuildPercent))
		}
	}

	switch {
	case !v.online && state == "configuring":
		v.health = store.HealthStatusUnavailable
		v.reasons = append(v.reasons, fmt.Sprintf("RAID %s is waiting for %s; reconnect the disk, or start the pool degraded",
			rb.Name, strings.Join(empty, ", ")))
	case !v.online:
		v.health = store.HealthStatusFaulted
		v.reasons = append(v.reasons, fmt.Sprintf("RAID %s is %s (%d of %d members operational)",
			rb.Name, rb.State, rb.NumBaseBdevsOperational, rb.NumBaseBdevs))
	case len(rebuilding) > 0:
		v.health = store.HealthStatusRebuilding
		v.reasons = append(v.reasons, "rebuilding "+strings.Join(rebuilding, ", "))
		if len(empty) > 0 {
			v.reasons = append(v.reasons, "also empty: "+strings.Join(empty, ", "))
		}
	case len(empty) > 0:
		v.health = store.HealthStatusDegraded
		v.reasons = append(v.reasons, fmt.Sprintf("RAID %s is running without %s; replace the disk to restore redundancy",
			rb.Name, strings.Join(empty, ", ")))
	}
	return v
}

func bdevName(d *store.Disk) string {
	if d == nil {
		return ""
	}
	return d.SPDKBdevName
}

func membersEqual(a, b []store.PoolMember) bool { return slices.Equal(a, b) }

// applySettings re-applies node-wide SPDK settings that differ from what spdk_tgt runs with
// (it forgets them when it restarts).
func (r *Reconciler) applySettings(ctx context.Context, p *pass) {
	st, err := r.store.GetSettings()
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("read settings: %v", err))
		return
	}
	cur, err := r.spdk.BdevRaidGetMaxBandwidth(ctx)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("read rebuild speed limit: %v", err))
		return
	}
	if cur == st.RebuildMaxMBps {
		return
	}
	if err := r.spdk.BdevRaidSetOptions(ctx, st.RebuildMaxMBps); err != nil {
		p.errs = append(p.errs, fmt.Sprintf("apply rebuild speed limit: %v", err))
		return
	}
	p.repaired++
}
