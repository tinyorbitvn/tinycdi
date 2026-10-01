#!/usr/bin/env bash
# check-kasm-catalog.sh — static enforcement of the kasm image catalog
# policy (docs/kasm-images.md, KASM-2/KASM-5). Fast, network-free; runs in
# PR CI. The heavyweight half — pulling each entry, extracting the engine
# version and running the trivy gate — lives in scan-kasm-catalog.sh and
# runs on the kasm-contract schedule job.
#
# Enforces:
#   1. Every kasmweb/* image ref in tracked files (chart ci values, docs,
#      this catalog) is digest-pinned (repo@sha256:<64 hex>).
#   2. Every such ref appears verbatim in build/kasm-catalog.txt — docs
#      and examples may only point at cataloged, gated images.
#   3. Every catalog entry declares engine + min-engine-major, and the
#      floor is >= major(CHROMIUM_APT_VERSION) - 4 (the documented lag
#      budget — kasmweb images ride Debian/Ubuntu rebuilds).
set -euo pipefail
cd "$(dirname "$0")/../.."

# Env overrides exist for the regression test (.github/tests/).
CATALOG="${KASM_CATALOG:-build/kasm-catalog.txt}"
DOCKERFILE="${KASM_CHROMIUM_DOCKERFILE:-build/browser/Dockerfile}"
if [ -n "${KASM_SCAN_FILES:-}" ]; then
  # shellcheck disable=SC2206
  SCAN_FILES=($KASM_SCAN_FILES)
else
  SCAN_FILES=(deploy/helm/tinycdi/ci/*.yaml docs/*.md "$CATALOG")
fi
fail=0

err() { echo "::error::$*"; fail=1; }

# -- catalog sanity ------------------------------------------------------
declare -A CATALOG_REFS=()
while read -r ref engine floor _; do
  [ -n "${ref:-}" ] || continue
  case "$ref" in \#*) continue;; esac
  case "$ref" in
    kasmweb/*@sha256:[0-9a-f][0-9a-f]*) ;;
    *) err "catalog entry not digest-pinned: $ref" ;;
  esac
  if [[ ! "$ref" =~ @sha256:[0-9a-f]{64}$ ]]; then
    err "catalog entry has a malformed digest: $ref"
  fi
  case "$engine" in
    chromium|firefox|none) ;;
    *) err "catalog entry $ref has unknown engine '$engine'" ;;
  esac
  if [[ ! "$floor" =~ ^[0-9]+$ ]]; then
    err "catalog entry $ref has a non-numeric engine floor '$floor'"
  fi
  CATALOG_REFS["$ref"]=1
done < "$CATALOG"

if [ "${#CATALOG_REFS[@]}" -eq 0 ]; then
  err "no catalog entries in $CATALOG"
fi

# -- floor vs the browser pin -------------------------------------------
PINNED="$(awk -F= '/^ARG CHROMIUM_APT_VERSION=/ {print $2; exit}' "$DOCKERFILE")"
PIN_MAJOR="${PINNED%%.*}"
FLOOR_MIN=$(( PIN_MAJOR - 4 ))
while read -r ref engine floor _; do
  case "${ref:-}" in ""|\#*) continue;; esac
  [ "$engine" = "none" ] && continue
  if [ "$floor" -lt "$FLOOR_MIN" ]; then
    err "catalog floor for $ref is $floor, below the documented minimum (CHROMIUM_APT_VERSION $PINNED major $PIN_MAJOR - 4 = $FLOOR_MIN)"
  fi
done < "$CATALOG"

# -- every referenced kasmweb image is pinned + cataloged -----------------
check_ref() { # <file> <ref>
  local f="$1" ref="$2" pinned
  case "$ref" in
    kasmweb/*@sha256:[0-9a-f][0-9a-f]*) ;;
    *@*) err "$f references non-digest-pinned kasm image: $ref"; return ;;
    *)
      err "$f references non-digest-pinned kasm image: $ref"; return ;;
  esac
  pinned="$ref"
  if [[ "$ref" =~ ^(kasmweb/[^:]+):[^@]+(@sha256:[0-9a-f]{64})$ ]]; then
    pinned="${BASH_REMATCH[1]}${BASH_REMATCH[2]}"
  fi
  if [ -z "${CATALOG_REFS[$pinned]:-}" ]; then
    err "$f references uncataloged kasm image: $pinned"
  fi
}

for f in "${SCAN_FILES[@]}"; do
  [ -f "$f" ] || continue
  case "$f" in
    *.md)
      # Prose may name repos/tags descriptively; only a digest pin is an
      # operational reference, and it must point at a cataloged image.
      while read -r ref; do
        [ -n "$ref" ] && check_ref "$f" "$ref"
      done < <(grep -oE 'kasmweb/[a-z0-9._-]+(:[a-zA-Z0-9._-]+)?@sha256:[0-9a-f]{64}' "$f" | sort -u)
      ;;
    *)
      # Operational files: comments are stripped; every remaining
      # kasmweb/* token is an image reference and must be digest-pinned.
      while read -r ref; do
        [ -n "$ref" ] && check_ref "$f" "$ref"
      done < <(sed 's/#.*//' "$f" | grep -oE 'kasmweb/[a-z0-9._-]+(:[a-zA-Z0-9._-]+)?(@sha256:[0-9a-f]{64})?' | sort -u)
      ;;
  esac
done

if [ "$fail" -ne 0 ]; then
  echo "kasm catalog check FAILED"
  exit 1
fi
echo "kasm catalog check OK: ${#CATALOG_REFS[@]} entries, floor >= $FLOOR_MIN, all references digest-pinned + cataloged"
