package reconciler

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/velocor/velocor/internal/meta"
)

// UnmanagedObject is something in spdk_tgt that no store record accounts for (docs/review-history.md P1-5):
// left behind by a failed rollback, a crash between an SPDK call and the store write, or a
// manual change, or, after the database was lost, the only trace of data that still exists.
// The reconciler reports these and never deletes them.
type UnmanagedObject struct {
	// Kind is LVSTORE, LVOL, RAID, BDEV, NVMF_SUBSYSTEM or ISCSI_TARGET.
	Kind string `json:"kind"`
	Name string `json:"name"`
	UUID string `json:"uuid,omitempty"`
	// Detail says where it is or what it holds.
	Detail    string    `json:"detail,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
}

// defaultUnmanagedGrace is how long an object must stay unaccounted for before it is
// reported: the API creates SPDK objects first and records them right after, so a pass can
// see one in between.
const defaultUnmanagedGrace = time.Minute

// SetUnmanagedGrace changes how long an unaccounted object waits before it is reported
// (tests; 0 reports at once).
func (r *Reconciler) SetUnmanagedGrace(d time.Duration) {
	r.passMu.Lock()
	defer r.passMu.Unlock()
	r.unmanagedGrace = &d
}

// findUnmanaged lists SPDK's objects and reports those no store record accounts for. Any
// failed listing skips the check for this pass rather than risk reporting live objects.
func (r *Reconciler) findUnmanaged(ctx context.Context, p *pass, examined bool) {
	found, ok := r.scanUnmanaged(ctx, examined)
	if !ok {
		return // keep the last report
	}
	now := r.clock()
	if r.unmanagedSeen == nil {
		r.unmanagedSeen = map[string]time.Time{}
	}
	seen := map[string]time.Time{}
	var report []UnmanagedObject
	for _, o := range found {
		key := o.Kind + "/" + o.Name + "/" + o.UUID
		first, known := r.unmanagedSeen[key]
		if !known {
			first = now
		}
		seen[key] = first
		grace := defaultUnmanagedGrace
		if r.unmanagedGrace != nil {
			grace = *r.unmanagedGrace
		}
		if now.Sub(first) < grace {
			continue
		}
		o.FirstSeen = first.UTC()
		report = append(report, o)
		if !r.unmanagedLogged[key] {
			r.logger.Warn("spdk_tgt holds an object Velocor doesn't manage; it is left alone",
				"kind", o.Kind, "name", o.Name, "uuid", o.UUID, "detail", o.Detail)
		}
	}
	r.unmanagedSeen = seen
	logged := map[string]bool{}
	for _, o := range report {
		logged[o.Kind+"/"+o.Name+"/"+o.UUID] = true
	}
	r.unmanagedLogged = logged
	sort.Slice(report, func(i, j int) bool {
		if report[i].Kind != report[j].Kind {
			return report[i].Kind < report[j].Kind
		}
		return report[i].Name < report[j].Name
	})
	p.unmanaged, p.unmanagedChecked = report, true
}

func (r *Reconciler) scanUnmanaged(ctx context.Context, examined bool) ([]UnmanagedObject, bool) {
	if !examined {
		return nil, false
	}
	pools, err1 := r.store.ListPools()
	disks, err2 := r.store.ListDisks()
	volumes, err3 := r.store.ListVolumes("")
	snaps, err4 := r.store.ListSnapshots("", "")
	exports, err5 := r.store.ListExports("", "")
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
		return nil, false
	}
	lvstores, err1 := r.spdk.BdevLvolGetLvstores(ctx, "", "")
	lvols, err2 := r.spdk.BdevLvolGetLvols(ctx)
	raids, err3 := r.spdk.BdevRaidGetBdevs(ctx, "all")
	bdevs, err4 := r.spdk.BdevGetBdevs(ctx, "")
	subsystems, err5 := r.spdk.NvmfGetSubsystems(ctx)
	targets, err6 := r.spdk.IscsiGetTargetNodes(ctx)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || err6 != nil {
		return nil, false
	}

	knownLvs, knownBdev, knownLvol := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, pl := range pools {
		knownLvs[pl.LVSUUID] = true
		knownBdev[pl.BaseBdevName], knownBdev[pl.EffectiveBdevName] = true, true
	}
	for _, d := range disks {
		knownBdev[d.SPDKBdevName] = true
	}
	for _, v := range volumes {
		knownLvol[v.UUID] = true
		knownBdev[v.EffectiveBdevName] = true
	}
	for _, s := range snaps {
		knownLvol[s.UUID] = true
	}
	delete(knownBdev, "")
	delete(knownLvol, "")

	var out []UnmanagedObject
	lvsName := map[string]string{}
	for _, l := range lvstores {
		lvsName[l.UUID] = l.Name
		if !knownLvs[l.UUID] {
			out = append(out, UnmanagedObject{Kind: "LVSTORE", Name: l.Name, UUID: l.UUID,
				Detail: fmt.Sprintf("on %s, %s used", l.BaseBdev, humanBytes((l.TotalDataClusters-l.FreeClusters)*uint64(l.ClusterSize)))})
		}
	}
	lvolBdevs := map[string]bool{}
	for _, l := range lvols {
		lvolBdevs[l.UUID] = true
		// Lvols in an unmanaged lvstore are covered by its report.
		if !knownLvs[l.LVS.UUID] || knownLvol[l.UUID] || l.Name == meta.LvolName {
			continue
		}
		kind := "volume"
		if l.IsSnapshot {
			kind = "snapshot"
		}
		out = append(out, UnmanagedObject{Kind: "LVOL", Name: l.Alias, UUID: l.UUID,
			Detail: fmt.Sprintf("a %s in pool lvstore %s", kind, lvsName[l.LVS.UUID])})
	}
	raidNames := map[string]bool{}
	for _, rb := range raids {
		raidNames[rb.Name] = true
		if !knownBdev[rb.Name] {
			out = append(out, UnmanagedObject{Kind: "RAID", Name: rb.Name, UUID: rb.UUID,
				Detail: fmt.Sprintf("%s, %s, %d of %d members", rb.RaidLevel, rb.State, rb.NumBaseBdevsDiscovered, rb.NumBaseBdevs)})
		}
	}
	for _, b := range bdevs {
		if lvolBdevs[b.Name] || lvolBdevs[b.UUID] || raidNames[b.Name] || knownBdev[b.Name] {
			continue
		}
		if len(b.Aliases) > 0 && knownBdev[b.Aliases[0]] {
			continue
		}
		detail := humanBytes(b.NumBlocks * uint64(b.BlockSize))
		if b.ProductName != "" {
			detail = b.ProductName + ", " + detail
		}
		out = append(out, UnmanagedObject{Kind: "BDEV", Name: b.Name, UUID: b.UUID, Detail: detail})
	}
	nqns, iqns := map[string]bool{}, map[string]bool{}
	for _, e := range exports {
		nqns[e.SubsystemNQN], iqns[e.TargetIQN] = true, true
	}
	for _, s := range subsystems {
		if s.Subtype == "Discovery" || nqns[s.Nqn] {
			continue
		}
		out = append(out, UnmanagedObject{Kind: "NVMF_SUBSYSTEM", Name: s.Nqn,
			Detail: "may give hosts access to a volume no export controls"})
	}
	for _, t := range targets {
		if iqns[t.Name] {
			continue
		}
		out = append(out, UnmanagedObject{Kind: "ISCSI_TARGET", Name: t.Name,
			Detail: "may give hosts access to a volume no export controls"})
	}
	return out, true
}

func humanBytes(b uint64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TiB", float64(b)/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	default:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	}
}

// UnmanagedSummary counts a report by kind ("1 LVOL, 2 NVMF_SUBSYSTEM"), or "" if empty.
func UnmanagedSummary(objs []UnmanagedObject) string {
	if len(objs) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, o := range objs {
		counts[o.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	s := ""
	for i, k := range kinds {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%d %s", counts[k], k)
	}
	return s
}
