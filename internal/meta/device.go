package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/velocor/velocor/internal/spdk"
)

// Device reads and writes the slots of a pool's metadata lvol.
type Device interface {
	// WriteSlot writes a slot image (block aligned) to slot 0 or 1 of the lvol.
	WriteSlot(ctx context.Context, lvolUUID string, slot int, img []byte) error
	// ReadSlots returns both slot images.
	ReadSlots(ctx context.Context, lvolUUID string) ([][]byte, error)
	// Recover releases a device left behind by an earlier failure (see UblkDevice.Recover).
	Recover(ctx context.Context) error
}

// UblkDevice reaches the metadata lvol through SPDK's ublk target. The I/O runs in a helper
// process ("velod meta-io"): if spdk_tgt dies while the device is open, I/O on it blocks in the
// kernel until a restarted spdk_tgt recovers the device, and only the helper waits, never velod
// itself. The device in use is recorded first, so it can be recovered after such a failure.
type UblkDevice struct {
	SPDK     *spdk.Client
	Helper   string // path of the velod binary
	StateDir string // e.g. /run/velocor
	ID       uint32 // ublk device ID reserved for metadata I/O
	Timeout  time.Duration
	Logger   *slog.Logger

	mu       sync.Mutex
	targetUp bool // ublk_create_target succeeded on the current spdk_tgt (as far as we know)
}

func (u *UblkDevice) logger() *slog.Logger {
	if u.Logger != nil {
		return u.Logger
	}
	return slog.Default()
}

// ensureTarget creates SPDK's ublk target once per spdk_tgt (each call on an existing target
// makes SPDK log an error).
func (u *UblkDevice) ensureTarget(ctx context.Context) error {
	if u.targetUp {
		return nil
	}
	if err := u.SPDK.UblkCreateTarget(ctx); err != nil {
		return err
	}
	u.targetUp = true
	return nil
}

// startDisk exposes lvol as the metadata device. The previous use of the ID may still be
// finishing its stop; a failure for any other reason may mean spdk_tgt restarted without a
// ublk target, so the target is created again once.
func (u *UblkDevice) startDisk(ctx context.Context, lvol string) error {
	retried := false
	for deadline := time.Now().Add(5 * time.Second); ; {
		err := u.SPDK.UblkStartDisk(ctx, lvol, u.ID)
		switch {
		case err == nil:
			return nil
		case time.Now().After(deadline):
			return err
		case strings.Contains(err.Error(), "exist") || strings.Contains(err.Error(), "busy"):
			time.Sleep(200 * time.Millisecond)
		case !retried:
			retried, u.targetUp = true, false
			if terr := u.ensureTarget(ctx); terr != nil {
				return fmt.Errorf("%w (and creating the ublk target: %v)", err, terr)
			}
		default:
			return err
		}
	}
}

// quiesce waits, before the device is stopped, until the kernel's and udev's own reads of a
// new block device (partition scan, blkid) are done: stopped under them they fail and log
// "I/O error, dev ublkbN" although nothing is wrong.
func (u *UblkDevice) quiesce(since time.Time) {
	inflight := fmt.Sprintf("/sys/block/ublkb%d/inflight", u.ID)
	idle := 0
	for deadline := since.Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		b, err := os.ReadFile(inflight)
		if err != nil {
			return
		}
		if f := strings.Fields(string(b)); len(f) == 2 && f[0] == "0" && f[1] == "0" {
			idle++
		} else {
			idle = 0
		}
		if idle >= 3 && time.Since(since) >= 300*time.Millisecond {
			return
		}
	}
}

// inflight records the ublk device being used, for recovery after a crash.
type inflight struct {
	ID   uint32 `json:"id"`
	Bdev string `json:"bdev"`
}

func (u *UblkDevice) stateFile() string { return filepath.Join(u.StateDir, "meta-ublk.json") }

func (u *UblkDevice) devPath() string { return fmt.Sprintf("/dev/ublkb%d", u.ID) }

func (u *UblkDevice) timeout() time.Duration {
	if u.Timeout > 0 {
		return u.Timeout
	}
	return 30 * time.Second
}

// with exposes the lvol, runs fn on the device path, and always stops the device again.
func (u *UblkDevice) with(ctx context.Context, lvol string, fn func(dev string) error) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.recoverLocked(ctx); err != nil {
		return fmt.Errorf("recover the previous metadata device: %w", err)
	}
	if err := u.ensureTarget(ctx); err != nil {
		return fmt.Errorf("start the ublk target: %w", err)
	}
	rec, _ := json.Marshal(inflight{ID: u.ID, Bdev: lvol})
	if err := os.WriteFile(u.stateFile(), rec, 0o600); err != nil {
		return fmt.Errorf("record the metadata device: %w", err)
	}
	if err := u.startDisk(ctx, lvol); err != nil {
		_ = os.Remove(u.stateFile())
		return fmt.Errorf("expose the metadata lvol: %w", err)
	}
	if err := waitFor(ctx, u.devPath(), 10*time.Second); err != nil {
		_ = u.SPDK.UblkStopDisk(context.WithoutCancel(ctx), u.ID)
		_ = os.Remove(u.stateFile())
		return err
	}
	appeared := time.Now()
	ioErr := fn(u.devPath())
	if errors.Is(ioErr, errHelperStuck) {
		// The helper may be blocked on the device; leave it recorded for Recover.
		return ioErr
	}
	u.quiesce(appeared)
	if err := u.SPDK.UblkStopDisk(context.WithoutCancel(ctx), u.ID); err != nil && ioErr == nil {
		ioErr = fmt.Errorf("stop the metadata device: %w", err)
	}
	_ = os.Remove(u.stateFile())
	return ioErr
}

var errHelperStuck = errors.New("metadata I/O helper did not finish")

func (u *UblkDevice) helper(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, u.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, u.Helper, append([]string{"meta-io"}, args...)...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%w within %s (spdk_tgt may have stopped; the device is recovered after it restarts)", errHelperStuck, u.timeout())
	}
	if err != nil {
		return nil, fmt.Errorf("metadata I/O: %v: %s", err, bytes.TrimSpace(errb.Bytes()))
	}
	return out.Bytes(), nil
}

// WriteSlot implements Device.
func (u *UblkDevice) WriteSlot(ctx context.Context, lvol string, slot int, img []byte) error {
	return u.with(ctx, lvol, func(dev string) error {
		_, err := u.helper(ctx, img, "write", dev, strconv.FormatInt(SlotOffset(slot), 10))
		return err
	})
}

// ReadSlots implements Device.
func (u *UblkDevice) ReadSlots(ctx context.Context, lvol string) ([][]byte, error) {
	var slots [][]byte
	err := u.with(ctx, lvol, func(dev string) error {
		for i := range 2 {
			b, err := u.helper(ctx, nil, "read", dev, strconv.FormatInt(SlotOffset(i), 10), strconv.Itoa(SlotBytes))
			if err != nil {
				return err
			}
			slots = append(slots, b)
		}
		return nil
	})
	return slots, err
}

// Recover implements Device: a metadata device recorded but no longer served (spdk_tgt died
// while it was open) is taken over by the current spdk_tgt and stopped, which completes any
// I/O still waiting on it.
func (u *UblkDevice) Recover(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.recoverLocked(ctx)
}

func (u *UblkDevice) recoverLocked(ctx context.Context) error {
	b, err := os.ReadFile(u.stateFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var rec inflight
	if err := json.Unmarshal(b, &rec); err != nil {
		return fmt.Errorf("parse %s: %w", u.stateFile(), err)
	}
	disks, err := u.SPDK.UblkGetDisks(ctx)
	if err != nil {
		disks = nil // no target yet after a restart
	}
	served := false
	for _, d := range disks {
		if d.ID == rec.ID {
			served = true
		}
	}
	if _, statErr := os.Stat(fmt.Sprintf("/dev/ublkc%d", rec.ID)); !served && statErr == nil {
		// spdk_tgt died with the device open (I/O on it waits in the kernel): take it over.
		u.targetUp = false
		if err := u.ensureTarget(ctx); err != nil {
			return err
		}
		if err := u.SPDK.UblkRecoverDisk(ctx, rec.Bdev, rec.ID); err != nil {
			return fmt.Errorf("recover ublk device %d: %w", rec.ID, err)
		}
		// Recovery completes asynchronously; stopping before it has makes SPDK fail its last
		// recovery step (UBLK_CMD_END_USER_RECOVERY).
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if ds, err := u.SPDK.UblkGetDisks(ctx); err == nil && slices.ContainsFunc(ds, func(d spdk.UblkDisk) bool { return d.ID == rec.ID }) {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
		u.logger().Warn("recovered the metadata device left open when spdk_tgt stopped", "ublk_id", rec.ID, "lvol", rec.Bdev)
		served = true
	}
	if served {
		if err := u.SPDK.UblkStopDisk(ctx, rec.ID); err != nil {
			return fmt.Errorf("stop ublk device %d: %w", rec.ID, err)
		}
	}
	return os.Remove(u.stateFile())
}

func waitFor(ctx context.Context, path string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not appear", path)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// RunHelper is "velod meta-io": direct I/O on the metadata device, outside velod's process.
//
//	meta-io write <dev> <offset>          (data on stdin, a multiple of 4096 bytes)
//	meta-io read  <dev> <offset> <length> (data on stdout)
//
// directIO is false only in tests (tmpfs has no O_DIRECT).
func RunHelper(args []string, stdin io.Reader, stdout io.Writer, directIO bool) error {
	if len(args) < 3 {
		return errors.New("usage: meta-io write <dev> <offset> | read <dev> <offset> <length>")
	}
	op, dev := args[0], args[1]
	off, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil || off%HeaderBytes != 0 {
		return fmt.Errorf("offset %q must be a multiple of %d", args[2], HeaderBytes)
	}
	flags := os.O_RDONLY
	if op == "write" {
		flags = os.O_WRONLY
	}
	if directIO {
		flags |= syscall.O_DIRECT
	}
	f, err := os.OpenFile(dev, flags, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	switch op {
	case "write":
		data, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		if len(data) == 0 || len(data)%HeaderBytes != 0 {
			return fmt.Errorf("write of %d bytes is not a multiple of %d", len(data), HeaderBytes)
		}
		buf := aligned(len(data))
		copy(buf, data)
		if _, err := f.WriteAt(buf, off); err != nil {
			return err
		}
		return f.Sync()
	case "read":
		if len(args) < 4 {
			return errors.New("read needs a length")
		}
		n, err := strconv.Atoi(args[3])
		if err != nil || n%HeaderBytes != 0 {
			return fmt.Errorf("length %q must be a multiple of %d", args[3], HeaderBytes)
		}
		buf := aligned(n)
		if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		_, err = stdout.Write(buf)
		return err
	}
	return fmt.Errorf("unknown operation %q", op)
}

// aligned returns a zeroed buffer whose address is aligned for direct I/O.
func aligned(n int) []byte {
	b := make([]byte, n+HeaderBytes)
	off := 0
	if r := int(uintptrOf(b) % HeaderBytes); r != 0 {
		off = HeaderBytes - r
	}
	return b[off : off+n]
}
