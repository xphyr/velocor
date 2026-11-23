package kernel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
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

// WrapperSuspended reports whether the wrapper is suspended (velod resumes wrappers left
// suspended by a crash in the middle of a group snapshot).
func (p *Plane) WrapperSuspended(ctx context.Context, volume string) (bool, error) {
	out, err := p.Run.Run(ctx, "dmsetup", "info", "-c", "--noheadings", "-o", "suspended", WrapperName(volume))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "Suspended", nil
}

// GroupMember is one volume of a group snapshot.
type GroupMember struct {
	Volume string // wrapper
	VG, LV string // the thin volume to snapshot
	Snap   string // the snapshot's LV name
}

// GroupSnapshot takes snapshots of several volumes at one instant: it suspends every member's
// wrapper (I/O in flight completes, new I/O waits), snapshots each thin volume while all are
// suspended, then resumes them all. No write that completed before the call is missing from its
// volume's snapshot, and no write submitted after the first suspend is in any. On failure the
// snapshots already taken are removed and every wrapper is resumed; the error names the step.
// The context's deadline bounds the whole operation (velod gives it a few seconds, as SPDK patch
// 0003 did).
func (p *Plane) GroupSnapshot(ctx context.Context, members []GroupMember) error {
	var suspended []string
	var taken []GroupMember
	resumeAll := func() error {
		var errs []error
		for _, v := range suspended {
			// A fresh context: resuming must happen even when ctx has run out.
			if err := p.ResumeWrapper(context.WithoutCancel(ctx), v); err != nil {
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
	for _, m := range members {
		if err := p.SuspendWrapper(ctx, m.Volume); err != nil {
			return fail(fmt.Errorf("suspend %s: %w", m.Volume, err))
		}
		suspended = append(suspended, m.Volume)
	}
	for _, m := range members {
		if err := p.Snapshot(ctx, m.VG, m.LV, m.Snap); err != nil {
			return fail(fmt.Errorf("snapshot %s/%s: %w", m.VG, m.LV, err))
		}
		taken = append(taken, m)
	}
	return resumeAll()
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
