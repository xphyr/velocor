package csi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// VolumeStats holds filesystem or block device capacity/inode usage.
type VolumeStats struct {
	AvailableBytes  int64
	TotalBytes      int64
	UsedBytes       int64
	AvailableInodes int64
	TotalInodes     int64
	UsedInodes      int64
	IsBlock         bool
}

// Mounter abstracts Linux NVMe-oF/iSCSI target connection, formatting, mounting, and stats.
type Mounter interface {
	ConnectTarget(ctx context.Context, publishCtx map[string]string, hostNQN, initiatorIQN string) (string, error)
	DisconnectTarget(ctx context.Context, publishCtx map[string]string) error
	// FlushDevice writes out what the node still caches for a raw block device.
	FlushDevice(ctx context.Context, devicePath string) error
	FormatAndMount(ctx context.Context, source, target, fsType string, mountOpts []string) error
	BindMount(ctx context.Context, source, target string, mountOpts []string, isBlock bool) error
	Unmount(ctx context.Context, target string) error
	IsMountPoint(target string) (bool, error)
	ResizeFilesystem(ctx context.Context, devicePath, mountPath string) error
	// RescanTarget makes the host pick up a capacity change of an attached volume.
	RescanTarget(ctx context.Context, publishCtx map[string]string) error
	GetVolumeStats(path string) (VolumeStats, error)
}

// LinuxMounter implements Mounter using nvme-cli, iscsiadm, mount/umount, mkfs, and unix syscalls.
type LinuxMounter struct {
	SysRoot string // defaults to /sys
}

// NewLinuxMounter creates a LinuxMounter.
func NewLinuxMounter() *LinuxMounter {
	return &LinuxMounter{SysRoot: "/sys"}
}

func (m *LinuxMounter) sys(parts ...string) string {
	root := m.SysRoot
	if root == "" {
		root = "/sys"
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

// hostMountNS is the host's mount namespace: the node plugin runs with hostPID, so PID 1 is the
// host's init.
const hostMountNS = "/proc/1/ns/mnt"

var (
	hostISCSIAdmOnce sync.Once
	hostISCSIAdm     bool
)

// iscsiadm runs open-iscsi's iscsiadm. iscsiadm passes node records to iscsid as raw C structs,
// so it must be the same build as the host's iscsid: a container's iscsiadm talking to a
// different host build fails logins with "iSCSI driver not found" (error 12) although
// everything is set up (seen on OpenShift/RHCOS with a Fedora-based image). So it runs the
// host's own iscsiadm, in the host's mount namespace, whenever the plugin runs beside a host;
// VELOCOR_ISCSIADM=container forces the bundled one.
func iscsiadm(ctx context.Context, args ...string) *exec.Cmd {
	hostISCSIAdmOnce.Do(func() {
		if os.Getenv("VELOCOR_ISCSIADM") == "container" {
			return
		}
		host, err1 := os.Readlink(hostMountNS)
		self, err2 := os.Readlink("/proc/self/ns/mnt")
		hostISCSIAdm = err1 == nil && err2 == nil && host != self
	})
	if !hostISCSIAdm {
		return exec.CommandContext(ctx, "iscsiadm", args...)
	}
	full := append([]string{"--mount=" + hostMountNS, "--", "/usr/bin/env", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "iscsiadm"}, args...)
	return exec.CommandContext(ctx, "nsenter", full...)
}

// iscsiIface returns the dedicated open-iscsi interface used for an initiator IQN, creating
// it on first use. Logging in through our own iface avoids rewriting the host's global
// /etc/iscsi/initiatorname.iscsi, which other iSCSI users on the node depend on.
func iscsiIface(ctx context.Context, initiatorIQN string) (string, error) {
	if initiatorIQN == "" {
		return "default", nil
	}
	sum := sha256.Sum256([]byte(initiatorIQN))
	name := "velocor-" + hex.EncodeToString(sum[:4])
	// Create it only if it's missing: for an existing iface, `-o new` fails with exit 15 and
	// "Could not create new interface" (iscsi-initiator-utils 6.2.1.11, RHCOS 9.8), which made
	// every attach after the node's first fail.
	if iscsiadm(ctx, "-m", "iface", "-I", name).Run() != nil {
		if out, err := iscsiadm(ctx, "-m", "iface", "-I", name, "-o", "new").CombinedOutput(); err != nil &&
			!strings.Contains(strings.ToLower(string(out)), "exists") {
			return "", fmt.Errorf("iscsiadm iface new %s: %w (%s)", name, err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := iscsiadm(ctx, "-m", "iface", "-I", name, "-o", "update",
		"-n", "iface.initiatorname", "-v", initiatorIQN).CombinedOutput(); err != nil {
		return "", fmt.Errorf("iscsiadm iface update %s: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return name, nil
}

// targetAddr is one portal address a volume can be reached at.
type targetAddr struct{ ip, port string }

// targetAddrs lists where a volume can be reached, in the controller's order:
// VELOCOR_PORTAL_OVERRIDE (one address, deprecated), else the publish context's "portals",
// else portal_ip/portal_port (controllers from before "portals", TODO.md P1-13).
func targetAddrs(publishCtx map[string]string) ([]targetAddr, error) {
	if override := strings.TrimSpace(os.Getenv("VELOCOR_PORTAL_OVERRIDE")); override != "" {
		if h, p, ok := strings.Cut(override, ":"); ok && h != "" && p != "" {
			return []targetAddr{{h, p}}, nil
		}
	}
	var out []targetAddr
	for _, a := range strings.Split(publishCtx["portals"], ",") {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		h, p, err := net.SplitHostPort(a)
		if err != nil {
			return nil, fmt.Errorf("bad portal %q in publish_context: %w", a, err)
		}
		out = append(out, targetAddr{h, p})
	}
	if len(out) == 0 {
		if ip, port := publishCtx["portal_ip"], publishCtx["portal_port"]; ip != "" && port != "" {
			out = append(out, targetAddr{ip, port})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("missing portal_ip or portal_port in publish_context")
	}
	return out, nil
}

func (m *LinuxMounter) ConnectTarget(ctx context.Context, publishCtx map[string]string, hostNQN, initiatorIQN string) (string, error) {
	proto := normalizeProtocol(publishCtx["protocol"])
	addrs, err := targetAddrs(publishCtx)
	if err != nil {
		return "", err
	}

	switch string(proto) {
	case "ISCSI":
		iqn := publishCtx["target_iqn"]
		lun := publishCtx["lun_id"]
		if lun == "" {
			lun = "0"
		}
		if iqn == "" {
			return "", fmt.Errorf("missing target_iqn in publish_context")
		}
		byPath := func(a targetAddr) string {
			return fmt.Sprintf("/dev/disk/by-path/ip-%s-iscsi-%s-lun-%s", iscsiPortalAddr(a.ip, a.port), iqn, lun)
		}
		// One session only: without dm-multipath, sessions on two portals would give the node
		// two separate disks for one volume. Keep a session that already exists (a restage,
		// or an earlier failover), else log in to the first portal that answers.
		for _, a := range addrs {
			if dev, err := filepath.EvalSymlinks(byPath(a)); err == nil {
				return dev, nil
			}
		}
		iface, err := iscsiIface(ctx, initiatorIQN)
		if err != nil {
			return "", err
		}
		var errs []error
		for _, a := range addrs {
			if err := iscsiLogin(ctx, iscsiPortalAddr(a.ip, a.port), iqn, iface); err != nil {
				errs = append(errs, err)
				continue
			}
			return waitForDevice(ctx, byPath(a))
		}
		return "", errors.Join(errs...)

	default:
		nqn := publishCtx["target_nqn"]
		if nqn == "" {
			return "", fmt.Errorf("missing target_nqn in publish_context")
		}
		trtype := "tcp"
		if proto == "NVMEOF_RDMA" {
			trtype = "rdma"
		}
		// Every portal is a path to the same namespace; the kernel's native NVMe multipath
		// merges them into one device. Without it each path would be a separate device, so
		// only one is connected.
		multipath := readTrim(m.sys("module", "nvme_core", "parameters", "multipath")) == "Y"
		connected := 0
		var errs []error
		for _, a := range addrs {
			if connected > 0 && !multipath {
				break
			}
			// Keep reconnecting for as long as it takes: an appliance or data-plane restart
			// (which can include volume-store recovery) must stall I/O, not fail it. With the
			// kernel's default ctrl_loss_tmo (600 s) pods got I/O errors during a 16-minute
			// recovery.
			args := []string{"connect", "-t", trtype, "-a", a.ip, "-s", a.port, "-n", nqn, "--ctrl-loss-tmo=-1", "--reconnect-delay=5"}
			if hostNQN != "" {
				args = append(args, "-q", hostNQN, "-I", deterministicHostID(hostNQN))
			}
			out, err := exec.CommandContext(ctx, "nvme", args...).CombinedOutput()
			if err != nil && !strings.Contains(strings.ToLower(string(out)), "already connected") {
				errs = append(errs, fmt.Errorf("nvme connect %s at %s: %w (%s)", nqn, net.JoinHostPort(a.ip, a.port), err, strings.TrimSpace(string(out))))
				continue
			}
			connected++
		}
		if connected == 0 {
			return "", errors.Join(errs...)
		}
		if len(errs) > 0 {
			log.Printf("volume %s: connected on %d of %d paths: %v", nqn, connected, len(addrs), errors.Join(errs...))
		}
		return m.waitForNVMeDevice(ctx, nqn, publishCtx["namespace_id"], publishCtx["volume_uuid"])
	}
}

// iscsiLogin discovers iqn at portal and logs in through iface.
func iscsiLogin(ctx context.Context, portal, iqn, iface string) error {
	if out, err := iscsiadm(ctx, "-m", "discovery", "-t", "sendtargets", "-p", portal, "-I", iface).CombinedOutput(); err != nil {
		return fmt.Errorf("iscsiadm discovery %s: %w (%s)", portal, err, strings.TrimSpace(string(out)))
	}
	// Kubernetes decides when this node uses the volume: iscsid must never log in on its own
	// at boot (node.startup defaults to automatic for discovered records).
	if out, err := iscsiadm(ctx, "-m", "node", "-T", iqn, "-p", portal, "-I", iface, "-o", "update",
		"-n", "node.startup", "-v", "manual").CombinedOutput(); err != nil {
		return fmt.Errorf("iscsiadm set node.startup for %s at %s: %w (%s)", iqn, portal, err, strings.TrimSpace(string(out)))
	}
	// Ignore error if already logged in (exit code 15).
	out, err := iscsiadm(ctx, "-m", "node", "-T", iqn, "-p", portal, "-I", iface, "--login").CombinedOutput()
	if err != nil && !strings.Contains(string(out), "already present") {
		return fmt.Errorf("iscsiadm login %s at %s: %w (%s)", iqn, portal, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *LinuxMounter) DisconnectTarget(ctx context.Context, publishCtx map[string]string) error {
	proto := normalizeProtocol(publishCtx["protocol"])
	switch string(proto) {
	case "ISCSI":
		iqn := publishCtx["target_iqn"]
		addrs, err := targetAddrs(publishCtx)
		if iqn == "" || err != nil {
			return nil
		}
		var errs []error
		for _, a := range addrs {
			portal := iscsiPortalAddr(a.ip, a.port)
			// Exit status 21: no matching session (already logged out, or never logged in on
			// this portal), which is fine.
			if out, err := iscsiadm(ctx, "-m", "node", "-T", iqn, "-p", portal, "--logout").CombinedOutput(); err != nil && exitCode(err) != 21 {
				errs = append(errs, fmt.Errorf("iscsiadm logout %s at %s: %w (%s)", iqn, portal, err, strings.TrimSpace(string(out))))
				continue
			}
			// Forget the target too, so the node keeps no record of volumes it no longer uses
			// (and iscsid doesn't keep retrying a target that's gone).
			_ = iscsiadm(ctx, "-m", "node", "-T", iqn, "-p", portal, "-o", "delete").Run()
		}
		return errors.Join(errs...)
	default:
		nqn := publishCtx["target_nqn"]
		if nqn != "" {
			// Disconnects every path (controller) of the subsystem.
			out, err := exec.CommandContext(ctx, "nvme", "disconnect", "-n", nqn).CombinedOutput()
			if err != nil && !strings.Contains(string(out), "disconnected 0 controller") {
				return fmt.Errorf("nvme disconnect %s: %w (%s)", nqn, err, strings.TrimSpace(string(out)))
			}
		}
		return nil
	}
}

// FlushDevice writes out what the kernel still holds for a raw block device. Raw block
// volumes go through the page cache with no filesystem to flush them; tearing the connection
// down first would race the data against the next node's reads.
func (m *LinuxMounter) FlushDevice(ctx context.Context, devicePath string) error {
	if out, err := exec.CommandContext(ctx, "blockdev", "--flushbufs", devicePath).CombinedOutput(); err != nil {
		return fmt.Errorf("blockdev --flushbufs %s: %w (%s)", devicePath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func deterministicHostID(hostNQN string) string {
	sum := sha256.Sum256([]byte(hostNQN))
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func (m *LinuxMounter) FormatAndMount(ctx context.Context, source, target, fsType string, mountOpts []string) error {
	if fsType == "" {
		fsType = "ext4"
	}
	if err := os.MkdirAll(target, 0750); err != nil {
		return err
	}
	mounted, err := m.IsMountPoint(target)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}

	// Check if filesystem already exists on device via blkid.
	existingFS, err := detectFilesystem(ctx, source)
	if err != nil {
		return err
	}
	if existingFS == "" {
		var mkfsArgs []string
		switch fsType {
		case "xfs":
			mkfsArgs = []string{"-f", source}
		default:
			fsType = "ext4"
			mkfsArgs = []string{"-F", source}
		}
		if out, err := exec.CommandContext(ctx, "mkfs."+fsType, mkfsArgs...).CombinedOutput(); err != nil {
			return fmt.Errorf("mkfs.%s %s: %w (%s)", fsType, source, err, strings.TrimSpace(string(out)))
		}
	} else {
		fsType = existingFS
	}

	args := []string{"-t", fsType}
	if len(mountOpts) > 0 {
		args = append(args, "-o", strings.Join(mountOpts, ","))
	}
	args = append(args, source, target)
	if out, err := exec.CommandContext(ctx, "mount", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s -> %s: %w (%s)", source, target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *LinuxMounter) BindMount(ctx context.Context, source, target string, mountOpts []string, isBlock bool) error {
	if isBlock {
		if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, 0660)
		if err != nil {
			return err
		}
		_ = f.Close()
	} else {
		if err := os.MkdirAll(target, 0750); err != nil {
			return err
		}
	}
	mounted, err := m.IsMountPoint(target)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	opts := append([]string{"bind"}, mountOpts...)
	args := []string{"-o", strings.Join(opts, ","), source, target}
	if out, err := exec.CommandContext(ctx, "mount", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("bind mount %s -> %s: %w (%s)", source, target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *LinuxMounter) Unmount(ctx context.Context, target string) error {
	mounted, err := m.IsMountPoint(target)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	if out, err := exec.CommandContext(ctx, "umount", target).CombinedOutput(); err != nil {
		return fmt.Errorf("umount %s: %w (%s)", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *LinuxMounter) IsMountPoint(target string) (bool, error) {
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	err := exec.Command("mountpoint", "-q", target).Run()
	return err == nil, nil
}

func (m *LinuxMounter) ResizeFilesystem(ctx context.Context, devicePath, mountPath string) error {
	if devicePath == "" {
		return fmt.Errorf("device path of staged volume is unknown")
	}
	fsType, err := detectFilesystem(ctx, devicePath)
	if err != nil {
		return err
	}
	if fsType == "" {
		return nil // raw block volume: nothing to grow
	}
	switch fsType {
	case "xfs":
		if out, err := exec.CommandContext(ctx, "xfs_growfs", mountPath).CombinedOutput(); err != nil {
			return fmt.Errorf("xfs_growfs %s: %w (%s)", mountPath, err, strings.TrimSpace(string(out)))
		}
	default:
		if out, err := exec.CommandContext(ctx, "resize2fs", devicePath).CombinedOutput(); err != nil {
			return fmt.Errorf("resize2fs %s: %w (%s)", devicePath, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (m *LinuxMounter) GetVolumeStats(path string) (VolumeStats, error) {
	info, err := os.Stat(path)
	if err != nil {
		return VolumeStats{}, err
	}
	if info.Mode()&os.ModeDevice != 0 {
		f, err := os.Open(path)
		if err != nil {
			return VolumeStats{}, err
		}
		defer func() { _ = f.Close() }()
		size, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKGETSIZE64)
		if err != nil {
			return VolumeStats{}, err
		}
		return VolumeStats{
			TotalBytes: int64(size),
			IsBlock:    true,
		}, nil
	}

	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return VolumeStats{}, err
	}
	total := int64(st.Blocks) * int64(st.Bsize)
	avail := int64(st.Bavail) * int64(st.Bsize)
	used := (int64(st.Blocks) - int64(st.Bfree)) * int64(st.Bsize)
	return VolumeStats{
		AvailableBytes:  avail,
		TotalBytes:      total,
		UsedBytes:       used,
		AvailableInodes: int64(st.Ffree),
		TotalInodes:     int64(st.Files),
		UsedInodes:      int64(st.Files - st.Ffree),
	}, nil
}

func waitForDevice(ctx context.Context, devPath string) (string, error) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if resolved, err := filepath.EvalSymlinks(devPath); err == nil {
			if _, err := os.Stat(resolved); err == nil {
				return resolved, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("timed out waiting for device %s", devPath)
}

func iscsiPortalAddr(ip, port string) string {
	if strings.Contains(ip, ":") && !strings.HasPrefix(ip, "[") {
		ip = "[" + ip + "]"
	}
	return ip + ":" + port
}

func (m *LinuxMounter) RescanTarget(ctx context.Context, publishCtx map[string]string) error {
	switch normalizeProtocol(publishCtx["protocol"]) {
	case "ISCSI":
		iqn := publishCtx["target_iqn"]
		if iqn == "" {
			return fmt.Errorf("missing target_iqn in publish_context")
		}
		if out, err := iscsiadm(ctx, "-m", "node", "-T", iqn, "-R").CombinedOutput(); err != nil {
			return fmt.Errorf("iscsiadm rescan %s: %w (%s)", iqn, err, strings.TrimSpace(string(out)))
		}
	default:
		// NVMe namespaces report capacity changes via AEN; force a rescan of every controller
		// of the subsystem in case the AEN was missed.
		ctrls, _ := filepath.Glob(m.sys("class", "nvme", "nvme*"))
		for _, c := range ctrls {
			if readTrim(filepath.Join(c, "subsysnqn")) == publishCtx["target_nqn"] {
				_ = os.WriteFile(filepath.Join(c, "rescan_controller"), []byte("1"), 0o200)
			}
		}
	}
	return nil
}

// parseBlkid interprets `blkid -p -o export` output and exit status. blkid exits 2 only
// when it positively found no signature; any other failure is reported, never treated as
// "blank", because the caller formats blank devices.
func parseBlkid(out []byte, exitCode int) (string, error) {
	switch exitCode {
	case 0:
	case 2:
		return "", nil
	default:
		return "", fmt.Errorf("blkid failed with exit status %d: %s", exitCode, strings.TrimSpace(string(out)))
	}
	var fsType, ptType string
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "TYPE":
			fsType = v
		case "PTTYPE":
			ptType = v
		}
	}
	if fsType == "" && ptType != "" {
		return "", fmt.Errorf("device carries a %s partition table; refusing to format it", ptType)
	}
	if fsType == "" {
		return "", fmt.Errorf("blkid reported a signature without a filesystem type; refusing to format")
	}
	return fsType, nil
}

func detectFilesystem(ctx context.Context, devicePath string) (string, error) {
	out, err := exec.CommandContext(ctx, "blkid", "-p", "-o", "export", devicePath).CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return "", fmt.Errorf("run blkid on %s: %w", devicePath, err)
		}
		code = ee.ExitCode()
	}
	fs, perr := parseBlkid(out, code)
	if perr != nil {
		return "", fmt.Errorf("%s: %w", devicePath, perr)
	}
	return fs, nil
}

func (m *LinuxMounter) waitForNVMeDevice(ctx context.Context, targetNQN, nsidStr, wantUUID string) (string, error) {
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		dev, err := m.findNVMeNamespace(targetNQN, nsidStr, wantUUID)
		if err == nil && dev != "" {
			return dev, nil
		}
		if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("timed out waiting for NVMe namespace %s of %s", nsidStr, targetNQN)
}

var nvmeNSRegex = regexp.MustCompile(`^nvme\d+n\d+$`)

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// findNVMeNamespace resolves the block device for namespace nsid of subsystem targetNQN
// purely from sysfs. With native multipath (the Fedora/RHEL default) block devices are
// named after the subsystem instance, not the controller, so "/dev/<ctrl>n<nsid>" can be an
// unrelated volume; we therefore match on subsysnqn + nsid and then verify the namespace
// UUID equals the Velocor volume UUID before the device is used.
func (m *LinuxMounter) findNVMeNamespace(targetNQN, nsidStr, wantUUID string) (string, error) {
	nsid := nsidStr
	if nsid == "" || nsid == "0" {
		nsid = "1"
	}
	var parents []string
	if subs, _ := filepath.Glob(m.sys("class", "nvme-subsystem", "*")); len(subs) > 0 {
		parents = append(parents, subs...)
	}
	if ctrls, _ := filepath.Glob(m.sys("class", "nvme", "nvme*")); len(ctrls) > 0 {
		parents = append(parents, ctrls...)
	}
	for _, parent := range parents {
		if readTrim(filepath.Join(parent, "subsysnqn")) != targetNQN {
			continue
		}
		entries, _ := os.ReadDir(parent)
		for _, e := range entries {
			name := e.Name()
			if !nvmeNSRegex.MatchString(name) {
				continue
			}
			blk := m.sys("class", "block", name)
			if readTrim(filepath.Join(blk, "nsid")) != nsid {
				continue
			}
			if wantUUID != "" {
				got := readTrim(filepath.Join(blk, "uuid"))
				if !strings.EqualFold(got, wantUUID) {
					return "", fmt.Errorf("NVMe namespace %s (nsid %s of %s) has UUID %q, expected volume UUID %q; refusing to use it",
						name, nsid, targetNQN, got, wantUUID)
				}
			}
			return "/dev/" + name, nil
		}
	}
	return "", nil
}
