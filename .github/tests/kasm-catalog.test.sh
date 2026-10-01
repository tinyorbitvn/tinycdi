#!/usr/bin/env bash
# kasm-catalog.test.sh — regression coverage for the kasm image catalog
# gate (.github/scripts/check-kasm-catalog.sh, KASM-2/KASM-5).
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHK="$ROOT/.github/scripts/check-kasm-catalog.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

DIGEST="sha256:$(printf 'a%.0s' $(seq 64))"
cat > "$D/Dockerfile" <<EOF
ARG CHROMIUM_APT_VERSION=154.0.8037.92-1~deb12u1
EOF

accept() { # accept <desc>
  if ! KASM_CATALOG="$D/cat" KASM_CHROMIUM_DOCKERFILE="$D/Dockerfile" \
       KASM_SCAN_FILES="$D/scan.yaml $D/doc.md" \
       bash "$CHK" >/dev/null 2>&1; then
    echo "FAIL: $1 should be accepted"; fails=1
  fi
}
reject() { # reject <desc>
  if KASM_CATALOG="$D/cat" KASM_CHROMIUM_DOCKERFILE="$D/Dockerfile" \
     KASM_SCAN_FILES="$D/scan.yaml $D/doc.md" \
     bash "$CHK" >/dev/null 2>&1; then
    echo "FAIL: $1 should be rejected"; fails=1
  fi
}

mk() { # mk <catalog> <scan.yaml> <doc.md>
  printf '%s\n' "$1" > "$D/cat"
  printf '%s\n' "$2" > "$D/scan.yaml"
  printf '%s\n' "$3" > "$D/doc.md"
}

ENTRY="kasmweb/chromium@$DIGEST  chromium  150"

# happy path: pinned, cataloged, floor meets the pin-major-minus-4 rule.
mk "$ENTRY" "image: kasmweb/chromium@$DIGEST" "prose mentions kasmweb/chromium:1.18.0 freely"
accept "pinned + cataloged + floor"

# tag-only ref in a values file.
mk "$ENTRY" "image: kasmweb/chromium:1.18.0" ""
reject "tag-only ref"

# digest-pinned but NOT in the catalog.
mk "$ENTRY" "image: kasmweb/other@$DIGEST" ""
reject "uncataloged digest ref"

# a comment mention must not count as a reference.
mk "$ENTRY" "# image: kasmweb/chromium:latest
image: kasmweb/chromium@$DIGEST" ""
accept "commented ref ignored"

# floor below pin-major-4 (154-4=150) is rejected.
mk "kasmweb/chromium@$DIGEST  chromium  149" "image: kasmweb/chromium@$DIGEST" ""
reject "floor below documented minimum"

# catalog entry without digest pin.
mk "kasmweb/chromium:1.18.0  chromium  150" "image: kasmweb/chromium@$DIGEST" ""
reject "catalog entry not digest-pinned"

# docs pinning an uncataloged digest is rejected; prose names are fine.
mk "$ENTRY" "image: kasmweb/chromium@$DIGEST" "pin kasmweb/firefox@$DIGEST here"
reject "docs digest pin not in catalog"

# unknown engine token.
mk "kasmweb/chromium@$DIGEST  webkit  150" "image: kasmweb/chromium@$DIGEST" ""
reject "unknown engine"

# 'none' engine entries carry no floor requirement beyond shape.
mk "kasmweb/ubuntu-noble-desktop@$DIGEST  none  0" "image: kasmweb/ubuntu-noble-desktop@$DIGEST" ""
accept "engine=none desktop entry"

# real repo catalog passes its own gate when scanned directly.
if ! (cd "$ROOT" && bash .github/scripts/check-kasm-catalog.sh) >/dev/null 2>&1; then
  echo "FAIL: repo catalog failed its own check"; fails=1
fi

if [ "$fails" -ne 0 ]; then
  echo "kasm-catalog.test.sh: FAILURES"
  exit 1
fi
echo "kasm-catalog.test.sh: all cases pass"
