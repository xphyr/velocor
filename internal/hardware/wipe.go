package hardware

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/velocor/velocor/internal/store"
)

const (
	wipeHeadBytes = 32 << 20 // RAID superblock, the 1 MiB data offset, volume store metadata, partition tables
	wipeTailBytes = 8 << 20  // backup GPT and anything else kept at the end
)

// WipeMetadata zeroes the start and end of a disk so that nothing on it is recognised any
// more: no old array, volume group, partition table or filesystem signature. It destroys the
// disk's data. The disk must not be the boot disk, in use or in an array.
func (m *Manager) WipeMetadata(d *store.Disk) error {
	if err := m.RefreshBootGuard(); err != nil {
		return fmt.Errorf("check which disks are in use: %w", err)
	}
	if d.IsBootDevice || m.IsBootDevice(d.PCIAddress, d.DevicePath) {
		return fmt.Errorf("%w: disk %s backs the OS or is mounted / used as swap", store.ErrBootDeviceProtected, d.ID)
	}
	if d.DevicePath == "" {
		return fmt.Errorf("%w: disk %s has no kernel device to wipe", store.ErrInvalidArgument, d.ID)
	}
	if kname := m.kernelNameForPath(d.DevicePath); kname != "" {
		if h := m.holders(kname); len(h) > 0 {
			return fmt.Errorf("%w: disk %s is in use by %s", store.ErrInUse, d.ID, strings.Join(h, ", "))
		}
	}
	// O_EXCL on a block device fails while anything else (a mount, md) holds it.
	f, err := os.OpenFile(d.DevicePath, os.O_WRONLY|syscall.O_EXCL, 0)
	if err != nil {
		return fmt.Errorf("open %s exclusively: %w", d.DevicePath, err)
	}
	defer func() { _ = f.Close() }()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("size of %s: %w", d.DevicePath, err)
	}
	zero := make([]byte, 1<<20)
	zeroRange := func(off, n int64) error {
		for n > 0 {
			chunk := min(n, int64(len(zero)))
			if _, err := f.WriteAt(zero[:chunk], off); err != nil {
				return fmt.Errorf("zero %s at %d: %w", d.DevicePath, off, err)
			}
			off, n = off+chunk, n-chunk
		}
		return nil
	}
	if err := zeroRange(0, min(size, wipeHeadBytes)); err != nil {
		return err
	}
	if tail := max(size-wipeTailBytes, min(size, wipeHeadBytes)); tail < size {
		if err := zeroRange(tail, size-tail); err != nil {
			return err
		}
	}
	return f.Sync()
}
