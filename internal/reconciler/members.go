package reconciler

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/velocor/velocor/internal/dataplane"
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
func inRaid(raids []dataplane.Array, bdev string) bool {
	if bdev == "" {
		return false // an empty slot has no member
	}
	for _, rb := range raids {
		for _, b := range rb.Members {
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
// The data plane is the source of truth for slot membership; the store supplies which disk a
// now-empty slot held. md resumes an interrupted rebuild itself (after a restart too), so a
// rebuild target that is gone from the array failed: FAILED, and never re-added on its own.
func (r *Reconciler) viewRaid(ctx context.Context, pool *store.Pool, rb *dataplane.Array,
	raids []dataplane.Array, disks []*store.Disk, bdevs map[string]dataplane.Device) raidView {
	prev := store.PoolMembers(pool)
	diskByBdev := map[string]*store.Disk{}
	diskByID := map[string]*store.Disk{}
	for _, d := range disks {
		diskByID[d.ID] = d
		if d.Handle != "" {
			diskByBdev[d.Handle] = d
		}
	}
	v := raidView{health: store.HealthStatusOnline}
	state := strings.ToLower(rb.State)
	v.online = state == "online"

	for i, b := range rb.Members {
		m := store.PoolMember{Slot: i}
		if i < len(prev) {
			m.DiskID = prev[i].DiskID
		}
		switch {
		case b.Name != "" && (b.Configured || rb.Rebuild != nil && rb.Rebuild.Target == b.Name):
			// md's rebuild target is in its slot but not in sync, so not Configured, until the
			// rebuild finishes (SPDK's was).
			if d := diskByBdev[b.Name]; d != nil {
				m.DiskID = d.ID
			}
			m.State = store.MemberStateActive
			m.BadBlockBytes = b.BadBlockBytes
			if rb.Rebuild != nil && rb.Rebuild.Target == b.Name {
				m.State, m.RebuildPercent = store.MemberStateRebuilding, rb.Rebuild.Percent
			}
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
			rb.Name, rb.State, rb.Operational, rb.Slots))
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

// sameSlots reports whether both pools have the same disks in the same slots, and the same disks
// detached from them.
func sameSlots(a, b *store.Pool) bool {
	ids := func(p *store.Pool) []string {
		var out []string
		for _, m := range store.PoolMembers(p) {
			out = append(out, m.DiskID)
		}
		return out
	}
	return slices.Equal(ids(a), ids(b)) && slices.Equal(a.DetachedDiskIDs, b.DetachedDiskIDs)
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
	cur, err := r.dp.RebuildLimit(ctx)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("read rebuild speed limit: %v", err))
		return
	}
	if cur == st.RebuildMaxMBps {
		return
	}
	if err := r.dp.SetRebuildLimit(ctx, st.RebuildMaxMBps); err != nil {
		p.errs = append(p.errs, fmt.Sprintf("apply rebuild speed limit: %v", err))
		return
	}
	p.repaired++
}
