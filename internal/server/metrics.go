package server

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/velocor/velocor/internal/store"
	"github.com/velocor/velocor/internal/version"
)

// The metrics below are what the shipped alert rules (deploy/monitoring) and the UI's history
// graphs use (TODO.md P1-1). Health metrics carry the current state as a label with the value
// 1 (velocor_pool_health{pool="p",status="DEGRADED"} 1), so a rule selects the states it
// cares about; when the state changes, the old series ends.

// metricSet collects samples by family and writes them in the Prometheus text format, each
// family's samples together after its HELP and TYPE.
type metricSet struct {
	order []string
	fams  map[string]*metricFamily
}

type metricFamily struct {
	help, typ string
	lines     []string
}

func newMetricSet() *metricSet { return &metricSet{fams: map[string]*metricFamily{}} }

// declare registers a family, so it is written (with HELP and TYPE) even without samples.
func (m *metricSet) declare(name, typ, help string) *metricFamily {
	f, ok := m.fams[name]
	if !ok {
		f = &metricFamily{help: help, typ: typ}
		m.fams[name] = f
		m.order = append(m.order, name)
	}
	return f
}

// add appends a sample; labels are name/value pairs.
func (m *metricSet) add(name, typ, help string, value float64, labels ...string) {
	f := m.declare(name, typ, help)
	var sb strings.Builder
	sb.WriteString(name)
	if len(labels) > 0 {
		sb.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(labels[i])
			sb.WriteString(`="`)
			sb.WriteString(escapeLabel(labels[i+1]))
			sb.WriteByte('"')
		}
		sb.WriteByte('}')
	}
	sb.WriteByte(' ')
	sb.WriteString(formatValue(value))
	f.lines = append(f.lines, sb.String())
}

func (m *metricSet) gauge(name, help string, value float64, labels ...string) {
	m.add(name, "gauge", help, value, labels...)
}

func (m *metricSet) counter(name, help string, value float64, labels ...string) {
	m.add(name, "counter", help, value, labels...)
}

func (m *metricSet) writeTo(w io.Writer) error {
	var sb strings.Builder
	for _, name := range m.order {
		f := m.fams[name]
		fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(f.help), name, f.typ)
		for _, l := range f.lines {
			sb.WriteString(l)
			sb.WriteByte('\n')
		}
	}
	_, err := io.WriteString(w, sb.String())
	return err
}

// escapeLabel escapes a label value as the text format requires: backslash, double quote and
// line feed; everything else, including non-ASCII, is written as UTF-8.
func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func unixSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// statusOrUnknown names an empty status, so a series always has a value to select on.
func statusOrUnknown[T ~string](s T) string {
	if s == "" {
		return "UNSPECIFIED"
	}
	return string(s)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m := s.collectMetrics(r.Context())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = m.writeTo(w)
}

func (s *Server) collectMetrics(ctx context.Context) *metricSet {
	m := newMetricSet()
	m.gauge("velocor_up", "Whether the Velocor control plane (velod) is up.", 1)
	m.gauge("velocor_build_info", "velod's version, as a label.", 1, "version", version.Version)
	m.gauge("velocor_status_warnings", "Warnings shown on the status page (insecure HTTP, bootstrap password file, unmanaged SPDK objects, ...).",
		float64(len(s.warnings())))

	s.reconcilerMetrics(m)
	pools, _ := s.store.ListPools()
	disks, _ := s.store.ListDisks()
	vols, _ := s.store.ListVolumes("")
	poolName := map[string]string{}
	for _, p := range pools {
		poolName[p.ID] = p.Name
	}
	poolMetrics(m, pools, vols)
	diskMetrics(m, disks, poolName)
	volumeMetrics(m, vols, poolName)
	if snaps, err := s.store.ListSnapshots("", ""); err == nil {
		m.declare("velocor_snapshot_allocated_bytes", "gauge", "Space a snapshot holds in its pool.")
		for _, sn := range snaps {
			if sn.Deleted() {
				continue
			}
			m.gauge("velocor_snapshot_allocated_bytes", "Space a snapshot holds in its pool.", float64(sn.AllocatedBytes),
				"snapshot", sn.Name, "pool", poolName[sn.PoolID])
		}
	}
	if exps, err := s.store.ListExports("", ""); err == nil {
		exportMetrics(m, exps, vols)
	}
	s.backupMetrics(m)
	s.ioMetrics(ctx, m, pools, disks, vols)
	return m
}

func (s *Server) reconcilerMetrics(m *metricSet) {
	if s.reconciler == nil {
		return
	}
	st := s.reconciler.State()
	m.counter("velocor_reconciler_drift_repaired_total", "Resources the reconciler has repaired.", float64(st.DriftRepairedCount))
	m.gauge("velocor_reconciler_degraded_resources", "Resources the reconciler currently reports as degraded.", float64(len(st.DegradedResources)))
	if !st.LastReconciledAt.IsZero() {
		m.gauge("velocor_reconciler_last_run_timestamp_seconds", "When the reconciler last completed a pass.", unixSeconds(st.LastReconciledAt))
	}
	m.gauge("velocor_reconciler_last_run_failed", "Whether the reconciler's last pass ended with an error.", b2f(st.LastError != ""))
	counts := map[string]int{}
	for _, o := range st.Unmanaged {
		counts[o.Kind]++
	}
	for _, k := range []string{"BDEV", "ISCSI_TARGET", "LVOL", "LVSTORE", "NVMF_SUBSYSTEM", "RAID"} {
		m.gauge("velocor_spdk_unmanaged_objects", "Objects in spdk_tgt that no Velocor record accounts for, by kind.", float64(counts[k]), "kind", k)
	}
}

func poolMetrics(m *metricSet, pools []*store.Pool, vols []*store.Volume) {
	const (
		hHealth    = "A pool's health (ONLINE, DEGRADED, REBUILDING, FAULTED, OFFLINE, UNAVAILABLE, ...), as the status label."
		hMembers   = "A pool's RAID members by state (ACTIVE, REBUILDING, FAILED, MISSING)."
		hRebuild   = "Rebuild progress of a pool member being rebuilt, 0 to 1."
		hDurable   = "Whether writes a pool acknowledges are durable: DURABLE, AT_RISK (a member caches writes without power-loss protection) or UNKNOWN, as the durability label."
		hMetaFail  = "Whether writing the pool's on-pool metadata copy failed on the last attempt."
		hMetaAt    = "When the pool's on-pool metadata copy was last written."
		hState     = "Pool space state: 0 OK, 1 WARNING, 2 CRITICAL (growth refused), 3 FULL (writes to new space fail)."
		hDeleted   = "Deleted volumes a pool keeps until their retention ends."
		hDeletedB  = "Space deleted volumes still hold in a pool."
		hTotal     = "Total capacity of a pool."
		hFree      = "Free capacity of a pool's volume store."
		hCommitted = "Space promised by a pool: every volume fully written plus what snapshots hold."
		hAvail     = "How much more a pool accepts under its space policy (0 at CRITICAL or FULL)."
		hUsed      = "Fraction of a pool's volume store that is allocated."
	)
	m.declare("velocor_pool_health", "gauge", hHealth)
	deleted, deletedBytes := map[string]int{}, map[string]uint64{}
	for _, v := range vols {
		if v.Deleted() {
			deleted[v.PoolID]++
			deletedBytes[v.PoolID] += v.AllocatedBytes
		}
	}
	for _, p := range pools {
		l := []string{"pool", p.Name}
		m.gauge("velocor_pool_total_bytes", hTotal, float64(p.TotalBytes), "pool", p.Name, "raid", string(p.RaidLevel))
		m.gauge("velocor_pool_free_bytes", hFree, float64(p.FreeBytes), "pool", p.Name, "raid", string(p.RaidLevel))
		m.gauge("velocor_pool_committed_bytes", hCommitted, float64(p.CommittedBytes), l...)
		m.gauge("velocor_pool_available_bytes", hAvail, float64(p.AvailableBytes), l...)
		m.gauge("velocor_pool_used_ratio", hUsed, store.UsedPercent(p)/100, l...)
		m.gauge("velocor_pool_space_state", hState, float64(spaceStateValue(p.SpaceState)), "pool", p.Name, "state", statusOrUnknown(p.SpaceState))
		m.gauge("velocor_pool_health", hHealth, 1, "pool", p.Name, "status", statusOrUnknown(p.HealthStatus))
		m.gauge("velocor_pool_durability", hDurable, 1, "pool", p.Name, "durability", statusOrUnknown(p.Durability))
		states := map[store.MemberState]int{store.MemberStateActive: 0, store.MemberStateRebuilding: 0, store.MemberStateFailed: 0, store.MemberStateMissing: 0}
		for _, mb := range p.Members {
			states[mb.State]++
			if mb.State == store.MemberStateRebuilding {
				m.gauge("velocor_pool_member_rebuild_ratio", hRebuild, float64(mb.RebuildPercent)/100,
					"pool", p.Name, "slot", strconv.Itoa(mb.Slot), "disk", mb.DiskID)
			}
		}
		for _, st := range []store.MemberState{store.MemberStateActive, store.MemberStateRebuilding, store.MemberStateFailed, store.MemberStateMissing} {
			m.gauge("velocor_pool_members", hMembers, float64(states[st]), "pool", p.Name, "state", string(st))
		}
		m.gauge("velocor_pool_metadata_copy_failed", hMetaFail, b2f(p.MetaError != ""), l...)
		if !p.MetaWrittenAt.IsZero() {
			m.gauge("velocor_pool_metadata_copy_timestamp_seconds", hMetaAt, unixSeconds(p.MetaWrittenAt), l...)
		}
		m.gauge("velocor_pool_deleted_volumes", hDeleted, float64(deleted[p.ID]), l...)
		m.gauge("velocor_pool_deleted_volume_bytes", hDeletedB, float64(deletedBytes[p.ID]), l...)
	}
}

func diskMetrics(m *metricSet, disks []*store.Disk, poolName map[string]string) {
	const (
		hHealth   = "A disk's health, as the status label."
		hTemp     = "Disk temperature from SMART, in Celsius (0 when unknown)."
		hCrit     = "Whether the disk reports a SMART critical warning."
		hMedia    = "Media and data-integrity errors the disk has reported (SMART)."
		hSpare    = "The disk's available spare capacity, 0 to 1 (NVMe)."
		hSpareMin = "The spare level below which the disk itself reports trouble, 0 to 1 (NVMe)."
		hWear     = "The disk's estimated wear, 0 to 1 and beyond (NVMe percentage used)."
		hUnsafe   = "Unsafe shutdowns the disk has counted (SMART)."
		hCache    = "Whether Velocor failed to apply the disk's write-cache policy."
	)
	m.declare("velocor_disk_health", "gauge", hHealth)
	for _, d := range disks {
		pool := poolName[d.PoolID]
		l := []string{"disk", d.ID, "type", string(d.Type), "pool", pool}
		m.gauge("velocor_disk_health", hHealth, 1, append(l, "status", statusOrUnknown(d.HealthStatus))...)
		m.gauge("velocor_disk_temperature_celsius", hTemp, float64(d.Smart.TemperatureCelsius), "disk", d.ID, "type", string(d.Type))
		m.gauge("velocor_disk_smart_critical_warning", hCrit, b2f(d.Smart.CriticalWarning), l...)
		m.counter("velocor_disk_media_errors_total", hMedia, float64(d.Smart.MediaErrors), l...)
		m.counter("velocor_disk_unsafe_shutdowns_total", hUnsafe, float64(d.Smart.UnsafeShutdowns), l...)
		if d.Type == store.DiskTypeNVMe {
			m.gauge("velocor_disk_available_spare_ratio", hSpare, float64(d.Smart.AvailableSparePercent)/100, l...)
			m.gauge("velocor_disk_available_spare_threshold_ratio", hSpareMin, float64(d.Smart.AvailableSpareThresholdPercent)/100, l...)
			m.gauge("velocor_disk_wear_ratio", hWear, float64(d.Smart.PercentageUsed)/100, l...)
		}
		m.gauge("velocor_disk_write_cache_error", hCache, b2f(d.WriteCacheError != ""), l...)
	}
}

func volumeMetrics(m *metricSet, vols []*store.Volume, poolName map[string]string) {
	const (
		hSize   = "Provisioned size of a volume."
		hAlloc  = "Space a volume holds in its pool."
		hHealth = "A volume's health, as the status label."
	)
	m.declare("velocor_volume_health", "gauge", hHealth)
	for _, v := range vols {
		if v.Deleted() {
			continue // its space shows in the pool's figures
		}
		l := []string{"volume", v.Name, "pool", poolName[v.PoolID]}
		m.gauge("velocor_volume_size_bytes", hSize, float64(v.SizeBytes), l...)
		m.gauge("velocor_volume_allocated_bytes", hAlloc, float64(v.AllocatedBytes), l...)
		m.gauge("velocor_volume_health", hHealth, 1, append(l, "status", statusOrUnknown(v.HealthStatus))...)
	}
}

func exportMetrics(m *metricSet, exps []*store.Export, vols []*store.Volume) {
	const (
		hHealth   = "An export's health, as the status label."
		hDisabled = "Whether an export is disabled (by an administrator, or because its portal's address is gone)."
	)
	volName := map[string]string{}
	for _, v := range vols {
		volName[v.ID] = v.Name
	}
	m.declare("velocor_export_health", "gauge", hHealth)
	for _, e := range exps {
		l := []string{"export", e.Name, "volume", volName[e.VolumeID], "protocol", string(e.Protocol)}
		m.gauge("velocor_export_health", hHealth, 1, append(l, "status", statusOrUnknown(e.HealthStatus))...)
		m.gauge("velocor_export_disabled", hDisabled, b2f(e.Disabled), l...)
	}
}

func (s *Server) backupMetrics(m *metricSet) {
	if s.backups == nil {
		return
	}
	list, err := s.backups.List()
	if err != nil {
		return
	}
	const h = "When the newest database backup of a kind (hourly, daily, manual, ...) was taken."
	m.declare("velocor_backup_last_timestamp_seconds", "gauge", h)
	newest := map[string]time.Time{}
	for _, b := range list {
		if b.CreatedAt.After(newest[b.Kind]) {
			newest[b.Kind] = b.CreatedAt
		}
	}
	kinds := make([]string, 0, len(newest))
	for k := range newest {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		m.gauge("velocor_backup_last_timestamp_seconds", h, unixSeconds(newest[k]), "kind", k)
	}
}

// ioMetrics reads SPDK's I/O counters, per volume and per pool (for the history graphs and
// for rate() in Prometheus), and reports whether the data plane answers at all.
func (s *Server) ioMetrics(ctx context.Context, m *metricSet, pools []*store.Pool, disks []*store.Disk, vols []*store.Volume) {
	const hUp = "Whether spdk_tgt (the data plane) answers velod."
	if s.spdk == nil {
		m.gauge("velocor_dataplane_up", hUp, 0)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	iostat, err := s.spdk.BdevGetIostat(ctx, "")
	if err != nil {
		m.gauge("velocor_dataplane_up", hUp, 0)
		return
	}
	m.gauge("velocor_dataplane_up", hUp, 1)

	type owner struct{ kind, name, pool string }
	byBdev := map[string]owner{}
	poolName := map[string]string{}
	for _, p := range pools {
		poolName[p.ID] = p.Name
	}
	for _, v := range vols {
		if v.Deleted() {
			continue
		}
		o := owner{"volume", v.Name, poolName[v.PoolID]}
		// SPDK names an lvol bdev by its UUID; the store keeps its alias.
		for _, n := range []string{v.UUID, v.EffectiveBdevName, v.LvolBdevName} {
			if n != "" {
				byBdev[n] = o
			}
		}
	}
	for _, d := range disks {
		if d.SPDKBdevName != "" {
			byBdev[d.SPDKBdevName] = owner{"disk", d.ID, poolName[d.PoolID]}
		}
	}
	for _, p := range pools { // after the disks: a one-disk pool's I/O counts as the pool's
		if p.EffectiveBdevName != "" {
			byBdev[p.EffectiveBdevName] = owner{"pool", p.Name, p.Name}
		}
	}
	tick := float64(iostat.TickRate)
	for _, b := range iostat.Bdevs {
		// Raw per-bdev counters, as before (any bdev, managed or not).
		m.counter("velocor_bdev_read_ops_total", "Read operations on an SPDK bdev.", float64(b.NumReadOps), "bdev", b.Name)
		m.counter("velocor_bdev_write_ops_total", "Write operations on an SPDK bdev.", float64(b.NumWriteOps), "bdev", b.Name)
		o, ok := byBdev[b.Name]
		if !ok {
			continue
		}
		var l []string
		switch o.kind {
		case "volume":
			l = []string{"volume", o.name, "pool", o.pool}
		case "pool":
			l = []string{"pool", o.name}
		case "disk":
			l = []string{"disk", o.name, "pool", o.pool}
		}
		p := "velocor_" + o.kind + "_"
		m.counter(p+"read_ops_total", "Read operations completed.", float64(b.NumReadOps), l...)
		m.counter(p+"write_ops_total", "Write operations completed.", float64(b.NumWriteOps), l...)
		m.counter(p+"read_bytes_total", "Bytes read.", float64(b.BytesRead), l...)
		m.counter(p+"written_bytes_total", "Bytes written.", float64(b.BytesWritten), l...)
		m.counter(p+"unmapped_bytes_total", "Bytes unmapped (discarded).", float64(b.BytesUnmapped), l...)
		if tick > 0 {
			m.counter(p+"read_seconds_total", "Time spent on reads (sum over all reads; divide its rate by the read rate for the average latency).",
				float64(b.ReadLatencyTicks)/tick, l...)
			m.counter(p+"write_seconds_total", "Time spent on writes (sum over all writes; divide its rate by the write rate for the average latency).",
				float64(b.WriteLatencyTicks)/tick, l...)
		}
	}
}
