package reconciler

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/meta"
)

// UnmanagedObject is something in the data plane that no store record accounts for
// (docs/review-history.md P1-5): left behind by a failed rollback, a crash between a data-plane
// call and the store write, or a manual change, or, after the database was lost, the only trace
// of data that still exists. The reconciler reports these and never deletes them.
type UnmanagedObject = dataplane.UnmanagedObject

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
	known := dataplane.Known{
		ThinPools: map[string]bool{}, Devices: map[string]bool{}, Volumes: map[string]bool{},
		Targets: map[string]bool{}, Skip: map[string]bool{meta.LvolName: true},
	}
	for _, pl := range pools {
		known.ThinPools[pl.LVSUUID] = true
		known.Devices[pl.BaseBdevName], known.Devices[pl.EffectiveBdevName] = true, true
	}
	for _, d := range disks {
		known.Devices[d.SPDKBdevName] = true
	}
	for _, v := range volumes {
		known.Volumes[v.UUID] = true
		known.Devices[v.EffectiveBdevName] = true
	}
	for _, s := range snaps {
		known.Volumes[s.UUID] = true
	}
	for _, e := range exports {
		known.Targets[e.SubsystemNQN], known.Targets[e.TargetIQN] = true, true
	}
	delete(known.Devices, "")
	delete(known.Volumes, "")
	delete(known.Targets, "")
	out, err := r.dp.Unmanaged(ctx, known)
	if err != nil {
		return nil, false
	}
	return out, true
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
