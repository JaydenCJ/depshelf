#!/usr/bin/env bash
# airgap-sync.sh — build a depshelf store from local artifact files and
# pack it for transfer to an airgapped machine. Everything happens offline;
# run it against tarballs/wheels you already have (vendored, downloaded on
# a connected host, or produced by your own builds).
#
# Usage: bash examples/airgap-sync.sh <artifact-dir> <out.tar.gz>
#        (artifact-dir holds *.tgz npm tarballs and *.whl/*.tar.gz wheels/sdists)
set -euo pipefail

SRC="${1:?usage: airgap-sync.sh <artifact-dir> <out.tar.gz>}"
OUT="${2:?usage: airgap-sync.sh <artifact-dir> <out.tar.gz>}"
STORE="$(mktemp -d)/shelf"

shopt -s nullglob
for tgz in "$SRC"/*.tgz; do
  depshelf import npm --store "$STORE" "$tgz"
done
for dist in "$SRC"/*.whl "$SRC"/*.tar.gz; do
  depshelf import pypi --store "$STORE" "$dist"
done

depshelf verify --store "$STORE"
depshelf list --store "$STORE"

tar -C "$(dirname "$STORE")" -czf "$OUT" "$(basename "$STORE")"
echo "packed store -> $OUT"
echo "on the airgapped side:"
echo "  tar -xzf $(basename "$OUT") && depshelf serve --store ./shelf --offline"
