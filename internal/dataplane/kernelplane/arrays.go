package kernelplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/kernel"
	"github.com/velocor/velocor/internal/store"
)

// raidSpeedLimitMax is md's global resync/rebuild limit (KiB/s); 200000 is the kernel default.
var raidSpeedLimitMax = "/proc/sys/dev/raid/speed_limit_max"

const defaultSpeedLimitMax = 200000

// Arrays lists the running md arrays and the velod arrays waiting for members.
func (p *Plane) Arrays(ctx context.Context) ([]dataplane.Array, error) {
	var out []dataplane.Array
	for _, name := range p.k.ListArrays() {
		st, err := p.k.ArrayStatus(name)
		if err != nil {
			continue // stopped between the listing and now
		}
		out = append(out, p.arrayOf(name, st))
	}
	p.mu.Lock()
	waiting := make([]kernel.ExaminedArray, 0, len(p.waiting))
	for _, a := range p.waiting {
		waiting = append(waiting, a)
	}
	p.mu.Unlock()
	for _, a := range waiting {
		if slices.Contains(p.k.ListArrays(), a.Name) {
			continue
		}
		out = append(out, p.waitingArray(ctx, a))
	}
	return out, nil
}

func (p *Plane) arrayOf(name string, st *kernel.ArrayStatus) dataplane.Array {
	a := dataplane.Array{
		Name:        name,
		UUID:        st.UUID,
		Level:       st.Level,
		Slots:       uint32(st.RaidDisks),
		Operational: uint32(max(st.RaidDisks-st.Degraded, 0)),
		Members:     make([]dataplane.ArrayMember, st.RaidDisks),
	}
	switch st.ArrayState {
	case "inactive":
		a.State = dataplane.ArrayConfiguring
	case "broken", "clear", "":
		a.State = dataplane.ArrayOffline
	default:
		a.State = dataplane.ArrayOnline
	}
	if st.Level == "raid0" {
		a.Operational = uint32(st.RaidDisks) // md reports no degraded count for raid0
	}
	for _, m := range st.Members {
		if m.Slot < 0 || m.Slot >= len(a.Members) || m.Has("faulty") {
			continue // spares and failed members hold no slot
		}
		a.Members[m.Slot] = dataplane.ArrayMember{Name: p.diskHandle(m.Device), Configured: m.Has("in_sync"), BadBlockBytes: m.BadSectors() * 512}
		a.Discovered++
		if st.SyncAction == "recover" && !m.Has("in_sync") {
			pct := uint32(0)
			if st.SyncTotal > 0 {
				pct = uint32(st.SyncDone * 100 / st.SyncTotal)
			}
			a.Rebuild = &dataplane.Rebuild{Target: a.Members[m.Slot].Name, Percent: pct}
		}
	}
	a.Scrub = p.scrubOf(name, st)
	a.UnreadableBytes = unreadableOf(st).sectors * 512
	return a
}

// waitingArray describes an incomplete array from its members' metadata, members in their
// slots and the missing ones empty, like SPDK's configuring RAID.
func (p *Plane) waitingArray(ctx context.Context, e kernel.ExaminedArray) dataplane.Array {
	a := dataplane.Array{
		Name: e.Name, UUID: e.UUID, State: dataplane.ArrayConfiguring, Level: e.Level,
		Slots: uint32(e.Devices), Members: make([]dataplane.ArrayMember, e.Devices),
	}
	for i := range a.Members {
		a.Members[i].UUID = "missing" // awaited: its slot has a member somewhere
	}
	for _, dev := range e.Found {
		slot, err := p.k.MemberRole(ctx, dev)
		if err != nil || slot < 0 || slot >= len(a.Members) {
			continue
		}
		a.Members[slot] = dataplane.ArrayMember{Name: p.diskHandle(kernelName(dev)), UUID: e.UUID, Configured: true}
		a.Discovered++
	}
	a.Operational = a.Discovered
	return a
}

func (p *Plane) CreateArray(ctx context.Context, name string, level store.RaidLevel, stripKB uint32, members []string) error {
	_, err := p.k.CreateArray(ctx, kernel.ArraySpec{Name: name, Level: strings.ToLower(string(level)), Devices: members, ChunkKB: stripKB})
	return err
}

// DeleteArray stops the array and erases md's metadata from its members (its volume group is
// already gone: DeleteThinPool).
func (p *Plane) DeleteArray(ctx context.Context, name string) error {
	p.mu.Lock()
	w, waiting := p.waiting[name]
	delete(p.waiting, name)
	delete(p.scrubs, name)
	p.saveScrubsLocked()
	p.mu.Unlock()
	var members []string
	if st, err := p.k.ArrayStatus(name); err == nil {
		for _, m := range st.Members {
			members = append(members, filepath.Join(p.k.DevRoot, m.Device))
		}
		if err := p.k.StopArray(ctx, name); err != nil {
			return err
		}
	} else if waiting {
		members = w.Found
	} else {
		return notFound("array %s not found", name)
	}
	var errs []error
	for _, m := range members {
		if err := p.k.ZeroSuperblock(ctx, m); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// AddMember adds a disk; md rebuilds onto it when the array has an empty slot. Members that
// failed are removed first, so their slots can be reused.
func (p *Plane) AddMember(ctx context.Context, array, member string) error {
	if _, err := p.k.ArrayStatus(array); err != nil {
		return notFound("array %s not found", array)
	}
	_, _ = p.k.Run.Run(ctx, "mdadm", p.k.ArrayPath(array), "--remove", "failed")
	return p.k.AddMember(ctx, array, member)
}

// RemoveMember fails a member and takes it out of its array.
func (p *Plane) RemoveMember(ctx context.Context, member string) error {
	kname := kernelName(member)
	for _, name := range p.k.ListArrays() {
		st, err := p.k.ArrayStatus(name)
		if err != nil {
			continue
		}
		for _, m := range st.Members {
			if m.Device != kname {
				continue
			}
			dev := filepath.Join(p.k.DevRoot, kname)
			if !m.Has("faulty") {
				if err := p.k.FailMember(ctx, name, dev); err != nil {
					return err
				}
			}
			// md lets go of a failed member asynchronously (longer while it resyncs): until
			// then the remove fails with EBUSY.
			var err error
			for deadline := time.Now().Add(15 * time.Second); ; {
				if err = p.k.RemoveMember(ctx, name, dev); err == nil || time.Now().After(deadline) || ctx.Err() != nil {
					return err
				}
				time.Sleep(250 * time.Millisecond)
			}
		}
	}
	return notFound("%s is not in an array", member)
}

// StartDegraded starts a waiting array with the members it has.
func (p *Plane) StartDegraded(ctx context.Context, array string) error {
	p.mu.Lock()
	w, ok := p.waiting[array]
	p.mu.Unlock()
	if !ok {
		return notFound("array %s is not waiting for members", array)
	}
	if err := p.k.AssembleArray(ctx, w.Name, w.UUID, w.Found, true); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.waiting, array)
	p.mu.Unlock()
	return nil
}

// RebuildLimit is md's global limit in MB/s; 0 when it is the kernel's default.
func (p *Plane) RebuildLimit(context.Context) (uint32, error) {
	b, err := os.ReadFile(raidSpeedLimitMax)
	if err != nil {
		return 0, err
	}
	kib, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
	if err != nil {
		return 0, err
	}
	if kib == defaultSpeedLimitMax {
		return 0, nil
	}
	return uint32(kib / 1024), nil
}

func (p *Plane) SetRebuildLimit(_ context.Context, mbps uint32) error {
	kib := uint64(defaultSpeedLimitMax)
	if mbps > 0 {
		kib = uint64(mbps) * 1024
	}
	return os.WriteFile(raidSpeedLimitMax, []byte(strconv.FormatUint(kib, 10)), 0o644)
}

// --- scrub ---

// StartScrub starts md's check (or repair) at offsetBlocks (sectors), limited to mbps.
func (p *Plane) StartScrub(_ context.Context, array string, repair bool, mbps uint32, offsetBlocks uint64) error {
	if _, err := p.k.ArrayStatus(array); err != nil {
		return notFound("array %s not found", array)
	}
	speed := "system"
	if mbps > 0 {
		speed = strconv.FormatUint(uint64(mbps)*1024, 10)
	}
	if err := p.k.SetSyncSpeedMax(array, speed); err != nil {
		return err
	}
	if err := p.k.SetSyncMin(array, offsetBlocks); err != nil {
		return err
	}
	if err := p.k.StartScrub(array, repair); err != nil {
		return err
	}
	p.mu.Lock()
	p.scrubs[array] = &scrubRun{Repair: repair, Start: offsetBlocks}
	p.saveScrubsLocked()
	p.mu.Unlock()
	return nil
}

func (p *Plane) StopScrub(_ context.Context, array string) error {
	if _, err := p.k.ArrayStatus(array); err != nil {
		return notFound("array %s not found", array)
	}
	p.mu.Lock()
	if r := p.scrubs[array]; r != nil {
		r.Stopped = true
		p.saveScrubsLocked()
	}
	p.mu.Unlock()
	if err := p.k.StopScrub(array); err != nil {
		return err
	}
	return p.k.SetSyncSpeedMax(array, "system")
}

// scrubOf reports the running scrub from md, or the last one this plane started: md says
// only that the last action was a check or repair, so whether it finished or was stopped,
// and where it began, come from the plane's own record.
func (p *Plane) scrubOf(name string, st *kernel.ArrayStatus) *dataplane.Scrub {
	p.mu.Lock()
	run := p.scrubs[name]
	p.mu.Unlock()
	sc := &dataplane.Scrub{MismatchedBlocks: st.MismatchCnt}
	switch {
	case st.SyncAction == "check" || st.SyncAction == "repair":
		sc.State, sc.Repair = dataplane.ScrubRunning, st.SyncAction == "repair"
		sc.OffsetBlocks, sc.TotalBlocks = st.SyncDone, st.SyncTotal
		if run != nil {
			sc.StartOffsetBlocks = run.Start
		}
	case run != nil && (st.LastSyncAction == "check" || st.LastSyncAction == "repair"):
		sc.Repair, sc.StartOffsetBlocks = run.Repair, run.Start
		sc.TotalBlocks = p.sizeOf(st.Kernel) / 512
		if st.Level == "raid1" {
			sc.TotalBlocks = p.componentSectors(st.Kernel, sc.TotalBlocks)
		}
		sc.State, sc.OffsetBlocks = dataplane.ScrubCompleted, sc.TotalBlocks
		if run.Stopped {
			sc.State, sc.OffsetBlocks = dataplane.ScrubStopped, 0
		}
		_ = p.k.SetSyncSpeedMax(name, "system")
	default:
		return nil
	}
	if sc.Repair {
		sc.MismatchRepairedBlocks = sc.MismatchedBlocks
	}
	return sc
}

// componentSectors is md's component_size (KiB) in sectors: what a raid1 sync covers.
func (p *Plane) componentSectors(kname string, fallback uint64) uint64 {
	b, err := os.ReadFile(filepath.Join(p.k.SysRoot, "block", kname, "md", "component_size"))
	if err != nil {
		return fallback
	}
	kib, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return fallback
	}
	return kib * 2
}

// scrubOf needs the scrubs started before a velod restart: they are kept in StateDir.

func (p *Plane) scrubsFile() string {
	if p.StateDir == "" {
		return ""
	}
	return filepath.Join(p.StateDir, "scrubs.json")
}

// LoadState reads the scrubs kept in StateDir (velod calls it once, after setting StateDir).
func (p *Plane) LoadState() error {
	f := p.scrubsFile()
	if f == "" {
		return nil
	}
	b, err := os.ReadFile(f)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	runs := map[string]*scrubRun{}
	if err := json.Unmarshal(b, &runs); err != nil {
		return fmt.Errorf("%s: %w", f, err)
	}
	p.mu.Lock()
	p.scrubs = runs
	p.mu.Unlock()
	return nil
}

// saveScrubsLocked writes the scrubs (p.mu held); best effort: losing it only loses where a
// scrub began.
func (p *Plane) saveScrubsLocked() {
	f := p.scrubsFile()
	if f == "" {
		return
	}
	b, err := json.Marshal(p.scrubs)
	if err == nil {
		tmp := f + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, f)
		}
	}
	if err != nil {
		slog.Warn("cannot keep the scrub state", "file", f, "err", err)
	}
}
