package hardware

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/velocor/velocor/internal/store"
)

const (
	nvmePCIClass = "0x010802"
)

var (
	protectedMountpoints = map[string]bool{
		"/":                true,
		"/boot":            true,
		"/boot/efi":        true,
		"/sysroot":         true,
		"/var":             true,
		"/var/lib/velocor": true,
	}
	pciBDFRegex = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)
)

// Config configures filesystem roots for HardwareManager (allowing hermetic unit testing).
type Config struct {
	SysfsRoot string
	ProcRoot  string
	DevRoot   string
}

// Manager handles Linux block/PCIe hardware discovery and boot-disk protection.
type Manager struct {
	cfg Config

	mu          sync.RWMutex
	bootDevs    map[string]bool // whole disks backing OS mountpoints/swap, e.g. "nvme0n1", "sda" (sticky)
	bootPCIAddr map[string]bool // NVMe controllers backing OS mountpoints, e.g. "0000:00:04.0" (sticky)
	inUseDevs   map[string]bool // whole disks backing any mount or swap (recomputed every refresh)
	inUsePCI    map[string]bool
}

// NewManager creates a new Linux Hardware Manager and initializes the boot-disk safety guard.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.SysfsRoot == "" {
		cfg.SysfsRoot = "/sys"
	}
	if cfg.ProcRoot == "" {
		cfg.ProcRoot = "/proc"
	}
	if cfg.DevRoot == "" {
		cfg.DevRoot = "/dev"
	}

	m := &Manager{
		cfg:         cfg,
		bootDevs:    make(map[string]bool),
		bootPCIAddr: make(map[string]bool),
		inUseDevs:   make(map[string]bool),
		inUsePCI:    make(map[string]bool),
	}

	if err := m.RefreshBootGuard(); err != nil {
		return nil, err
	}
	return m, nil
}

// RefreshBootGuard parses /proc/self/mountinfo, /proc/1/mountinfo and /proc/swaps to find
// every whole disk (and NVMe controller) that backs a mounted filesystem or active swap,
// resolving through partitions, device-mapper (LVM, LUKS), md and NVMe multipath.
//
// Disks backing OS mountpoints (/, /boot, /var, ...) or swap are remembered for the
// lifetime of the process; other mounted disks are protected only while mounted.
func (m *Manager) RefreshBootGuard() error {
	sets := newGuardSets()
	for _, p := range []string{
		filepath.Join(m.cfg.ProcRoot, "self", "mountinfo"),
		filepath.Join(m.cfg.ProcRoot, "1", "mountinfo"),
	} {
		if err := m.scanMountinfo(p, sets); err != nil {
			return err
		}
	}
	m.scanSwaps(sets)

	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range sets.bootDevs {
		m.bootDevs[k] = true
	}
	for k := range sets.bootPCI {
		m.bootPCIAddr[k] = true
	}
	m.inUseDevs, m.inUsePCI = sets.inUseDevs, sets.inUsePCI
	return nil
}

// IsBootDevice returns true if the given PCIe BDF address or block device path (kernel
// name, /dev path or /dev/disk/by-* link) backs an OS mountpoint, any mounted filesystem,
// or swap.
func (m *Manager) IsBootDevice(pciAddr, devicePath string) bool {
	var disks []string
	if devicePath != "" {
		disks = m.physicalDisks(m.kernelNameForPath(devicePath))
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	if pci := strings.ToLower(pciAddr); pci != "" && (m.bootPCIAddr[pci] || m.inUsePCI[pci]) {
		return true
	}
	for _, d := range disks {
		if m.bootDevs[d] || m.inUseDevs[d] {
			return true
		}
	}
	return false
}

// DiscoverDisks scans sysfs for PCIe NVMe controllers (class 0x010802) and non-rotational
// SATA/SAS/VirtIO SSDs, returning their current state and boot-device protection status.
func (m *Manager) DiscoverDisks() ([]*store.Disk, error) {
	var disks []*store.Disk
	seenBlockDevs := make(map[string]bool)

	// 1. Discover PCIe NVMe controllers (/sys/bus/pci/devices/* with class 0x010802).
	pciDevicesDir := filepath.Join(m.cfg.SysfsRoot, "bus", "pci", "devices")
	if entries, err := os.ReadDir(pciDevicesDir); err == nil {
		for _, entry := range entries {
			bdf := strings.ToLower(entry.Name())
			devDir := filepath.Join(pciDevicesDir, entry.Name())
			classStr := readSysfsString(filepath.Join(devDir, "class"))
			if !strings.HasPrefix(strings.ToLower(classStr), nvmePCIClass) {
				continue
			}

			driverName := readSymlinkBase(filepath.Join(devDir, "driver"))
			// A drive on a userspace driver (passed through to a VM, say) belongs to whatever
			// put it there: it is listed, and left alone until an operator claims it.
			driverState := store.DiskDriverStateKernel
			if driverName == "vfio-pci" || driverName == "uio_pci_generic" || driverName == "igb_uio" {
				driverState = store.DiskDriverStateUserspace
			}

			numaNode := int32(readSysfsInt(filepath.Join(devDir, "numa_node"), 0))
			if numaNode < 0 {
				numaNode = 0
			}

			// Check if kernel nvme block device exists under this PCI device.
			blockName, model, serial, fwRev, sizeBytes, blockSize := m.inspectNVMeSysfs(devDir)
			if blockName != "" {
				seenBlockDevs[blockName] = true
			}

			var devPath string
			if blockName != "" {
				devPath = filepath.Join(m.cfg.DevRoot, blockName)
			}
			name := blockName
			if name == "" {
				name = "nvme_" + strings.ReplaceAll(bdf, ":", "_")
			}
			id := "nvme-" + strings.ReplaceAll(strings.ReplaceAll(bdf, ":", "-"), ".", "-")

			isBoot := m.IsBootDevice(bdf, devPath)

			disks = append(disks, &store.Disk{
				ID:               id,
				Name:             name,
				Type:             store.DiskTypeNVMe,
				PCIAddress:       bdf,
				DevicePath:       devPath,
				SerialNumber:     serial,
				Model:            model,
				FirmwareRevision: fwRev,
				NUMANode:         numaNode,
				SizeBytes:        sizeBytes,
				BlockSize:        blockSize,
				DriverState:      driverState,
				IsBootDevice:     isBoot,
				HealthStatus:     store.HealthStatusOnline,
				WriteCache:       m.writeCacheState(blockName),
				UpdatedAt:        time.Now().UTC(),
			})
		}
	}

	// 2. Discover non-rotational SATA/SAS/VirtIO SSDs in /sys/block/*.
	sysBlockDir := filepath.Join(m.cfg.SysfsRoot, "block")
	if entries, err := os.ReadDir(sysBlockDir); err == nil {
		for _, entry := range entries {
			devName := entry.Name()
			if seenBlockDevs[devName] || !isCandidateSSDBlockName(devName) {
				continue
			}

			blockDir := filepath.Join(sysBlockDir, devName)
			devPath := m.stableDiskPath(devName)
			pciAddr := m.resolveBlockDevicePCIAddress(devName)
			// The HBA's PCI address is shared by OS and data disks, so judge by device only.
			isBoot := m.IsBootDevice("", filepath.Join(m.cfg.DevRoot, devName))
			model := readSysfsString(filepath.Join(blockDir, "device", "model"))

			rotational := readSysfsString(filepath.Join(blockDir, "queue", "rotational"))
			if rotational != "0" && !isBoot && !isVirtualDiskModel(model) {
				continue
			}

			sectors := readSysfsUint64(filepath.Join(blockDir, "size"), 0)
			sizeBytes := sectors * 512
			// Ignore tiny virtual block devices (e.g. < 64 MiB cloud-init cidata volumes).
			if sizeBytes < 64*1024*1024 {
				continue
			}
			logicalBlockSize := uint32(readSysfsUint64(filepath.Join(blockDir, "queue", "logical_block_size"), 512))

			serial := readSysfsString(filepath.Join(blockDir, "device", "serial"))
			if serial == "" {
				serial = readSysfsString(filepath.Join(blockDir, "serial"))
			}
			fwRev := readSysfsString(filepath.Join(blockDir, "device", "rev"))
			numaNode := int32(readSysfsInt(filepath.Join(blockDir, "device", "numa_node"), 0))
			if numaNode < 0 {
				numaNode = 0
			}

			wwid := readSysfsString(filepath.Join(blockDir, "device", "wwid"))
			if wwid == "" {
				wwid = readSysfsString(filepath.Join(blockDir, "wwid"))
			}
			disks = append(disks, &store.Disk{
				ID:               stableSSDID(wwid, serial, devName),
				Name:             devName,
				Type:             store.DiskTypeSSD,
				PCIAddress:       pciAddr,
				DevicePath:       devPath,
				SerialNumber:     serial,
				Model:            model,
				FirmwareRevision: fwRev,
				NUMANode:         numaNode,
				SizeBytes:        sizeBytes,
				BlockSize:        logicalBlockSize,
				DriverState:      store.DiskDriverStateKernel,
				IsBootDevice:     isBoot,
				HealthStatus:     store.HealthStatusOnline,
				WriteCache:       m.writeCacheState(devName),
				UpdatedAt:        time.Now().UTC(),
			})
		}
	}

	sort.Slice(disks, func(i, j int) bool {
		return disks[i].ID < disks[j].ID
	})
	return disks, nil
}

// PCIDriver returns the driver a PCI device is bound to right now ("" if none or unknown).
func (m *Manager) PCIDriver(pciAddr string) string {
	bdf := strings.ToLower(strings.TrimSpace(pciAddr))
	if !pciBDFRegex.MatchString(bdf) {
		return ""
	}
	return readSymlinkBase(filepath.Join(m.cfg.SysfsRoot, "bus", "pci", "devices", bdf, "driver"))
}

// BindNVMeToKernel unbinds an NVMe controller at pciAddr from a userspace driver (vfio-pci, uio)
// and rebinds it to the kernel's nvme driver.
func (m *Manager) BindNVMeToKernel(pciAddr string) error {
	bdf := strings.ToLower(strings.TrimSpace(pciAddr))
	if !pciBDFRegex.MatchString(bdf) {
		return fmt.Errorf("%w: invalid PCIe BDF address %q", store.ErrInvalidArgument, pciAddr)
	}
	if m.IsBootDevice(bdf, "") {
		return fmt.Errorf("%w: boot NVMe controller %s cannot be rebound", store.ErrBootDeviceProtected, bdf)
	}

	devDir := filepath.Join(m.cfg.SysfsRoot, "bus", "pci", "devices", bdf)
	if _, err := os.Stat(devDir); err != nil {
		return fmt.Errorf("pci device %s: %w", bdf, store.ErrNotFound)
	}

	currentDriver := readSymlinkBase(filepath.Join(devDir, "driver"))
	if currentDriver != "" && currentDriver != "nvme" {
		unbindPath := filepath.Join(devDir, "driver", "unbind")
		if err := os.WriteFile(unbindPath, []byte(bdf), 0200); err != nil {
			return fmt.Errorf("unbind %s from %s: %w", bdf, currentDriver, err)
		}
	}

	// Clear driver_override (writing "\n" clears override in Linux PCI sysfs).
	overridePath := filepath.Join(devDir, "driver_override")
	if err := os.WriteFile(overridePath, []byte("\n"), 0644); err != nil {
		return fmt.Errorf("clear driver_override for %s: %w", bdf, err)
	}

	probePath := filepath.Join(m.cfg.SysfsRoot, "bus", "pci", "drivers_probe")
	if err := os.WriteFile(probePath, []byte(bdf), 0200); err != nil {
		return fmt.Errorf("drivers_probe %s: %w", bdf, err)
	}
	// Writing to drivers_probe succeeds whether or not a driver took the device.
	if d := readSymlinkBase(filepath.Join(devDir, "driver")); d != "nvme" {
		return fmt.Errorf("the nvme driver didn't take %s (driver now: %q)", bdf, d)
	}
	return nil
}

func (m *Manager) resolveBlockDevicePCIAddress(baseDev string) string {
	blockPath := filepath.Join(m.cfg.SysfsRoot, "class", "block", baseDev)
	// Check /sys/class/block/<dev>/device/address (common on NVMe controllers).
	if addr := readSysfsString(filepath.Join(blockPath, "device", "address")); pciBDFRegex.MatchString(addr) {
		return strings.ToLower(addr)
	}
	// Resolve symlink target for PCI BDF segment (e.g. ../../devices/pci0000:00/0000:00:04.0/nvme/nvme0/nvme0n1).
	if target, err := filepath.EvalSymlinks(blockPath); err == nil {
		parts := strings.Split(target, string(filepath.Separator))
		// Walk from deepest to shallowest to find the leaf PCI endpoint BDF.
		for i := len(parts) - 1; i >= 0; i-- {
			if pciBDFRegex.MatchString(parts[i]) {
				return strings.ToLower(parts[i])
			}
		}
	}
	return ""
}

func (m *Manager) inspectNVMeSysfs(pciDevDir string) (blockName, model, serial, fwRev string, sizeBytes uint64, blockSize uint32) {
	blockSize = 512
	nvmeDir := filepath.Join(pciDevDir, "nvme")
	ctrlEntries, err := os.ReadDir(nvmeDir)
	if err != nil || len(ctrlEntries) == 0 {
		return "", "", "", "", 0, blockSize
	}
	ctrlDir := filepath.Join(nvmeDir, ctrlEntries[0].Name())
	model = readSysfsString(filepath.Join(ctrlDir, "model"))
	serial = readSysfsString(filepath.Join(ctrlDir, "serial"))
	fwRev = readSysfsString(filepath.Join(ctrlDir, "firmware_rev"))

	nsEntries, err := os.ReadDir(ctrlDir)
	if err == nil {
		for _, entry := range nsEntries {
			name := entry.Name()
			nsDir := filepath.Join(ctrlDir, name)
			switch {
			case nvmeHeadRegex.MatchString(name) && strings.HasPrefix(name, ctrlEntries[0].Name()+"n"):
				blockName = name
			case nvmePathRegex.MatchString(name):
				// Native NVMe multipath: the controller only holds the hidden path device
				// (nvme0c0n1); the usable block device is the subsystem head (nvmeXnY), whose
				// instance number need not match the controller's.
				blockName = m.multipathHead(name)
				if blockName != "" {
					nsDir = filepath.Join(m.cfg.SysfsRoot, "class", "block", blockName)
				}
			}
			if blockName != "" {
				sizeBytes = readSysfsUint64(filepath.Join(nsDir, "size"), 0) * 512
				blockSize = uint32(readSysfsUint64(filepath.Join(nsDir, "queue", "logical_block_size"), 512))
				break
			}
		}
	}
	return blockName, model, serial, fwRev, sizeBytes, blockSize
}

// multipathHead returns the NVMe multipath head device (nvmeXnY) that owns pathDev (nvmeXcYnZ).
func (m *Manager) multipathHead(pathDev string) string {
	heads, _ := filepath.Glob(filepath.Join(m.cfg.SysfsRoot, "class", "block", "nvme*n*"))
	for _, h := range heads {
		name := filepath.Base(h)
		if !nvmeHeadRegex.MatchString(name) {
			continue
		}
		if _, err := os.Stat(filepath.Join(h, "multipath", pathDev)); err == nil {
			return name
		}
	}
	return ""
}

func stripPartitionSuffix(devName string) string {
	// NVMe or loop/mmcblk style: nvme0n1p1 -> nvme0n1
	if strings.HasPrefix(devName, "nvme") || strings.HasPrefix(devName, "mmcblk") || strings.HasPrefix(devName, "loop") {
		if idx := strings.LastIndexByte(devName, 'p'); idx > 0 {
			suffix := devName[idx+1:]
			if _, err := strconv.Atoi(suffix); err == nil {
				return devName[:idx]
			}
		}
		return devName
	}
	// SCSI/VirtIO style: sda1 -> sda, vda2 -> vda
	i := len(devName)
	for i > 0 && devName[i-1] >= '0' && devName[i-1] <= '9' {
		i--
	}
	if i > 0 {
		return devName[:i]
	}
	return devName
}

func isCandidateSSDBlockName(name string) bool {
	return strings.HasPrefix(name, "sd") || strings.HasPrefix(name, "vd")
}

func isVirtualDiskModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, kw := range []string{"qemu", "vmware", "vbox", "virtio", "virtual disk"} {
		if strings.Contains(m, kw) {
			return true
		}
	}
	return false
}

func readSysfsString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// Some devices pad their identity strings with NULs (virtio-scsi serials, found on the lab).
	return strings.TrimFunc(string(data), func(r rune) bool { return unicode.IsSpace(r) || r == 0 })
}

func readSysfsInt(path string, defaultVal int) int {
	s := readSysfsString(path)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return defaultVal
	}
	return v
}

func readSysfsUint64(path string, defaultVal uint64) uint64 {
	s := readSysfsString(path)
	if s == "" {
		return defaultVal
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return defaultVal
	}
	return v
}

func readSymlinkBase(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}
