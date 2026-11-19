# Velocor

**Velocor** is an open-source, API-first, all-flash SAN/NAS storage appliance distribution built on **SPDK** (Storage Performance Development Kit) and **Fedora Image Mode (`bootc`)**, written in Go.

Velocor decouples the user-space block data plane (`spdk_tgt`) from the control plane (`velod`) using Podman Quadlet containers on a thin, immutable `fedora-bootc:45` host OS—so the control plane can be upgraded or restarted without interrupting low-latency NVMe-oF and iSCSI I/O. (Updating the `spdk_tgt` data-plane container itself *does* interrupt I/O and is therefore never automatic.)

---

## Table of Contents

- [Core Components & Features](#core-components--features)
- [Architecture Overview](#architecture-overview)
- [Building from Source](#building-from-source)
- [Installing the Velocor Storage Appliance](#installing-the-velocor-storage-appliance)
  - [Option 1: Bare-Metal / VM Installation via Fedora Image Mode (`bootc`)](#option-1-bare-metal--vm-installation-via-fedora-image-mode-bootc)
  - [Option 2: Virtual Machine Disk Image (`qcow2`)](#option-2-virtual-machine-disk-image-qcow2)
  - [Option 3: Podman Quadlet Deployment on an Existing Linux Host](#option-3-podman-quadlet-deployment-on-an-existing-linux-host)
  - [Option 4: Local 2-Node QEMU/KVM Test Lab](#option-4-local-2-node-qemukvm-test-lab)
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

- **`velod` (`cmd/velod`)**: Node control-plane daemon, hardware manager (`vfio-pci` & hugepages), transactional desired-state store (`bbolt`), background drift reconciler, and single-port (`:8443`) REST/JSON + OpenAPI server, authenticated Prometheus `/metrics` endpoint, and embedded Web UI console.
- **`veloctl` (`cmd/veloctl`)**: Operator CLI for managing users, tokens, disks, pools, volumes, snapshots, network portals, initiator groups, and exports.
- **`velocor-spdk` (`spdk_tgt`)**: User-space, polled-mode SPDK `v26.09` data plane managing PCIe NVMe (`vfio-pci` / `bdev_nvme`) and SATA/SAS SSD (`io_uring` / `bdev_uring`) devices directly.
- **`velocor-csi` (`cmd/velocor-csi`)**: Kubernetes CSI driver (`csi.velocor.io`) supporting dynamic NVMe-oF/TCP and iSCSI provisioning, online volume expansion, CoW snapshots, cloning, and `VolumeGroupSnapshot`. Changed Block Tracking (CSI `SnapshotMetadata`, KEP-3314) is not supported yet; see [Changed Block Tracking](#changed-block-tracking-cbt-not-yet-supported).
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
|  | - REST + Embedded Web UI            |-->| - bdev_nvme (vfio-pci)            |  |
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

- **Go**: the version in `go.mod`'s `toolchain` line (container builds use exactly that version)
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

Images are named `$(IMAGE_PREFIX)/<name>:$(TAG)`. `IMAGE_PREFIX` defaults to `ghcr.io/xphyr`;
`TAG` defaults to `dev-<short sha>`, so a local build never overwrites a shared tag (`latest`
and version tags are pushed only by CI's release job).

```bash
# Build velocor-spdk, velocor-velod and velocor-csi locally
make images IMAGE_PREFIX=registry.example.com/velocor

# Push them, then build and push velocor-os (refuses a dirty tree: commit first)
make push IMAGE_PREFIX=registry.example.com/velocor
```

The OS image (`velocor-os`) embeds the `velocor-spdk` and `velocor-velod` images of the same
build, pinned by digest, so `make push` pushes those first and the OS image build resolves their
digests from the registry. An appliance therefore runs exactly the containers it was built and
tested with, needs no registry at boot, and changes them only through an OS upgrade.

Local fixes to SPDK live in `deploy/containers/spdk-patches/` (applied at build time; each
carries unit tests, which `make spdk-unit-tests` and CI run).

The Go toolchain, the SPDK source commit and the GitHub Actions are pinned (`make check-pins`
verifies it; Renovate proposes bumps). The Fedora base images follow Fedora's rolling release tag
(`:45`) to pick up its daily fixes: each build resolves the tag to a digest, builds from exactly
that digest and records it in the image's `org.opencontainers.image.base.name` /
`org.opencontainers.image.base.digest` labels (`skopeo inspect` shows which base a build used).

### Build a Local `qcow2` VM Image

To try a build of your own, push it (above) and turn its OS image into a disk image:
`make qcow2 IMAGE_PREFIX=... QCOW2_KEYS="a.pub b.pub"` runs `hack/build-qcow2.sh` (see
[Option 2](#option-2-virtual-machine-disk-image-qcow2)) for the OS image of the current commit.

### Logging in to the appliance

The appliance's only login is **`velocor-admin`** (member of `wheel`, passwordless sudo, SSH keys
only). Root cannot log in over SSH, and password authentication is disabled. Neither the image nor
the repository contains keys or passwords; give `velocor-admin` keys in one of these ways:

- `hack/build-qcow2.sh --authorized-keys <file.pub>` writes them to
  `/etc/ssh/authorized_keys.d/velocor-admin` in the installed disk.
- **cloud-init** (included in `velocor-os`): keys supplied by the hypervisor or a seed ISO go to
  `velocor-admin`.
- After installing with `bootc install to-disk` (below), add the file
  `/etc/ssh/authorized_keys.d/velocor-admin` (mode 0644, owned by root) to the installed system.

---

## Installing the Velocor Storage Appliance

### Option 1: Bare-Metal / VM Installation via Fedora Image Mode (`bootc`)

The recommended production deployment installs the immutable `velocor-os` image (`deploy/bootc/Containerfile`) onto a target boot disk. The image comes preconfigured with `vfio-pci`, `nvme_tcp`, and `ublk_drv` kernel modules, hugepage sysctls, `veloctl`, and the systemd Podman Quadlet definitions.

1. **Boot the target machine or VM** using a Fedora Live ISO (or any Linux rescue environment with Podman installed).
2. **Install `velocor-os` to the target boot drive** (e.g., `/dev/sda` or `/dev/nvme0n1`):

   ```bash
   export BOOTC_IMAGE="ghcr.io/xphyr/velocor-os:<version>"
   export TARGET_DISK="/dev/sda"

   sudo podman pull "${BOOTC_IMAGE}"

   sudo podman run --rm --privileged --pid=host \
     --security-opt label=type:unconfined_t \
     -v /var/lib/containers:/var/lib/containers \
     -v /dev:/dev \
     "${BOOTC_IMAGE}" \
     bootc install to-disk \
       --filesystem xfs \
       --generic-image \
       "${TARGET_DISK}"
   ```

   Then give `velocor-admin` your SSH key (root SSH login is disabled):

   ```bash
   sudo mount /dev/disk/by-label/root /mnt
   for d in /mnt/ostree/deploy/*/deploy/*.0; do
     sudo install -d -m 0755 "$d/etc/ssh/authorized_keys.d"
     sudo install -m 0644 ~/.ssh/id_ed25519.pub "$d/etc/ssh/authorized_keys.d/velocor-admin"
   done
   sudo umount /mnt
   ```

3. **Reboot into the installed disk.** On first boot, systemd automatically generates and starts `velocor-spdk.service` and `velocor-velod.service` from `/usr/share/containers/systemd/`.
4. **Verify service status on the appliance:**

   ```bash
   bootc status
   systemctl status velocor-spdk.service velocor-velod.service
   ```

5. **Atomic OS Upgrades & Rollbacks:**

   ```bash
   # Upgrade the appliance OS image via veloctl (or `bootc upgrade` on the host). The OS image
   # carries velod and spdk_tgt, so this is also how the containers are updated.
   veloctl system upgrade ghcr.io/xphyr/velocor-os:v0.2.0

   # Roll back to the previous OS deployment if needed
   veloctl system rollback
   ```

---

### Option 2: Virtual Machine Disk Image (`qcow2`)

Releases don't include a disk image. Build one from the published `velocor-os` image with
`hack/build-qcow2.sh` from this repository (check out the release's tag). The script needs no
root: it starts a throwaway Fedora Cloud VM under KVM, which pulls the image and runs
`bootc install to-disk` onto a second virtual disk; that disk becomes the qcow2. Nothing from
the build host goes into the image.

**Requirements on the build host:** x86_64 Linux with access to `/dev/kvm`, `qemu-system-x86_64`,
`qemu-img`, `genisoimage`, `ssh` and `curl`, and about 10 GB free in `~/.cache/velocor-qcow2`
(the builder's Fedora Cloud image is downloaded there once and reused).

```bash
git checkout <version>
hack/build-qcow2.sh \
  --image ghcr.io/xphyr/velocor-os:<version> \
  --output velocor-os-<version>.qcow2 \
  --size 32G \
  --authorized-keys ~/.ssh/id_ed25519.pub   # optional, repeatable
```

- `--image` takes a tag or a digest (`ghcr.io/xphyr/velocor-os@sha256:…`); pin a digest to
  build exactly the release you verified.
- `--size` is the boot disk's virtual size (thin; at least 32G leaves room for OS upgrades).
- `--authorized-keys` gives `velocor-admin` SSH keys (see
  [Logging in to the appliance](#logging-in-to-the-appliance)). Without it, supply keys with
  cloud-init, which the image includes.

Create the VM from a copy of the qcow2: UEFI firmware, a Q35 machine type, at least 4 vCPUs and
6 GiB of memory (SPDK reserves 1 GiB of hugepages), the qcow2 as the boot disk, the data disks
as separate virtio-scsi or NVMe devices, and a separate NIC for the SAN if you have one. On first
boot, read the initial admin password as described in the
[Appliance Quickstart](#appliance-quickstart-veloctl--web-ui). Later OS upgrades come from the
registry (`veloctl system upgrade`); there is no need to rebuild the qcow2.

### Option 3: Podman Quadlet Deployment on an Existing Linux Host

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

   # Lab VMs without a vIOMMU only: velod refuses to bind NVMe drives without an IOMMU
   # unless VELOCOR_ALLOW_UNSAFE_NOIOMMU=true is set in velocor-velod.container. Never
   # enable this on production hardware: it lets SPDK DMA anywhere in host memory.

   # Allocate at least 1024 MiB of 2 MiB hugepages for SPDK
   echo "vm.nr_hugepages = 512" | sudo tee /etc/sysctl.d/90-velocor.conf

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

### Option 4: Local 2-Node QEMU/KVM Test Lab

For local development and end-to-end testing, `hack/vm-lab/lab.sh` provisions a 2-node QEMU/KVM lab (3 GiB total RAM) consisting of `velocor-san` (with 2 emulated PCIe NVMe drives + 2 VirtIO-SCSI SSDs) and `velocor-initiator`:

```bash
# 1. Start velocor-san (API forwarded to https://127.0.0.1:18443) and velocor-initiator
./hack/vm-lab/lab.sh up

# 2. Run end-to-end verification (bootc, Quadlets, disk claim, RAID1 pool, NVMe-oF/TCP, snapshots, iSCSI)
./hack/vm-lab/lab.sh verify

# 3. Stop the lab VMs
./hack/vm-lab/lab.sh down
```

See [hack/vm-lab/README.md](hack/vm-lab/README.md) for environment overrides and SSH helpers.

---

## Appliance Quickstart (`veloctl` & Web UI)

### 1. Access the Web UI or Authenticate `veloctl`

- **Web UI Console**: Open `https://<appliance-ip>:8443` in your browser.
- **Bootstrap Credentials**:
  - **Username**: `admin`
  - **Password**: generated randomly on first boot and written to `/var/lib/velocor/initial-admin-password` (root-only), unless `VELOCOR_INIT_PASSWORD` is set on first boot. There is no shared default password.
  - The bootstrap password must be changed at first login; until then the API only allows changing it. velod deletes the file once the password has been changed.

Authenticate via `veloctl` (session token is saved to `~/.config/velocor/token`):

```bash
export VELOCOR_ENDPOINT="https://<appliance-ip>:8443"
# veloctl verifies TLS. Trust the appliance's self-signed certificate (copy it once):
scp root@<appliance-ip>:/var/lib/velocor/tls.crt ~/.config/velocor/appliance.crt
export VELOCOR_CA_CERT=~/.config/velocor/appliance.crt   # or pass --ca-cert

sudo cat /var/lib/velocor/initial-admin-password   # on the appliance
veloctl login --username admin                      # prompts for the password

# Required before any other API call: replace the bootstrap password
# (or simply sign in to the Web UI, which prompts for it)
veloctl passwd

# Check appliance health, SPDK connection, hugepages, and configuration warnings
veloctl system status
```

`VELOCOR_INSECURE=true` (or `--insecure`) skips certificate verification entirely; use it only in a lab.

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
# Create an SPDK RAID1 pool (1 MiB clusters, the default)
veloctl pool create \
  --name flash-pool-0 \
  --raid RAID1 \
  --disks nvme-0000-00-03-0,nvme-0000-00-04-0 \
  --cluster-size 1048576

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

# Say which hosts may connect (NQN from /etc/nvme/hostnqn on each host)
veloctl initiator-group create --name app-hosts --nqns nqn.2014-08.org.nvmexpress:uuid:<host-uuid>

# Export the volume over NVMe-oF/TCP to that group only
veloctl export create \
  --name exp-app-data \
  --volume <volume-id> \
  --protocol NVMEOF_TCP \
  --portals <portal-id> \
  --initiator-groups <initiator-group-id>

# Later: add a host to the group (re-applied to every export using it), see who's connected,
# or change who may reach one export
veloctl initiator-group update <initiator-group-id> --nqns nqn...:host-a,nqn...:host-b
veloctl export connections <export-id>
veloctl export access <export-id> --initiator-groups <id1>,<id2>
veloctl volume usage <volume-id>
```

Exports with no initiator groups are refused unless `--allow-any-host` is given explicitly, which lets anyone who can reach the portal connect.

#### Deleting volumes

A deleted volume isn't destroyed at once. It disappears from the API, the UI and Kubernetes, and
can't be exported, but its data is kept for **24 hours** by default. Until then an administrator
can bring it back, or purge it early. This applies to deletes from the CSI driver too, so a
deleted PVC can be recovered. A deleted volume keeps holding the space it used, and a pool
can't be deleted while it holds deleted volumes.

```bash
veloctl volume deleted                              # deleted volumes and when they'll be purged
veloctl volume restore [--name new-name] <volume-id>
veloctl volume purge --yes <volume-id>              # destroy its data now
veloctl system volume-retention --hours 72          # 0 = destroy volumes at once; at most 720
veloctl volume protect <volume-id>                  # refuse deletes; --off to allow them again
```

Volumes with deletion protection (`veloctl volume create --protect`, or **Protect** on the
volume's page) can't be deleted, by anyone, until an administrator turns protection off. A CSI
`DeleteVolume` for one fails with `FailedPrecondition`, and the provisioner keeps retrying.
Restoring, purging, protection and the retention period need an administrator; a CSI token can
only delete.

A snapshot that volumes were cloned from can be deleted too. It disappears at once, and its
data goes when the last of those volumes has been deleted and purged.

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

#### Shared volumes and KubeVirt live migration

- **ReadWriteMany (and ReadOnlyMany) are supported for raw block volumes (`volumeMode: Block`)**,
  which is what a KubeVirt VM needs to live-migrate: during a migration the VM's disk is used by
  the source and the target node at once. A shared volume gets one export (one NVMe-oF
  subsystem) that each node using it is allowed into; nodes are added and removed while the
  others stay connected. Whatever shares the device must coordinate its writes and use direct
  I/O, as QEMU does.
- **A ReadWriteMany filesystem is refused:** a filesystem written from two nodes is corrupted.
- **CDI StorageProfile:** on clusters with KubeVirt CDI, the chart's hook Job fills in each
  Velocor StorageClass's StorageProfile (`cdi.storageProfile` in `values.yaml`): ReadWriteMany
  block first, driver clones (`csi-clone`), snapshots for golden images. DataVolumes then need no
  access or volume mode of their own, and VMs created from them are live-migratable.

#### Changed Block Tracking (CBT): not yet supported

The driver does **not** implement the CSI `SnapshotMetadata` service (KEP-3314) and does not
advertise the `SNAPSHOT_METADATA_SERVICE` capability, so backup tools such as Kasten take full
backups of Velocor volumes rather than incremental, CBT-based ones. Full backups and restores
work normally.

The reason: the SPDK release Velocor ships (v26.09) has no RPC that reports which clusters of a
snapshot hold data, which is what CBT needs. Reporting changed blocks it can't actually measure
would let an incremental backup silently miss data, so the feature stays off until it can be
built correctly (an SPDK extension is planned; see `TODO.md`, item L-2). The appliance's
`/api/v1/snapshots/{id}/blocks:allocated` and `blocks:changed` endpoints return
`501 Not Implemented` for the same reason.

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
  --from-file=VELOCOR_CA_CERT=./velocor-tls.crt   # copy of /var/lib/velocor/tls.crt from the appliance
```

| Environment Key | Description |
| :--- | :--- |
| `VELOCOR_ENDPOINT` | Base HTTPS URL of the `velod` API on the Velocor appliance (e.g., `https://10.0.2.15:8443`). |
| `VELOCOR_TOKEN` | Personal/Service Access Token (`velo_pat_...`) with role `CSI_PROVISIONER` or `ADMIN`. |
| `VELOCOR_CA_CERT` | PEM certificate used to verify the appliance. For the auto-generated self-signed certificate, use the contents of `/var/lib/velocor/tls.crt`. Not needed when the appliance uses a certificate from a CA the cluster already trusts. |
| `VELOCOR_INSECURE` | Optional. `"true"` disables TLS verification entirely, exposing the API token to anyone on the network path. Lab use only. |

---

### 4. Deploy the CSI Driver & Snapshot Classes

> **Custom Registry & Release Manifests:** If you pushed `velocor-csi` to a custom registry, update the `image:` field in [deploy/k8s/csi/velocor-csi.yaml](deploy/k8s/csi/velocor-csi.yaml) (default: `ghcr.io/xphyr/velocor-csi:latest`). For tagged GitHub Releases, a `velocor-csi.yaml` pinned to that release's image digest is attached to the release; the Helm chart from a release pins the digest too (`image.digest`).

Apply the CSI driver manifest (creates the `CSIDriver`, RBAC roles, `velocor-csi-controller` Deployment, `velocor-csi-node` DaemonSet, and the `velocor-nvme-tcp` and `velocor-iscsi` StorageClasses):

```bash
kubectl apply -f deploy/k8s/csi/velocor-csi.yaml
```

Or install using the Helm chart ([deploy/charts/velocor-csi](deploy/charts/velocor-csi)):

```bash
helm install velocor-csi deploy/charts/velocor-csi \
  --namespace velocor-csi \
  --create-namespace \
  --set auth.existingSecret=velocor-csi-auth
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
| `portals` | Portal names or IDs, comma-separated | The chart's `defaultPortals`, else the protocol's only portal | Portals the class's volumes are exported on, all of the class's protocol. See below. |
| `csi.storage.k8s.io/fstype` | `ext4`, `xfs` | `ext4` | Filesystem formatted onto mounted volumes by the CSI node plugin. |

**Choosing portals.** An appliance often has portals on more than one network, for example a
management network and one or two SAN networks. The driver never guesses which one Kubernetes
should use: if the appliance has more than one portal of a class's protocol, set `portals` on
the class (or `defaultPortals` in the Helm values, which covers every class without its own
list and may name portals of both protocols). Otherwise provisioning fails with a message
listing the portals. Portals that don't exist, or of the wrong protocol, are refused when a
volume is provisioned.

Each volume is exported on every listed portal, in the listed order:

- **NVMe/TCP:** nodes connect to every portal, and the kernel's native NVMe multipath (on by
  default on Fedora, RHEL and RHCOS) merges the paths into one device. With native multipath
  off, nodes use only the first portal that answers.
- **iSCSI:** nodes log in to the first portal that answers. The others are a fallback for the
  next attach, not active paths (that needs dm-multipath, which the driver doesn't set up yet).

A volume keeps its class's list for its lifetime (Kubernetes doesn't let StorageClass
parameters change), while `defaultPortals` can be changed and applies at each volume's next
attach. A portal listening on every address (`0.0.0.0`) can't be used: nodes need an address
to connect to, so create a portal for each SAN address. The older `auth.portalOverride` value
(one `IP:port` for every volume) still works but is deprecated.

Example custom `StorageClass` pinning to a specific pool, exported on two SAN portals, with XFS:

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
  portals: san-a,san-b
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

- [Architecture Guide](docs/ARCHITECTURE.md) — Decoupled control/data planes, two-tier flash management, filter bdevs, snapshots, and unified API design.
- [Implementation Roadmap](docs/ROADMAP.md) — Phase-by-phase breakdown of completed subsystems.
- [QEMU/KVM 2-Node Lab Guide](hack/vm-lab/README.md) — Local virtualized SAN + initiator testing instructions.

---

## License

Velocor is licensed under the [Apache License 2.0](LICENSE).
