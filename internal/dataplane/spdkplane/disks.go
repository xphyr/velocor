package spdkplane

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/velocor/velocor/internal/dataplane"
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

// SSDBackend selects how SATA/SAS/virtio SSDs are handed to SPDK: "auto" (the default:
// io_uring when ProbeIOURing finds it usable, else AIO), "uring" (io_uring, the fastest,
// regardless of the probe) or "aio" (Linux AIO with O_DIRECT). A failed io_uring setup crashes
// spdk_tgt instead of returning an error (docs/review-history.md P0-6), so "auto" checks before every attach.
var SSDBackend = "auto"

// ProbeIOURing checks whether io_uring is usable (replaced in tests).
var ProbeIOURing = hardware.ProbeIOURing

// Logger receives the backend choice when "auto" changes its mind.
var Logger = slog.Default()

var (
	backendMu   sync.Mutex
	lastBackend string
)

// ssdBackend resolves SSDBackend to "uring" or "aio".
func ssdBackend() string {
	switch SSDBackend {
	case "uring", "aio":
		return SSDBackend
	}
	probe := ProbeIOURing()
	backend := "aio"
	if probe.Usable {
		backend = "uring"
	}
	backendMu.Lock()
	changed := backend != lastBackend
	lastBackend = backend
	backendMu.Unlock()
	if changed {
		if probe.Usable {
			Logger.Info("SSDs are attached to SPDK with io_uring")
		} else {
			Logger.Warn("SSDs are attached to SPDK with AIO, because io_uring may not work for spdk_tgt (a failed io_uring setup crashes it)", "reason", probe.Reason)
		}
	}
	return backend
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
		if hw != nil {
			// Check the binding the drive has now, not the recorded one: after a reboot every
			// NVMe drive is back on the kernel nvme driver although the store still says vfio-pci
			// (found on the P0-1 lab: every NVMe pool was unavailable after a reboot).
			switch hw.PCIDriver(d.PCIAddress) {
			case "vfio-pci":
				drv = store.DiskDriverStateVfioPCI
			case "uio_pci_generic":
				drv = store.DiskDriverStateUioPCIGeneric
			default:
				st, err := hw.BindNVMeToUserspace(d.PCIAddress, "")
				if err != nil {
					return "", "", fmt.Errorf("bind %s to userspace driver: %w", d.PCIAddress, err)
				}
				drv, rebound = st, true
			}
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
		if ssdBackend() == "aio" {
			name, err := c.BdevAioCreate(ctx, UringBdevName(d), d.DevicePath, d.BlockSize, dataplane.DiskBdevUUID(d))
			if err != nil {
				return "", "", fmt.Errorf("create aio bdev for %s: %w", d.DevicePath, err)
			}
			return name, store.DiskDriverStateAIOClaimed, nil
		}
		name, err := c.BdevUringCreate(ctx, UringBdevName(d), d.DevicePath, d.BlockSize, dataplane.DiskBdevUUID(d))
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
	if d.Handle == "" {
		return nil
	}
	var err error
	switch d.Type {
	case store.DiskTypeNVMe:
		err = c.BdevNvmeDetachController(ctx, NVMeControllerFromBdev(d.Handle))
	case store.DiskTypeSSD:
		if d.DriverState == store.DiskDriverStateAIOClaimed {
			err = c.BdevAioDelete(ctx, d.Handle)
		} else {
			err = c.BdevUringDelete(ctx, d.Handle)
		}
	}
	if err != nil && !spdk.IsNotFound(err) {
		return fmt.Errorf("detach %s: %w", d.Handle, err)
	}
	return nil
}
