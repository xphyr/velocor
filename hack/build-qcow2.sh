#!/usr/bin/env bash
# Build a bootable velocor-os qcow2 from a published bootc image, without root on the host.
#
# A throwaway Fedora Cloud VM runs `bootc install to-disk` against a second virtual disk,
# which becomes the qcow2. Only KVM access is needed (no sudo, no rootful podman), so this runs
# on an unprivileged lab host. Nothing from the build host ends up in the image: no registry
# credentials, and no SSH key unless one is passed explicitly. The appliance's login is `velocor-admin` (root can't log in over SSH): give
# it keys with --authorized-keys (repeatable; written to /etc/ssh/authorized_keys.d/velocor-admin
# in the installed system), or provision them at boot with cloud-init.
#
# Usage: hack/build-qcow2.sh --image <registry>/<org>/velocor-os:dev-<sha>[@sha256:...] \
#          --output velocor-os.qcow2 [--size 32G] [--authorized-keys keys.pub ...] [--work DIR]
set -euo pipefail

KEY_FILES=()
IMAGE="" OUTPUT="" SIZE="32G" WORK="${XDG_CACHE_HOME:-$HOME/.cache}/velocor-qcow2"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --image) IMAGE="$2"; shift 2 ;;
    --output) OUTPUT="$2"; shift 2 ;;
    --size) SIZE="$2"; shift 2 ;;
    --authorized-keys) KEY_FILES+=("$2"); shift 2 ;;
    --work) WORK="$2"; shift 2 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
[[ -n "$IMAGE" && -n "$OUTPUT" ]] || { sed -n 2,15p "$0"; exit 2; }
for f in "${KEY_FILES[@]}"; do
  [[ -f "$f" ]] || { echo "authorized keys file $f not found" >&2; exit 2; }
  grep -q PRIVATE "$f" && { echo "$f contains a private key; pass public keys only" >&2; exit 2; }
done

mkdir -p "$WORK"
BASE="$WORK/fedora-cloud-base.qcow2"
if [[ ! -f "$BASE" ]]; then
  for rel in 45 44; do
    dir="https://download.fedoraproject.org/pub/fedora/linux/releases/${rel}/Cloud/x86_64/images/"
    f=$(curl -fsSL "$dir" 2>/dev/null | grep -oE 'Fedora-Cloud-Base-Generic-[0-9]+-[0-9.]+\.x86_64\.qcow2' | head -n1 || true)
    if [[ -n "$f" ]]; then
      echo "Downloading builder base image $f"
      curl -fL --retry 3 -o "$BASE.part" "$dir$f" && mv "$BASE.part" "$BASE"
      break
    fi
  done
fi
[[ -f "$BASE" ]] || { echo "could not download a Fedora Cloud base image" >&2; exit 1; }

run="$WORK/run-$$"
mkdir -p "$run"
cleanup() {
  local rc=$?
  [[ -f "$run/vm.pid" ]] && kill "$(cat "$run/vm.pid")" 2>/dev/null || true
  if [[ $rc -ne 0 && -f "$run/console.log" ]]; then
    cp "$run/console.log" "$WORK/last-failed-console.log"
    echo "builder console saved to $WORK/last-failed-console.log" >&2
  fi
  rm -rf "$run"
}
trap cleanup EXIT

# A key that only ever exists for this build, to drive the builder VM.
ssh-keygen -t ed25519 -N "" -q -f "$run/builder"
cat > "$run/meta-data" <<EOF
instance-id: velocor-qcow2-builder-$$
local-hostname: velocor-qcow2-builder
EOF
cat > "$run/user-data" <<EOF
#cloud-config
users:
  - name: builder
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys: ["$(cat "$run/builder.pub")"]
EOF
genisoimage -quiet -output "$run/seed.iso" -volid cidata -joliet -rock "$run/user-data" "$run/meta-data"

qemu-img create -q -f qcow2 -F qcow2 -b "$BASE" "$run/builder.qcow2" 20G
qemu-img create -q -f qcow2 "$run/target.qcow2" "$SIZE"

port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
qemu-system-x86_64 -name velocor-qcow2-builder -machine q35,accel=kvm -cpu host -m 3072 -smp 4 \
  -daemonize -pidfile "$run/vm.pid" -display none -serial "file:$run/console.log" \
  -drive "file=$run/builder.qcow2,if=none,id=os,format=qcow2" \
  -device "virtio-blk-pci,drive=os,bootindex=1" \
  -drive "file=$run/target.qcow2,if=none,id=target,format=qcow2" \
  -device "virtio-blk-pci,drive=target,serial=VELOCORTARGET" \
  -drive "file=$run/seed.iso,if=none,id=seed,format=raw,readonly=on" \
  -device "virtio-blk-pci,drive=seed" \
  -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:${port}-:22" -device virtio-net-pci,netdev=n0

ssh_b=(ssh -i "$run/builder" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o LogLevel=ERROR -p "$port" builder@127.0.0.1)
echo "Waiting for the builder VM..."
for _ in $(seq 1 90); do "${ssh_b[@]}" true 2>/dev/null && break; sleep 2; done
"${ssh_b[@]}" true || { echo "builder VM did not come up; console log:" >&2; tail -30 "$run/console.log" >&2; exit 1; }

: > "$run/admin-keys.pub"
for f in "${KEY_FILES[@]}"; do cat "$f" >> "$run/admin-keys.pub"; echo >> "$run/admin-keys.pub"; done
scp -q -i "$run/builder" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -P "$port" "$run/admin-keys.pub" builder@127.0.0.1:/tmp/admin-keys.pub

echo "Installing $IMAGE onto the target disk..."
"${ssh_b[@]}" "sudo IMAGE='$IMAGE' bash -s" <<'EOF'
set -euo pipefail
dnf install -y -q podman >/dev/null
target=$(readlink -f /dev/disk/by-id/virtio-VELOCORTARGET)
podman pull -q "$IMAGE" >/dev/null
podman run --rm --privileged --pid=host --security-opt label=type:unconfined_t \
  -v /var/lib/containers:/var/lib/containers -v /dev:/dev \
  "$IMAGE" bootc install to-disk --filesystem xfs --generic-image "$target"
sync

# Admin SSH keys go into the installed deployment's /etc (root-owned, labelled like the rest of
# /etc/ssh), never into the image itself.
if [[ -s /tmp/admin-keys.pub ]]; then
  udevadm settle
  root=$(lsblk -nrpo NAME,FSTYPE,LABEL "$target" | awk '$2=="xfs" && $3=="root" {print $1}')
  [[ -n "$root" ]] || { echo "could not find the installed root filesystem" >&2; lsblk -f "$target" >&2; exit 1; }
  mkdir -p /mnt/target && mount "$root" /mnt/target
  n=0
  for d in /mnt/target/ostree/deploy/*/deploy/*.0; do
    [[ -d "$d/etc/ssh" ]] || continue
    install -d -m 0755 -o root -g root "$d/etc/ssh/authorized_keys.d"
    install -m 0644 -o root -g root /tmp/admin-keys.pub "$d/etc/ssh/authorized_keys.d/velocor-admin"
    chcon --reference="$d/etc/ssh/sshd_config" "$d/etc/ssh/authorized_keys.d" "$d/etc/ssh/authorized_keys.d/velocor-admin" 2>/dev/null || true
    n=$((n+1))
  done
  sync; umount /mnt/target
  [[ $n -gt 0 ]] || { echo "no ostree deployment found to add keys to" >&2; exit 1; }
  echo "Added $(grep -c . /tmp/admin-keys.pub) key(s) for velocor-admin to $n deployment(s)"
fi
EOF

kill "$(cat "$run/vm.pid")"; rm -f "$run/vm.pid"; sleep 2
qemu-img convert -O qcow2 -c "$run/target.qcow2" "$OUTPUT"
echo "Wrote $OUTPUT ($(du -h "$OUTPUT" | cut -f1)) from $IMAGE"
