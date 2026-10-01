#!/usr/bin/env bash
# validate-image-refs.sh — verify that a directory holds exactly one
# <image>.ref per released image, each containing the canonical ghcr
# repo@sha256:<64hex> reference.
#
# SUPF-1: image refs are the only pointer between the digest-only builds
# and the jobs that sign/attest/promote them — a malformed or unexpected
# ref must fail the job before anything is signed or tagged.
#
# Usage: validate-image-refs.sh <refs-dir> <image> [<image>...]
set -euo pipefail

DIR="$1"; shift
[ "$#" -gt 0 ] || { echo "::error::no images given"; exit 1; }

shopt -s nullglob
files=("$DIR"/*.ref)
[ "${#files[@]}" -eq "$#" ] \
  || { echo "::error::expected $# image ref(s) in $DIR, found ${#files[@]}"; exit 1; }

for img in "$@"; do
  f="$DIR/$img.ref"
  [ -f "$f" ] || { echo "::error::missing ref for image '$img'"; exit 1; }
  ref="$(tr -d '[:space:]' < "$f")"
  [[ "$ref" =~ ^ghcr\.io/tinyorbitvn/tinycdi-${img}@sha256:[0-9a-f]{64}$ ]] \
    || { echo "::error::$f is not ghcr.io/tinyorbitvn/tinycdi-$img@sha256:<64hex> ('$ref')"; exit 1; }
done
echo "image refs ok: $*"
