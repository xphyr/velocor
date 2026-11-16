package hardware

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/store"
)

func newWriteCacheManager(t *testing.T) (*Manager, string, string) {
	t.Helper()
	root := t.TempDir()
	sysfs, devfs, procfs := filepath.Join(root, "sys"), filepath.Join(root, "dev"), filepath.Join(root, "proc")
	mustWriteFile(t, filepath.Join(procfs, "self", "mountinfo"), "")
	mustWriteFile(t, filepath.Join(procfs, "swaps"), "Filename Type Size Used Priority\n")
	m, err := NewManager(Config{SysfsRoot: sysfs, ProcRoot: procfs, DevRoot: devfs})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, sysfs, devfs
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return strings.TrimSpace(string(b))
}

func TestWriteCacheState(t *testing.T) {
	m, sysfs, _ := newWriteCacheManager(t)
	mustWriteFile(t, filepath.Join(sysfs, "block", "sdb", "queue", "write_cache"), "write back\n")
	mustWriteFile(t, filepath.Join(sysfs, "block", "sdc", "queue", "write_cache"), "write through\n")
	for kname, want := range map[string]store.WriteCacheState{
		"sdb": store.WriteCacheWriteBack, "sdc": store.WriteCacheWriteThrough, "sdz": store.WriteCacheUnknown, "": store.WriteCacheUnknown,
	} {
		if got := m.writeCacheState(kname); got != want {
			t.Errorf("writeCacheState(%q) = %q, want %q", kname, got, want)
		}
	}
}

// The drive's setting is changed through scsi_disk/*/cache_type (MODE SELECT), never through
// the "temporary" values that only change the kernel's view; the result is verified by
// re-reading the kernel's state.
func TestSetWriteCacheSCSI(t *testing.T) {
	m, sysfs, devfs := newWriteCacheManager(t)
	cacheType := filepath.Join(sysfs, "block", "sdb", "device", "scsi_disk", "1:0:0:0", "cache_type")
	queue := filepath.Join(sysfs, "block", "sdb", "queue", "write_cache")
	mustWriteFile(t, cacheType, "write back\n")
	mustWriteFile(t, filepath.Join(devfs, "sdb"), "")
	d := &store.Disk{ID: "ssd-1", Type: store.DiskTypeSSD, DevicePath: filepath.Join(devfs, "sdb")}

	// A drive that ignores the request: the kernel still reports write back, so it's an error.
	mustWriteFile(t, queue, "write back\n")
	if state, err := m.SetWriteCache(d, false); err == nil || state != store.WriteCacheWriteBack {
		t.Fatalf("drive refusing the change: got %q, %v; want WRITE_BACK and an error", state, err)
	}
	if got := readFile(t, cacheType); got != "write through" {
		t.Fatalf("cache_type = %q, want \"write through\"", got)
	}

	// A drive that accepts it (the kernel revalidates and reports write through).
	mustWriteFile(t, queue, "write through\n")
	if state, err := m.SetWriteCache(d, false); err != nil || state != store.WriteCacheWriteThrough {
		t.Fatalf("disable: got %q, %v", state, err)
	}

	// Re-enabling writes "write back".
	mustWriteFile(t, queue, "write back\n")
	if state, err := m.SetWriteCache(d, true); err != nil || state != store.WriteCacheWriteBack {
		t.Fatalf("enable: got %q, %v", state, err)
	}
	if got := readFile(t, cacheType); got != "write back" {
		t.Fatalf("cache_type = %q, want \"write back\"", got)
	}
}

func TestSetWriteCacheVirtio(t *testing.T) {
	m, sysfs, devfs := newWriteCacheManager(t)
	mustWriteFile(t, filepath.Join(sysfs, "block", "vdb", "cache_type"), "write back\n")
	mustWriteFile(t, filepath.Join(sysfs, "block", "vdb", "queue", "write_cache"), "write through\n")
	mustWriteFile(t, filepath.Join(devfs, "vdb"), "")
	d := &store.Disk{ID: "ssd-v", Type: store.DiskTypeSSD, DevicePath: filepath.Join(devfs, "vdb")}
	if state, err := m.SetWriteCache(d, false); err != nil || state != store.WriteCacheWriteThrough {
		t.Fatalf("virtio disable: got %q, %v", state, err)
	}
	if got := readFile(t, filepath.Join(sysfs, "block", "vdb", "cache_type")); got != "write through" {
		t.Fatalf("cache_type = %q", got)
	}
}

func TestSetWriteCacheUnsupported(t *testing.T) {
	m, _, devfs := newWriteCacheManager(t)
	for _, d := range []*store.Disk{
		{ID: "nvme-1", Type: store.DiskTypeNVMe, DevicePath: filepath.Join(devfs, "nvme1n1")},
		{ID: "ssd-x", Type: store.DiskTypeSSD, DevicePath: filepath.Join(devfs, "sdx")}, // no cache_type file
	} {
		if _, err := m.SetWriteCache(d, false); !errors.Is(err, ErrWriteCacheUnsupported) {
			t.Errorf("%s: got %v, want ErrWriteCacheUnsupported", d.ID, err)
		}
	}
}
