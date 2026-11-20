package hardware

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unsafe"

	"golang.org/x/sys/unix"
)

// IOURingProbe says whether spdk_tgt can be trusted to set up io_uring for SSD bdevs
// (docs/review-history.md P0-6). In SPDK v26.09 a failed io_uring setup in bdev_uring crashes spdk_tgt,
// taking every export down, and after a restart the reconciler re-attaches the same disks,
// so it would crash again. AIO has no such failure, so it is used whenever this is unsure.
type IOURingProbe struct {
	Usable bool
	// Reason explains a negative result (empty when usable).
	Reason string
}

// iouringEnv is what ProbeIOURing looks at, replaceable in tests.
type iouringEnv struct {
	procRoot       string       // "/proc"
	selinuxEnforce string       // "/sys/fs/selinux/enforce"
	setup          func() error // io_uring_setup in this process
	release        string       // kernel release, e.g. "6.12.4-200.fc41.x86_64"
}

// unconfinedSELinuxTypes are SELinux domains that don't restrict io_uring. Podman gives
// privileged containers with SecurityLabelDisable spc_t (as the appliance's quadlets do); a
// confined container_t process gets EPERM from io_uring_setup (seen with rootless podman on
// Fedora, 2026-11-18: spdk_tgt segfaulted on the first bdev_uring_create).
var unconfinedSELinuxTypes = map[string]bool{
	"spc_t": true, "unconfined_t": true, "unconfined_service_t": true, "init_t": true, "initrc_t": true, "kernel_t": true,
}

// ProbeIOURing checks, in order: the kernel.io_uring_disabled sysctl; that this process can
// set up a ring (velod runs privileged like spdk_tgt, so a kernel without io_uring, or one
// that refuses it, shows here); and, when spdk_tgt's process is visible (both containers use
// the host PID namespace), that it is not confined by an enforcing SELinux policy or a seccomp
// filter (either may block io_uring) and, on kernels before 5.12 that charge rings to it,
// that its locked-memory limit is unlimited.
func ProbeIOURing() IOURingProbe {
	var uts unix.Utsname
	_ = unix.Uname(&uts)
	return probeIOURing(iouringEnv{procRoot: "/proc", selinuxEnforce: "/sys/fs/selinux/enforce", setup: setupIOURing,
		release: unix.ByteSliceToString(uts.Release[:])})
}

func probeIOURing(env iouringEnv) IOURingProbe {
	no := func(format string, args ...any) IOURingProbe {
		return IOURingProbe{Reason: fmt.Sprintf(format, args...)}
	}
	if b, err := os.ReadFile(filepath.Join(env.procRoot, "sys/kernel/io_uring_disabled")); err == nil {
		if v := strings.TrimSpace(string(b)); v == "2" {
			return no("io_uring is disabled (kernel.io_uring_disabled=2)")
		}
	}
	if err := env.setup(); err != nil {
		return no("io_uring_setup fails: %v", err)
	}
	for _, pid := range findSPDKProcesses(env.procRoot) {
		if r := probeSPDKProcess(env, pid); !r.Usable {
			return r
		}
	}
	return IOURingProbe{Usable: true}
}

// probeSPDKProcess checks one spdk_tgt process's confinement and limits.
func probeSPDKProcess(env iouringEnv, pid string) IOURingProbe {
	no := func(format string, args ...any) IOURingProbe {
		return IOURingProbe{Reason: fmt.Sprintf(format, args...)}
	}
	status, err := os.ReadFile(filepath.Join(env.procRoot, pid, "status"))
	if err != nil {
		return no("can't read spdk_tgt's status: %v", err)
	}
	if b, err := os.ReadFile(env.selinuxEnforce); err == nil && strings.TrimSpace(string(b)) == "1" {
		label, err := os.ReadFile(filepath.Join(env.procRoot, pid, "attr/current"))
		if err != nil {
			return no("can't read spdk_tgt's SELinux label: %v", err)
		}
		ctx := strings.TrimFunc(string(label), func(r rune) bool { return unicode.IsSpace(r) || r == 0 })
		if parts := strings.Split(ctx, ":"); len(parts) < 3 || !unconfinedSELinuxTypes[parts[2]] {
			return no("spdk_tgt runs confined by SELinux (%s), which blocks io_uring for containers; run it with SecurityLabelDisable=true as the appliance does", ctx)
		}
	}
	if v := statusField(string(status), "Seccomp"); v != "" && v != "0" {
		return no("spdk_tgt runs under a seccomp filter (Seccomp: %s), which may block io_uring", v)
	}
	if kernelBefore(env.release, 5, 12) {
		limits, err := os.ReadFile(filepath.Join(env.procRoot, pid, "limits"))
		if err != nil {
			return no("can't read spdk_tgt's limits: %v", err)
		}
		if soft := memlockSoftLimit(string(limits)); soft != "unlimited" {
			return no("spdk_tgt's locked-memory limit is %s bytes, and this kernel (%s) charges io_uring rings to it", soft, env.release)
		}
	}
	return IOURingProbe{Usable: true}
}

// setupIOURing creates and closes a small ring.
func setupIOURing() error {
	var params [120]byte // struct io_uring_params, zeroed
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, 4, uintptr(unsafe.Pointer(&params[0])), 0)
	if errno != 0 {
		return errno
	}
	return unix.Close(int(fd))
}

// findSPDKProcesses returns the PIDs (as /proc entry names) of the visible spdk_tgt
// processes (normally one).
func findSPDKProcesses(procRoot string) []string {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var pids []string
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cmdline"))
		if err != nil || len(cmdline) == 0 {
			continue
		}
		argv0, _, _ := strings.Cut(string(cmdline), "\x00")
		if filepath.Base(argv0) == "spdk_tgt" {
			pids = append(pids, e.Name())
		}
	}
	return pids
}

func statusField(status, name string) string {
	sc := bufio.NewScanner(strings.NewReader(status))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && k == name {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// memlockSoftLimit returns the soft "Max locked memory" from /proc/<pid>/limits.
func memlockSoftLimit(limits string) string {
	sc := bufio.NewScanner(strings.NewReader(limits))
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "Max locked memory"); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				return f[0]
			}
		}
	}
	return ""
}

// kernelBefore reports whether release (e.g. "5.10.0-xyz") is older than major.minor. An
// unparsable release counts as old, which only makes the probe stricter.
func kernelBefore(release string, major, minor int) bool {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return true
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
	if err := errors.Join(err1, err2); err != nil {
		return true
	}
	return ma < major || (ma == major && mi < minor)
}
