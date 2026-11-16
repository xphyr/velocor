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

	"github.com/velocor/velocor/internal/store"
)

const (
	nvmePCIClass = "0x010802"
	hugepageSize = 2 // 2 MiB per hugepage
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
	// AllowUnsafeNoIOMMU permits binding NVMe drives to vfio-pci in "unsafe noiommu" mode (or
	// to uio_pci_generic) when the host has no IOMMU. Without an IOMMU, SPDK can DMA anywhere
	// in host memory, so this is only appropriate for VMs/labs and must be opted into.
	AllowUnsafeNoIOMMU bool
}

// HugepageStatus reports 2 MiB hugepage allocation on the node.
type HugepageStatus struct {
	TotalPages uint64
	FreePages  uint64
	TotalMB    uint64
	FreeMB     uint64
}

// Manager handles Linux block/PCIe hardware discovery, boot-disk protection,
// hugepage allocation, and VFIO/UIO driver binding.
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
			driverState := store.DiskDriverStateKernel
			claimed := false
			switch driverName {
			case "vfio-pci":
				driverState = store.DiskDriverStateVfioPCI
				claimed = true
			case "uio_pci_generic", "igb_uio":
				driverState = store.DiskDriverStateUioPCIGeneric
				claimed = true
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
			if isBoot {
				claimed = false
			}

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
				Claimed:          claimed,
				HealthStatus:     store.HealthStatusOnline,
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
				Claimed:          false,
				HealthStatus:     store.HealthStatusOnline,
				UpdatedAt:        time.Now().UTC(),
			})
		}
	}

	sort.Slice(disks, func(i, j int) bool {
		return disks[i].ID < disks[j].ID
	})
	return disks, nil
}

// GetHugepageStatus reads current 2 MiB hugepage counts from sysfs.
func (m *Manager) GetHugepageStatus() (*HugepageStatus, error) {
	hpDir := filepath.Join(m.cfg.SysfsRoot, "kernel", "mm", "hugepages", "hugepages-2048kB")
	totalRaw, err := os.ReadFile(filepath.Join(hpDir, "nr_hugepages"))
	if err != nil {
		return nil, fmt.Errorf("read nr_hugepages: %w", err)
	}
	freeRaw, err := os.ReadFile(filepath.Join(hpDir, "free_hugepages"))
	if err != nil {
		return nil, fmt.Errorf("read free_hugepages: %w", err)
	}

	totalPages, err := strconv.ParseUint(strings.TrimSpace(string(totalRaw)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse nr_hugepages: %w", err)
	}
	freePages, err := strconv.ParseUint(strings.TrimSpace(string(freeRaw)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse free_hugepages: %w", err)
	}

	return &HugepageStatus{
		TotalPages: totalPages,
		FreePages:  freePages,
		TotalMB:    totalPages * hugepageSize,
		FreeMB:     freePages * hugepageSize,
	}, nil
}

// EnsureHugepages ensures at least minMB of 2 MiB hugepages are allocated in the kernel.
func (m *Manager) EnsureHugepages(minMB uint64) (*HugepageStatus, error) {
	requiredPages := (minMB + hugepageSize - 1) / hugepageSize
	hpDir := filepath.Join(m.cfg.SysfsRoot, "kernel", "mm", "hugepages", "hugepages-2048kB")
	nrPath := filepath.Join(hpDir, "nr_hugepages")

	status, err := m.GetHugepageStatus()
	if err == nil && status.TotalPages >= requiredPages {
		return status, nil
	}

	if err := os.WriteFile(nrPath, []byte(fmt.Sprintf("%d\n", requiredPages)), 0644); err != nil {
		return nil, fmt.Errorf("write %d to %s: %w", requiredPages, nrPath, err)
	}

	status, err = m.GetHugepageStatus()
	if err != nil {
		return nil, err
	}
	if status.TotalPages < requiredPages {
		return status, fmt.Errorf("hugepage allocation short: requested %d pages (%d MiB), got %d pages (%d MiB)",
			requiredPages, minMB, status.TotalPages, status.TotalMB)
	}
	return status, nil
}

// BindNVMeToUserspace unbinds an NVMe controller at pciAddr from the Linux kernel nvme driver
// and binds it to vfio-pci (or uio_pci_generic), automatically enabling vfio noiommu mode
// if running in a VM without an IOMMU. Refuses if the device is protected by the boot-disk guard.
func (m *Manager) BindNVMeToUserspace(pciAddr string, preferredDriver string) (store.DiskDriverState, error) {
	bdf := strings.ToLower(strings.TrimSpace(pciAddr))
	if !pciBDFRegex.MatchString(bdf) {
		return "", fmt.Errorf("%w: invalid PCIe BDF address %q", store.ErrInvalidArgument, pciAddr)
	}

	if m.IsBootDevice(bdf, "") {
		return "", fmt.Errorf("%w: refusing to unbind boot NVMe controller %s", store.ErrBootDeviceProtected, bdf)
	}

	devDir := filepath.Join(m.cfg.SysfsRoot, "bus", "pci", "devices", bdf)
	if _, err := os.Stat(devDir); err != nil {
		return "", fmt.Errorf("pci device %s: %w", bdf, store.ErrNotFound)
	}

	// Double-check whether any child block device under this PCI controller is a boot device.
	if blockName, _, _, _, _, _ := m.inspectNVMeSysfs(devDir); blockName != "" {
		if m.IsBootDevice(bdf, filepath.Join(m.cfg.DevRoot, blockName)) {
			return "", fmt.Errorf("%w: refusing to unbind boot NVMe block device %s (%s)", store.ErrBootDeviceProtected, blockName, bdf)
		}
	}

	targetDriver := preferredDriver
	if targetDriver == "" {
		targetDriver = "vfio-pci"
	}

	if targetDriver == "vfio-pci" && !m.hasHardwareIOMMU(devDir) {
		if !m.cfg.AllowUnsafeNoIOMMU {
			return "", fmt.Errorf("%w: no IOMMU is available for %s; enable the IOMMU (intel_iommu=on / amd_iommu=on) "+
				"or explicitly allow unsafe no-IOMMU DMA with VELOCOR_ALLOW_UNSAFE_NOIOMMU=true", store.ErrInvalidArgument, bdf)
		}
		// In VMs without vIOMMU, enable vfio unsafe noiommu mode if supported.
		noiommuParam := filepath.Join(m.cfg.SysfsRoot, "module", "vfio", "parameters", "enable_unsafe_noiommu_mode")
		if _, err := os.Stat(noiommuParam); err == nil {
			if err := os.WriteFile(noiommuParam, []byte("1\n"), 0644); err != nil {
				return "", fmt.Errorf("enable vfio noiommu mode: %w", err)
			}
		} else if _, err := os.Stat(filepath.Join(m.cfg.SysfsRoot, "bus", "pci", "drivers", "uio_pci_generic")); err == nil {
			targetDriver = "uio_pci_generic"
		}
	}

	currentDriver := readSymlinkBase(filepath.Join(devDir, "driver"))
	if currentDriver != "" && currentDriver != targetDriver {
		unbindPath := filepath.Join(devDir, "driver", "unbind")
		if err := os.WriteFile(unbindPath, []byte(bdf), 0200); err != nil {
			return "", fmt.Errorf("unbind %s from %s: %w", bdf, currentDriver, err)
		}
	}

	overridePath := filepath.Join(devDir, "driver_override")
	if err := os.WriteFile(overridePath, []byte(targetDriver+"\n"), 0644); err != nil {
		return "", fmt.Errorf("write driver_override %s for %s: %w", targetDriver, bdf, err)
	}

	probePath := filepath.Join(m.cfg.SysfsRoot, "bus", "pci", "drivers_probe")
	if err := os.WriteFile(probePath, []byte(bdf), 0200); err != nil {
		bindPath := filepath.Join(m.cfg.SysfsRoot, "bus", "pci", "drivers", targetDriver, "bind")
		if bindErr := os.WriteFile(bindPath, []byte(bdf), 0200); bindErr != nil {
			return "", fmt.Errorf("probe/bind %s to %s: %w", bdf, targetDriver, err)
		}
	}

	if targetDriver == "uio_pci_generic" {
		return store.DiskDriverStateUioPCIGeneric, nil
	}
	return store.DiskDriverStateVfioPCI, nil
}

// BindNVMeToKernel unbinds an NVMe controller at pciAddr from vfio-pci/uio_pci_generic
// and rebinds it to the Linux kernel nvme driver.
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
	return nil
}

func (m *Manager) hasHardwareIOMMU(devDir string) bool {
	if _, err := os.Stat(filepath.Join(devDir, "iommu")); err == nil {
		return true
	}
	groupsDir := filepath.Join(m.cfg.SysfsRoot, "kernel", "iommu_groups")
	entries, err := os.ReadDir(groupsDir)
	return err == nil && len(entries) > 0
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
	return strings.TrimSpace(string(data))
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
