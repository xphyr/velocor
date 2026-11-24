package kernel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// WrapperName is the device-mapper name of a volume's wrapper; its device is
// DevRoot/mapper/<WrapperName>.
func WrapperName(volume string) string { return "velocor-" + volume }

// WrapperPath is the wrapper's device node, the device the targets export.
func (p *Plane) WrapperPath(volume string) string {
	return p.DevRoot + "/mapper/" + WrapperName(volume)
}

// EnsureWrapper makes the volume's wrapper a dm-linear map of all of lower: created when
// missing, reloaded when lower changed or grew (a resize).
func (p *Plane) EnsureWrapper(ctx context.Context, volume, lower string) error {
	if p.WrapperCurrent(volume, lower) {
		return nil
	}
	sectors, err := p.blockSectors(ctx, lower)
	if err != nil {
		return err
	}
	table := fmt.Sprintf("0 %d linear %s 0", sectors, lower)
	name := WrapperName(volume)
	cur, err := p.Run.Run(ctx, "dmsetup", "table", name)
	if err != nil {
		_, err = p.Run.Run(ctx, "dmsetup", "create", name, "--table", table)
		return err
	}
	// dmsetup reports the device as major:minor; compare the size and the target device.
	want, err := p.Run.Run(ctx, "stat", "-L", "-c", "%t:%T", lower)
	if err != nil {
		return err
	}
	f := strings.Fields(strings.TrimSpace(string(cur)))
	if len(f) == 5 && f[1] == strconv.FormatUint(sectors, 10) && sameDevNum(f[3], strings.TrimSpace(string(want))) {
		return nil
	}
	if _, err := p.Run.Run(ctx, "dmsetup", "load", name, "--table", table); err != nil {
		return err
	}
	_, err = p.Run.Run(ctx, "dmsetup", "resume", name)
	return err
}

// RemoveWrapper removes a volume's wrapper (after its exports are gone).
func (p *Plane) RemoveWrapper(ctx context.Context, volume string) error {
	_, err := p.Run.Run(ctx, "dmsetup", "remove", "--retry", WrapperName(volume))
	return err
}

// SuspendWrapper waits for the I/O in flight to the volume to finish and holds new I/O until
// ResumeWrapper. No filesystem is frozen: the device belongs to a remote host.
func (p *Plane) SuspendWrapper(ctx context.Context, volume string) error {
	_, err := p.Run.Run(ctx, "dmsetup", "suspend", "--nolockfs", WrapperName(volume))
	return err
}

func (p *Plane) ResumeWrapper(ctx context.Context, volume string) error {
	_, err := p.Run.Run(ctx, "dmsetup", "resume", WrapperName(volume))
	return err
}

// SuspendedWrappers lists the suspended wrappers' volumes (velod resumes wrappers left suspended
// by a crash in the middle of a group snapshot).
func (p *Plane) SuspendedWrappers(ctx context.Context) ([]string, error) {
	out, err := p.Run.Run(ctx, "dmsetup", "info", "-c", "--noheadings", "--separator", " ", "-o", "name,suspended")
	if err != nil {
		return nil, err
	}
	var vols []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || f[1] != "Suspended" {
			continue
		}
		if v, ok := strings.CutPrefix(f[0], WrapperName("")); ok {
			vols = append(vols, v)
		}
	}
	return vols, nil
}

// dmsetupAll runs one dmsetup command on several volumes' wrappers.
func (p *Plane) dmsetupAll(ctx context.Context, volumes []string, args ...string) error {
	for _, v := range volumes {
		args = append(args, WrapperName(v))
	}
	_, err := p.Run.Run(ctx, "dmsetup", args...)
	return err
}

// WrapperSuspended reports whether the wrapper is suspended.
func (p *Plane) WrapperSuspended(ctx context.Context, volume string) (bool, error) {
	out, err := p.Run.Run(ctx, "dmsetup", "info", "-c", "--noheadings", "-o", "suspended", WrapperName(volume))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "Suspended", nil
}

// GroupMember is one volume of a group snapshot.
type GroupMember struct {
	Volume string   // wrapper
	VG, LV string   // the thin volume to snapshot
	Snap   string   // the snapshot's LV name
	Tags   []string // LVM tags for the snapshot
}

// GroupSnapshot takes snapshots of several volumes at one instant: it suspends every member's
// wrapper (I/O in flight completes, new I/O waits), snapshots each thin volume while all are
// suspended, then resumes them all. No write that completed before the call is missing from its
// volume's snapshot, and no write submitted after the first suspend is in any. On failure the
// snapshots already taken are removed and every wrapper is resumed; the error names the step.
// The context's deadline bounds the whole operation.
func (p *Plane) GroupSnapshot(ctx context.Context, members []GroupMember) error {
	return p.GroupSnapshotWithin(ctx, members, 0)
}

// GroupSnapshotWithin is GroupSnapshot with quiesce (if > 0) bounding only the pause: in-flight
// I/O must drain from every member within it (velod gives it a few seconds), or no snapshot is taken and the error wraps context.DeadlineExceeded. Taking the
// snapshots afterwards is bounded by ctx alone.
func (p *Plane) GroupSnapshotWithin(ctx context.Context, members []GroupMember, quiesce time.Duration) error {
	var suspended []string
	var taken []GroupMember
	resumeAll := func() error {
		if len(suspended) == 0 {
			return nil
		}
		// A fresh context: resuming must happen even when ctx has run out. All at once; dmsetup
		// stops at the first that fails, so then one by one.
		cleanup := context.WithoutCancel(ctx)
		if p.dmsetupAll(cleanup, suspended, "resume") == nil {
			return nil
		}
		var errs []error
		for _, v := range suspended {
			if err := p.ResumeWrapper(cleanup, v); err != nil {
				errs = append(errs, fmt.Errorf("resume %s: %w", v, err))
			}
		}
		return errors.Join(errs...)
	}
	fail := func(err error) error {
		rerr := resumeAll()
		for _, m := range taken {
			if e := p.RemoveVolume(context.WithoutCancel(ctx), m.VG, m.Snap); e != nil {
				rerr = errors.Join(rerr, fmt.Errorf("remove %s/%s: %w", m.VG, m.Snap, e))
			}
		}
		return errors.Join(err, rerr)
	}
	sctx, cancel := ctx, context.CancelFunc(func() {})
	if quiesce > 0 {
		sctx, cancel = context.WithTimeout(ctx, quiesce)
	}
	defer cancel()
	// All counted as suspended even if the command fails: it may have got to some of them, and
	// killed at the deadline, dmsetup may already have suspended one (it still waits for udev
	// then). Resuming a wrapper that isn't suspended does nothing.
	for _, m := range members {
		suspended = append(suspended, m.Volume)
	}
	if err := p.dmsetupAll(sctx, suspended, "suspend", "--nolockfs"); err != nil {
		if sctx.Err() != nil {
			err = fmt.Errorf("%w: %v", sctx.Err(), err)
		}
		return fail(fmt.Errorf("suspend %s: %w", strings.Join(suspended, ", "), err))
	}
	for _, m := range members {
		if err := p.Snapshot(ctx, m.VG, m.LV, m.Snap, m.Tags...); err != nil {
			return fail(fmt.Errorf("snapshot %s/%s: %w", m.VG, m.LV, err))
		}
		taken = append(taken, m)
	}
	if err := resumeAll(); err != nil {
		return fail(err) // which tries again, and removes the snapshots the caller won't know of
	}
	return nil
}

func (p *Plane) blockSectors(ctx context.Context, dev string) (uint64, error) {
	out, err := p.Run.Run(ctx, "blockdev", "--getsz", dev)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
}

// sameDevNum compares dmsetup's "253:4" with stat's hex "fd:4".
func sameDevNum(dm, statHex string) bool {
	maj, min, ok := strings.Cut(statHex, ":")
	if !ok {
		return false
	}
	a, err1 := strconv.ParseUint(maj, 16, 32)
	b, err2 := strconv.ParseUint(min, 16, 32)
	return err1 == nil && err2 == nil && dm == fmt.Sprintf("%d:%d", a, b)
}
