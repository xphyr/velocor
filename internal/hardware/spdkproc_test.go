package hardware

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSPDKInstance(t *testing.T) {
	root := t.TempDir()
	proc := func(pid, argv0, stat string) {
		d := filepath.Join(root, pid)
		_ = os.MkdirAll(d, 0o755)
		_ = os.WriteFile(filepath.Join(d, "cmdline"), []byte(argv0+"\x00-m\x000x1\x00"), 0o644)
		_ = os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o644)
	}
	if got := spdkInstance(root); got != "" {
		t.Fatalf("no process: %q", got)
	}
	// The command name in parentheses can contain spaces; start time is field 22.
	proc("4242", "/usr/bin/spdk_tgt", "4242 (reactor 0) S 1 4242 4242 0 -1 4194560 100 0 0 0 5 6 0 0 20 0 3 0 987654 1000 200")
	proc("77", "/usr/bin/velod", "77 (velod) S 1 77 77 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 11 1 1")
	if got := spdkInstance(root); got != "4242:987654" {
		t.Fatalf("instance %q, want 4242:987654", got)
	}
	proc("5000", "spdk_tgt", "5000 (reactor_0) S 1 5000 5000 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 999 1 1")
	if got := spdkInstance(root); got != "" {
		t.Fatalf("two spdk_tgt processes must be ambiguous, got %q", got)
	}
}
