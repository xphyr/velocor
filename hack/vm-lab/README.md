# Velocor QEMU/KVM 2-Node Lab (Fedora Image Mode `bootc` 45)

This directory contains scripts and configuration for testing **Velocor** across two lightweight QEMU/KVM virtual machines (3 GiB total RAM):

1. **`velocor-san` (2048 MiB RAM, 2 vCPUs, 512 MiB Hugepages)**:
   - Boots directly from **Fedora Image Mode (`quay.io/fedora/fedora-bootc:45`)** installed to `velocor-bootc.qcow2` via `bootc install to-disk` from `registry.xphyrlab.net/velocor/velocor-os:latest`.
   - Runs decoupled **Podman Quadlet** containers (`velocor-spdk.service` running SPDK `v26.09` `spdk_tgt` and `velocor-velod.service` running `velod`) pulled from `registry.xphyrlab.net/velocor/`.
   - Boot disk on VirtIO (`/dev/vda`) — automatically protected by `velod`'s boot-disk safety guard.
   - 2 × emulated PCIe NVMe drives (`VELONVME0001`, `VELONVME0002`) for `vfio-pci` (`noiommu`) user-space polling (`bdev_nvme`).
   - 2 × emulated non-rotational VirtIO-SCSI SSDs (`rotation_rate=1`, `VELOSSD0001`, `VELOSSD0002`) for Linux `io_uring` (`bdev_uring`).
2. **`velocor-initiator` (1024 MiB RAM, 2 vCPUs)**:
   - Linux initiator node for testing NVMe-oF/TCP (`nvme-cli`) and iSCSI (`iscsiadm`) block exports.

## Workflow

```bash
# 1. Build and push velocor-spdk, velocor-velod, and velocor-os (fedora-bootc:45) to registry.xphyrlab.net
make push-containers

# 2. Build the Fedora bootc 45 qcow2 disk image (if not already cached) and start the 2-node lab
./hack/vm-lab/lab.sh up

# 3. Verify bootc status, Podman Quadlets, SPDK v26.09 JSON-RPC, boot-disk protection, and disk claiming
./hack/vm-lab/lab.sh verify

# 4. Stop the lab VMs
./hack/vm-lab/lab.sh down
```
