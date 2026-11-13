#!/usr/bin/env bash
set -euo pipefail

# Velocor Lightweight 2-Node QEMU/KVM Test Lab (3 GiB Total RAM)
# Node 1 (velocor-san):       2048 MiB RAM, 2 vCPUs, 512 MiB hugepages, Fedora bootc 45 + Podman Quadlets
# Node 2 (velocor-initiator): 1024 MiB RAM, 2 vCPUs, NVMe-oF/TCP & iSCSI client

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
LAB_DIR="${VELOCOR_LAB_DIR:-${SCRIPT_DIR}/.vms}"

SAN_RAM_MB="${SAN_RAM_MB:-2048}"
SAN_CPUS="${SAN_CPUS:-2}"
SAN_API_PORT="${SAN_API_PORT:-18443}"
SAN_SSH_PORT="${SAN_SSH_PORT:-12221}"
SAN_NVMEOF_PORT="${SAN_NVMEOF_PORT:-14420}"

INIT_RAM_MB="${INIT_RAM_MB:-1024}"
INIT_CPUS="${INIT_CPUS:-2}"
INIT_SSH_PORT="${INIT_SSH_PORT:-12222}"

FEDORA_RELEASE="${FEDORA_RELEASE:-45}"
BASE_IMAGE="${BASE_IMAGE:-${LAB_DIR}/fedora-cloud-base.qcow2}"
BOOTC_DISK="${BOOTC_DISK:-${LAB_DIR}/velocor-bootc.qcow2}"
BOOTC_IMAGE="${BOOTC_IMAGE:-registry.xphyrlab.net/velocor/velocor-os:latest}"

usage() {
  cat <<EOF
Usage: $(basename "$0") <command>

Commands:
  fetch-image      Download Fedora Cloud Base qcow2 image (used for initiator & bootc disk builder)
  build-bootc-disk Build bootable Fedora bootc 45 qcow2 disk (${BOOTC_DISK}) from ${BOOTC_IMAGE}
  init-disks       Create synthetic NVMe (.raw), SSD (.raw), and cloud-init seed ISOs in ${LAB_DIR}
  up               Start velocor-san (booting Fedora bootc 45) and velocor-initiator QEMU/KVM VMs
  verify           Verify Fedora bootc 45 status, Podman Quadlets, SPDK JSON-RPC, and veloctl APIs
  ssh-san          Run a command (or interactive shell) on velocor-san as root
  ssh-init         Run a command (or interactive shell) on velocor-initiator as fedora
  down             Stop running lab VMs cleanly
  status           Show PID and port bindings for lab VMs
  clean            Stop lab VMs and remove ${LAB_DIR}

Environment Overrides:
  VELOCOR_LAB_DIR=${LAB_DIR}
  BOOTC_IMAGE=${BOOTC_IMAGE}
  BOOTC_DISK=${BOOTC_DISK}
  SAN_RAM_MB=${SAN_RAM_MB}  INIT_RAM_MB=${INIT_RAM_MB}
  SAN_API_PORT=${SAN_API_PORT}  SAN_SSH_PORT=${SAN_SSH_PORT}  SAN_NVMEOF_PORT=${SAN_NVMEOF_PORT}
  INIT_SSH_PORT=${INIT_SSH_PORT}
EOF
}

ensure_ssh_key() {
  mkdir -p "${LAB_DIR}"
  local ssh_key="${LAB_DIR}/id_ed25519"
  if [[ ! -f "${ssh_key}" ]]; then
    ssh-keygen -t ed25519 -N "" -f "${ssh_key}" -q
  fi
}

find_host_auth_json() {
  local uid
  uid="$(id -u)"
  for candidate in \
    "${REGISTRY_AUTH_FILE:-}" \
    "/run/user/${uid}/containers/auth.json" \
    "${HOME}/.config/containers/auth.json"; do
    if [[ -n "${candidate}" && -f "${candidate}" ]]; then
      echo "${candidate}"
      return 0
    fi
  done
  return 1
}

fetch_image() {
  mkdir -p "${LAB_DIR}"
  if [[ -f "${BASE_IMAGE}" ]]; then
    echo "Base image already cached at ${BASE_IMAGE}"
    return 0
  fi
  local img_url="${FEDORA_CLOUD_URL:-}"
  if [[ -z "${img_url}" ]]; then
    for rel in "${FEDORA_RELEASE}" 44; do
      local base_dir="https://download.fedoraproject.org/pub/fedora/linux/releases/${rel}/Cloud/x86_64/images/"
      local fname
      fname="$(curl -fsSL "${base_dir}" 2>/dev/null | grep -oE 'Fedora-Cloud-Base-Generic-[0-9]+-[0-9.]+\.x86_64\.qcow2' | head -n1 || true)"
      if [[ -n "${fname}" ]]; then
        img_url="${base_dir}${fname}"
        break
      fi
    done
  fi
  if [[ -z "${img_url}" ]]; then
    echo "Error: Could not resolve Fedora Cloud Base qcow2 URL" >&2
    exit 1
  fi
  echo "Downloading ${img_url} to ${BASE_IMAGE}..."
  curl -fL --retry 3 -o "${BASE_IMAGE}.part" "${img_url}"
  mv "${BASE_IMAGE}.part" "${BASE_IMAGE}"
  echo "Downloaded ${BASE_IMAGE}"
}

build_seed_iso() {
  local node_name="$1"
  local iso_path="${LAB_DIR}/${node_name}-cidata.iso"
  if [[ -f "${iso_path}" ]]; then
    return 0
  fi

  ensure_ssh_key
  local pubkey
  pubkey="$(cat "${LAB_DIR}/id_ed25519.pub")"

  local tmp_dir="${LAB_DIR}/cidata-${node_name}"
  mkdir -p "${tmp_dir}"

  cat > "${tmp_dir}/meta-data" <<EOF
instance-id: ${node_name}
local-hostname: ${node_name}
EOF

  cat > "${tmp_dir}/user-data" <<EOF
#cloud-config
users:
  - name: root
    ssh_authorized_keys:
      - ${pubkey}
  - name: fedora
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - ${pubkey}
chpasswd:
  expire: false
  users:
    - name: root
      password: velocor-lab-password
      type: text
ssh_pwauth: true
EOF

  genisoimage -output "${iso_path}" -volid cidata -joliet -rock "${tmp_dir}/user-data" "${tmp_dir}/meta-data" >/dev/null 2>&1
  rm -rf "${tmp_dir}"
}

build_bootc_disk() {
  if [[ -f "${BOOTC_DISK}" ]]; then
    echo "Fedora bootc disk already exists at ${BOOTC_DISK}"
    return 0
  fi

  fetch_image
  ensure_ssh_key
  build_seed_iso "velocor-bib"

  local bib_os="${LAB_DIR}/bib-os.qcow2"
  local bib_pidfile="${LAB_DIR}/bib.pid"
  local bib_ssh_port=12229
  local target_disk="${BOOTC_DISK}.part"

  rm -f "${bib_os}" "${target_disk}"
  qemu-img create -f qcow2 -F qcow2 -b "${BASE_IMAGE}" "${bib_os}" 20G >/dev/null
  qemu-img create -f qcow2 "${target_disk}" 20G >/dev/null

  echo "Starting temporary builder VM to run 'bootc install to-disk' for ${BOOTC_IMAGE}..."
  qemu-system-x86_64 \
    -name velocor-bib \
    -machine q35,accel=kvm \
    -cpu host \
    -m 2048 \
    -smp 4 \
    -daemonize \
    -pidfile "${bib_pidfile}" \
    -display none \
    -serial "file:${LAB_DIR}/bib-console.log" \
    -drive "file=${bib_os},if=none,id=osdisk,format=qcow2" \
    -device "virtio-blk-pci,drive=osdisk,bootindex=1" \
    -drive "file=${target_disk},if=none,id=targetdisk,format=qcow2" \
    -device "virtio-blk-pci,drive=targetdisk,serial=BOOTCTARGET" \
    -drive "file=${LAB_DIR}/velocor-bib-cidata.iso,if=virtio,media=cdrom,readonly=on" \
    -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:${bib_ssh_port}-:22" \
    -device virtio-net-pci,netdev=net0

  cleanup_bib() {
    if [[ -f "${bib_pidfile}" ]]; then
      local pid
      pid="$(cat "${bib_pidfile}")"
      kill "${pid}" 2>/dev/null || true
      rm -f "${bib_pidfile}"
    fi
    rm -f "${bib_os}"
  }
  trap cleanup_bib EXIT RETURN

  local ssh_opts=(-i "${LAB_DIR}/id_ed25519" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5)
  echo "Waiting for SSH on builder VM (127.0.0.1:${bib_ssh_port})..."
  for _ in $(seq 1 40); do
    if ssh "${ssh_opts[@]}" -p "${bib_ssh_port}" fedora@127.0.0.1 "true" 2>/dev/null; then
      break
    fi
    sleep 2
  done

  local auth_file
  if auth_file="$(find_host_auth_json)"; then
    scp "${ssh_opts[@]}" -P "${bib_ssh_port}" "${auth_file}" fedora@127.0.0.1:/tmp/auth.json
  fi
  scp "${ssh_opts[@]}" -P "${bib_ssh_port}" "${LAB_DIR}/id_ed25519.pub" fedora@127.0.0.1:/tmp/id_ed25519.pub

  echo "Installing ${BOOTC_IMAGE} onto /dev/vdb via 'bootc install to-disk'..."
  ssh "${ssh_opts[@]}" -p "${bib_ssh_port}" fedora@127.0.0.1 "sudo BOOTC_IMAGE='${BOOTC_IMAGE}' bash -s" <<'EOF'
set -euo pipefail
dnf install -y podman xfsprogs >/dev/null

mkdir -p /etc/ostree /root/.config/containers
if [[ -f /tmp/auth.json ]]; then
  install -m 0600 /tmp/auth.json /etc/ostree/auth.json
  install -m 0600 /tmp/auth.json /root/.config/containers/auth.json
fi

podman pull "${BOOTC_IMAGE}"

podman run --rm --privileged --pid=host \
  --security-opt label=type:unconfined_t \
  -v /var/lib/containers:/var/lib/containers \
  -v /dev:/dev \
  -v /etc/ostree:/etc/ostree \
  -v /tmp/id_ed25519.pub:/tmp/id_ed25519.pub:ro \
  "${BOOTC_IMAGE}" \
  bootc install to-disk \
    --filesystem xfs \
    --generic-image \
    --root-ssh-authorized-keys /tmp/id_ed25519.pub \
    /dev/vdb

# Inject registry auth into the installed ostree deployment so Quadlets can pull on first boot
if [[ -f /tmp/auth.json ]]; then
  mkdir -p /mnt/bootc-target
  mount /dev/vdb3 /mnt/bootc-target
  for deploy_dir in /mnt/bootc-target/ostree/deploy/*/deploy/*.0; do
    mkdir -p "${deploy_dir}/etc/ostree" "${deploy_dir}/etc/containers"
    install -m 0600 /tmp/auth.json "${deploy_dir}/etc/ostree/auth.json"
  done
  for var_dir in /mnt/bootc-target/ostree/deploy/*/var; do
    mkdir -p "${var_dir}/roothome/.config/containers"
    install -m 0600 /tmp/auth.json "${var_dir}/roothome/.config/containers/auth.json"
  done
  sync
  umount /mnt/bootc-target
fi
sync
EOF

  cleanup_bib
  trap - EXIT RETURN
  sleep 1
  mv "${target_disk}" "${BOOTC_DISK}"
  echo "Successfully created Fedora bootc 45 disk image: ${BOOTC_DISK}"
}

init_disks() {
  mkdir -p "${LAB_DIR}"
  for i in 1 2; do
    nvme_img="${LAB_DIR}/san-nvme${i}.raw"
    if [[ ! -f "${nvme_img}" ]]; then
      truncate -s 10G "${nvme_img}"
      echo "Created 10 GiB emulated NVMe image: ${nvme_img}"
    fi
    ssd_img="${LAB_DIR}/san-ssd${i}.raw"
    if [[ ! -f "${ssd_img}" ]]; then
      truncate -s 10G "${ssd_img}"
      echo "Created 10 GiB emulated SSD image:  ${ssd_img}"
    fi
  done
  build_seed_iso "velocor-san"
  build_seed_iso "velocor-initiator"
}

status_vms() {
  for node in san initiator; do
    pidfile="${LAB_DIR}/${node}.pid"
    if [[ -f "${pidfile}" ]] && kill -0 "$(cat "${pidfile}")" 2>/dev/null; then
      echo "${node}: RUNNING (PID $(cat "${pidfile}"))"
    else
      echo "${node}: STOPPED"
    fi
  done
}

down_vms() {
  for node in san initiator bib; do
    pidfile="${LAB_DIR}/${node}.pid"
    if [[ -f "${pidfile}" ]]; then
      pid="$(cat "${pidfile}")"
      if kill -0 "${pid}" 2>/dev/null; then
        echo "Stopping ${node} (PID ${pid})..."
        kill "${pid}" || true
      fi
      rm -f "${pidfile}"
    fi
  done
  pkill -f "velocor-(san|initiator|bib)" 2>/dev/null || true
  sleep 1
}

up_vms() {
  down_vms
  fetch_image
  build_bootc_disk
  rm -f "${LAB_DIR}/san-nvme"*.raw "${LAB_DIR}/san-ssd"*.raw
  init_disks

  san_os="${LAB_DIR}/san-os.qcow2"
  init_os="${LAB_DIR}/initiator-os.qcow2"
  # Always recreate san-os overlay on top of the Fedora bootc 45 base disk
  rm -f "${san_os}"
  qemu-img create -f qcow2 -F qcow2 -b "${BOOTC_DISK}" "${san_os}" 20G >/dev/null
  [[ -f "${init_os}" ]] || qemu-img create -f qcow2 -F qcow2 -b "${BASE_IMAGE}" "${init_os}" 20G >/dev/null

  echo "Starting velocor-san (Fedora bootc 45, ${SAN_RAM_MB} MiB RAM, ${SAN_CPUS} vCPUs, API -> https://127.0.0.1:${SAN_API_PORT}, SSH -> ${SAN_SSH_PORT})..."
  qemu-system-x86_64 \
    -name velocor-san \
    -machine q35,accel=kvm \
    -cpu host \
    -m "${SAN_RAM_MB}" \
    -smp "${SAN_CPUS}" \
    -daemonize \
    -pidfile "${LAB_DIR}/san.pid" \
    -display none \
    -serial "file:${LAB_DIR}/san-console.log" \
    -drive "file=${san_os},if=none,id=osdisk,format=qcow2" \
    -device "virtio-blk-pci,drive=osdisk,bootindex=1" \
    -drive "file=${LAB_DIR}/velocor-san-cidata.iso,if=virtio,media=cdrom,readonly=on" \
    -drive "file=${LAB_DIR}/san-nvme1.raw,if=none,id=nvme1,format=raw" \
    -device "nvme,drive=nvme1,serial=VELONVME0001" \
    -drive "file=${LAB_DIR}/san-nvme2.raw,if=none,id=nvme2,format=raw" \
    -device "nvme,drive=nvme2,serial=VELONVME0002" \
    -device virtio-scsi-pci,id=scsi0 \
    -drive "file=${LAB_DIR}/san-ssd1.raw,if=none,id=ssd1,format=raw" \
    -device "scsi-hd,drive=ssd1,bus=scsi0.0,rotation_rate=1,serial=VELOSSD0001" \
    -drive "file=${LAB_DIR}/san-ssd2.raw,if=none,id=ssd2,format=raw" \
    -device "scsi-hd,drive=ssd2,bus=scsi0.0,rotation_rate=1,serial=VELOSSD0002" \
    -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:${SAN_SSH_PORT}-:22,hostfwd=tcp:127.0.0.1:${SAN_API_PORT}-:8443,hostfwd=tcp:127.0.0.1:${SAN_NVMEOF_PORT}-:4420" \
    -device virtio-net-pci,netdev=net0

  echo "Starting velocor-initiator VM (${INIT_RAM_MB} MiB RAM, ${INIT_CPUS} vCPUs, SSH -> ${INIT_SSH_PORT})..."
  qemu-system-x86_64 \
    -name velocor-initiator \
    -machine q35,accel=kvm \
    -cpu host \
    -m "${INIT_RAM_MB}" \
    -smp "${INIT_CPUS}" \
    -daemonize \
    -pidfile "${LAB_DIR}/initiator.pid" \
    -display none \
    -serial "file:${LAB_DIR}/initiator-console.log" \
    -drive "file=${init_os},if=none,id=osdisk,format=qcow2" \
    -device "virtio-blk-pci,drive=osdisk,bootindex=1" \
    -drive "file=${LAB_DIR}/velocor-initiator-cidata.iso,if=virtio,media=cdrom,readonly=on" \
    -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:${INIT_SSH_PORT}-:22" \
    -device virtio-net-pci,netdev=net0

  status_vms
}

verify_san() {
  local ssh_key="${LAB_DIR}/id_ed25519"
  local ssh_opts=(-i "${ssh_key}" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5)

  echo "Waiting for SSH on velocor-san (root@127.0.0.1:${SAN_SSH_PORT})..."
  for _ in $(seq 1 45); do
    if ssh "${ssh_opts[@]}" -p "${SAN_SSH_PORT}" root@127.0.0.1 "true" 2>/dev/null; then
      break
    fi
    sleep 2
  done

  echo "=== Verifying Fedora Image Mode (bootc 45) & Podman Quadlet Services on velocor-san ==="
  scp "${ssh_opts[@]}" -P "${SAN_SSH_PORT}" \
    "${REPO_ROOT}/deploy/quadlets/velocor-spdk.container" \
    "${REPO_ROOT}/deploy/quadlets/velocor-velod.container" \
    root@127.0.0.1:/etc/containers/systemd/
  ssh "${ssh_opts[@]}" -p "${SAN_SSH_PORT}" root@127.0.0.1 "bash -s" <<'EOF'
set -euo pipefail
echo "--- bootc status ---"
bootc status

systemctl daemon-reload
systemctl start velocor-spdk.service velocor-velod.service

echo ""
echo "--- Waiting for Quadlet containers (velocor-spdk & velocor-velod) to pull and start ---"
for _ in $(seq 1 60); do
  if systemctl is-active --quiet velocor-spdk.service && systemctl is-active --quiet velocor-velod.service; then
    if [[ -S /run/velocor/spdk.sock ]]; then
      break
    fi
  fi
  sleep 2
done

systemctl --no-pager status velocor-spdk.service velocor-velod.service
echo ""
echo "--- Running Podman Containers ---"
podman ps
EOF

  if [[ ! -x "${REPO_ROOT}/bin/veloctl" ]]; then
    make -C "${REPO_ROOT}" build-veloctl
  fi

  echo ""
  echo "=== Authenticating veloctl against https://127.0.0.1:${SAN_API_PORT} ==="
  for _ in $(seq 1 15); do
    if "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure \
      login --username admin --password velocor-admin-changeme 2>/dev/null; then
      break
    fi
    sleep 2
  done

  echo ""
  echo "=== System Status ==="
  "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure system status

  echo ""
  echo "=== Discovered Physical Disks in velocor-san ==="
  "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure disk list

  echo ""
  echo "=== Testing Boot-Disk Safety Guard on /dev/vda (ssd-vda) ==="
  if "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure disk claim ssd-vda 2>&1; then
    echo "ERROR: Boot disk claim should have failed!" >&2
    exit 1
  else
    echo "PASS: Boot disk ssd-vda was protected and rejected claim request."
  fi

  echo ""
  echo "=== Claiming Emulated NVMe (nvme-0000-00-03-0, nvme-0000-00-04-0 -> vfio-pci) & SSD (ssd-sda -> io_uring) ==="
  "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure disk claim nvme-0000-00-03-0 >/dev/null
  "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure disk claim nvme-0000-00-04-0 >/dev/null
  "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure disk claim ssd-sda >/dev/null
  "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure disk list

  echo ""
  echo "=== Creating SPDK RAID1 Pool & Thin-Provisioned Volumes ==="
  local pool_id vol_nvmf_id vol_iscsi_id portal_nvmf_id portal_iscsi_id snap1_id snap2_id
  pool_id="$("${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure pool create --name nvme-pool --raid RAID1 --disks nvme-0000-00-03-0,nvme-0000-00-04-0 | jq -r '.pool.id')"
  vol_nvmf_id="$("${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure volume create --name vol-nvmf --pool "${pool_id}" --size-gib 2 --thin=true | jq -r '.volume.id')"
  vol_iscsi_id="$("${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure volume create --name vol-iscsi --pool "${pool_id}" --size-gib 2 --thin=true | jq -r '.volume.id')"
  "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure pool list
  "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure volume list

  echo ""
  echo "=== Creating NVMe-oF/TCP & iSCSI Portals and Exports ==="
  portal_nvmf_id="$("${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure portal create --name nvmf-tcp-0 --protocol NVMEOF_TCP --ip 10.0.2.15 --port 4420 | jq -r '.portal.id')"
  portal_iscsi_id="$("${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure portal create --name iscsi-0 --protocol ISCSI --ip 10.0.2.15 --port 3260 | jq -r '.portal.id')"
  "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure export create --name exp-nvmf --volume "${vol_nvmf_id}" --protocol NVMEOF_TCP --portals "${portal_nvmf_id}" --allow-any-host=true >/dev/null
  "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure export create --name exp-iscsi --volume "${vol_iscsi_id}" --protocol ISCSI --portals "${portal_iscsi_id}" --allow-any-host=true >/dev/null
  "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure export list

  echo ""
  echo "=== Testing Live NVMe-oF/TCP Discovery, Connect & Block I/O ==="
  ssh "${ssh_opts[@]}" -p "${SAN_SSH_PORT}" root@127.0.0.1 "bash -s" <<'EOF'
set -euo pipefail
modprobe nvme-tcp
echo "--- nvme discover -t tcp -a 10.0.2.15 -s 4420 ---"
nvme discover -t tcp -a 10.0.2.15 -s 4420
nvme connect -t tcp -n nqn.2026-11.io.velocor:exp-nvmf -a 10.0.2.15 -s 4420
sleep 1
nvme list
dd if=/dev/urandom of=/dev/nvme0n1 bs=64k count=4 seek=0 oflag=direct status=none
sync
EOF

  echo ""
  echo "=== Testing CoW Snapshots & Changed Block Tracking (CBT) on NVMe-oF/TCP Volume ==="
  snap1_id="$("${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure snapshot create --volume "${vol_nvmf_id}" --name snap-1 | jq -r '.snapshot.id')"
  ssh "${ssh_opts[@]}" -p "${SAN_SSH_PORT}" root@127.0.0.1 "dd if=/dev/urandom of=/dev/nvme0n1 bs=64k count=2 seek=16 oflag=direct status=none && sync && nvme disconnect -n nqn.2026-11.io.velocor:exp-nvmf"
  snap2_id="$("${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure snapshot create --volume "${vol_nvmf_id}" --name snap-2 | jq -r '.snapshot.id')"
  "${REPO_ROOT}/bin/veloctl" --endpoint "https://127.0.0.1:${SAN_API_PORT}" --insecure snapshot changed-blocks --base "${snap1_id}" --target "${snap2_id}"

  echo ""
  echo "=== Testing Live iSCSI Discovery (SendTargets), Login & Block I/O ==="
  ssh "${ssh_opts[@]}" -p "${SAN_SSH_PORT}" root@127.0.0.1 "bash -s" <<'EOF'
set -euo pipefail
echo "--- iscsiadm -m discovery -t sendtargets -p 10.0.2.15:3260 ---"
iscsiadm -m discovery -t sendtargets -p 10.0.2.15:3260
iscsiadm -m node -T iqn.2026-11.io.velocor:exp-iscsi -p 10.0.2.15:3260 --login
sleep 1
lsblk -S
dd if=/dev/urandom of=/dev/sdc bs=64k count=4 oflag=direct status=none
sync
iscsiadm -m node -T iqn.2026-11.io.velocor:exp-iscsi -p 10.0.2.15:3260 --logout
EOF
}

ssh_san() {
  local ssh_key="${LAB_DIR}/id_ed25519"
  exec ssh -i "${ssh_key}" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -p "${SAN_SSH_PORT}" root@127.0.0.1 "$@"
}

ssh_init() {
  local ssh_key="${LAB_DIR}/id_ed25519"
  exec ssh -i "${ssh_key}" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -p "${INIT_SSH_PORT}" fedora@127.0.0.1 "$@"
}

case "${1:-}" in
  fetch-image)      fetch_image ;;
  build-bootc-disk) build_bootc_disk ;;
  init-disks)       init_disks ;;
  up)               up_vms ;;
  verify)           verify_san ;;
  ssh-san)          shift; ssh_san "$@" ;;
  ssh-init)         shift; ssh_init "$@" ;;
  down)             down_vms ;;
  status)           status_vms ;;
  clean)            down_vms; rm -rf "${LAB_DIR}" ;;
  *)                usage; exit 1 ;;
esac
