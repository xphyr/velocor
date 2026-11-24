package hardware

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/store"
)

type fakeHost struct {
	t                *testing.T
	sys, proc, dev   string
	mountinfo, swaps []string
}

func newFakeHost(t *testing.T) *fakeHost {
	root := t.TempDir()
	return &fakeHost{t: t, sys: filepath.Join(root, "sys"), proc: filepath.Join(root, "proc"), dev: filepath.Join(root, "dev")}
}

func (h *fakeHost) symlink(target, link string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		h.t.Fatal(err)
	}
}

// blockDev creates /sys/devices/.../<name> and /sys/class/block/<name> -> it, and /sys/dev/block/<majmin>.
func (h *fakeHost) blockDev(devicesPath, name, majMin string) string {
	dir := filepath.Join(h.sys, "devices", devicesPath, name)
	mustWriteFile(h.t, filepath.Join(dir, "size"), "2097152\n")
	h.symlink(dir, filepath.Join(h.sys, "class", "block", name))
	if majMin != "" {
		h.symlink(dir, filepath.Join(h.sys, "dev", "block", majMin))
	}
	return dir
}

func (h *fakeHost) partition(parentDir, name, majMin string) {
	dir := filepath.Join(parentDir, name)
	mustWriteFile(h.t, filepath.Join(dir, "partition"), "1\n")
	h.symlink(dir, filepath.Join(h.sys, "class", "block", name))
	if majMin != "" {
		h.symlink(dir, filepath.Join(h.sys, "dev", "block", majMin))
	}
}

func (h *fakeHost) manager() *Manager {
	h.t.Helper()
	mustWriteFile(h.t, filepath.Join(h.proc, "self", "mountinfo"), strings.Join(h.mountinfo, "\n")+"\n")
	if len(h.swaps) > 0 {
		mustWriteFile(h.t, filepath.Join(h.proc, "swaps"), "Filename Type Size Used Priority\n"+strings.Join(h.swaps, "\n")+"\n")
	}
	m, err := NewManager(Config{SysfsRoot: h.sys, ProcRoot: h.proc, DevRoot: h.dev})
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}

func TestGuardResolvesLVMOnLUKSRootToPhysicalDisk(t *testing.T) {
	h := newFakeHost(t)
	sda := h.blockDev("pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block", "sda", "8:0")
	h.partition(sda, "sda2", "8:2")
	luks := h.blockDev("virtual/block", "dm-0", "253:0")
	mustWriteFile(t, filepath.Join(luks, "dm", "name"), "luks-abc\n")
	h.symlink("../../sda2", filepath.Join(luks, "slaves", "sda2"))
	lv := h.blockDev("virtual/block", "dm-1", "253:1")
	mustWriteFile(t, filepath.Join(lv, "dm", "name"), "fedora-root\n")
	h.symlink("../../dm-0", filepath.Join(lv, "slaves", "dm-0"))

	h.mountinfo = []string{"29 1 253:1 / / rw shared:1 - xfs /dev/mapper/fedora-root rw"}
	m := h.manager()
	if !m.IsBootDevice("", "/dev/sda") {
		t.Fatalf("physical disk under LVM-on-LUKS root must be protected")
	}
}

func TestGuardProtectsAnyMountedDiskAndSwap(t *testing.T) {
	h := newFakeHost(t)
	sdb := h.blockDev("pci0000:00/0000:00:1f.2/ata2/host1/target1:0:0/1:0:0:0/block", "sdb", "8:16")
	h.partition(sdb, "sdb1", "8:17")
	h.blockDev("pci0000:00/0000:00:1f.2/ata3/host2/target2:0:0/2:0:0:0/block", "sdc", "8:32")
	h.blockDev("pci0000:00/0000:00:1f.2/ata4/host3/target3:0:0/3:0:0:0/block", "sdd", "8:48")
	mustWriteFile(t, filepath.Join(h.dev, "sdc"), "")

	h.mountinfo = []string{"40 1 8:17 / /u01 rw shared:9 - xfs /dev/sdb1 rw"}
	h.swaps = []string{"/dev/sdc partition 8388604 0 -2"}
	m := h.manager()
	if !m.IsBootDevice("", "/dev/sdb") {
		t.Fatalf("disk mounted at /u01 must be protected")
	}
	if !m.IsBootDevice("", "/dev/sdc") {
		t.Fatalf("swap disk must be protected")
	}
	if m.IsBootDevice("", "/dev/sdd") {
		t.Fatalf("unused disk must not be protected")
	}
}

func TestGuardResolvesMultipathNVMeBootToController(t *testing.T) {
	h := newFakeHost(t)
	// Native multipath: head nvme0n1 lives under the virtual subsystem; its path device
	// nvme0c1n1 lives under controller nvme1 at PCI 0000:03:00.0 (instance numbers differ).
	head := h.blockDev("virtual/nvme-subsystem/nvme-subsys0", "nvme0n1", "259:0")
	h.partition(head, "nvme0n1p2", "259:2")
	ctrlDir := filepath.Join(h.sys, "devices", "pci0000:00", "0000:00:01.0", "0000:03:00.0", "nvme", "nvme1")
	pathDev := filepath.Join(ctrlDir, "nvme0c1n1")
	mustWriteFile(t, filepath.Join(pathDev, "dev"), "")
	h.symlink(pathDev, filepath.Join(h.sys, "class", "block", "nvme0c1n1"))
	h.symlink(pathDev, filepath.Join(head, "multipath", "nvme0c1n1"))

	pciDir := filepath.Join(h.sys, "bus", "pci", "devices", "0000:03:00.0")
	mustWriteFile(t, filepath.Join(pciDir, "class"), "0x010802\n")
	h.symlink(filepath.Join(h.sys, "devices", "pci0000:00", "0000:00:01.0", "0000:03:00.0", "nvme"), filepath.Join(pciDir, "nvme"))

	h.mountinfo = []string{"29 1 259:2 / / rw shared:1 - xfs /dev/nvme0n1p2 rw"}
	m := h.manager()
	if !m.IsBootDevice("0000:03:00.0", "") {
		t.Fatalf("boot NVMe controller behind a multipath head must be protected")
	}
	if err := m.BindNVMeToKernel("0000:03:00.0"); !errors.Is(err, store.ErrBootDeviceProtected) {
		t.Fatalf("rebinding the boot NVMe must be refused, got %v", err)
	}
	disks, err := m.DiscoverDisks()
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 || disks[0].Name != "nvme0n1" || !disks[0].IsBootDevice {
		t.Fatalf("multipath NVMe discovery wrong: %+v", disks)
	}
}

func TestCheckClaimableRefusesExistingData(t *testing.T) {
	h := newFakeHost(t)
	h.blockDev("pci0000:00/x/block", "sde", "8:64")
	h.blockDev("pci0000:00/y/block", "sdf", "8:80")
	h.blockDev("pci0000:00/z/block", "sdg", "8:96")
	h.symlink("../../sdf", filepath.Join(h.sys, "class", "block", "sdg", "holders", "dm-5"))

	xfs := make([]byte, 256*1024)
	copy(xfs, "XFSB")
	mustWriteFile(t, filepath.Join(h.dev, "sde"), string(xfs))
	mustWriteFile(t, filepath.Join(h.dev, "sdf"), string(make([]byte, 256*1024)))
	mustWriteFile(t, filepath.Join(h.dev, "sdg"), string(make([]byte, 256*1024)))
	m := h.manager()

	if err := m.CheckClaimable(&store.Disk{ID: "e", DevicePath: filepath.Join(h.dev, "sde")}, false); !errors.Is(err, store.ErrInUse) || !strings.Contains(err.Error(), "XFS") {
		t.Fatalf("disk with XFS must be refused without force, got %v", err)
	}
	if err := m.CheckClaimable(&store.Disk{ID: "e", DevicePath: filepath.Join(h.dev, "sde")}, true); err != nil {
		t.Fatalf("force must override signature check, got %v", err)
	}
	if err := m.CheckClaimable(&store.Disk{ID: "f", DevicePath: filepath.Join(h.dev, "sdf")}, false); err != nil {
		t.Fatalf("blank disk must be claimable, got %v", err)
	}
	if err := m.CheckClaimable(&store.Disk{ID: "g", DevicePath: filepath.Join(h.dev, "sdg")}, true); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("disk with holders must be refused even with force, got %v", err)
	}
}

func TestStableSSDIdentity(t *testing.T) {
	h := newFakeHost(t)
	sdb := h.blockDev("pci0000:00/0000:00:1f.2/ata2/host1/target1:0:0/1:0:0:0/block", "sdb", "8:16")
	mustWriteFile(t, filepath.Join(sysBlock(h, "sdb"), "queue", "rotational"), "0\n")
	mustWriteFile(t, filepath.Join(sysBlock(h, "sdb"), "size"), "419430400\n")
	mustWriteFile(t, filepath.Join(sdb, "device", "wwid"), "naa.5000c500a1b2c3d4\n")
	mustWriteFile(t, filepath.Join(h.dev, "sdb"), "")
	h.symlink("../../sdb", filepath.Join(h.dev, "disk", "by-id", "ata-Samsung_SSD_S1"))
	h.symlink("../../sdb", filepath.Join(h.dev, "disk", "by-id", "wwn-0x5000c500a1b2c3d4"))
	h.symlink("../../sdb1", filepath.Join(h.dev, "disk", "by-id", "wwn-0x5000c500a1b2c3d4-part1"))

	disks, err := h.manager().DiscoverDisks()
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 1 {
		t.Fatalf("got %+v", disks)
	}
	if disks[0].ID != "ssd-naa.5000c500a1b2c3d4" {
		t.Fatalf("SSD ID must derive from WWID, got %s", disks[0].ID)
	}
	if want := filepath.Join(h.dev, "disk", "by-id", "wwn-0x5000c500a1b2c3d4"); disks[0].DevicePath != want {
		t.Fatalf("SSD path must be the stable WWN link, got %s", disks[0].DevicePath)
	}
}

func sysBlock(h *fakeHost, name string) string {
	dir := filepath.Join(h.sys, "block", name)
	if _, err := os.Lstat(dir); err != nil {
		h.symlink(filepath.Join(h.sys, "class", "block", name), dir)
	}
	return dir
}
