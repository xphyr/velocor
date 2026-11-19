package hardware

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// fakeProc builds a /proc with one spdk_tgt process (pid 4242) and an unrelated one.
func fakeProc(t *testing.T, label, seccomp, memlock string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("1/cmdline", "/usr/lib/systemd/systemd\x00--switched-root\x00")
	write("4242/cmdline", "spdk_tgt\x00-r\x00/run/velocor/spdk.sock\x00")
	write("4242/status", "Name:\treactor_0\nSeccomp:\t"+seccomp+"\nSeccomp_filters:\t0\n")
	write("4242/attr/current", label+"\x00")
	write("4242/limits", "Limit                     Soft Limit           Hard Limit           Units\nMax locked memory         "+memlock+"              "+memlock+"              bytes\n")
	write("sys/kernel/io_uring_disabled", "0\n")
	return root
}

func TestProbeIOURing(t *testing.T) {
	ok := func() error { return nil }
	enforcing := filepath.Join(t.TempDir(), "enforce")
	if err := os.WriteFile(enforcing, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	spc, confined := "unconfined_u:unconfined_r:spc_t:s0", "system_u:system_r:container_t:s0:c6,c850"
	for _, tc := range []struct {
		name      string
		proc      string
		enforce   string
		setup     func() error
		release   string
		disabled  string
		usable    bool
		reasonHas string
	}{
		{name: "appliance: spc_t, no seccomp", proc: fakeProc(t, spc, "0", "unlimited"), enforce: enforcing, usable: true},
		{name: "8 MiB memlock is fine on a modern kernel", proc: fakeProc(t, spc, "0", "8388608"), enforce: enforcing, usable: true},
		{name: "8 MiB memlock on 5.10", proc: fakeProc(t, spc, "0", "8388608"), enforce: enforcing, release: "5.10.0-28-amd64", reasonHas: "locked-memory"},
		{name: "confined container", proc: fakeProc(t, confined, "0", "unlimited"), enforce: enforcing, reasonHas: "container_t"},
		{name: "confined, SELinux not enforcing", proc: fakeProc(t, confined, "0", "unlimited"), enforce: "/nonexistent", usable: true},
		{name: "seccomp filter", proc: fakeProc(t, spc, "2", "unlimited"), enforce: enforcing, reasonHas: "seccomp"},
		{name: "sysctl disables io_uring", proc: fakeProc(t, spc, "0", "unlimited"), enforce: enforcing, disabled: "2", reasonHas: "io_uring_disabled=2"},
		{name: "kernel refuses", proc: fakeProc(t, spc, "0", "unlimited"), enforce: enforcing, setup: func() error { return syscall.ENOSYS }, reasonHas: "io_uring_setup fails"},
		{name: "spdk_tgt not visible", proc: t.TempDir(), enforce: enforcing, usable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.disabled != "" {
				_ = os.MkdirAll(filepath.Join(tc.proc, "sys/kernel"), 0o755)
				_ = os.WriteFile(filepath.Join(tc.proc, "sys/kernel/io_uring_disabled"), []byte(tc.disabled), 0o644)
			}
			env := iouringEnv{procRoot: tc.proc, selinuxEnforce: tc.enforce, setup: ok, release: "7.2.15-200.fc44.x86_64"}
			if tc.setup != nil {
				env.setup = tc.setup
			}
			if tc.release != "" {
				env.release = tc.release
			}
			got := probeIOURing(env)
			if got.Usable != tc.usable || !strings.Contains(got.Reason, tc.reasonHas) {
				t.Fatalf("got %+v, want usable=%v reason containing %q", got, tc.usable, tc.reasonHas)
			}
		})
	}
}

func TestKernelBefore(t *testing.T) {
	for rel, want := range map[string]bool{"5.10.0-28-amd64": true, "5.12.0": false, "6.1.0": false, "4.19.0": true, "7.2.15-200.fc44.x86_64": false, "garbage": true} {
		if got := kernelBefore(rel, 5, 12); got != want {
			t.Errorf("kernelBefore(%q) = %v", rel, got)
		}
	}
}

// The real probe works on this machine, unless it can't set up io_uring itself.
func TestSetupIOURing(t *testing.T) {
	if err := setupIOURing(); err != nil && !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.ENOSYS) {
		t.Fatalf("setupIOURing: %v", err)
	}
}
