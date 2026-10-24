#!/usr/bin/env bash
# Runs the nodelet's KVM integration test (internal/nodelet/kvm_test.go) inside
# the nodelet's own image, shaped like the pod: /dev/kvm, /dev/net/tun,
# NET_ADMIN, its own network namespace. Never on the host's network, never
# with sudo.
#
#   scripts/kvm-test.sh              # template: the guest image (images/guest, loam-guest:dev)
#   LOAM_KVM_IMAGE=docker.io/library/ubuntu:24.04 scripts/kvm-test.sh
#                                    # template: pulled from a registry
#
# CGROUPS=1 adds CAP_SYS_ADMIN (AppArmor unconfined) and a private cgroup
# namespace, which lets the nodelet remount its cgroup2 read-write and give
# every VM its own cgroup; without it the VMs share the container's, as they
# do in the unprivileged DaemonSet. LOAM_KVM_DATA_HOST=<dir> mounts a host
# directory as the data directory (a reflink-capable one tests real
# reflinks); by default the data directory is inside the container, where
# every clone is a copy. LOAM_KVM_DATA_HOST_B=<dir> does the same for the
# second node of TestKVMStore (two nodelets, one store).
#
# The store is a directory under the first data directory unless
# LOAM_KVM_STORE_URL names another (s3://... for Garage, with
# AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, passed through). Lazy memory
# needs /dev/userfaultfd, which docker maps into the container as root's.
set -euo pipefail
cd "$(dirname "$0")/.."

./scripts/build-initramfs.sh
docker build -q -f images/nodelet/Containerfile -t loam-nodelet:kvm-test . >/dev/null
CGO_ENABLED=0 go test -c -tags kvm -o bin/nodelet-kvm.test ./internal/nodelet

args=(-v "$PWD/bin/nodelet-kvm.test:/nodelet-kvm.test:ro")
if [ -z "${LOAM_KVM_IMAGE:-}" ]; then
  if ! docker image inspect loam-guest:dev >/dev/null 2>&1; then
    docker build -t loam-guest:dev -f images/guest/Containerfile images/guest
  fi
  # Firewall userspace is a test dependency, not a change to the production
  # guest image. Keep it in an isolated derived image used only by this suite.
  docker build -q -t loam-guest:kvm-test - <<'EOF'
FROM loam-guest:dev
RUN apt-get update && apt-get install -y --no-install-recommends nftables && rm -rf /var/lib/apt/lists/*
EOF
  mkdir -p dist/dev
  docker save loam-guest:kvm-test -o dist/dev/guest.tar
  args+=(-v "$PWD/dist/dev/guest.tar:/guest.tar:ro" -e LOAM_KVM_IMAGE_TAR=/guest.tar)
else
  args+=(-e LOAM_KVM_IMAGE="$LOAM_KVM_IMAGE")
fi
if [ -n "${CGROUPS:-}" ]; then
  args+=(--cap-add SYS_ADMIN --security-opt apparmor=unconfined --cgroupns private)
fi
if [ -n "${LOAM_KVM_DATA_HOST:-}" ]; then
  args+=(-v "$LOAM_KVM_DATA_HOST:/data" -e LOAM_KVM_DATA=/data)
fi
if [ -n "${LOAM_KVM_DATA_HOST_B:-}" ]; then
  args+=(-v "$LOAM_KVM_DATA_HOST_B:/data-b" -e LOAM_KVM_DATA_B=/data-b)
fi
if [ -n "${LOAM_KVM_STORE_URL:-}" ]; then
  args+=(-e LOAM_KVM_STORE_URL -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY)
fi
exec docker run --rm \
  --device /dev/kvm --device /dev/net/tun --device /dev/userfaultfd --cap-add NET_ADMIN \
  "${args[@]}" \
  --entrypoint /nodelet-kvm.test \
  loam-nodelet:kvm-test -test.v -test.run TestKVM -test.timeout 30m "$@"
