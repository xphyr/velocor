package csi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	FormatAndMount(ctx context.Context, source, target, fsType string, mountOpts []string) error
	BindMount(ctx context.Context, source, target string, mountOpts []string, isBlock bool) error
	Unmount(ctx context.Context, target string) error
	IsMountPoint(target string) (bool, error)
	ResizeFilesystem(ctx context.Context, devicePath, mountPath string) error
	GetVolumeStats(path string) (VolumeStats, error)
}

// LinuxMounter implements Mounter using nvme-cli, iscsiadm, mount/umount, mkfs, and unix syscalls.
type LinuxMounter struct{}

// NewLinuxMounter creates a LinuxMounter.
func NewLinuxMounter() *LinuxMounter {
	return &LinuxMounter{}
}

func (m *LinuxMounter) ConnectTarget(ctx context.Context, publishCtx map[string]string, hostNQN, initiatorIQN string) (string, error) {
	proto := normalizeProtocol(publishCtx["protocol"])
	ip := publishCtx["portal_ip"]
	port := publishCtx["portal_port"]
	if override := strings.TrimSpace(os.Getenv("VELOCOR_PORTAL_OVERRIDE")); override != "" {
		if h, p, ok := strings.Cut(override, ":"); ok && h != "" && p != "" {
			ip, port = h, p
		}
	}
	if ip == "" || port == "" {
		return "", fmt.Errorf("missing portal_ip or portal_port in publish_context")
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
		if initiatorIQN != "" {
			_ = os.WriteFile("/etc/iscsi/initiatorname.iscsi", []byte("InitiatorName="+initiatorIQN+"\n"), 0644)
		}
		portal := ip + ":" + port
		if out, err := exec.CommandContext(ctx, "iscsiadm", "-m", "discovery", "-t", "sendtargets", "-p", portal).CombinedOutput(); err != nil {
			return "", fmt.Errorf("iscsiadm discovery %s: %w (%s)", portal, err, strings.TrimSpace(string(out)))
		}
		// Ignore error if already logged in (exit code 15).
		out, err := exec.CommandContext(ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", portal, "--login").CombinedOutput()
		if err != nil && !strings.Contains(string(out), "already present") {
			return "", fmt.Errorf("iscsiadm login %s: %w (%s)", iqn, err, strings.TrimSpace(string(out)))
		}
		byPath := fmt.Sprintf("/dev/disk/by-path/ip-%s-iscsi-%s-lun-%s", portal, iqn, lun)
		return waitForDevice(ctx, byPath)

	default:
		nqn := publishCtx["target_nqn"]
		if nqn == "" {
			return "", fmt.Errorf("missing target_nqn in publish_context")
		}
		args := []string{"connect", "-t", "tcp", "-a", ip, "-s", port, "-n", nqn}
		if hostNQN != "" {
			args = append(args, "-q", hostNQN, "-I", deterministicHostID(hostNQN))
		}
		out, err := exec.CommandContext(ctx, "nvme", args...).CombinedOutput()
		if err != nil && !strings.Contains(strings.ToLower(string(out)), "already connected") {
			return "", fmt.Errorf("nvme connect %s: %w (%s)", nqn, err, strings.TrimSpace(string(out)))
		}
		return waitForNVMeDevice(ctx, nqn, publishCtx["namespace_id"])
	}
}

func (m *LinuxMounter) DisconnectTarget(ctx context.Context, publishCtx map[string]string) error {
	proto := normalizeProtocol(publishCtx["protocol"])
	switch string(proto) {
	case "ISCSI":
		iqn := publishCtx["target_iqn"]
		ip := publishCtx["portal_ip"]
		port := publishCtx["portal_port"]
		if iqn != "" && ip != "" && port != "" {
			_ = exec.CommandContext(ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", ip+":"+port, "--logout").Run()
		}
		return nil
	default:
		nqn := publishCtx["target_nqn"]
		if nqn != "" {
			_ = exec.CommandContext(ctx, "nvme", "disconnect", "-n", nqn).Run()
		}
		return nil
	}
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
	fsType, err := detectFilesystem(ctx, devicePath)
	if err != nil || fsType == "" {
		return err
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

func detectFilesystem(ctx context.Context, devicePath string) (string, error) {
	out, err := exec.CommandContext(ctx, "blkid", "-o", "value", "-s", "TYPE", devicePath).Output()
	if err != nil {
		// blkid exits 2 when no filesystem is present on the device.
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
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

func waitForNVMeDevice(ctx context.Context, targetNQN, nsidStr string) (string, error) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if dev := findNVMeDeviceByNQN(ctx, targetNQN, nsidStr); dev != "" {
			return dev, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return "", fmt.Errorf("timed out waiting for NVMe device for %s", targetNQN)
}

func findNVMeDeviceByNQN(ctx context.Context, targetNQN, nsidStr string) string {
	out, err := exec.CommandContext(ctx, "nvme", "list-subsys", "-o", "json").Output()
	if err != nil {
		return ""
	}
	var parsed []struct {
		Subsystems []struct {
			NQN   string `json:"NQN"`
			Paths []struct {
				Name string `json:"Name"`
			} `json:"Paths"`
		} `json:"Subsystems"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return ""
	}
	nsid := "1"
	if nsidStr != "" && nsidStr != "0" {
		nsid = nsidStr
	}
	for _, host := range parsed {
		for _, sub := range host.Subsystems {
			if sub.NQN != targetNQN {
				continue
			}
			for _, p := range sub.Paths {
				// /sys/class/nvme/<ctrl>/<ctrl>n<nsid>
				candidate := "/dev/" + p.Name + "n" + nsid
				if _, err := os.Stat(candidate); err == nil {
					return candidate
				}
			}
		}
	}
	return ""
}
