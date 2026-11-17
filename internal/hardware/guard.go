package hardware

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/velocor/velocor/internal/store"
)

var (
	nvmeHeadRegex = regexp.MustCompile(`^nvme\d+n\d+$`)
	nvmePathRegex = regexp.MustCompile(`^nvme\d+c\d+n\d+$`)
)

// guardSets is the result of one boot/in-use scan.
type guardSets struct {
	bootDevs  map[string]bool // physical disks backing OS mountpoints (sticky)
	bootPCI   map[string]bool
	inUseDevs map[string]bool // physical disks backing any mount or swap (recomputed)
	inUsePCI  map[string]bool
}

func newGuardSets() guardSets {
	return guardSets{map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}}
}

// kernelNameForMount resolves a mountinfo entry to a kernel block device name (e.g. "dm-0",
// "nvme0n1p3", "sda2"). It prefers the major:minor field, which works for /dev/mapper/*,
// /dev/disk/by-*, LVM and LUKS sources alike, and falls back to the source path.
func (m *Manager) kernelNameForMount(majMin, source string) string {
	if majMin != "" && !strings.HasPrefix(majMin, "0:") {
		if target, err := os.Readlink(filepath.Join(m.cfg.SysfsRoot, "dev", "block", majMin)); err == nil {
			return filepath.Base(target)
		}
	}
	if !strings.HasPrefix(source, "/dev/") {
		return ""
	}
	return m.kernelNameForPath(source)
}

// kernelNameForPath resolves a /dev path (possibly a by-id / mapper symlink) to a kernel name.
func (m *Manager) kernelNameForPath(p string) string {
	rel := strings.TrimPrefix(strings.TrimPrefix(p, m.cfg.DevRoot+"/"), "/dev/")
	if resolved, err := filepath.EvalSymlinks(filepath.Join(m.cfg.DevRoot, rel)); err == nil {
		rel = filepath.Base(resolved)
	}
	if strings.HasPrefix(rel, "mapper/") {
		name := strings.TrimPrefix(rel, "mapper/")
		dms, _ := filepath.Glob(filepath.Join(m.cfg.SysfsRoot, "class", "block", "dm-*"))
		for _, dm := range dms {
			if readSysfsString(filepath.Join(dm, "dm", "name")) == name {
				return filepath.Base(dm)
			}
		}
		return ""
	}
	return filepath.Base(rel)
}

// physicalDisks walks partitions and stacked devices (dm, md, LUKS, LVM) down to whole disks.
func (m *Manager) physicalDisks(kname string) []string {
	return m.physicalDisksDepth(kname, 0)
}

func (m *Manager) physicalDisksDepth(kname string, depth int) []string {
	if kname == "" || depth > 16 {
		return nil
	}
	blockDir := filepath.Join(m.cfg.SysfsRoot, "class", "block", kname)
	if entries, err := os.ReadDir(filepath.Join(blockDir, "slaves")); err == nil && len(entries) > 0 {
		var out []string
		for _, e := range entries {
			out = append(out, m.physicalDisksDepth(e.Name(), depth+1)...)
		}
		return out
	}
	// A partition's sysfs directory lives inside its parent disk's directory.
	if _, err := os.Stat(filepath.Join(blockDir, "partition")); err == nil {
		if resolved, err := filepath.EvalSymlinks(blockDir); err == nil {
			return []string{filepath.Base(filepath.Dir(resolved))}
		}
	}
	return []string{stripPartitionSuffix(kname)}
}

// pciAddressesForDisk returns the PCIe BDF(s) of the controller(s) behind a whole-disk
// kernel name. NVMe native-multipath head devices (nvmeXnY, a virtual subsystem device)
// are resolved through their multipath/ path devices to every backing controller.
func (m *Manager) pciAddressesForDisk(disk string) []string {
	blockDir := filepath.Join(m.cfg.SysfsRoot, "class", "block", disk)
	var out []string
	if paths, err := os.ReadDir(filepath.Join(blockDir, "multipath")); err == nil && len(paths) > 0 {
		for _, p := range paths {
			if addr := m.resolveBlockDevicePCIAddress(p.Name()); addr != "" {
				out = append(out, addr)
			}
		}
		return out
	}
	if addr := m.resolveBlockDevicePCIAddress(disk); addr != "" {
		out = append(out, addr)
	}
	return out
}

func (m *Manager) markDisk(sets guardSets, kname string, boot bool) {
	for _, disk := range m.physicalDisks(kname) {
		sets.inUseDevs[disk] = true
		if boot {
			sets.bootDevs[disk] = true
		}
		// Whole NVMe controllers are unbound to vfio, so protect their PCI address. SATA/SAS
		// HBAs can host both OS and data disks and are never unbound, so their BDF is not marked.
		if strings.HasPrefix(disk, "nvme") {
			for _, pci := range m.pciAddressesForDisk(disk) {
				sets.inUsePCI[pci] = true
				if boot {
					sets.bootPCI[pci] = true
				}
			}
		}
	}
}

func (m *Manager) scanMountinfo(path string, sets guardSets) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return nil
		}
		return fmt.Errorf("open mountinfo %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		// 36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
		left, right, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		lf, rf := strings.Fields(left), strings.Fields(right)
		if len(lf) < 5 || len(rf) < 2 {
			continue
		}
		kname := m.kernelNameForMount(lf[2], rf[1])
		if kname == "" {
			continue
		}
		m.markDisk(sets, kname, protectedMountpoints[lf[4]])
	}
	return sc.Err()
}

func (m *Manager) scanSwaps(sets guardSets) {
	data, err := os.ReadFile(filepath.Join(m.cfg.ProcRoot, "swaps"))
	if err != nil {
		return
	}
	for i, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if i == 0 || len(fields) < 2 || fields[1] != "partition" {
			continue
		}
		if kname := m.kernelNameForPath(fields[0]); kname != "" {
			m.markDisk(sets, kname, true) // active swap is as critical as the root fs
		}
	}
}

// holders returns the kernel names of devices stacked on disk or any of its partitions
// (LVM, md, dm-crypt, multipath, ...).
func (m *Manager) holders(disk string) []string {
	var out []string
	dirs := []string{filepath.Join(m.cfg.SysfsRoot, "class", "block", disk)}
	parts, _ := filepath.Glob(filepath.Join(m.cfg.SysfsRoot, "class", "block", disk, disk+"*"))
	dirs = append(dirs, parts...)
	for _, d := range dirs {
		entries, _ := os.ReadDir(filepath.Join(d, "holders"))
		for _, e := range entries {
			out = append(out, e.Name())
		}
	}
	return out
}

func (m *Manager) hasPartitions(disk string) bool {
	parts, _ := filepath.Glob(filepath.Join(m.cfg.SysfsRoot, "class", "block", disk, disk+"*", "partition"))
	return len(parts) > 0
}

// detectSignature returns a human-readable description of on-disk metadata found at the
// start of dev, or "" if the first 128 KiB look blank. It deliberately avoids exec'ing blkid.
func detectSignature(dev string) (string, error) {
	f, err := os.Open(dev)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 128*1024)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	buf = buf[:n]
	at := func(off int, magic string) bool {
		return off+len(magic) <= len(buf) && bytes.Equal(buf[off:off+len(magic)], []byte(magic))
	}
	switch {
	case at(512, "EFI PART"):
		return "GPT partition table", nil
	case at(0, "LUKS\xba\xbe"):
		return "LUKS encrypted volume", nil
	case at(512+0x18, "LVM2 001") || at(0x18, "LVM2 001"):
		return "LVM physical volume", nil
	case at(0, "XFSB"):
		return "XFS filesystem", nil
	case at(0x10040, "_BHRfS_M"):
		return "btrfs filesystem", nil
	case len(buf) >= 0x43a && binary.LittleEndian.Uint16(buf[0x438:]) == 0xEF53:
		return "ext2/3/4 filesystem", nil
	case at(4086, "SWAPSPACE2") || at(4086, "SWAP-SPACE"):
		return "swap area", nil
	case len(buf) >= 4100 && binary.LittleEndian.Uint32(buf[4096:]) == 0xa92b4efc:
		return "Linux md RAID member", nil
	case at(0x8001, "CD001"):
		return "ISO9660 filesystem", nil
	case at(3, "NTFS    "):
		return "NTFS filesystem", nil
	case len(buf) >= 512 && buf[510] == 0x55 && buf[511] == 0xAA:
		return "MBR partition table / boot sector", nil
	}
	return "", nil
}

// CheckClaimable refuses to hand a disk to SPDK when it backs the OS, is mounted, is used as
// swap, has stacked holders (LVM/md/LUKS), or carries partition-table or filesystem
// signatures. Signature/partition checks can be overridden with force; the others cannot.
func (m *Manager) CheckClaimable(d *store.Disk, force bool) error {
	if d.IsBootDevice || m.IsBootDevice(d.PCIAddress, d.DevicePath) {
		return fmt.Errorf("%w: disk %s backs the OS or is mounted / used as swap", store.ErrBootDeviceProtected, d.ID)
	}
	kname := ""
	if d.DevicePath != "" {
		kname = m.kernelNameForPath(d.DevicePath)
	}
	if kname == "" {
		// e.g. an NVMe controller already bound to vfio-pci: no kernel view to inspect.
		return nil
	}
	if h := m.holders(kname); len(h) > 0 {
		return fmt.Errorf("%w: disk %s is in use by %s", store.ErrInUse, d.ID, strings.Join(h, ", "))
	}
	if force {
		return nil
	}
	if m.hasPartitions(kname) {
		return fmt.Errorf("%w: disk %s has partitions; pass force=true to destroy them", store.ErrInUse, d.ID)
	}
	sig, err := detectSignature(filepath.Join(m.cfg.DevRoot, kname))
	if err != nil {
		return fmt.Errorf("inspect %s for existing data: %w", kname, err)
	}
	if sig != "" {
		return fmt.Errorf("%w: disk %s contains a %s; pass force=true to destroy it", store.ErrInUse, d.ID, sig)
	}
	return nil
}

// stableDiskPath returns the most stable /dev/disk/by-id path for kname (WWN first), or
// /dev/<kname> when no by-id link exists.
func (m *Manager) stableDiskPath(kname string) string {
	byID := filepath.Join(m.cfg.DevRoot, "disk", "by-id")
	entries, _ := os.ReadDir(byID)
	best, bestRank := "", 99
	for _, e := range entries {
		name := e.Name()
		if strings.Contains(name, "-part") {
			continue
		}
		target, err := filepath.EvalSymlinks(filepath.Join(byID, name))
		if err != nil || filepath.Base(target) != kname {
			continue
		}
		rank := 5
		for i, prefix := range []string{"wwn-", "nvme-eui.", "scsi-3", "ata-", "virtio-", "scsi-"} {
			if strings.HasPrefix(name, prefix) {
				rank = i
				break
			}
		}
		if rank < bestRank || (rank == bestRank && name < best) {
			best, bestRank = name, rank
		}
	}
	if best == "" {
		return filepath.Join(m.cfg.DevRoot, kname)
	}
	return filepath.Join(byID, best)
}

var nonIDChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// stableSSDID derives a disk ID from hardware identity (WWID, then serial) so it survives
// kernel device renames (sdb <-> sdc) across reboots. Falls back to the kernel name.
func stableSSDID(wwid, serial, kname string) string {
	for _, v := range []string{wwid, serial} {
		v = strings.Trim(nonIDChars.ReplaceAllString(strings.TrimSpace(v), "_"), "_")
		if v != "" {
			if len(v) > 48 {
				v = v[len(v)-48:]
			}
			return "ssd-" + v
		}
	}
	return "ssd-" + kname
}
