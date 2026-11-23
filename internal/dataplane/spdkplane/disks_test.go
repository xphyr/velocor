package spdkplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/spdk/spdktest"
	"github.com/velocor/velocor/internal/store"
)

// fakeSysfs builds just enough of /sys for binding the NVMe controller at bdf, currently on
// driver, with an IOMMU present.
func fakeSysfs(t *testing.T, bdf, driver string) (root string, devDir string) {
	t.Helper()
	root = t.TempDir()
	sys := filepath.Join(root, "sys")
	devDir = filepath.Join(sys, "bus", "pci", "devices", bdf)
	drvDir := filepath.Join(sys, "bus", "pci", "drivers", driver)
	for _, d := range []string{devDir, drvDir, filepath.Join(sys, "kernel", "iommu_groups", "7"), filepath.Join(root, "proc", "self"), filepath.Join(root, "dev")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(drvDir, filepath.Join(devDir, "driver")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(drvDir, "unbind"), filepath.Join(devDir, "driver_override"), filepath.Join(sys, "bus", "pci", "drivers_probe"), filepath.Join(root, "proc", "self", "mountinfo")} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, devDir
}

func TestAttachNVMeRebindsAfterReboot(t *testing.T) {
	const pci = "0000:05:00.0"
	for _, tc := range []struct {
		name, driver string
		wantBind     bool
	}{
		{"back on the kernel driver after a reboot", "nvme", true},
		{"still on vfio-pci", "vfio-pci", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, devDir := fakeSysfs(t, pci, tc.driver)
			hw, err := hardware.NewManager(hardware.Config{SysfsRoot: filepath.Join(root, "sys"), ProcRoot: filepath.Join(root, "proc"), DevRoot: filepath.Join(root, "dev")})
			if err != nil {
				t.Fatal(err)
			}
			f := spdktest.New(t)
			f.AddNVMe(pci)
			// The store says vfio-pci, as it does after a reboot.
			d := &store.Disk{ID: "n", Type: store.DiskTypeNVMe, PCIAddress: pci, DriverState: store.DiskDriverStateVfioPCI}
			bdev, drv, err := AttachDisk(context.Background(), f.Client(), hw, d)
			if err != nil {
				t.Fatal(err)
			}
			override, _ := os.ReadFile(filepath.Join(devDir, "driver_override"))
			if got := strings.TrimSpace(string(override)) == "vfio-pci"; got != tc.wantBind {
				t.Fatalf("bound to vfio-pci: %v, want %v (driver_override %q)", got, tc.wantBind, override)
			}
			if bdev == "" || drv != store.DiskDriverStateVfioPCI {
				t.Fatalf("attach: bdev %q state %s", bdev, drv)
			}
		})
	}
}
