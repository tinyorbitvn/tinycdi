#!/usr/bin/env bash
# write-runtime-manifest.sh — emit runtime-images.json, the manifest the
# runtime release train attaches to its runtime-YYYY.MM.DD GitHub
# Release (D27/P7). Operators pin chart images.*.digest and
# images.*.builtAt from it; it is the only contract between the train
# and a deployment's values.
#
# Usage: write-runtime-manifest.sh <refs-dir> <out-file>
#   refs-dir holds one <image>.ref per published runtime image, each a
#   canonical ghcr.io/tinyorbitvn/tinycdi-<image>@sha256:<64hex> ref
#   (the same files validate-image-refs.sh checks).
# Env: RT_TAG             — required; the rt-YYYYMMDD.N tag just promoted
#      BUILT_AT           — manifest timestamp (default: now, UTC)
#      BROWSER_DOCKERFILE — chromium pin source
#                          (default: build/browser/Dockerfile)
set -euo pipefail

DIR="${1:?usage: write-runtime-manifest.sh <refs-dir> <out-file>}"
OUT="${2:?usage: write-runtime-manifest.sh <refs-dir> <out-file>}"
TAG="${RT_TAG:?RT_TAG unset (the rt-YYYYMMDD.N tag this train promoted)}"
BUILT_AT="${BUILT_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
DOCKERFILE="${BROWSER_DOCKERFILE:-build/browser/Dockerfile}"

[[ "$TAG" =~ ^rt-[0-9]{8}\.[0-9]+$ ]] \
  || { echo "::error::RT_TAG '$TAG' is not rt-YYYYMMDD.N"; exit 1; }

shopt -s nullglob
files=("$DIR"/*.ref)
[ "${#files[@]}" -gt 0 ] || { echo "::error::no image refs in $DIR"; exit 1; }

# linux-desktop first (it is browser's base), then browser, then any
# extra runtime images in glob order — matches the documented manifest.
order=()
for want in linux-desktop browser; do
  [ -f "$DIR/$want.ref" ] && order+=("$want")
done
for f in "${files[@]}"; do
  n="$(basename "$f" .ref)"
  case " ${order[*]} " in
    *" $n "*) ;;
    *) order+=("$n") ;;
  esac
done

OBJS="$(mktemp)"; trap 'rm -f "$OBJS" "$OUT.tmp"' EXIT
for name in "${order[@]}"; do
  [[ "$name" =~ ^[a-z0-9][a-z0-9-]*$ ]] \
    || { echo "::error::invalid image name '$name'"; exit 1; }
  ref="$(tr -d '[:space:]' < "$DIR/$name.ref")"
  [[ "$ref" =~ ^ghcr\.io/tinyorbitvn/tinycdi-${name}@sha256:[0-9a-f]{64}$ ]] \
    || { echo "::error::$DIR/$name.ref is not a canonical ghcr repo@sha256 ref ('$ref')"; exit 1; }
  repo="${ref%@*}"
  digest="${ref#*@}"
  if [ "$name" = "browser" ]; then
    PIN="$(awk -F= '/^ARG CHROMIUM_APT_VERSION=/ {print $2; exit}' "$DOCKERFILE")"
    [ -n "$PIN" ] \
      || { echo "::error::CHROMIUM_APT_VERSION not found in $DOCKERFILE"; exit 1; }
    # Strip the Debian revision — the manifest carries the engine version.
    CHROMIUM="${PIN%%-*}"
    jq -n --arg name "$name" --arg ref "$repo" --arg digest "$digest" \
      --arg tag "$TAG" --arg chromium "$CHROMIUM" \
      '{name: $name, ref: $ref, digest: $digest, tag: $tag, chromium: $chromium}' \
      >> "$OBJS"
  else
    jq -n --arg name "$name" --arg ref "$repo" --arg digest "$digest" \
      --arg tag "$TAG" \
      '{name: $name, ref: $ref, digest: $digest, tag: $tag}' >> "$OBJS"
  fi
done

jq -n --arg builtAt "$BUILT_AT" --slurpfile images "$OBJS" \
  '{builtAt: $builtAt, images: $images}' > "$OUT.tmp"
mv "$OUT.tmp" "$OUT"
echo "wrote $OUT ($(jq '.images | length' "$OUT") images, tag $TAG)"
