#!/usr/bin/env bash
# collect-publish-inputs.sh — validate the publish job's artifact inputs
# and assemble refs/, sboms/ and bundle/ (SUPF-1).
#
# The publish job downloads each artifact by EXACT NAME into its own
# folder under <downloads-dir> (`gh run download -n <name>`) — never a
# merge-multiple glob: artifacts can be uploaded by any earlier job in
# the run, and a forged artifact could overwrite a same-named file while
# merging. This script then requires:
#   * the folder set to be exactly the expected artifact names
#   * each folder to hold exactly its expected file(s)
#   * every <img>.ref to match ghcr.io/tinyorbitvn/tinycdi-<img>@sha256:<64hex>
#   * every SBOM to be non-empty valid JSON
#   * the digests stamped into the packaged chart to equal the image refs
# Anything unexpected fails closed before a byte is signed or pushed.
#
# Usage: collect-publish-inputs.sh <downloads-dir> <out-dir>
# env:
#   RELEASE_IMAGES  JSON array of released image names
#   VERSION         release tag (vX.Y.Z)
#   CHART_VERSION   chart/asset version (X.Y.Z)
#   KASMVNC_SRC     "true" when the release ships a GPL runtime image —
#                   the release-assets artifact must then also carry the
#                   KasmVNC corresponding-source tarball + .sha256 (PUB-2).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DL="$1"; OUT="$2"
: "${RELEASE_IMAGES:?}" "${VERSION:?}" "${CHART_VERSION:?}"
KASMVNC_SRC="${KASMVNC_SRC:-false}"

die() { echo "::error::$*"; exit 1; }

mapfile -t expected < <(jq -r '.[]' <<< "$RELEASE_IMAGES" | sort)
[ "${#expected[@]}" -gt 0 ] || die "RELEASE_IMAGES is empty"

# image name -> "<values.yaml section> <key>" (mirror of
# stamp-image-digests.sh path_for — keep in sync). The chart pins the
# digest at <section>.<key>.digest: images.* for the component/runtime
# images, kasmAdapter.image for the adapter init image.
path_for() {
  case "$1" in
    linux-base)    echo "images linuxBase" ;;
    linux-desktop) echo "images linuxDesktop" ;;
    kasm-adapter)  echo "kasmAdapter image" ;;
    *)             echo "images $1" ;;
  esac
}

# exact_files <dir> <file>... — the directory must contain exactly these.
exact_files() {
  local d="$1"; shift
  local have
  mapfile -t have < <(find "$d" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort)
  [ "$(printf '%s\n' "${have[@]}")" = "$(printf '%s\n' "$@" | sort)" ] \
    || die "$d holds [${have[*]:-nothing}] — expected exactly [$*]"
}

# ---- 0. the download dir must hold exactly the expected artifacts ------
want=()
for img in "${expected[@]}"; do
  want+=("image-ref-$img" "sbom-$img")
done
want+=(release-chart release-assets)
mapfile -t got < <(find "$DL" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort)
[ "$(printf '%s\n' "${got[@]:-}")" = "$(printf '%s\n' "${want[@]}" | sort)" ] \
  || die "artifact folder set is [${got[*]:-none}] — expected exactly [${want[*]}]"

# ---- 1. image refs: exactly one file per artifact, strict ref format ---
for img in "${expected[@]}"; do
  exact_files "$DL/image-ref-$img" "$img.ref"
done
mkdir -p "$OUT/refs"
for img in "${expected[@]}"; do
  cp "$DL/image-ref-$img/$img.ref" "$OUT/refs/$img.ref"
done
"$SCRIPT_DIR/validate-image-refs.sh" "$OUT/refs" "${expected[@]}"

# ---- 2. SBOMs: one per image, allowlisted name, valid JSON -------------
mkdir -p "$OUT/sboms"
for img in "${expected[@]}"; do
  exact_files "$DL/sbom-$img" "sbom-$img.spdx.json"
  f="$DL/sbom-$img/sbom-$img.spdx.json"
  [ -s "$f" ] || die "missing SBOM for '$img'"
  jq -e . "$f" >/dev/null || die "$f is not valid JSON"
  cp "$f" "$OUT/sboms/sbom-$img.spdx.json"
done

# ---- 3. release-chart: exactly the packaged chart ----------------------
exact_files "$DL/release-chart" "tinycdi-$CHART_VERSION.tgz"

# ---- 4. release-assets: binaries + CRDs + (GPL) KasmVNC source bundle --
mkdir -p "$OUT/bundle"
assets_want=(
  "tinycdi-backend-$VERSION-linux-amd64"
  "tinycdi-operator-$VERSION-linux-amd64"
  "tinycdi-crds-$VERSION.yaml"
)
kasmvnc_tar="" ; kasmvnc_sha=""
mapfile -t have < <(find "$DL/release-assets" -mindepth 1 -maxdepth 1 -printf '%f\n')
for b in "${have[@]}"; do
  skip=""
  for w in "${assets_want[@]}"; do [ "$b" = "$w" ] && skip=1; done
  [ -n "$skip" ] && continue
  case "$b" in
    kasmvnc-*-corresponding-source.tar.gz)        kasmvnc_tar="$b" ;;
    kasmvnc-*-corresponding-source.tar.gz.sha256) kasmvnc_sha="$b" ;;
    *) die "unexpected release-assets file '$b'" ;;
  esac
done
if [ "$KASMVNC_SRC" = "true" ]; then
  [ -n "$kasmvnc_tar" ] && [ -n "$kasmvnc_sha" ] \
    || die "release-assets lacks the KasmVNC corresponding-source bundle (+ .sha256)"
  [ "${kasmvnc_sha%.sha256}" = "$kasmvnc_tar" ] \
    || die "KasmVNC source checksum '$kasmvnc_sha' does not pair with '$kasmvnc_tar'"
else
  [ -z "$kasmvnc_tar" ] && [ -z "$kasmvnc_sha" ] \
    || die "unexpected KasmVNC source bundle — no GPL runtime image is released"
fi
for w in "${assets_want[@]}"; do
  [ -f "$DL/release-assets/$w" ] || die "release-assets is missing '$w'"
done

cp "$DL/release-chart/tinycdi-$CHART_VERSION.tgz" "$OUT/bundle/"
cp "$DL/release-assets/"* "$OUT/bundle/"

# ---- 5. the packaged chart must pin exactly these digests (SEC-18) -----
tgz="$OUT/bundle/tinycdi-$CHART_VERSION.tgz"
mapfile -t vpaths < <(tar -tzf "$tgz" | grep -E '^[^/]+/values\.yaml$')
[ "${#vpaths[@]}" -eq 1 ] || die "chart tgz holds ${#vpaths[@]} values.yaml files"
tar -xzf "$tgz" -O "${vpaths[0]}" > "$OUT/.chart-values.yaml"
for img in "${expected[@]}"; do
  read -r section key <<< "$(path_for "$img")"
  stamped="$(awk -v section="$section" -v key="$key" '
    /^[^ #]/                          { in_sec = ($0 ~ ("^" section ":")) }
    in_sec && /^  [a-zA-Z]+:/         { cur = substr($1, 1, length($1) - 1) }
    in_sec && cur == key && /^    digest:/ {
      gsub(/[" ]/, "", $2); print $2; exit
    }' "$OUT/.chart-values.yaml")"
  [ -n "$stamped" ] || die "chart values $section.$key.digest is unset — digests were not stamped"
  ref="$(tr -d '[:space:]' < "$OUT/refs/$img.ref")"
  [ "$stamped" = "${ref#*@}" ] \
    || die "chart $section.$key.digest ($stamped) != $img.ref digest (${ref#*@})"
done
rm -f "$OUT/.chart-values.yaml"

# Validated SBOMs join the bundle so checksums, sign-blob and the GitHub
# Release cover every published asset.
cp "$OUT"/sboms/*.spdx.json "$OUT/bundle/"

echo "publish inputs ok: ${#expected[@]} refs + ${#expected[@]} SBOMs + chart + assets"
