#!/usr/bin/env bash
# Packs the statically linked loam-agent as /init in a gzipped newc
# initramfs: dist/initramfs.cpio.gz (OUT= to put it elsewhere). The nodelet's
# image ships it beside the kernel; docs/AGENT.md says what the kernel
# command line must carry.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT="${OUT:-dist/initramfs.cpio.gz}"
GOARCH="${GOARCH:-$(go env GOARCH)}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -trimpath -ldflags '-s -w' -o "$work/init" ./cmd/loam-agent
# The archive itself (the mount points /init needs, then /init, with fixed
# owners and mtimes so the same agent packs to the same bytes) is written in
# Go: CI runners have no cpio.
mkdir -p "$(dirname "$OUT")"
go run ./internal/tools/mkinitramfs -init "$work/init" -o "$OUT"
echo "built $OUT ($(du -h "$OUT" | cut -f1), $GOARCH)"
