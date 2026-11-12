# Velocor QEMU/KVM 2-Node Lab

This directory contains scripts and configuration for testing **Velocor** across two virtual machines:

1. **`velocor-san`**:
   - Boot disk on VirtIO (`/dev/vda`) — automatically protected by `velod`'s boot-disk safety guard.
   - 2 × emulated PCIe NVMe drives (`VELONVME0001`, `VELONVME0002`) for testing `vfio-pci` (or `vfio` `noiommu`) user-space polling (`bdev_nvme`).
   - 2 × emulated non-rotational VirtIO-SCSI SSDs (`rotation_rate=1`, `VELOSSD0001`, `VELOSSD0002`) for testing Linux `io_uring` (`bdev_uring`).
2. **`velocor-initiator`**:
   - Linux initiator node with `nvme-cli` (`nvme connect -t tcp ...`) and `iscsiadm` for end-to-end block I/O and CBT verification.
