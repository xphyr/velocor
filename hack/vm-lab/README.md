# Velocor QEMU/KVM 2-Node Lab (Fedora Image Mode `bootc` 45)

This directory contains scripts and configuration for testing **Velocor** across two lightweight QEMU/KVM virtual machines (3 GiB total RAM):

1. **`velocor-san` (2048 MiB RAM, 2 vCPUs)**:
   - Boots directly from **Fedora Image Mode (`quay.io/fedora/fedora-bootc:45`)** installed to `velocor-bootc.qcow2` via `bootc install to-disk` from the OS image given in `BOOTC_IMAGE`.
   - Runs `velod` as a **Podman Quadlet** container (`velocor-velod.service`) embedded in that OS image; the data plane is the kernel's.
   - Boot disk on VirtIO (`/dev/vda`) — automatically protected by `velod`'s boot-disk safety guard.
   - 2 × emulated PCIe NVMe drives (`VELONVME0001`, `VELONVME0002`).
   - 2 × emulated non-rotational VirtIO-SCSI SSDs (`rotation_rate=1`, `VELOSSD0001`, `VELOSSD0002`).
2. **`velocor-initiator` (1024 MiB RAM, 2 vCPUs)**:
   - Linux initiator node for testing NVMe-oF/TCP (`nvme-cli`) and iSCSI (`iscsiadm`) block exports.

## Workflow

```bash
# 1. Build and push all images for this commit (dev-<sha>) to your registry
make push IMAGE_PREFIX=<registry>/<org>
export BOOTC_IMAGE=<registry>/<org>/velocor-os:dev-$(git rev-parse --short=7 HEAD)

# 2. Build the Fedora bootc 45 qcow2 disk image (if not already cached) and start the 2-node lab
./hack/vm-lab/lab.sh up

# 3. Verify bootc status, Podman Quadlets, boot-disk protection, and disk claiming
./hack/vm-lab/lab.sh verify

# 4. Stop the lab VMs
./hack/vm-lab/lab.sh down
```
