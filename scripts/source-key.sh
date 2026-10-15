#!/usr/bin/env bash
# Prints the source key of a commit (default HEAD): a hash of everything .github/workflows/
# images.yml builds from, the same paths its triggers name. Two commits with the same key build
# the same images, so main skips a build whose key is already pinned (deploy/base/pinned-source).
set -euo pipefail
cd "$(dirname "$0")/.."
git ls-tree -r "${1:-HEAD}" -- cmd internal pkg docs/embed.go docs/primer.md docs/reference \
  images scripts/build-initramfs.sh go.mod go.sum .github/workflows/images.yml |
  git hash-object --stdin
