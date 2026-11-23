package reconciler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/dataplane/spdkplane"
	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/store"
)

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A drive that refuses to turn its write cache off is recorded on the disk and tried again
// only after a restart, instead of filling every pass's degraded resources and errors.
func TestRefusedWriteCacheIsRecordedNotRetried(t *testing.T) {
	e := newEnv(t)
	root := t.TempDir()
	sysfs, devfs, procfs := filepath.Join(root, "sys"), filepath.Join(root, "dev"), filepath.Join(root, "proc")
	writeFile(t, filepath.Join(procfs, "self", "mountinfo"), "")
	writeFile(t, filepath.Join(procfs, "swaps"), "Filename Type Size Used Priority\n")
	cacheType := filepath.Join(sysfs, "block", "sdb", "device", "scsi_disk", "1:0:0:0", "cache_type")
	writeFile(t, cacheType, "write back\n")
	writeFile(t, filepath.Join(sysfs, "block", "sdb", "queue", "write_cache"), "write back\n") // the drive ignores the change
	writeFile(t, filepath.Join(devfs, "sdb"), "")
	hw, err := hardware.NewManager(hardware.Config{SysfsRoot: sysfs, ProcRoot: procfs, DevRoot: devfs})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpsertDisk(&store.Disk{ID: "ssd-1", Name: "sdb", Type: store.DiskTypeSSD, DevicePath: filepath.Join(devfs, "sdb"),
		WriteCache: store.WriteCacheWriteBack, WriteCachePolicy: store.WriteCachePolicyDisabled}); err != nil {
		t.Fatal(err)
	}
	rec := func() *Reconciler {
		r := New(e.st, spdkplane.New(e.fake.Client(), hw), hw, nil)
		r.discover = func() ([]*store.Disk, error) { return []*store.Disk{}, nil }
		return r
	}
	r := rec()

	st, _ := r.ReconcileOnce(e.ctx)
	if strings.Contains(strings.Join(st.DegradedResources, " "), "ssd-1") || strings.Contains(st.LastError, "write cache") {
		t.Fatalf("refusal reported on the pass: %v %q", st.DegradedResources, st.LastError)
	}
	d, _ := e.st.GetDisk("ssd-1")
	if !strings.Contains(d.WriteCacheError, `refused "write through"`) && !strings.Contains(d.WriteCacheError, "did not accept") {
		t.Fatalf("WriteCacheError = %q", d.WriteCacheError)
	}

	writeFile(t, cacheType, "write back\n")
	_, _ = r.ReconcileOnce(e.ctx)
	if got := strings.TrimSpace(readSysfs(t, cacheType)); got != "write back" {
		t.Fatalf("retried on the next pass (cache_type %q)", got)
	}

	// After a restart it is tried once more; a drive that now accepts clears the error.
	writeFile(t, filepath.Join(sysfs, "block", "sdb", "queue", "write_cache"), "write through\n")
	_, _ = rec().ReconcileOnce(e.ctx)
	if d, _ := e.st.GetDisk("ssd-1"); d.WriteCacheError != "" || d.WriteCache != store.WriteCacheWriteThrough {
		t.Fatalf("after the restart: %q %s", d.WriteCacheError, d.WriteCache)
	}
}

func readSysfs(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
