#!/usr/bin/env bash
# check-browser-freshness.sh — fail when a browser engine pinned in the
# browser image (chromium AND firefox-esr) is out of date against Debian
# bookworm-security.
#
# SUPR-4: the apt pins keep the browser build reproducible, but pins go
# stale — and bookworm-security keeps only the newest build of a package,
# so a stale pin soon stops EXISTING and the image can no longer be built
# from scratch (FX-R15: firefox-esr 140.16.0esr vanished and the images
# workflow went red on a cache miss). Per engine the check reports one of:
#
#   current  — the pin is the newest build in the index
#   newer    — the pin is still in the index but a newer build exists
#   gone     — "pinned version gone": the pin no longer exists in the index
#              (apt would fail "Version '<pin>' for '<pkg>' was not found")
#
# Anything but `current` fails the run (exit 1) and prints one machine-
# readable line per stale engine for runtime-freshness.yml to bump:
#
#   BUMP <engine> <new-version> <deb-sha256>
#
# Exit 2 means the check itself could not run (pin missing from the
# Dockerfile, index unreachable, package absent) — never a stale pin.
#
# Usage: check-browser-freshness.sh [dockerfile]
# Env:   BROWSER_FRESHNESS_INDEX — path to an uncompressed Packages file
#        used instead of downloading the index (regression tests only).
set -euo pipefail

DOCKERFILE="${1:-build/browser/Dockerfile}"
IDX_URL="https://security.debian.org/debian-security/dists/bookworm-security/main/binary-amd64/Packages.xz"
# engine : Dockerfile ARG : Debian package
ENGINES=(
  "chromium:CHROMIUM_APT_VERSION:chromium"
  "firefox-esr:FIREFOX_ESR_APT_VERSION:firefox-esr"
)

IDX="$(mktemp)"; trap 'rm -f "$IDX"' EXIT
if [ -n "${BROWSER_FRESHNESS_INDEX:-}" ]; then
  cp "$BROWSER_FRESHNESS_INDEX" "$IDX"
else
  # Download to a file first — an early-exiting awk on a pipe would
  # SIGPIPE curl/unxz and trip pipefail.
  curl -fsSL "$IDX_URL" | unxz > "$IDX"
fi

# index_entries <package> — one "<version> <sha256>" line per stanza.
index_entries() {
  awk -v pkg="$1" '
    /^Package: / { if (inpkg && ver != "") print ver, sha; inpkg = ($2 == pkg); ver = ""; sha = ""; next }
    inpkg && /^Version: / { ver = $2 }
    inpkg && /^SHA256: /  { sha = $2 }
    END { if (inpkg && ver != "") print ver, sha }' "$IDX"
}

rc=0
for spec in "${ENGINES[@]}"; do
  IFS=: read -r engine arg pkg <<< "$spec"

  pinned="$(awk -F= -v arg="$arg" '$1 == "ARG " arg {print $2; exit}' "$DOCKERFILE")"
  if [ -z "$pinned" ]; then
    echo "::error::$arg not found in $DOCKERFILE"
    exit 2
  fi

  entries="$(index_entries "$pkg")"
  if [ -z "$entries" ]; then
    echo "::error::could not determine the bookworm-security $pkg version ($IDX_URL)"
    exit 2
  fi

  # Newest build in the index, and whether the pin is among the builds.
  newest="" newest_sha="" present=no
  while read -r v sha; do
    [ "$v" = "$pinned" ] && present=yes
    if [ -z "$newest" ] || dpkg --compare-versions "$v" gt "$newest"; then
      newest="$v"; newest_sha="$sha"
    fi
  done <<< "$entries"

  echo "$engine: pinned $arg=$pinned; bookworm-security offers $newest"
  if [ "$present" = no ]; then
    echo "::error::$engine: pinned version gone — $pinned no longer exists in bookworm-security (offers $newest); the browser image cannot be built from scratch until $arg is bumped in $DOCKERFILE"
    echo "BUMP $engine $newest $newest_sha"
    rc=1
  elif dpkg --compare-versions "$newest" gt "$pinned"; then
    echo "::error::$engine: newer available — bookworm-security has $newest > pinned $pinned; bump $arg in $DOCKERFILE (docs/images.md)"
    echo "BUMP $engine $newest $newest_sha"
    rc=1
  else
    echo "$engine pin is current"
  fi
done
exit "$rc"
