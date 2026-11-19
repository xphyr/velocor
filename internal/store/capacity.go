package store

import (
	"errors"
	"fmt"
	"math"
)

// Pool capacity, overcommit and space safety (TODO P0-2; docs/design/p0-2-capacity.md).

// ErrInsufficientCapacity: the pool can't take the requested growth (HTTP 507).
var ErrInsufficientCapacity = errors.New("insufficient pool capacity")

// SpaceState is how full a pool's volume store is, separate from its RAID health.
type SpaceState string

const (
	SpaceStateOK       SpaceState = "OK"
	SpaceStateWarning  SpaceState = "WARNING"  // used >= warning threshold
	SpaceStateCritical SpaceState = "CRITICAL" // used >= critical threshold: growth refused
	SpaceStateFull     SpaceState = "FULL"     // no free clusters: writes to new space fail
)

const (
	DefaultMaxOvercommit   = 1.0
	DefaultReservePercent  = 5.0
	DefaultWarningPercent  = 80.0
	DefaultCriticalPercent = 90.0
)

// SpacePolicy limits what a pool accepts. Zero values mean the defaults (a reserve of 0% has
// to be set explicitly through ReservePercent).
type SpacePolicy struct {
	// MaxOvercommit: committed space (every volume fully written plus what snapshots hold) may
	// reach this many times the usable capacity. 1.0: everything committed fits.
	MaxOvercommit float64 `json:"max_overcommit,omitempty"`
	// ReservePercent of the capacity is never committed (nil: the default).
	ReservePercent  *float64 `json:"reserve_percent,omitempty"`
	WarningPercent  float64  `json:"warning_percent,omitempty"`
	CriticalPercent float64  `json:"critical_percent,omitempty"`
}

// Effective returns the policy with defaults filled in.
func (sp SpacePolicy) Effective() SpacePolicy {
	out := sp
	if out.MaxOvercommit == 0 {
		out.MaxOvercommit = DefaultMaxOvercommit
	}
	if out.ReservePercent == nil {
		r := DefaultReservePercent
		out.ReservePercent = &r
	}
	if out.WarningPercent == 0 {
		out.WarningPercent = DefaultWarningPercent
	}
	if out.CriticalPercent == 0 {
		out.CriticalPercent = DefaultCriticalPercent
	}
	return out
}

// Validate checks an effective policy.
func (sp SpacePolicy) Validate() error {
	e := sp.Effective()
	switch {
	case e.MaxOvercommit < 0.1 || e.MaxOvercommit > 100:
		return fmt.Errorf("%w: max_overcommit must be between 0.1 and 100", ErrInvalidArgument)
	case *e.ReservePercent < 0 || *e.ReservePercent >= 50:
		return fmt.Errorf("%w: reserve_percent must be at least 0 and below 50", ErrInvalidArgument)
	case e.WarningPercent <= 0 || e.CriticalPercent > 100 || e.WarningPercent >= e.CriticalPercent:
		return fmt.Errorf("%w: need 0 < warning_percent < critical_percent <= 100", ErrInvalidArgument)
	}
	return nil
}

// CommittedBytes is the worst case a pool has promised: every volume (thin or thick, clones
// included) fully written, plus the clusters every snapshot and every deleted volume waiting
// to be purged holds now.
func CommittedBytes(volumes []*Volume, snapshots []*Snapshot) uint64 {
	var k uint64
	for _, v := range volumes {
		if v.Deleted() {
			k += v.AllocatedBytes // can't grow any more: only what it holds
			continue
		}
		k += v.SizeBytes
	}
	for _, s := range snapshots {
		k += s.AllocatedBytes
	}
	return k
}

// MetaLvolBytes is the size of each pool's metadata-copy lvol (meta.LvolBytes).
const MetaLvolBytes = 8 << 20

// LimitBytes is how much the pool may commit under its policy.
func LimitBytes(p *Pool) uint64 {
	e := p.SpacePolicy.Effective()
	total := p.TotalBytes
	if p.MetaLvolUUID != "" { // the thick metadata-copy lvol (P0-3) is not for volumes
		total -= min(total, MetaLvolBytes)
	}
	usable := float64(total) * (1 - *e.ReservePercent/100)
	return uint64(math.Max(0, usable*e.MaxOvercommit))
}

// UsedPercent of the volume store's capacity that is allocated.
func UsedPercent(p *Pool) float64 {
	if p.TotalBytes == 0 {
		return 0
	}
	used := p.TotalBytes - min(p.FreeBytes, p.TotalBytes)
	return 100 * float64(used) / float64(p.TotalBytes)
}

// SpaceStateOf a pool from its capacity, free space and policy.
func SpaceStateOf(p *Pool) SpaceState {
	if p.TotalBytes == 0 {
		return SpaceStateOK // no volume store yet
	}
	e := p.SpacePolicy.Effective()
	used := UsedPercent(p)
	switch {
	case p.FreeBytes == 0:
		return SpaceStateFull
	case used >= e.CriticalPercent:
		return SpaceStateCritical
	case used >= e.WarningPercent:
		return SpaceStateWarning
	}
	return SpaceStateOK
}

// AvailableBytes is how much more the pool accepts: nothing at CRITICAL or FULL, otherwise
// the limit minus what is committed.
func AvailableBytes(p *Pool, committed uint64) uint64 {
	if st := SpaceStateOf(p); st == SpaceStateCritical || st == SpaceStateFull {
		return 0
	}
	if limit := LimitBytes(p); limit > committed {
		return limit - committed
	}
	return 0
}

// CheckGrowth returns ErrInsufficientCapacity if the pool can't commit delta more bytes.
func CheckGrowth(p *Pool, committed, delta uint64) error {
	e := p.SpacePolicy.Effective()
	switch st := SpaceStateOf(p); st {
	case SpaceStateCritical, SpaceStateFull:
		return fmt.Errorf("%w: pool %s is %.0f%% full (%s, critical at %.0f%%): nothing new can be allocated until space is freed",
			ErrInsufficientCapacity, p.Name, UsedPercent(p), st, e.CriticalPercent)
	}
	if limit := LimitBytes(p); committed+delta > limit {
		return fmt.Errorf("%w: pool %s has %s committed of %s allowed (%.2gx overcommit of %s, %.0f%% reserved); this needs %s more",
			ErrInsufficientCapacity, p.Name, humanBytes(committed), humanBytes(limit), e.MaxOvercommit,
			humanBytes(p.TotalBytes), *e.ReservePercent, humanBytes(delta))
	}
	return nil
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
