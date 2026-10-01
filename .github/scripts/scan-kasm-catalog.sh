#!/usr/bin/env bash
# scan-kasm-catalog.sh — the heavyweight half of the kasm catalog gate
# (KASM-2/KASM-5): for every entry in build/kasm-catalog.txt, pull the
# digest-pinned image, prove the bundled browser engine meets the
# declared freshness floor, and run the release trivy gate
# (--severity CRITICAL,HIGH --ignore-unfixed).
#
#   - Engine versions are extracted by running the browser binary with
#     --version inside the image — trivy's OS-package view does not cover
#     the browser reliably (the kasmweb images carry a Debian chromium
#     inside an Ubuntu base and report 0 vulns for it), so the freshness
#     check is the gate that keeps stale engines out.
#   - Images are pulled ONE AT A TIME and removed afterwards (~4.5 GB
#     uncompressed each). Set KEEP_IMAGES=1 to skip removal.
#
# Usage: scan-kasm-catalog.sh [catalog-file]
# Env:   TRIVY (default: trivy on PATH), DOCKER (default: docker).
set -euo pipefail
cd "$(dirname "$0")/../.."

CATALOG="${1:-build/kasm-catalog.txt}"
TRIVY="${TRIVY:-trivy}"
DOCKER="${DOCKER:-docker}"
fail=0
err() { echo "::error::$*"; fail=1; }

# Catalog-scoped exceptions: build/kasm-catalog.trivyignore carries only
# dormant-path findings, time-boxed the same as .trivyignore (SEC-44). The
# effective file pins trivy's native exp: so an expired exception can
# never suppress a finding at scan time.
IGN="$(mktemp)"; trap 'rm -f "$IGN"' EXIT
.github/scripts/check-trivyignore.sh --out "$IGN" build/kasm-catalog.trivyignore

engine_version() { # <image> <engine> -> prints version string
  local image="$1" engine="$2"
  case "$engine" in
    chromium)
      "$DOCKER" run --rm --entrypoint /bin/sh "$image" -c \
        'for b in /usr/lib/chromium/chromium /usr/bin/chromium-orig /usr/bin/chromium \
                  /opt/google/chrome/google-chrome /opt/microsoft/msedge/microsoft-edge; do
           [ -x "$b" ] && { "$b" --version 2>/dev/null; exit 0; }; done; exit 1' ;;
    firefox)
      "$DOCKER" run --rm --entrypoint /bin/sh "$image" -c \
        'firefox --version 2>/dev/null || firefox-esr --version' ;;
    none)
      return 0 ;;
    *) return 1 ;;
  esac
}

while read -r ref engine floor _; do
  case "${ref:-}" in ""|\#*) continue;; esac
  echo "=== catalog entry: $ref (engine=$engine floor=$floor)"

  "$DOCKER" pull "$ref"

  if [ "$engine" != "none" ]; then
    ver="$(engine_version "$ref" "$engine" || true)"
    major="$(echo "$ver" | grep -oE '[0-9]+(\.[0-9]+){2,3}' | head -1 | cut -d. -f1)"
    echo "    engine version: ${ver:-<none>} (major=${major:-?})"
    if [ -z "$major" ]; then
      err "$ref: could not determine $engine version"
    elif [ "$major" -lt "$floor" ]; then
      err "$ref: $engine $ver is below the freshness floor ($floor) — stale browser engine (KASM-2)"
    fi
  fi

  echo "    trivy gate: --severity CRITICAL,HIGH --ignore-unfixed"
  if ! "$TRIVY" image --scanners vuln --severity CRITICAL,HIGH --ignore-unfixed \
       --ignorefile "$IGN" --exit-code 1 --quiet "$ref"; then
    err "$ref: trivy gate failed (fixable CRITICAL/HIGH present)"
  fi

  if [ "${KEEP_IMAGES:-0}" != "1" ]; then
    "$DOCKER" rmi -f "$ref" >/dev/null 2>&1 || true
  fi
done < "$CATALOG"

if [ "$fail" -ne 0 ]; then
  echo "kasm catalog scan FAILED"
  exit 1
fi
echo "kasm catalog scan OK"
