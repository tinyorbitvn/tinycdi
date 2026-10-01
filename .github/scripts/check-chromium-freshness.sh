#!/usr/bin/env bash
# check-chromium-freshness.sh — fail when Debian bookworm-security offers a
# newer chromium than the browser image's pinned CHROMIUM_APT_VERSION.
#
# SUPR-4: the apt pin keeps the browser build reproducible, but pins go
# stale. Once bookworm-security publishes anything newer this check fails
# until CHROMIUM_APT_VERSION is bumped (docs/images.md), so a known-stale
# engine can't drift quietly.
set -euo pipefail

DOCKERFILE="${1:-build/browser/Dockerfile}"
IDX_URL="https://security.debian.org/debian-security/dists/bookworm-security/main/binary-amd64/Packages.xz"

PINNED="$(awk -F= '/^ARG CHROMIUM_APT_VERSION=/ {print $2; exit}' "$DOCKERFILE")"
if [ -z "$PINNED" ]; then
  echo "::error::CHROMIUM_APT_VERSION not found in $DOCKERFILE"
  exit 1
fi

IDX="$(mktemp)"; trap 'rm -f "$IDX"' EXIT
# Download to a file first — an early-exiting awk on a pipe would SIGPIPE
# curl/unxz and trip pipefail.
curl -fsSL "$IDX_URL" | unxz > "$IDX"
AVAIL="$(awk '/^Package: chromium$/ {inpkg=1; next}
             /^Package: /          {inpkg=0}
             inpkg && /^Version: / {print $2; exit}' "$IDX")"
if [ -z "$AVAIL" ]; then
  echo "::error::could not determine the bookworm-security chromium version ($IDX_URL)"
  exit 1
fi

echo "pinned CHROMIUM_APT_VERSION=$PINNED; bookworm-security offers $AVAIL"
if dpkg --compare-versions "$AVAIL" gt "$PINNED"; then
  echo "::error::bookworm-security has chromium $AVAIL > pinned $PINNED — bump CHROMIUM_APT_VERSION in $DOCKERFILE (docs/images.md)"
  exit 1
fi
echo "chromium pin is current"
