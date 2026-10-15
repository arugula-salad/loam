#!/usr/bin/env bash
# Boots loam-agent for real: the initramfs (scripts/build-initramfs.sh) and a
# disk built from images/guest, under Firecracker in a container with
# /dev/kvm, the shape of the nodelet pod. No sudo and no network: the VM has
# no NIC, and everything is checked over vsock through Firecracker's socket.
#
#   FIRECRACKER=/path/to/firecracker KERNEL=/path/to/vmlinux ./scripts/boot-test.sh
#
# It checks, from the host side: eth0 from the kernel command line, and moved
# (address and MAC) by a wake, over a tap in the container's own network
# namespace; /internal/wake, /exec/run as loam and as
# root, the file API, the guest API socket and the host channel (GET /self
# from inside the VM reaches a stand-in for the nodelet on vsock port 1025),
# then /internal/poweroff, after which Firecracker must exit by itself.
set -euo pipefail

cd "$(dirname "$0")/.."
: "${FIRECRACKER:?set FIRECRACKER to a firecracker binary}"
: "${KERNEL:?set KERNEL to an uncompressed guest kernel (vmlinux)}"
IMAGE="${IMAGE:-loam-guest:dev}"
DISK_GB="${DISK_GB:-4}"

./scripts/build-initramfs.sh
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  docker build -t "$IMAGE" -f images/guest/Containerfile images/guest
fi

# The disk: the image's filesystem, written to ext4 inside a container (root
# there, so ownership comes out right without root here).
if [ ! -f dist/guest.ext4 ] || [ -n "${REBUILD_DISK:-}" ]; then
  cid=$(docker create "$IMAGE" true)
  trap 'docker rm -f "$cid" >/dev/null 2>&1 || true' EXIT
  docker export "$cid" | docker run --rm -i -v "$PWD/dist:/out" --entrypoint sh "$IMAGE" -euc '
    mkdir /r && tar -x -C /r --numeric-owner -f -
    rm -f /out/guest.ext4.tmp
    truncate -s '"$DISK_GB"'G /out/guest.ext4.tmp
    mkfs.ext4 -q -F -L loam -d /r -E lazy_itable_init=1,lazy_journal_init=1 /out/guest.ext4.tmp
    chown '"$(id -u):$(id -g)"' /out/guest.ext4.tmp
    mv /out/guest.ext4.tmp /out/guest.ext4'
fi

docker run --rm -i --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN \
  -v "$(realpath "$FIRECRACKER"):/fc/firecracker:ro" \
  -v "$(realpath "$KERNEL"):/fc/vmlinux:ro" \
  -v "$PWD/dist:/dist:ro" \
  -v "$PWD/scripts:/scripts:ro" \
  --entrypoint bash "$IMAGE" -euc '
cp --sparse=always /dist/guest.ext4 /tmp/disk.ext4
# A tap in the container'"'"'s own network namespace, as the nodelet sets up in its pod.
ip tuntap add dev tap0 mode tap
ip addr add 10.240.0.1/16 dev tap0
ip link set tap0 up
cat > /tmp/vm.json <<JSON
{
  "boot-source": {"kernel_image_path": "/fc/vmlinux", "initrd_path": "/dist/initramfs.cpio.gz",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.dumbkbd loam.root=/dev/vda loam.hostname=boottest loam.ip=10.240.0.7/16 loam.gw=10.240.0.1 loam.dns=10.240.0.1"},
  "drives": [{"drive_id": "root", "path_on_host": "/tmp/disk.ext4", "is_root_device": false, "is_read_only": false}],
  "machine-config": {"vcpu_count": 2, "mem_size_mib": 1024},
  "network-interfaces": [{"iface_id": "eth0", "host_dev_name": "tap0", "guest_mac": "06:00:0a:f0:00:07"}],
  "vsock": {"guest_cid": 3, "uds_path": "/tmp/v.sock"}
}
JSON
/fc/firecracker --no-api --config-file /tmp/vm.json > /tmp/console.log 2>&1 &
fc=$!
python3 /scripts/boot-test.py
for i in $(seq 1 100); do
  kill -0 $fc 2>/dev/null || break
  sleep 0.1
done
if kill -0 $fc 2>/dev/null; then
  echo "FAIL: firecracker still running 10 s after poweroff"; kill $fc; cat /tmp/console.log; exit 1
fi
wait $fc || true
echo "--- console (tail)"; tail -n 15 /tmp/console.log
echo "PASS: booted, served the API on vsock, reached the host channel and powered off"
'
