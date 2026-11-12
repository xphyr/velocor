# Velocor

**Velocor** is an open-source, API-first, all-flash SAN/NAS storage appliance distribution built on **SPDK** (Storage Performance Development Kit) and **Fedora Image Mode (`bootc`)**.

## Core Components

- **`velod`**: The node control-plane daemon, hardware manager, state reconciler (`bbolt`), and unified gRPC + REST/OpenAPI API server.
- **`veloctl`**: The CLI client for managing disks, storage pools, volumes, snapshots, and NVMe-oF / iSCSI exports.
- **`spdk_tgt`**: User-space, polled-mode data plane managing NVMe (`vfio-pci` / `bdev_nvme`) and SSD (`io_uring` / `bdev_uring`) storage devices directly.
- **Fedora Image Mode (`bootc`)**: Immutable, atomic container-native OS image with transactional upgrades and rollbacks.
