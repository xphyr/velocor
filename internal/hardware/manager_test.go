package hardware

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/store"
)

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestHardwareDiscoveryBootGuardAndVFIOBinding(t *testing.T) {
	root := t.TempDir()
	sysfs := filepath.Join(root, "sys")
	procfs := filepath.Join(root, "proc")
	devfs := filepath.Join(root, "dev")

	// 1. Setup synthetic /proc/self/mountinfo where:
	//    - /sysroot is backed by /dev/nvme0n1p3 (PCI 0000:00:04.0)
	//    - /boot is backed by /dev/sda1
	mountinfo := strings.Join([]string{
		"29 1 259:3 / /sysroot rw,relatime shared:1 - xfs /dev/nvme0n1p3 rw,attr2,inode64",
		"30 29 8:1 / /boot rw,relatime shared:2 - ext4 /dev/sda1 rw",
	}, "\n") + "\n"
	mustWriteFile(t, filepath.Join(procfs, "self", "mountinfo"), mountinfo)

	// 2. Setup /sys/class/block/nvme0n1 with PCIe address 0000:00:04.0.
	mustWriteFile(t, filepath.Join(sysfs, "class", "block", "nvme0n1", "device", "address"), "0000:00:04.0\n")

	// 3. Setup two PCIe NVMe controllers:
	//    - 0000:00:04.0 (boot NVMe bound to kernel "nvme")
	//    - 0000:01:00.0 (data NVMe bound to kernel "nvme")
	bootPCIDir := filepath.Join(sysfs, "bus", "pci", "devices", "0000:00:04.0")
	mustWriteFile(t, filepath.Join(bootPCIDir, "class"), "0x010802\n")
	mustWriteFile(t, filepath.Join(bootPCIDir, "numa_node"), "0\n")
	mustWriteFile(t, filepath.Join(bootPCIDir, "nvme", "nvme0", "model"), "Velocor Boot NVMe\n")
	mustWriteFile(t, filepath.Join(bootPCIDir, "nvme", "nvme0", "serial"), "BOOT0001\n")
	mustWriteFile(t, filepath.Join(bootPCIDir, "nvme", "nvme0", "firmware_rev"), "1.0\n")
	mustWriteFile(t, filepath.Join(bootPCIDir, "nvme", "nvme0", "nvme0n1", "size"), "209715200\n")
	mustWriteFile(t, filepath.Join(bootPCIDir, "nvme", "nvme0", "nvme0n1", "queue", "logical_block_size"), "4096\n")

	dataPCIDir := filepath.Join(sysfs, "bus", "pci", "devices", "0000:01:00.0")
	mustWriteFile(t, filepath.Join(dataPCIDir, "class"), "0x010802\n")
	mustWriteFile(t, filepath.Join(dataPCIDir, "numa_node"), "1\n")
	mustWriteFile(t, filepath.Join(dataPCIDir, "nvme", "nvme1", "model"), "Enterprise NVMe Gen5\n")
	mustWriteFile(t, filepath.Join(dataPCIDir, "nvme", "nvme1", "serial"), "DATA0001\n")
	mustWriteFile(t, filepath.Join(dataPCIDir, "nvme", "nvme1", "firmware_rev"), "2.4\n")
	mustWriteFile(t, filepath.Join(dataPCIDir, "nvme", "nvme1", "nvme1n1", "size"), "1073741824\n")
	mustWriteFile(t, filepath.Join(dataPCIDir, "nvme", "nvme1", "nvme1n1", "queue", "logical_block_size"), "4096\n")

	// Setup driver symlinks and unbind/drivers_probe files.
	nvmeDriverDir := filepath.Join(sysfs, "bus", "pci", "drivers", "nvme")
	mustWriteFile(t, filepath.Join(nvmeDriverDir, "unbind"), "")
	if err := os.Symlink(nvmeDriverDir, filepath.Join(bootPCIDir, "driver")); err != nil {
		t.Fatalf("symlink boot driver: %v", err)
	}
	if err := os.Symlink(nvmeDriverDir, filepath.Join(dataPCIDir, "driver")); err != nil {
		t.Fatalf("symlink data driver: %v", err)
	}
	mustWriteFile(t, filepath.Join(sysfs, "bus", "pci", "drivers_probe"), "")
	mustWriteFile(t, filepath.Join(sysfs, "module", "vfio", "parameters", "enable_unsafe_noiommu_mode"), "0\n")

	// 4. Setup /sys/block/sda (boot SSD) and /sys/block/sdb (data SSD) sharing the same SATA HBA PCIe BDF,
	// plus /sys/block/sdc (rotational HDD).
	hbaDir := filepath.Join(sysfs, "devices", "pci0000:00", "0000:00:1f.2")
	mustWriteFile(t, filepath.Join(hbaDir, "class"), "0x010601\n")
	sdaTarget := filepath.Join(hbaDir, "ata1", "host0", "target0:0:0", "0:0:0:0")
	sdbTarget := filepath.Join(hbaDir, "ata2", "host1", "target1:0:0", "1:0:0:0")

	mustWriteFile(t, filepath.Join(sdaTarget, "model"), "Boot SATA SSD\n")
	mustWriteFile(t, filepath.Join(sysfs, "block", "sda", "queue", "rotational"), "0\n")
	mustWriteFile(t, filepath.Join(sysfs, "block", "sda", "size"), "104857600\n")
	mustWriteFile(t, filepath.Join(sysfs, "block", "sda", "queue", "logical_block_size"), "512\n")
	if err := os.Symlink(sdaTarget, filepath.Join(sysfs, "block", "sda", "device")); err != nil {
		t.Fatalf("symlink sda device: %v", err)
	}

	mustWriteFile(t, filepath.Join(sdbTarget, "model"), "Samsung Enterprise SSD\n")
	mustWriteFile(t, filepath.Join(sdbTarget, "serial"), "SSD-SDB-01\x00\x00\n") // NUL padding, as virtio-scsi disks report
	mustWriteFile(t, filepath.Join(sysfs, "block", "sdb", "queue", "rotational"), "0\n")
	mustWriteFile(t, filepath.Join(sysfs, "block", "sdb", "size"), "419430400\n")
	mustWriteFile(t, filepath.Join(sysfs, "block", "sdb", "queue", "logical_block_size"), "4096\n")
	if err := os.Symlink(sdbTarget, filepath.Join(sysfs, "block", "sdb", "device")); err != nil {
		t.Fatalf("symlink sdb device: %v", err)
	}

	mustWriteFile(t, filepath.Join(sysfs, "block", "sdc", "queue", "rotational"), "1\n")
	mustWriteFile(t, filepath.Join(sysfs, "block", "sdc", "size"), "1000000000\n")

	// Virtual hypervisor disk (QEMU HARDDISK) with rotational=1 sharing the SATA HBA must still be discovered as non-boot.
	sddTarget := filepath.Join(hbaDir, "ata3", "host2", "target2:0:0", "2:0:0:0")
	mustWriteFile(t, filepath.Join(sddTarget, "model"), "QEMU HARDDISK\n")
	mustWriteFile(t, filepath.Join(sysfs, "block", "sdd", "queue", "rotational"), "1\n")
	mustWriteFile(t, filepath.Join(sysfs, "block", "sdd", "size"), "419430400\n")
	mustWriteFile(t, filepath.Join(sysfs, "block", "sdd", "queue", "logical_block_size"), "512\n")
	if err := os.Symlink(sddTarget, filepath.Join(sysfs, "block", "sdd", "device")); err != nil {
		t.Fatalf("symlink sdd device: %v", err)
	}

	// 5. Setup hugepages in sysfs.
	hpDir := filepath.Join(sysfs, "kernel", "mm", "hugepages", "hugepages-2048kB")
	mustWriteFile(t, filepath.Join(hpDir, "nr_hugepages"), "0\n")
	mustWriteFile(t, filepath.Join(hpDir, "free_hugepages"), "0\n")

	mgr, err := NewManager(Config{
		SysfsRoot:          sysfs,
		ProcRoot:           procfs,
		DevRoot:            devfs,
		AllowUnsafeNoIOMMU: true,
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	if !mgr.IsBootDevice("0000:00:04.0", "") {
		t.Fatalf("expected 0000:00:04.0 to be marked as boot device")
	}
	if !mgr.IsBootDevice("", "/dev/sda") {
		t.Fatalf("expected /dev/sda to be marked as boot device")
	}
	if mgr.IsBootDevice("0000:01:00.0", "/dev/nvme1n1") {
		t.Fatalf("expected 0000:01:00.0 / /dev/nvme1n1 not to be marked as boot device")
	}

	disks, err := mgr.DiscoverDisks()
	if err != nil {
		t.Fatalf("DiscoverDisks failed: %v", err)
	}
	// Expect 5 disks: nvme-0000-00-04-0 (boot), nvme-0000-01-00-0 (data), ssd-sda (boot), ssd-SSD-SDB-01 (data, serial-based ID), ssd-sdd (virtual data).
	// Bare rotational HDD sdc must be ignored.
	if len(disks) != 5 {
		t.Fatalf("expected 5 disks, got %d: %+v", len(disks), disks)
	}

	byID := make(map[string]*store.Disk)
	for _, d := range disks {
		byID[d.ID] = d
	}

	if !byID["nvme-0000-00-04-0"].IsBootDevice {
		t.Fatalf("expected nvme-0000-00-04-0 IsBootDevice=true")
	}
	if byID["nvme-0000-01-00-0"].IsBootDevice || byID["nvme-0000-01-00-0"].Model != "Enterprise NVMe Gen5" {
		t.Fatalf("unexpected data NVMe disk: %+v", byID["nvme-0000-01-00-0"])
	}
	if !byID["ssd-sda"].IsBootDevice {
		t.Fatalf("expected ssd-sda IsBootDevice=true")
	}
	if byID["ssd-SSD-SDB-01"].IsBootDevice || byID["ssd-SSD-SDB-01"].BlockSize != 4096 || byID["ssd-SSD-SDB-01"].SerialNumber != "SSD-SDB-01" {
		t.Fatalf("unexpected data SSD disk: %+v", byID["ssd-SSD-SDB-01"])
	}
	if byID["ssd-sdd"] == nil || byID["ssd-sdd"].IsBootDevice {
		t.Fatalf("expected virtual QEMU HARDDISK sdd to be discovered as non-boot: %+v", byID["ssd-sdd"])
	}

	// Verify BindNVMeToUserspace refuses the boot NVMe controller.
	if _, err := mgr.BindNVMeToUserspace("0000:00:04.0", "vfio-pci"); !errors.Is(err, store.ErrBootDeviceProtected) {
		t.Fatalf("expected ErrBootDeviceProtected on boot NVMe, got %v", err)
	}

	// Verify BindNVMeToUserspace succeeds on the data NVMe controller and enables noiommu mode.
	state, err := mgr.BindNVMeToUserspace("0000:01:00.0", "vfio-pci")
	if err != nil || state != store.DiskDriverStateVfioPCI {
		t.Fatalf("BindNVMeToUserspace(0000:01:00.0) got (%s, %v), want VFIO_PCI", state, err)
	}
	override := readSysfsString(filepath.Join(dataPCIDir, "driver_override"))
	if override != "vfio-pci" {
		t.Fatalf("expected driver_override=vfio-pci, got %q", override)
	}
	noiommu := readSysfsString(filepath.Join(sysfs, "module", "vfio", "parameters", "enable_unsafe_noiommu_mode"))
	if noiommu != "1" {
		t.Fatalf("expected vfio enable_unsafe_noiommu_mode=1 when no IOMMU groups exist, got %q", noiommu)
	}

	// Verify EnsureHugepages allocates 1024 MiB (512 x 2 MiB pages).
	hpStatus, err := mgr.EnsureHugepages(1024)
	if err != nil {
		t.Fatalf("EnsureHugepages(1024) failed: %v", err)
	}
	if hpStatus.TotalPages != 512 || hpStatus.TotalMB != 1024 {
		t.Fatalf("unexpected HugepageStatus: %+v", hpStatus)
	}
}

func TestReleaseHugepages(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "kernel", "mm", "hugepages", "hugepages-2048kB")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{"nr_hugepages": "512\n", "free_hugepages": "512\n"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{cfg: Config{SysfsRoot: root}}
	if _, err := m.ReleaseHugepages(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "nr_hugepages")); string(b) != "0\n" {
		t.Errorf("nr_hugepages = %q", b)
	}
}

// A drive on vfio-pci was put there by SPDK: claimed with the SPDK data plane, but not with
// the kernel one, which would otherwise take it from whatever holds it.
func TestDiscoveryVFIODriveClaimedOnlyWithSPDK(t *testing.T) {
	root := t.TempDir()
	sysfs := filepath.Join(root, "sys")
	dev := filepath.Join(sysfs, "bus", "pci", "devices", "0000:01:00.0")
	mustWriteFile(t, filepath.Join(dev, "class"), "0x010802\n")
	mustWriteFile(t, filepath.Join(dev, "numa_node"), "0\n")
	drv := filepath.Join(sysfs, "bus", "pci", "drivers", "vfio-pci")
	mustWriteFile(t, filepath.Join(drv, "bind"), "")
	if err := os.Symlink(drv, filepath.Join(dev, "driver")); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(root, "proc", "self", "mountinfo"), "")
	for _, kernel := range []bool{false, true} {
		m := &Manager{cfg: Config{SysfsRoot: sysfs, ProcRoot: filepath.Join(root, "proc"), DevRoot: filepath.Join(root, "dev"), KernelDataPlane: kernel}}
		disks, err := m.DiscoverDisks()
		if err != nil || len(disks) != 1 {
			t.Fatalf("kernel=%v: %+v %v", kernel, disks, err)
		}
		if d := disks[0]; d.DriverState != store.DiskDriverStateVfioPCI || d.Claimed == kernel {
			t.Errorf("kernel data plane %v: driver %s, claimed %v", kernel, d.DriverState, d.Claimed)
		}
	}
}
