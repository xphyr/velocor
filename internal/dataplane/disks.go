package dataplane

import (
	"context"
	"fmt"
	"strings"

	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

// NVMeControllerName returns the deterministic SPDK controller name for a PCIe NVMe drive.
func NVMeControllerName(pciAddr string) string {
	return "Nvme_" + strings.NewReplacer(":", "_", ".", "_").Replace(pciAddr)
}

// NVMeControllerFromBdev returns the controller name of an NVMe namespace bdev ("<ctrl>n<nsid>").
func NVMeControllerFromBdev(bdev string) string {
	if i := strings.LastIndex(bdev, "n"); i > 0 {
		return bdev[:i]
	}
	return bdev
}

// UringBdevName returns the deterministic io_uring bdev name for a disk.
func UringBdevName(d *store.Disk) string {
	return "uring_" + d.ID
}

// AttachDisk hands a claimed disk to SPDK and returns the resulting base bdev name and
// driver state. On failure every partial step is undone (the NVMe drive is returned to
// the kernel nvme driver) so a failed claim never leaves the host without the disk.
func AttachDisk(ctx context.Context, c *spdk.Client, hw *hardware.Manager, d *store.Disk) (string, store.DiskDriverState, error) {
	if d.IsBootDevice {
		return "", "", store.ErrBootDeviceProtected
	}
	switch d.Type {
	case store.DiskTypeNVMe:
		if d.PCIAddress == "" {
			return "", "", fmt.Errorf("%w: nvme disk %s has no PCI address", store.ErrInvalidArgument, d.ID)
		}
		drv := d.DriverState
		rebound := false
		if hw != nil && drv != store.DiskDriverStateVfioPCI && drv != store.DiskDriverStateUioPCIGeneric {
			st, err := hw.BindNVMeToUserspace(d.PCIAddress, "")
			if err != nil {
				return "", "", fmt.Errorf("bind %s to userspace driver: %w", d.PCIAddress, err)
			}
			drv, rebound = st, true
		}
		bdevs, err := c.BdevNvmeAttachController(ctx, NVMeControllerName(d.PCIAddress), "PCIe", d.PCIAddress)
		if err == nil && len(bdevs) == 0 {
			err = fmt.Errorf("controller %s exposed no namespaces", d.PCIAddress)
		}
		if err != nil {
			if rebound && hw != nil {
				_ = hw.BindNVMeToKernel(d.PCIAddress)
			}
			return "", "", fmt.Errorf("attach nvme controller %s: %w", d.PCIAddress, err)
		}
		return bdevs[0], drv, nil
	case store.DiskTypeSSD:
		if d.DevicePath == "" {
			return "", "", fmt.Errorf("%w: disk %s has no device path", store.ErrInvalidArgument, d.ID)
		}
		name, err := c.BdevUringCreate(ctx, UringBdevName(d), d.DevicePath, d.BlockSize)
		if err != nil {
			return "", "", fmt.Errorf("create io_uring bdev for %s: %w", d.DevicePath, err)
		}
		return name, store.DiskDriverStateUringClaimed, nil
	default:
		return "", "", fmt.Errorf("%w: unsupported disk type %q", store.ErrInvalidArgument, d.Type)
	}
}

// DetachDisk releases a disk's SPDK base bdev. A missing bdev is not an error.
func DetachDisk(ctx context.Context, c *spdk.Client, d *store.Disk) error {
	if d.SPDKBdevName == "" {
		return nil
	}
	var err error
	switch d.Type {
	case store.DiskTypeNVMe:
		err = c.BdevNvmeDetachController(ctx, NVMeControllerFromBdev(d.SPDKBdevName))
	case store.DiskTypeSSD:
		err = c.BdevUringDelete(ctx, d.SPDKBdevName)
	}
	if err != nil && !spdk.IsNotFound(err) {
		return fmt.Errorf("detach %s: %w", d.SPDKBdevName, err)
	}
	return nil
}
