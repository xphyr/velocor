# Velocor

**Velocor** is an open-source, API-first, all-flash SAN/NAS storage appliance distribution built on **SPDK** (Storage Performance Development Kit) and **Fedora Image Mode (`bootc`)**, written in Go.

Velocor decouples the user-space block data plane (`spdk_tgt`) from the control plane (`velod`) using Podman Quadlet containers on a thin, immutable `fedora-bootc:45` host OS—enabling sub-second, zero-disruption control-plane upgrades while sustaining low-latency NVMe-oF and iSCSI I/O.

---

## Table of Contents

- [Core Components & Features](#core-components--features)
- [Architecture Overview](#architecture-overview)
- [Building from Source](#building-from-source)
- [Installing the Velocor Storage Appliance](#installing-the-velocor-storage-appliance)
  - [Option 1: Bare-Metal / VM Installation via Fedora Image Mode (`bootc`)](#option-1-bare-metal--vm-installation-via-fedora-image-mode-bootc)
  - [Option 2: Podman Quadlet Deployment on an Existing Linux Host](#option-2-podman-quadlet-deployment-on-an-existing-linux-host)
  - [Option 3: Local 2-Node QEMU/KVM Test Lab](#option-3-local-2-node-qemukvm-test-lab)
- [Appliance Quickstart (`veloctl` & Web UI)](#appliance-quickstart-veloctl--web-ui)
- [Installing the Kubernetes CSI Driver (`csi.velocor.io`)](#installing-the-kubernetes-csi-driver-csivelocorio)
  - [1. Kubernetes Worker Node Prerequisites](#1-kubernetes-worker-node-prerequisites)
  - [2. Prepare the Velocor Appliance & Service Token](#2-prepare-the-velocor-appliance--service-token)
  - [3. Create the Kubernetes Authentication Secret](#3-create-the-kubernetes-authentication-secret)
  - [4. Deploy the CSI Driver & Snapshot Classes](#4-deploy-the-csi-driver--snapshot-classes)
  - [5. StorageClass Parameters](#5-storageclass-parameters)
  - [6. Usage Examples (PVC, Snapshots, Clones, Raw Block, Expansion)](#6-usage-examples-pvc-snapshots-clones-raw-block-expansion)
- [Documentation](#documentation)

---

## Core Components & Features

- **`velod` (`cmd/velod`)**: Node control-plane daemon, hardware manager (`vfio-pci` & hugepages), transactional desired-state store (`bbolt`), background drift reconciler, and single-port (`:8443`) gRPC + REST/OpenAPI server, Prometheus `/metrics` endpoint, and embedded Web UI console.
- **`veloctl` (`cmd/veloctl`)**: Operator CLI for managing users, tokens, disks, pools, volumes, snapshots, Changed Block Tracking (CBT), network portals, initiator groups, and exports.
- **`velocor-spdk` (`spdk_tgt`)**: User-space, polled-mode SPDK `v26.09` data plane managing PCIe NVMe (`vfio-pci` / `bdev_nvme`) and SATA/SAS SSD (`io_uring` / `bdev_uring`) devices directly.
- **`velocor-csi` (`cmd/velocor-csi`)**: Kubernetes CSI driver (`csi.velocor.io`) supporting dynamic NVMe-oF/TCP and iSCSI provisioning, online volume expansion, CoW snapshots, cloning, `VolumeGroupSnapshot`, and CSI `SnapshotMetadata` (KEP-3314 CBT).
- **Boot-Disk Safety Guard**: Automatically resolves backing devices and PCIe BDF addresses for `/`, `/boot`, `/boot/efi`, and `/sysroot` on startup and permanently marks them `is_boot_device = true` so OS drives can never be unbound or formatted.

---

## Architecture Overview

```text
+-----------------------------------------------------------------------------------+
|                        Velocor Appliance (fedora-bootc:45)                        |
|                                                                                   |
|  +-------------------------------------+   +-----------------------------------+  |
|  | velocor-velod.service (Quadlet)     |   | velocor-spdk.service (Quadlet)    |  |
|  | - velod Control Plane (:8443 TLS)   |   | - spdk_tgt (User-Space Polled IO) |  |
|  | - gRPC + REST + Embedded Web UI     |-->| - bdev_nvme (vfio-pci)            |  |
|  | - bbolt DB (/var/lib/velocor/*.db)  |   | - bdev_uring (Linux io_uring)     |  |
|  | - Reconciler & Hardware Manager     |   | - bdev_raid + lvol_store + lvol   |  |
|  +-------------------------------------+   | - NVMe-oF/TCP (:4420) & iSCSI     |  |
|                     |                      +-----------------------------------+  |
|                     +---> /run/velocor/spdk.sock (JSON-RPC 2.0)                   |
+-----------------------------------------------------------------------------------+
```

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for a detailed breakdown of the storage hierarchy, filter bdevs, and control/data plane separation.

---

## Building from Source

### Prerequisites

- **Go** `1.23+`
- **Podman** (for building workload and `bootc` OS container images)
- **QEMU/KVM**, `qemu-img`, `genisoimage`, and `jq` (optional, for the local 2-node VM lab)

### Build Binaries

```bash
# Build velod, veloctl, and velocor-csi into ./bin/
make build

# Run unit tests with the race detector
make test
```

### Build & Push Container Images

By default, images use `IMAGE_PREFIX=registry.xphyrlab.net/velocor`. Override `IMAGE_PREFIX` to push to your own container registry:

```bash
# Build all container images (velocor-spdk, velocor-velod, velocor-csi, velocor-os)
make containers IMAGE_PREFIX=registry.example.com/velocor

# Push all container images to your registry
make push-containers IMAGE_PREFIX=registry.example.com/velocor
```

---

## Installing the Velocor Storage Appliance

### Option 1: Bare-Metal / VM Installation via Fedora Image Mode (`bootc`)

The recommended production deployment installs the immutable `velocor-os` image (`deploy/bootc/Containerfile`) onto a target boot disk. The image comes preconfigured with `vfio-pci`, `nvme_tcp`, and `ublk_drv` kernel modules, hugepage sysctls, `veloctl`, and the systemd Podman Quadlet definitions.

1. **Boot the target machine or VM** using a Fedora Live ISO (or any Linux rescue environment with Podman installed).
2. **Install `velocor-os` to the target boot drive** (e.g., `/dev/sda` or `/dev/nvme0n1`):

   ```bash
   export BOOTC_IMAGE="registry.xphyrlab.net/velocor/velocor-os:latest"
   export TARGET_DISK="/dev/sda"

   sudo podman pull "${BOOTC_IMAGE}"

   sudo podman run --rm --privileged --pid=host \
     --security-opt label=type:unconfined_t \
     -v /var/lib/containers:/var/lib/containers \
     -v /dev:/dev \
     -v /etc/ostree:/etc/ostree \
     -v "${HOME}/.ssh/authorized_keys:/tmp/authorized_keys:ro" \
     "${BOOTC_IMAGE}" \
     bootc install to-disk \
       --filesystem xfs \
       --generic-image \
       --root-ssh-authorized-keys /tmp/authorized_keys \
       "${TARGET_DISK}"
   ```

3. **Reboot into the installed disk.** On first boot, systemd automatically generates and starts `velocor-spdk.service` and `velocor-velod.service` from `/usr/share/containers/systemd/`.
4. **Verify service status on the appliance:**

   ```bash
   bootc status
   systemctl status velocor-spdk.service velocor-velod.service
   ```

5. **Atomic OS Upgrades & Rollbacks:**

   ```bash
   # Upgrade the appliance OS image via veloctl (or `bootc upgrade` on the host)
   veloctl system upgrade registry.xphyrlab.net/velocor/velocor-os:v0.2.0

   # Roll back to the previous OS deployment if needed
   veloctl system rollback
   ```

---

### Option 2: Podman Quadlet Deployment on an Existing Linux Host

You can run Velocor on an existing **Fedora 45+**, **RHEL / CentOS Stream 9/10**, or compatible Linux host with Podman `4.8+`:

1. **Install required host packages:**

   ```bash
   sudo dnf install -y podman nvme-cli iscsi-initiator-utils pciutils iproute xfsprogs jq
   ```

2. **Configure kernel modules, hugepages, and runtime directories:**

   ```bash
   cat <<'EOF' | sudo tee /etc/modules-load.d/velocor.conf
   vfio
   vfio_iommu_type1
   vfio_pci
   uio_pci_generic
   nvme_tcp
   ublk_drv
   EOF

   # Enable noiommu fallback only if running inside a VM without vIOMMU
   echo "options vfio enable_unsafe_noiommu_mode=1" | sudo tee /etc/modprobe.d/velocor-vfio.conf

   # Allocate at least 512 MiB of 2 MiB hugepages for SPDK
   echo "vm.nr_hugepages = 256" | sudo tee /etc/sysctl.d/90-velocor.conf

   sudo systemd-modules-load
   sudo sysctl --system
   sudo mkdir -p /var/lib/velocor /run/velocor /etc/containers/systemd
   ```

3. **Install the Podman Quadlet service units:**

   ```bash
   sudo cp deploy/quadlets/velocor-spdk.container /etc/containers/systemd/
   sudo cp deploy/quadlets/velocor-velod.container /etc/containers/systemd/

   sudo systemctl daemon-reload
   sudo systemctl start velocor-spdk.service velocor-velod.service
   ```

4. **Install the `veloctl` CLI on your workstation or host:**

   ```bash
   make build-veloctl
   sudo install -m 0755 bin/veloctl /usr/local/bin/veloctl
   ```

---

### Option 3: Local 2-Node QEMU/KVM Test Lab

For local development and end-to-end testing, `hack/vm-lab/lab.sh` provisions a 2-node QEMU/KVM lab (3 GiB total RAM) consisting of `velocor-san` (with 2 emulated PCIe NVMe drives + 2 VirtIO-SCSI SSDs) and `velocor-initiator`:

```bash
# 1. Start velocor-san (API forwarded to https://127.0.0.1:18443) and velocor-initiator
./hack/vm-lab/lab.sh up

# 2. Run end-to-end verification (bootc, Quadlets, disk claim, RAID1 pool, NVMe-oF/TCP, CBT, iSCSI)
./hack/vm-lab/lab.sh verify

# 3. Stop the lab VMs
./hack/vm-lab/lab.sh down
```

See [hack/vm-lab/README.md](hack/vm-lab/README.md) for environment overrides and SSH helpers.

---

## Appliance Quickstart (`veloctl` & Web UI)

### 1. Access the Web UI or Authenticate `veloctl`

- **Web UI Console**: Open `https://<appliance-ip>:8443` in your browser.
- **Default Bootstrap Credentials**:
  - **Username**: `admin`
  - **Password**: `velocor-admin-changeme` (unless overridden via `VELOCOR_INIT_PASSWORD` on first boot)

Authenticate via `veloctl` (session token is saved to `~/.config/velocor/token`):

```bash
export VELOCOR_ENDPOINT="https://<appliance-ip>:8443"

veloctl login --username admin --password velocor-admin-changeme

# Recommended: change the default admin password immediately
veloctl user update --username admin --password "<new-strong-password>"

# Check appliance health, SPDK connection, and hugepage status
veloctl system status
```

### 2. Discover & Claim Physical Disks

```bash
# Rescan hardware and list discovered NVMe and SSD drives
veloctl disk scan
veloctl disk list

# Claim non-boot NVMe drives (binds to vfio-pci and attaches to SPDK bdev_nvme)
veloctl disk claim nvme-0000-00-03-0
veloctl disk claim nvme-0000-00-04-0
```

### 3. Create a Storage Pool & Network Portal

```bash
# Create an SPDK RAID1 pool with a 64 KiB cluster size
veloctl pool create \
  --name flash-pool-0 \
  --raid RAID1 \
  --disks nvme-0000-00-03-0,nvme-0000-00-04-0 \
  --cluster-size 65536

veloctl pool list

# Create an NVMe-oF/TCP portal (port 4420) and/or iSCSI portal (port 3260)
veloctl portal create --name nvmf-tcp-0 --protocol NVMEOF_TCP --ip <appliance-san-ip> --port 4420
veloctl portal create --name iscsi-0    --protocol ISCSI      --ip <appliance-san-ip> --port 3260

veloctl portal list
```

### 4. Provision Volumes, Snapshots & Manual SAN Exports

```bash
# Create a 50 GiB thin-provisioned volume
veloctl volume create --name app-data --pool <pool-id> --size-gib 50 --thin=true

# Export the volume over NVMe-oF/TCP
veloctl export create \
  --name exp-app-data \
  --volume <volume-id> \
  --protocol NVMEOF_TCP \
  --portals <portal-id> \
  --allow-any-host=true
```

---

## Installing the Kubernetes CSI Driver (`csi.velocor.io`)

The Velocor CSI driver (`velocor-csi`) integrates Kubernetes clusters with a Velocor storage appliance over **NVMe-oF/TCP** and **iSCSI**.

### Supported CSI Capabilities

- **Dynamic Volume Provisioning & Deletion** (`ReadWriteOnce`, `ReadOnlyMany`, `ReadWriteOncePod`)
- **Filesystem (`ext4`, `xfs`) & Raw Block (`volumeMode: Block`) Volumes**
- **Per-Node Host ACLs**: Automatically registers each Kubernetes node's NQN/IQN in a dedicated Velocor `Host` and `InitiatorGroup` (`csi-node-<node-name>`) during `ControllerPublishVolume`
- **Online Volume Expansion** (`allowVolumeExpansion: true`)
- **CoW Volume Snapshots & Restores** (`VolumeSnapshot`)
- **Volume Cloning** (`dataSource` referencing an existing `PersistentVolumeClaim` or `VolumeSnapshot`)
- **Volume Group Snapshots** (`VolumeGroupSnapshot` via CSI `GroupController`)
- **Changed Block Tracking (CBT)** (`SnapshotMetadata` service implementing `GetMetadataAllocated` and `GetMetadataDelta`, KEP-3314)

---

### 1. Kubernetes Worker Node Prerequisites

Every Kubernetes worker node that mounts Velocor volumes must have the initiator utilities and kernel modules installed for your chosen protocol(s):

- **For NVMe-oF/TCP (`velocor-nvme-tcp`):**
  ```bash
  # Install nvme-cli and load the nvme-tcp kernel module on each worker node
  sudo dnf install -y nvme-cli xfsprogs e2fsprogs
  sudo modprobe nvme_tcp
  echo "nvme_tcp" | sudo tee /etc/modules-load.d/nvme-tcp.conf
  ```
- **For iSCSI (`velocor-iscsi`):**
  ```bash
  # Install open-iscsi / iscsi-initiator-utils and enable iscsid on each worker node
  sudo dnf install -y iscsi-initiator-utils xfsprogs e2fsprogs
  sudo systemctl enable --now iscsid
  ```

> **Note:** If your cluster uses `VolumeSnapshot` or `VolumeGroupSnapshot`, ensure the standard Kubernetes [external-snapshotter CRDs and snapshot-controller](https://github.com/kubernetes-csi/external-snapshotter) are installed on the cluster.

---

### 2. Prepare the Velocor Appliance & Service Token

Before deploying the CSI driver, verify that your Velocor appliance has:
1. At least one healthy **Storage Pool** (`veloctl pool list`).
2. At least one **Network Portal** matching your desired protocol (`veloctl portal list`, e.g., `NVMEOF_TCP` on `:4420` or `ISCSI` on `:3260`) reachable from the Kubernetes worker nodes.
3. A scoped **`CSI_PROVISIONER` service token**:

```bash
veloctl token create --name k8s-csi --role CSI_PROVISIONER
```

Copy the output `Secret Token` (`velo_pat_...`); it is shown only once.

---

### 3. Create the Kubernetes Authentication Secret

Create the `velocor-csi` namespace and the `velocor-csi-auth` Secret referenced by the `velocor-csi-controller` Deployment:

```bash
kubectl create namespace velocor-csi

kubectl -n velocor-csi create secret generic velocor-csi-auth \
  --from-literal=VELOCOR_ENDPOINT="https://<velocor-appliance-ip>:8443" \
  --from-literal=VELOCOR_TOKEN="velo_pat_<your-token>" \
  --from-literal=VELOCOR_INSECURE="true"
```

| Environment Key | Description |
| :--- | :--- |
| `VELOCOR_ENDPOINT` | Base HTTPS URL of the `velod` API on the Velocor appliance (e.g., `https://10.0.2.15:8443`). |
| `VELOCOR_TOKEN` | Personal/Service Access Token (`velo_pat_...`) with role `CSI_PROVISIONER` or `ADMIN`. |
| `VELOCOR_INSECURE` | Set to `"true"` when using the appliance's auto-generated self-signed TLS certificate, or `"false"` if using a trusted CA certificate. |

---

### 4. Deploy the CSI Driver & Snapshot Classes

> **Custom Registry Note:** If you pushed `velocor-csi` to a custom registry, update the `image:` field in [deploy/k8s/csi/velocor-csi.yaml](deploy/k8s/csi/velocor-csi.yaml) (default: `registry.xphyrlab.net/velocor/velocor-csi:latest`).

Apply the CSI driver manifest (creates the `CSIDriver`, RBAC roles, `velocor-csi-controller` Deployment, `velocor-csi-node` DaemonSet, and the `velocor-nvme-tcp` and `velocor-iscsi` StorageClasses):

```bash
kubectl apply -f deploy/k8s/csi/velocor-csi.yaml
```

Optionally, install the `VolumeSnapshotClass` and `VolumeGroupSnapshotClass`:

```bash
kubectl apply -f deploy/k8s/csi/volumesnapshotclass.yaml
kubectl apply -f deploy/k8s/csi/volumegroupsnapshotclass.yaml
```

Verify that the controller and node pods are running:

```bash
kubectl -n velocor-csi get pods -o wide
kubectl get storageclass
```

---

### 5. StorageClass Parameters

[deploy/k8s/csi/velocor-csi.yaml](deploy/k8s/csi/velocor-csi.yaml) installs two ready-to-use `StorageClass` resources (`velocor-nvme-tcp` and `velocor-iscsi`). You can also define custom `StorageClass` objects with the following parameters:

| Parameter | Values | Default | Description |
| :--- | :--- | :--- | :--- |
| `protocol` | `NVMEOF_TCP`, `ISCSI` | `NVMEOF_TCP` | SAN transport protocol used to export the volume to Kubernetes nodes. |
| `pool` | Pool ID or Pool Name | Auto-selected | Target Velocor storage pool. If omitted, selects the first online pool with sufficient free space. |
| `thin` | `"true"`, `"false"` | `"true"` | Provision thin (`true`) or thick (`false`) SPDK logical volumes. |
| `csi.storage.k8s.io/fstype` | `ext4`, `xfs` | `ext4` | Filesystem formatted onto mounted volumes by the CSI node plugin. |

Example custom `StorageClass` pinning to a specific pool with XFS:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: velocor-nvme-xfs
provisioner: csi.velocor.io
allowVolumeExpansion: true
reclaimPolicy: Delete
volumeBindingMode: Immediate
parameters:
  protocol: NVMEOF_TCP
  pool: flash-pool-0
  thin: "true"
  csi.storage.k8s.io/fstype: xfs
```

---

### 6. Usage Examples (PVC, Snapshots, Clones, Raw Block, Expansion)

#### Dynamic PVC & Pod Verification

A sample manifest is included at [deploy/k8s/csi/test-pvc-pod.yaml](deploy/k8s/csi/test-pvc-pod.yaml):

```bash
kubectl apply -f deploy/k8s/csi/test-pvc-pod.yaml
kubectl get pvc test-velocor-pvc
kubectl get pod test-velocor-pod
kubectl logs test-velocor-pod
```

#### Online Volume Expansion

Both `velocor-nvme-tcp` and `velocor-iscsi` have `allowVolumeExpansion: true` enabled. To grow an existing PVC:

```bash
kubectl patch pvc test-velocor-pvc -p '{"spec":{"resources":{"requests":{"storage":"5Gi"}}}}'
```

#### Creating a CoW `VolumeSnapshot`

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: test-velocor-snap
spec:
  volumeSnapshotClassName: velocor-snapshots
  source:
    persistentVolumeClaimName: test-velocor-pvc
```

#### Cloning a Volume from a Snapshot or PVC

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: cloned-velocor-pvc
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: velocor-nvme-tcp
  resources:
    requests:
      storage: 5Gi
  dataSource:
    name: test-velocor-snap
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
```

---

## Documentation

- [Architecture Guide](docs/ARCHITECTURE.md) — Decoupled control/data planes, two-tier flash management, filter bdevs, CBT, and unified API design.
- [Implementation Roadmap](docs/ROADMAP.md) — Phase-by-phase breakdown of completed subsystems.
- [QEMU/KVM 2-Node Lab Guide](hack/vm-lab/README.md) — Local virtualized SAN + initiator testing instructions.
