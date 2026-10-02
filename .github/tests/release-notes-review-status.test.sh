#!/usr/bin/env bash
# release-notes-review-status.test.sh — every release's notes must say the
# project has not been independently security-reviewed (FX-R11), and a
# pre-release must additionally say it is a test build.
#
# The notes are built by the "container image manifest + release notes" step
# of release.yml. This test extracts that step's `run:` script and executes it
# in a scratch directory against fixture image refs, once for a final tag and
# once for an -rc.N tag, then asserts on the generated notes.md.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
REL="$ROOT/.github/workflows/release.yml"
STEP='container image manifest + release notes'
REVIEW_LINE='Not independently security-reviewed; a review is planned before v1.0.'
PRE_LINE='Pre-release test build.'
fails=0

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Step indentation: "      - name: <STEP>" (6 cols); its keys sit at 8 and the
# script body at 10. Emit the body dedented by 10 columns.
awk -v step="$STEP" '
  $0 == "      - name: " step { instep = 1; next }
  instep && /^      - / { exit }
  instep && /^        run: \|[[:space:]]*$/ { inrun = 1; next }
  instep && inrun { sub(/^          /, ""); print }
' "$REL" > "$TMP/step.sh"

if [ ! -s "$TMP/step.sh" ]; then
  echo "FAIL: could not extract the '$STEP' run script from release.yml"
  exit 1
fi

DIGEST="sha256:$(printf 'a%.0s' {1..64})"

run_notes() { # run_notes <version> <chart_version> <prerelease> -> notes on stdout
  local version="$1" chart="$2" pre="$3" d="$TMP/run-$1"
  mkdir -p "$d/refs" "$d/bundle"
  echo "ghcr.io/tinyorbitvn/tinycdi-backend@$DIGEST" > "$d/refs/backend.ref"
  echo "ghcr.io/tinyorbitvn/tinycdi-frontend@$DIGEST" > "$d/refs/frontend.ref"
  (
    cd "$d" || exit 1
    VERSION="$version" CHART_VERSION="$chart" PRERELEASE="$pre" \
      GITHUB_REPOSITORY="tinyorbitvn/tinycdi" \
      CHART_REF="ghcr.io/tinyorbitvn/charts/tinycdi@$DIGEST" \
      bash --noprofile --norc -eo pipefail "$TMP/step.sh" > "$d/step.log" 2>&1
  ) || { echo "FAIL: release-notes step exited non-zero for $version"; sed 's/^/  | /' "$d/step.log"; fails=1; return 1; }
  cat "$d/notes.md"
}

has_line() { grep -qxF -- "$2" <<< "$1"; }

# --- final tag ---------------------------------------------------------------
notes="$(run_notes v0.2.0 0.2.0 false)" || notes=""
has_line "$notes" "$REVIEW_LINE" || { echo "FAIL: v0.2.0 notes lack: $REVIEW_LINE"; fails=1; }
if has_line "$notes" "$PRE_LINE"; then
  echo "FAIL: v0.2.0 (final) notes must not say: $PRE_LINE"; fails=1
fi
grep -q '^## Container images' <<< "$notes" \
  || { echo "FAIL: v0.2.0 notes lost the container image table"; fails=1; }

# --- pre-release tag ---------------------------------------------------------
notes="$(run_notes v0.2.0-rc.1 0.2.0-rc.1 true)" || notes=""
has_line "$notes" "$REVIEW_LINE" || { echo "FAIL: v0.2.0-rc.1 notes lack: $REVIEW_LINE"; fails=1; }
has_line "$notes" "$PRE_LINE"    || { echo "FAIL: v0.2.0-rc.1 notes lack: $PRE_LINE"; fails=1; }
grep -q '^## Container images' <<< "$notes" \
  || { echo "FAIL: v0.2.0-rc.1 notes lost the container image table"; fails=1; }

# The status lines come first, ahead of the image table.
first="$(grep -n -m1 -F -- "$REVIEW_LINE" <<< "$notes" | cut -d: -f1)"
table="$(grep -n -m1 '^## Container images' <<< "$notes" | cut -d: -f1)"
if [ -z "$first" ] || [ -z "$table" ] || [ "$first" -ge "$table" ]; then
  echo "FAIL: review-status line must precede the container image table"; fails=1
fi

[ "$fails" -eq 0 ] && echo "release-notes-review-status: final and pre-release notes carry the review status"
exit "$fails"
