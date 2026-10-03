#!/usr/bin/env bash
# check-kasm-catalog.sh — static enforcement of the kasm image catalog
# policy (docs/kasm-images.md, KASM-2/KASM-5, E13). Fast, network-free;
# runs in PR CI. The heavyweight half — pulling each entry, extracting
# the engine version and running the trivy gate — lives in
# scan-kasm-catalog.sh and runs on the kasm-contract schedule job.
#
# Enforces:
#   1. Every kasmweb/* image ref in tracked files is digest-pinned
#      (repo@sha256:<64 hex>).
#   2. Every such ref appears verbatim in build/kasm-catalog.txt — docs
#      and examples may only point at cataloged, gated images.
#   3. Every catalog entry declares engine + min-engine-major + class,
#      and the floor meets the class's budget (E13):
#        browser: >= major(CHROMIUM_APT_VERSION) - 2
#        desktop: >= major(CHROMIUM_APT_VERSION) - 4
#      (the documented lag budgets — kasmweb images ride Debian/Ubuntu
#      rebuilds; the browser gate tightened from -4 to -2 in v0.3).
#   4. Every ref on the chart's kasmAdapter.browserAllowlist
#      (deploy/helm/tinycdi/values.yaml) is a browser-class catalog
#      entry — the allowlist is only as fresh as the catalog says.
set -euo pipefail
# KASM_REPO_ROOT exists for the regression test (.github/tests/): the
# default scan is `git ls-files`, so tests run it against a scratch repo.
cd "${KASM_REPO_ROOT:-$(cd "$(dirname "$0")/../.." && pwd)}"

# Env overrides exist for the regression test (.github/tests/).
CATALOG="${KASM_CATALOG:-build/kasm-catalog.txt}"
DOCKERFILE="${KASM_CHROMIUM_DOCKERFILE:-build/browser/Dockerfile}"
VALUES="${KASM_CHART_VALUES:-deploy/helm/tinycdi/values.yaml}"
if [ -n "${KASM_SCAN_FILES:-}" ]; then
  # shellcheck disable=SC2206
  SCAN_FILES=($KASM_SCAN_FILES)
else
  # KASM-5: every tracked text file is in scope — an uncataloged
  # kasmweb/* ref must fail whether it sits in a chart values file, a
  # nested docs page, or the README, not just ci/*.yaml + docs/*.md.
  # Excluded: binary blobs and fixtures that plant deliberately
  # uncataloged refs (.github/tests/, *_test.go, testdata/).
  mapfile -t SCAN_FILES < <(git ls-files | grep -vE \
    '(^|/)\.github/tests/|(^|/)testdata/|_test\.go$|\.(png|jpe?g|gif|webp|ico|pdf|zip|gz|tgz|xz|bz2|7z|jar|war|woff2?|ttf|otf|eot|so|dll|exe|bin|wasm|pyc)$')
  # The catalog is itself a scan target; keep it covered even when
  # KASM_CATALOG points outside the repo (the tests do that).
  SCAN_FILES+=("$CATALOG")
fi
fail=0

err() { echo "::error::$*"; fail=1; }

# -- catalog sanity ------------------------------------------------------
declare -A CATALOG_REFS=()
declare -A CATALOG_CLASS=()
while read -r ref engine floor class _; do
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
  case "$class" in
    browser)
      # A browser-class entry without a bundled engine is nonsense —
      # the class exists to bind a freshness floor to an engine.
      if [ "$engine" = "none" ]; then
        err "catalog entry $ref is class browser but has engine 'none'"
      fi
      ;;
    desktop) ;;
    *) err "catalog entry $ref has unknown class '$class' (want browser|desktop)" ;;
  esac
  CATALOG_REFS["$ref"]=1
  CATALOG_CLASS["$ref"]="$class"
done < "$CATALOG"

if [ "${#CATALOG_REFS[@]}" -eq 0 ]; then
  err "no catalog entries in $CATALOG"
fi

# -- floor vs the browser pin (E13 two-tier budgets) ---------------------
PINNED="$(awk -F= '/^ARG CHROMIUM_APT_VERSION=/ {print $2; exit}' "$DOCKERFILE")"
PIN_MAJOR="${PINNED%%.*}"
BROWSER_FLOOR_MIN=$(( PIN_MAJOR - 2 ))
DESKTOP_FLOOR_MIN=$(( PIN_MAJOR - 4 ))
while read -r ref engine floor class _; do
  case "${ref:-}" in ""|\#*) continue;; esac
  [ "$engine" = "none" ] && continue
  case "$class" in
    browser)
      if [ "$floor" -lt "$BROWSER_FLOOR_MIN" ]; then
        err "browser catalog floor for $ref is $floor, below the browser minimum (CHROMIUM_APT_VERSION $PINNED major $PIN_MAJOR - 2 = $BROWSER_FLOOR_MIN)"
      fi
      ;;
    *)
      if [ "$floor" -lt "$DESKTOP_FLOOR_MIN" ]; then
        err "catalog floor for $ref is $floor, below the documented minimum (CHROMIUM_APT_VERSION $PINNED major $PIN_MAJOR - 4 = $DESKTOP_FLOOR_MIN)"
      fi
      ;;
  esac
done < "$CATALOG"

# -- chart browser allowlist ⊆ browser-class catalog entries -------------
# A seeded adapter=kasm template with experience=Browser may only use an
# allowlisted image (tinycdi.validate in _helpers.tpl); every allowlisted
# ref must therefore be a browser-class catalog entry or the allowlist
# would admit images the ≤2 gate rejects. Only the block-list items under
# the kasmAdapter.browserAllowlist key are extracted — other kasmweb refs
# in values.yaml are covered by the generic scan below.
if [ -f "$VALUES" ]; then
  while read -r ref; do
    [ -n "$ref" ] || continue
    if [ -z "${CATALOG_REFS[$ref]:-}" ]; then
      err "$VALUES: browserAllowlist entry not in the catalog: $ref"
    elif [ "${CATALOG_CLASS[$ref]}" != "browser" ]; then
      err "$VALUES: browserAllowlist entry $ref is catalog class '${CATALOG_CLASS[$ref]}' — only browser-class entries may back experience=Browser kasm templates"
    fi
  done < <(awk '
    /^[[:space:]]*browserAllowlist:[[:space:]]*\[/ { print; inlist = ($0 !~ /\]/); next }
    /^[[:space:]]*browserAllowlist:/ { inlist = 1; print; next }
    inlist && /^[[:space:]]*(#|$)/ { next }
    inlist && /^[[:space:]]*-[[:space:]]/ { print; next }
    inlist { inlist = 0 }
  ' "$VALUES" | sed 's/#.*//' | grep -oE 'kasmweb/[a-z0-9._-]+@sha256:[0-9a-f]{64}' | sort -u)
fi

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
    *.md|*.markdown|*.rst|*.adoc|NOTICE|SOURCE-OFFER|LICENSE*|COPYING*|CHANGELOG*)
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
echo "kasm catalog check OK: ${#CATALOG_REFS[@]} entries, browser floor >= $BROWSER_FLOOR_MIN, desktop floor >= $DESKTOP_FLOOR_MIN, all references digest-pinned + cataloged"
