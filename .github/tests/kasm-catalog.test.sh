#!/usr/bin/env bash
# kasm-catalog.test.sh — regression coverage for the kasm image catalog
# gate (.github/scripts/check-kasm-catalog.sh, KASM-2/KASM-5, E13).
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHK="$ROOT/.github/scripts/check-kasm-catalog.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

DIGEST="sha256:$(printf 'a%.0s' $(seq 64))"
cat > "$D/Dockerfile" <<EOF
ARG CHROMIUM_APT_VERSION=154.0.8037.92-1~deb12u1
EOF
# pin major 154 → browser floor >= 152, desktop floor >= 150.

accept() { # accept <desc>
  if ! KASM_CATALOG="$D/cat" KASM_CHROMIUM_DOCKERFILE="$D/Dockerfile" \
       KASM_CHART_VALUES="$D/values.yaml" \
       KASM_SCAN_FILES="$D/scan.yaml $D/doc.md" \
       bash "$CHK" >/dev/null 2>&1; then
    echo "FAIL: $1 should be accepted"; fails=1
  fi
}
reject() { # reject <desc>
  if KASM_CATALOG="$D/cat" KASM_CHROMIUM_DOCKERFILE="$D/Dockerfile" \
     KASM_CHART_VALUES="$D/values.yaml" \
     KASM_SCAN_FILES="$D/scan.yaml $D/doc.md" \
     bash "$CHK" >/dev/null 2>&1; then
    echo "FAIL: $1 should be rejected"; fails=1
  fi
}

mk() { # mk <catalog> <scan.yaml> <doc.md> [values.yaml]
  printf '%s\n' "$1" > "$D/cat"
  printf '%s\n' "$2" > "$D/scan.yaml"
  printf '%s\n' "$3" > "$D/doc.md"
  printf '%s\n' "${4:-kasmAdapter: {browserAllowlist: []}}" > "$D/values.yaml"
}

ENTRY="kasmweb/chromium@$DIGEST  chromium  150  desktop"

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
mk "kasmweb/chromium@$DIGEST  chromium  149  desktop" "image: kasmweb/chromium@$DIGEST" ""
reject "floor below documented minimum"

# catalog entry without digest pin.
mk "kasmweb/chromium:1.18.0  chromium  150  desktop" "image: kasmweb/chromium@$DIGEST" ""
reject "catalog entry not digest-pinned"

# docs pinning an uncataloged digest is rejected; prose names are fine.
mk "$ENTRY" "image: kasmweb/chromium@$DIGEST" "pin kasmweb/firefox@$DIGEST here"
reject "docs digest pin not in catalog"

# unknown engine token.
mk "kasmweb/chromium@$DIGEST  webkit  150  desktop" "image: kasmweb/chromium@$DIGEST" ""
reject "unknown engine"

# 'none' engine entries carry no floor requirement beyond shape.
mk "kasmweb/ubuntu-noble-desktop@$DIGEST  none  0  desktop" "image: kasmweb/ubuntu-noble-desktop@$DIGEST" ""
accept "engine=none desktop entry"

# -- E13: two-tier engine-lag gate ---------------------------------------
# browser class tightens to pin-major-2 (152): a browser entry 3 majors
# behind fails the gate.
mk "kasmweb/chromium@$DIGEST  chromium  151  browser" "image: kasmweb/chromium@$DIGEST" ""
reject "browser lag 3 fails"

# a browser entry exactly 2 majors behind passes.
mk "kasmweb/chromium@$DIGEST  chromium  152  browser" "image: kasmweb/chromium@$DIGEST" ""
accept "browser lag 2 passes"

# a desktop entry 4 majors behind passes (the non-browser budget).
mk "kasmweb/chromium@$DIGEST  chromium  150  desktop" "image: kasmweb/chromium@$DIGEST" ""
accept "non-browser lag 4 passes"

# the same image as class=browser fails — class is what tightens it.
mk "kasmweb/chromium@$DIGEST  chromium  150  browser" "image: kasmweb/chromium@$DIGEST" ""
reject "desktop floor as browser class"

# missing or unknown class is a malformed entry.
mk "kasmweb/chromium@$DIGEST  chromium  152" "image: kasmweb/chromium@$DIGEST" ""
reject "entry without class"
mk "kasmweb/chromium@$DIGEST  chromium  152  kiosk" "image: kasmweb/chromium@$DIGEST" ""
reject "unknown class"

# browser class requires a real engine.
mk "kasmweb/x@$DIGEST  none  0  browser" "image: kasmweb/x@$DIGEST" ""
reject "browser class with engine=none"

# -- E13: chart browserAllowlist ⊆ browser-class catalog entries ---------
ALLOWVALUES='kasmAdapter:
  enabled: false
  browserAllowlist:
    - kasmweb/chromium@DIGESTP
  image: {repository: x, digest: sha256:y}'
mk "kasmweb/chromium@$DIGEST  chromium  152  browser" "image: kasmweb/chromium@$DIGEST" "" \
   "${ALLOWVALUES/DIGESTP/$DIGEST}"
accept "allowlist names a browser-class catalog entry"

# an allowlisted desktop-class entry is refused — the ≤2 gate would be
# bypassed by listing a non-browser image.
mk "kasmweb/chromium@$DIGEST  chromium  152  desktop" "image: kasmweb/chromium@$DIGEST" "" \
   "${ALLOWVALUES/DIGESTP/$DIGEST}"
reject "allowlist names a desktop-class entry"

# an allowlisted ref absent from the catalog is refused.
mk "$ENTRY" "" "" "kasmAdapter:
  browserAllowlist:
    - kasmweb/other@$DIGEST"
reject "allowlist names an uncataloged ref"

# -- default scan coverage (KASM-5) --------------------------------------
# The default scan is `git ls-files` — an uncataloged ref must fail no
# matter where it is planted. These cases run the script against a
# scratch repo via KASM_REPO_ROOT with no KASM_SCAN_FILES override, so
# the real file discovery is what gets exercised.
BADDIGEST="sha256:$(printf 'b%.0s' $(seq 64))"
BADREF="kasmweb/firefox@$BADDIGEST"
SCRATCH="$D/repo"
mkdir -p "$SCRATCH/deploy/helm/tinycdi" "$SCRATCH/docs/nested/deep" \
  "$SCRATCH/.github/tests" "$SCRATCH/pkg/testdata"
git -C "$SCRATCH" init -q

scratch() { # scratch <ok|fail> <desc>
  local want="$1" desc="$2"
  git -C "$SCRATCH" add -A
  if KASM_REPO_ROOT="$SCRATCH" KASM_CATALOG="$D/cat" \
     KASM_CHROMIUM_DOCKERFILE="$D/Dockerfile" \
     KASM_CHART_VALUES="$D/novalues.yaml" \
     bash "$CHK" >/dev/null 2>&1; then
    [ "$want" = ok ] || { echo "FAIL: $desc should be rejected"; fails=1; }
  else
    [ "$want" = fail ] || { echo "FAIL: $desc should be accepted"; fails=1; }
  fi
}

mk "$ENTRY" "" ""
printf 'spec:\n  image: kasmweb/chromium@%s\n' "$DIGEST" \
  > "$SCRATCH/deploy/helm/tinycdi/values.yaml"
printf 'upstream vendored the kasmweb/noVNC submodule\n' > "$SCRATCH/NOTICE"
# Fixtures plant bad refs on purpose — they are excluded from the scan.
printf 'kasmweb/chromium:latest\n' > "$SCRATCH/.github/tests/fixture.sh"
printf 'const img = "kasmweb/chromium:1.0"\n' > "$SCRATCH/pkg/x_test.go"
printf 'image: kasmweb/chromium:1.0\n' > "$SCRATCH/pkg/testdata/v.yaml"
scratch ok "cataloged digest in a nested values file; fixtures + NOTICE ignored"

# MF-2 probe 1: uncataloged digest in the chart values file.
printf 'spec:\n  image: %s\n' "$BADREF" \
  > "$SCRATCH/deploy/helm/tinycdi/values.yaml"
scratch fail "uncataloged digest in deploy/helm/tinycdi/values.yaml"

# MF-2 probe 2: uncataloged digest in a nested docs page (docs/**, not
# just docs/*.md).
printf 'spec:\n  image: kasmweb/chromium@%s\n' "$DIGEST" \
  > "$SCRATCH/deploy/helm/tinycdi/values.yaml"
printf 'pin this: %s\n' "$BADREF" > "$SCRATCH/docs/nested/deep/guide.md"
scratch fail "uncataloged digest in a nested docs file"

# An untracked file is outside the gate by definition (the gate covers
# tracked files) — fixture files stay tracked and stay excluded.
git -C "$SCRATCH" rm -q --cached "docs/nested/deep/guide.md" >/dev/null 2>&1 || true
rm -f "$SCRATCH/docs/nested/deep/guide.md"
scratch ok "cleaned scratch repo passes again"

# real repo catalog passes its own gate when scanned directly.
if ! (cd "$ROOT" && bash .github/scripts/check-kasm-catalog.sh) >/dev/null 2>&1; then
  echo "FAIL: repo catalog failed its own check"; fails=1
fi

if [ "$fails" -ne 0 ]; then
  echo "kasm-catalog.test.sh: FAILURES"
  exit 1
fi
echo "kasm-catalog.test.sh: all cases pass"
