#!/usr/bin/env bash
# hack/kasmvnc-source-bundle.sh <out-dir>
#
# Assembles the GPL-2.0 "complete corresponding source" bundle for the
# KasmVNC build shipped in the linux-base runtime image, and so in the
# linux-desktop and browser images built FROM it
# (kasmvncserver_bookworm_1.5.0_amd64.deb, sha256
# 770fd3df51510beecc89666879d82faf411276e68c6e11df612f736b891b5f71,
# published by upstream on 2026-07-29).
#
# Contents (all pinned by sha256 below):
#   * the KasmVNC source at tag v1.5.0 — the exact commit the .deb was
#     released from — with the `kasmweb` git submodule (noVNC web client
#     baked into the .deb) populated at its recorded commit;
#   * the third-party source trees that upstream's build scripts
#     (builder/build.sh + builder/scripts/build-*) fetch and link at
#     build time: xorg-server, libjpeg-turbo, libwebp, oneTBB, libcpuid,
#     fmt and libyuv. Upstream pins xorg-server (XORG_VER=21.1.7) and
#     libwebp (1.5.0); the other deps are "latest release" fetches in
#     upstream's scripts, pinned here to the releases that were current
#     on the .deb build date (libyuv: the `stable` branch tip, which is
#     static since 2021).
#
# Output: <out-dir>/kasmvnc-1.5.0-corresponding-source.tar.gz (+ .sha256).
# The tar is deterministic (sorted, owner 0:0, clamped mtime, gzip -n):
# re-running produces a byte-identical archive.

set -euo pipefail

BUNDLE_NAME="kasmvnc-1.5.0-corresponding-source"
# Upstream .deb publication stamp; used as the deterministic tar mtime.
SOURCE_DATE_EPOCH=1785337852   # 2026-07-29T15:10:52Z

# label|subdir-in-bundle|url|sha256
# Every tarball below carries one leading directory that is stripped on
# extraction. Integrity is pinned by the sha256 of the downloaded bytes.
COMPONENTS=(
  "KasmVNC 1.5.0 (tag v1.5.0)|KasmVNC-1.5.0|https://github.com/kasmtech/KasmVNC/archive/refs/tags/v1.5.0.tar.gz|e80d79205b12139c16d8131780e8b75b8d1e8e4dbb67c4c7c41a0f4951426af9"
  "noVNC kasmweb submodule @ 475ecfa|KasmVNC-1.5.0/kasmweb|https://github.com/kasmtech/noVNC/archive/475ecfa5356579ef222983c7ce4619a7576a3bce.tar.gz|325084abe7af9174812f06933a9f154d044b485254eda9f49ce511552a262420"
  "xorg-server 21.1.7 (XORG_VER, builder/build.sh)|deps/xorg-server-21.1.7|https://www.x.org/archive/individual/xserver/xorg-server-21.1.7.tar.gz|1a9005f47c7ea83645a977581324439628a32c4426303e5a4b9c2d6615becfbf"
  "libjpeg-turbo 3.2.0 (latest release at .deb build date)|deps/libjpeg-turbo-3.2.0|https://github.com/libjpeg-turbo/libjpeg-turbo/archive/3.2.0.tar.gz|980dd81f425082aa6d7c9e47fef27554ce7a9ffc8e2f6e863b97d263c5c50858"
  "libwebp 1.5.0 (pinned by builder/scripts/build-webp)|deps/libwebp-1.5.0|https://storage.googleapis.com/downloads.webmproject.org/releases/webp/libwebp-1.5.0.tar.gz|7d6fab70cf844bf6769077bd5d7a74893f8ffd4dfb42861745750c63c2a5c92c"
  "oneTBB v2023.1.0 (latest release at .deb build date)|deps/oneTBB-2023.1.0|https://github.com/uxlfoundation/oneTBB/archive/v2023.1.0.tar.gz|191288b52e1e6b17198000b64d77d194bb65e791be46ebc606e9b091781e2070"
  "libcpuid v0.8.1 (latest release at .deb build date)|deps/libcpuid-0.8.1|https://github.com/anrieff/libcpuid/archive/v0.8.1.tar.gz|81f2f40da5d66b8220476e116cb40bca4e6a62c0d22bdeeb8e3856cf14607007"
  "fmt 12.2.0 (latest release at .deb build date)|deps/fmt-12.2.0|https://github.com/fmtlib/fmt/archive/12.2.0.tar.gz|8b852bb5aa6e7d8564f9e81394055395dd1d1936d38dfd3a17792a02bebd7af0"
)

# libyuv ships no release tarballs (upstream clones the `stable` branch),
# so it is pinned by commit instead; the commit object id is the integrity
# check. `stable` has been static since 2021, so this is what the .deb
# build fetched.
LIBYUV_REPO="https://chromium.googlesource.com/libyuv/libyuv"
LIBYUV_COMMIT="eb6e7bb63738e29efd82ea3cf2a115238a89fa51"

OUT_DIR="${1:?usage: $0 <out-dir>}"
mkdir -p "$OUT_DIR"
OUT_DIR="$(cd "$OUT_DIR" && pwd)"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
STAGE="$WORK/stage/$BUNDLE_NAME"
mkdir -p "$STAGE"

for entry in "${COMPONENTS[@]}"; do
  IFS='|' read -r label subdir url sha <<<"$entry"
  tmp="$WORK/download.tar.gz"
  echo ">> $label"
  curl -fsSL -o "$tmp" "$url"
  echo "$sha  $tmp" | sha256sum -c - >/dev/null
  mkdir -p "$STAGE/$subdir"
  tar -xzf "$tmp" -C "$STAGE/$subdir" --strip-components=1
  rm -f "$tmp"
done

echo ">> libyuv stable @ ${LIBYUV_COMMIT:0:7} (builder/scripts/build-libyuv)"
git init -q "$WORK/libyuv"
git -C "$WORK/libyuv" remote add origin "$LIBYUV_REPO"
git -C "$WORK/libyuv" fetch -q --depth 1 origin "$LIBYUV_COMMIT"
git -C "$WORK/libyuv" checkout -q FETCH_HEAD
[ "$(git -C "$WORK/libyuv" rev-parse HEAD)" = "$LIBYUV_COMMIT" ]
mkdir -p "$STAGE/deps/libyuv-${LIBYUV_COMMIT:0:7}"
git -C "$WORK/libyuv" archive HEAD | tar -x -C "$STAGE/deps/libyuv-${LIBYUV_COMMIT:0:7}"

cat > "$STAGE/CORRESPONDING-SOURCE.md" <<'EOF'
# KasmVNC 1.5.0 — complete corresponding source

This archive is the corresponding source for the KasmVNC binaries
redistributed inside the TinyCDI runtime images
(`kasmvncserver_bookworm_1.5.0_amd64.deb`, sha256
`770fd3df51510beecc89666879d82faf411276e68c6e11df612f736b891b5f71`,
upstream release published 2026-07-29). KasmVNC is licensed under
GPL-2.0 (see `KasmVNC-1.5.0/LICENSE.TXT`); the other components keep
their own licenses as recorded in each source tree.

## Layout

- `KasmVNC-1.5.0/` — upstream KasmVNC source at tag `v1.5.0`, the commit
  the released .deb was built from, including its `builder/` scripts.
- `KasmVNC-1.5.0/kasmweb/` — the `kasmweb` git submodule (kasmtech/noVNC
  fork) populated at the commit recorded by tag v1.5.0
  (`475ecfa5356579ef222983c7ce4619a7576a3bce`); this web client ships
  inside the .deb.
- `deps/` — the third-party source trees upstream's build fetches and
  statically links via `builder/build.sh` and `builder/scripts/build-*`:

| Source | Version | How upstream's build selects it |
|---|---|---|
| xorg-server | 21.1.7 | `XORG_VER=21.1.7` in `builder/dockerfile.debian_bookworm.*`, fetched by `builder/build.sh` |
| libjpeg-turbo | 3.2.0 | latest GitHub release at the .deb build date (2026-07-29) |
| libwebp | 1.5.0 | pinned `WEBP_VERSION` in `builder/scripts/build-webp` |
| oneTBB | v2023.1.0 | latest GitHub release at the .deb build date |
| libcpuid | v0.8.1 | latest GitHub release at the .deb build date |
| fmt | 12.2.0 | latest GitHub release at the .deb build date |
| libyuv | `eb6e7bb` (`stable` tip) | `git clone --branch stable` in `builder/scripts/build-libyuv`; the branch tip has been static since 2021 |

Build-time distro packages (compiler, cmake/ninja, X headers from
`apt-get build-dep xorg-server`) come from Debian bookworm; their
corresponding source is available via `deb-src` entries for the
bookworm suite at deb.debian.org.

## Rebuilding

The upstream entry point is `builder/build-deb` →
`builder/dockerfile.debian_bookworm.deb.build` → `builder/build.sh`.
With this bundle, the network fetches performed by `builder/build.sh`
and `builder/scripts/build-deps.sh` can be satisfied from `deps/` and
`KasmVNC-1.5.0/kasmweb/`.
EOF

cd "$WORK/stage"
tar --sort=name --format=posix --owner=0 --group=0 --numeric-owner \
    --pax-option=delete=atime,delete=ctime,delete=btime \
    --clamp-mtime --mtime="@$SOURCE_DATE_EPOCH" \
    -cf - "$BUNDLE_NAME" | gzip -n9 > "$OUT_DIR/$BUNDLE_NAME.tar.gz"
(cd "$OUT_DIR" && sha256sum "$BUNDLE_NAME.tar.gz" > "$BUNDLE_NAME.tar.gz.sha256")
echo "wrote $OUT_DIR/$BUNDLE_NAME.tar.gz"
cat "$OUT_DIR/$BUNDLE_NAME.tar.gz.sha256"
