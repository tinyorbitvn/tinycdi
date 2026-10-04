#!/usr/bin/env bash
# write-runtime-manifest.sh — emit runtime-images.json, the manifest the
# runtime release train attaches to its runtime-YYYY.MM.DD GitHub
# Release (D27/P7). Operators pin chart images.*.digest and
# images.*.builtAt from it; it is the only contract between the train
# and a deployment's values.
#
# Usage: write-runtime-manifest.sh <refs-dir> <sboms-dir> <out-file>
#   refs-dir  holds one <image>.ref per published runtime image, each a
#             canonical ghcr.io/tinyorbitvn/tinycdi-<image>@sha256:<64hex>
#             ref (the same files validate-image-refs.sh checks).
#   sboms-dir holds the matching sbom-<image>.spdx.json that the build
#             jobs generated from the pushed image and that the publish
#             job attests (SUPR-2). The manifest's browser-engine fields
#             are read out of it — the package versions actually
#             installed in the shipped image, not the Dockerfile pins
#             that asked for them.
# Env: RT_TAG   — required; the rt-YYYYMMDD.N tag just promoted
#      BUILT_AT — manifest timestamp (default: now, UTC)
set -euo pipefail

DIR="${1:?usage: write-runtime-manifest.sh <refs-dir> <sboms-dir> <out-file>}"
SBOMS="${2:?usage: write-runtime-manifest.sh <refs-dir> <sboms-dir> <out-file>}"
OUT="${3:?usage: write-runtime-manifest.sh <refs-dir> <sboms-dir> <out-file>}"
TAG="${RT_TAG:?RT_TAG unset (the rt-YYYYMMDD.N tag this train promoted)}"
BUILT_AT="${BUILT_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

[[ "$TAG" =~ ^rt-[0-9]{8}\.[0-9]+$ ]] \
  || { echo "::error::RT_TAG '$TAG' is not rt-YYYYMMDD.N"; exit 1; }

# installed_version <image> <deb-package> — print the full Debian
# version the built image carries, per its attested SPDX SBOM (what
# `dpkg-query -W <pkg>` would report inside it). Exactly one package of
# that name with one distinct versionInfo must exist: zero means the
# image does not ship it (a packaging or wiring bug), two means the
# SBOM is ambiguous — the manifest never guesses.
installed_version() {
  local img="$1" pkg="$2" f="$SBOMS/sbom-$1.spdx.json" n
  [ -s "$f" ] || { echo "::error::missing SBOM '$f' for image '$img'" >&2; exit 1; }
  n="$(jq -r --arg pkg "$pkg" \
    '[.packages[]? | select(.name == $pkg) | .versionInfo // empty] | unique | length' \
    "$f")" || { echo "::error::$f is not valid JSON" >&2; exit 1; }
  [ "$n" -eq 0 ] && { echo "::error::$f has no usable '$pkg' package version" >&2; exit 1; }
  [ "$n" -gt 1 ] && { echo "::error::$f lists $n distinct '$pkg' versions" >&2; exit 1; }
  jq -r --arg pkg "$pkg" \
    '[.packages[]? | select(.name == $pkg) | .versionInfo // empty] | unique | .[0]' "$f"
}

shopt -s nullglob
files=("$DIR"/*.ref)
[ "${#files[@]}" -gt 0 ] || { echo "::error::no image refs in $DIR"; exit 1; }

# linux-base first (it is the profiles' base), then linux-desktop, then
# browser, then any extra runtime images in glob order — matches the
# documented manifest.
order=()
for want in linux-base linux-desktop browser; do
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
    CHROMIUM="$(installed_version browser chromium)"
    FIREFOX="$(installed_version browser firefox-esr)"
    jq -n --arg name "$name" --arg ref "$repo" --arg digest "$digest" \
      --arg tag "$TAG" --arg chromium "$CHROMIUM" --arg firefox "$FIREFOX" \
      '{name: $name, ref: $ref, digest: $digest, tag: $tag, chromium: $chromium, firefox: $firefox}' \
      >> "$OBJS"
  elif [ "$name" = "linux-desktop" ]; then
    FIREFOX="$(installed_version linux-desktop firefox-esr)"
    jq -n --arg name "$name" --arg ref "$repo" --arg digest "$digest" \
      --arg tag "$TAG" --arg firefox "$FIREFOX" \
      '{name: $name, ref: $ref, digest: $digest, tag: $tag, firefox: $firefox}' \
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
