package hardware

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/velocor/velocor/internal/store"
)

// ErrWriteCacheUnsupported is returned when Velocor can't switch a disk's write cache itself
// from the kernel side (e.g. NVMe drives, which are handled through nvme-cli once claimed).
var ErrWriteCacheUnsupported = errors.New("switching the write cache of this disk from the kernel is not supported")

// writeCacheState reads the kernel's view of a block device's volatile write cache. For NVMe
// it reflects the controller's VWC bit, for SCSI/SATA the caching mode page's WCE bit.
func (m *Manager) writeCacheState(kname string) store.WriteCacheState {
	if kname == "" {
		return store.WriteCacheUnknown
	}
	switch readSysfsString(filepath.Join(m.cfg.SysfsRoot, "block", kname, "queue", "write_cache")) {
	case "write back":
		return store.WriteCacheWriteBack
	case "write through":
		return store.WriteCacheWriteThrough
	}
	return store.WriteCacheUnknown
}

// cacheTypeFile is the sysfs file through which the kernel changes the drive's own cache
// setting: for SCSI and SATA disks writing it issues MODE SELECT (WCE), for virtio-blk it
// updates the device's writeback configuration. The "temporary ..." values would only change
// the kernel's view, never the drive, so they are never written.
func (m *Manager) cacheTypeFile(kname string) (string, error) {
	if matches, _ := filepath.Glob(filepath.Join(m.cfg.SysfsRoot, "block", kname, "device", "scsi_disk", "*", "cache_type")); len(matches) == 1 {
		return matches[0], nil
	}
	if f := filepath.Join(m.cfg.SysfsRoot, "block", kname, "cache_type"); fileExists(f) {
		return f, nil // virtio-blk
	}
	return "", fmt.Errorf("%w (%s)", ErrWriteCacheUnsupported, kname)
}

// SetWriteCache switches the drive's volatile write cache on or off and returns the state the
// kernel reports afterwards. Only for kernel-attached disks (SATA/SAS/virtio SSDs).
func (m *Manager) SetWriteCache(d *store.Disk, enabled bool) (store.WriteCacheState, error) {
	if d.Type != store.DiskTypeSSD {
		return store.WriteCacheUnknown, fmt.Errorf("%w (%s is %s)", ErrWriteCacheUnsupported, d.ID, d.Type)
	}
	kname := m.kernelNameForPath(d.DevicePath)
	if kname == "" || strings.Contains(kname, "/") {
		return store.WriteCacheUnknown, fmt.Errorf("cannot resolve %s to a kernel device", d.DevicePath)
	}
	f, err := m.cacheTypeFile(kname)
	if err != nil {
		return store.WriteCacheUnknown, err
	}
	want, value := store.WriteCacheWriteThrough, "write through"
	if enabled {
		want, value = store.WriteCacheWriteBack, "write back"
	}
	if readSysfsString(f) != value {
		if err := os.WriteFile(f, []byte(value), 0); err != nil {
			var pe *fs.PathError
			if errors.As(err, &pe) {
				err = pe.Err // the sysfs path says nothing to an operator
			}
			return m.writeCacheState(kname), fmt.Errorf("%s refused %q: %w", kname, value, err)
		}
	}
	got := m.writeCacheState(kname)
	if got != want {
		return got, fmt.Errorf("write cache of %s is %q after setting %q: the drive did not accept the change", kname, got, value)
	}
	return got, nil
}

// RefreshWriteCache re-reads a kernel-attached disk's write cache state.
func (m *Manager) RefreshWriteCache(d *store.Disk) store.WriteCacheState {
	if d.DevicePath == "" {
		return store.WriteCacheUnknown
	}
	return m.writeCacheState(m.kernelNameForPath(d.DevicePath))
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
