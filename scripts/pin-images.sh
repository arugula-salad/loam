#!/usr/bin/env bash
# Pins the images a run of .github/workflows/images.yml built into deploy/base (and the
# templates of deploy/components, which repeat the base's), and records the source key they were
# built from (scripts/source-key.sh) in deploy/base/pinned-source. Every pin rewrites that file,
# so two pins made from different sources always conflict: a pull request pinned before another
# one merged has to be rebased, which builds and pins it again.
#
# Reads the image names (API_IMAGE, NODELET_IMAGE, GUEST_IMAGE, E2B_IMAGE, FOUNTAIN_IMAGE), their
# digests (API_DIGEST, ...) and SOURCE_KEY from the environment. Leaves the changes uncommitted.
set -euo pipefail
cd "$(dirname "$0")/.."

for d in "$API_DIGEST" "$NODELET_DIGEST" "$GUEST_DIGEST" "$E2B_DIGEST" "$FOUNTAIN_DIGEST"; do
  [[ "$d" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "bad digest: '$d'" >&2; exit 1; }
done
[[ "$SOURCE_KEY" =~ ^[0-9a-f]{40}$ ]] || { echo "bad source key: '$SOURCE_KEY'" >&2; exit 1; }

api=deploy/base/kustomization.yaml
nodelet=deploy/base/nodelet/kustomization.yaml
templates=(deploy/base/templates.json deploy/components/*/templates.json)
fountain=deploy/components/e2b-fountain/templates.json

# In each kustomization.yaml the image's digest line follows its newName.
sed -i -E "\#newName: ${API_IMAGE}\$#{n;s#(digest: ).*#\1${API_DIGEST}#}" "$api"
sed -i -E "\#newName: ${NODELET_IMAGE}\$#{n;s#(digest: ).*#\1${NODELET_DIGEST}#}" "$nodelet"
for f in "${templates[@]}"; do
  sed -i -E "s#(${GUEST_IMAGE})@sha256:[0-9a-f]+#\1@${GUEST_DIGEST}#" "$f"
  sed -i -E "s#(${E2B_IMAGE})@sha256:[0-9a-f]+#\1@${E2B_DIGEST}#" "$f"
  grep -q "${GUEST_DIGEST}" "$f"
  grep -q "${E2B_DIGEST}" "$f"
done
sed -i -E "s#(${FOUNTAIN_IMAGE})@sha256:[0-9a-f]+#\1@${FOUNTAIN_DIGEST}#" "$fountain"
grep -q "${API_DIGEST}" "$api"
grep -q "${NODELET_DIGEST}" "$nodelet"
grep -q "${FOUNTAIN_DIGEST}" "$fountain"
echo "$SOURCE_KEY" > deploy/base/pinned-source
