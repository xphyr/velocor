#!/usr/bin/env bash
# Run the kernel data plane's root integration tests (internal/kernel, internal/dataplane/kernelplane;
# build tag kernelintegration) in a throwaway Fedora Cloud VM under KVM, without root on the host.
#
# CI's runners boot kernels whose extra modules (dm-dust, nvmet, LIO) aren't installable, and the
# tests need root anyway: a Fedora VM has the appliance's kernel family and tools. Needs /dev/kvm,
# qemu-system-x86_64, qemu-img, ssh, and genisoimage (or xorriso / cloud-localds).
#
# Usage: hack/kernel-tests-vm.sh [go test -run filter]
# Environment:
#   GO=go                     Go toolchain used to build the test binaries
#   KERNEL_TEST_BIN, KERNELPLANE_TEST_BIN   prebuilt test binaries (skip the build)
#   CACHE_DIR=~/.cache/velocor-kernel-tests the downloaded image is kept here
#   KEEP=1                    leave the VM and its directory (for debugging)
set -euo pipefail

# Fedora's release image, verified against the checksum Fedora publishes for it.
IMAGE=Fedora-Cloud-Base-Generic-45-1.4.x86_64.qcow2
IMAGE_SHA256=50bdc5e70f6a24f540932d15411c1c6ae9fcfa40fb990d876d83c5cc823b2ad0
IMAGE_URL=${FEDORA_MIRROR:-https://download.fedoraproject.org/pub/fedora/linux}/releases/45/Cloud/x86_64/images/$IMAGE

RUN_FILTER=${1:-}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
CACHE_DIR=${CACHE_DIR:-$HOME/.cache/velocor-kernel-tests}
WORK=$(mktemp -d "${TMPDIR:-/tmp}/velocor-kernel-tests.XXXXXX")
PORT=$((20000 + RANDOM % 20000))
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*"; }

# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() {
  if [[ ${KEEP:-} == 1 ]]; then
    log "kept: $WORK (ssh -p $PORT -i $WORK/key ci@127.0.0.1)"
    return
  fi
  [[ -f $WORK/vm.pid ]] && kill "$(cat "$WORK/vm.pid")" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# 1. Test binaries (static: the VM has no Go).
if [[ -z ${KERNEL_TEST_BIN:-} || -z ${KERNELPLANE_TEST_BIN:-} ]]; then
  log "building the test binaries"
  (cd "$ROOT" && CGO_ENABLED=0 "${GO:-go}" test -c -tags kernelintegration -o "$WORK/kernel.test" ./internal/kernel)
  (cd "$ROOT" && CGO_ENABLED=0 "${GO:-go}" test -c -tags kernelintegration -o "$WORK/kernelplane.test" ./internal/dataplane/kernelplane)
else
  cp "$KERNEL_TEST_BIN" "$WORK/kernel.test"
  cp "$KERNELPLANE_TEST_BIN" "$WORK/kernelplane.test"
fi

# 2. The image.
mkdir -p "$CACHE_DIR"
if ! echo "$IMAGE_SHA256  $CACHE_DIR/$IMAGE" | sha256sum -c --quiet - 2>/dev/null; then
  log "downloading $IMAGE"
  curl -fsSL --retry 3 -o "$CACHE_DIR/$IMAGE.part" "$IMAGE_URL"
  echo "$IMAGE_SHA256  $CACHE_DIR/$IMAGE.part" | sha256sum -c --quiet -
  mv "$CACHE_DIR/$IMAGE.part" "$CACHE_DIR/$IMAGE"
fi

# 3. The VM: an overlay of the image, cloud-init for a user with this run's key.
ssh-keygen -q -t ed25519 -N '' -f "$WORK/key"
cat > "$WORK/user-data" <<EOF
#cloud-config
users:
  - name: ci
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys: [$(cat "$WORK/key.pub")]
EOF
printf 'instance-id: velocor-kernel-tests\nlocal-hostname: kernel-tests\n' > "$WORK/meta-data"
if command -v cloud-localds >/dev/null; then
  cloud-localds "$WORK/seed.iso" "$WORK/user-data" "$WORK/meta-data"
elif command -v genisoimage >/dev/null; then
  genisoimage -quiet -output "$WORK/seed.iso" -volid cidata -joliet -rock "$WORK/user-data" "$WORK/meta-data"
else
  xorriso -as mkisofs -quiet -output "$WORK/seed.iso" -volid cidata -joliet -rock "$WORK/user-data" "$WORK/meta-data"
fi
qemu-img create -q -f qcow2 -F qcow2 -b "$CACHE_DIR/$IMAGE" "$WORK/os.qcow2" 20G
log "starting the VM (ssh on 127.0.0.1:$PORT)"
qemu-system-x86_64 -name velocor-kernel-tests -machine q35,accel=kvm -cpu host -m 4096 -smp 4 \
  -drive "file=$WORK/os.qcow2,if=virtio,format=qcow2" -drive "file=$WORK/seed.iso,if=virtio,format=raw,readonly=on" \
  -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$PORT-:22" -device virtio-net-pci,netdev=n0 \
  -display none -serial "file:$WORK/console.log" -daemonize -pidfile "$WORK/vm.pid"

SSH=(ssh -q -p "$PORT" -i "$WORK/key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 ci@127.0.0.1)
for _ in $(seq 120); do
  "${SSH[@]}" true 2>/dev/null && break
  sleep 5
done
"${SSH[@]}" true || { log "the VM didn't come up"; tail -30 "$WORK/console.log"; exit 1; }

# 4. The tools and the running kernel's modules (the cloud image has only the core ones).
log "installing the storage tools in the VM"
# shellcheck disable=SC2016 # runs in the VM
"${SSH[@]}" 'set -e; k=$(uname -r); echo "kernel $k"
  sudo dnf install -q -y mdadm lvm2 device-mapper-persistent-data nvme-cli iscsi-initiator-utils iproute \
    "kernel-modules-$k" "kernel-modules-extra-$k" >/dev/null
  sudo modprobe -a raid1 dm_thin_pool dm_dust nvmet nvmet-tcp nvme-tcp target_core_mod target_core_iblock iscsi_target_mod iscsi_tcp'

# 5. The tests: internal/kernel's unit tests read testdata/ next to the binary.
tar -C "$WORK" -cf - kernel.test kernelplane.test | "${SSH[@]}" 'mkdir -p kt && tar -C kt -xf -'
tar -C "$ROOT/internal/kernel" -cf - testdata | "${SSH[@]}" 'tar -C kt -xf -'
run=()
[[ -n $RUN_FILTER ]] && run=(-test.run "$RUN_FILTER")
rc=0
for t in kernel kernelplane; do
  log "running $t.test"
  "${SSH[@]}" "cd kt && sudo ./$t.test -test.v -test.timeout 25m ${run[*]@Q}" || rc=1
done
# shellcheck disable=SC2016 # runs in the VM
"${SSH[@]}" 'echo "left behind: md=$(ls /dev/md 2>/dev/null | wc -l) loops=$(sudo losetup -a | wc -l)"' || true
exit $rc
