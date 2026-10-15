#!/usr/bin/env bash
# check-runtime-image-age.sh — D28 SLO enforcement: the newest runtime-*
# GitHub Release may never be older than 14 days. The release train
# publishes on every runtime-image change and at least weekly, so a gap
# means the train is broken, not that nothing needed rebuilding.
#
# Usage: check-runtime-image-age.sh [--releases-json FILE|-] [--max-age-days N]
#   --releases-json: a `gh release list --json tagName,createdAt` payload
#                    (FILE, or '-' for stdin); without it the script
#                    queries the repo via `gh` (needs GH_TOKEN and
#                    GITHUB_REPOSITORY).
# Env: RUNTIME_AGE_NOW_EPOCH — override "now" (testing).
set -euo pipefail

MAX_DAYS=14
SRC=""
while [ $# -gt 0 ]; do
  case "$1" in
    --releases-json) SRC="${2:?--releases-json needs a path or -}"; shift 2 ;;
    --max-age-days)  MAX_DAYS="${2:?--max-age-days needs a count}"; shift 2 ;;
    *) echo "::error::unknown argument: $1"; exit 2 ;;
  esac
done

if [ -z "$SRC" ] || [ "$SRC" = "-" ]; then
  TMP="$(mktemp)"; trap 'rm -f "$TMP"' EXIT
  if [ -z "$SRC" ]; then
    gh release list --repo "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY unset}" \
      --json tagName,createdAt --limit 200 > "$TMP"
  else
    cat > "$TMP"
  fi
  SRC="$TMP"
fi

NOW="${RUNTIME_AGE_NOW_EPOCH:-$(date -u +%s)}"
NEWEST="$(jq -r '
  [ .[]
    | select(.tagName | test("^runtime-[0-9]{4}\\.[0-9]{2}\\.[0-9]{2}$"))
    | {e: (.createdAt | fromdateiso8601), t: .tagName} ]
  | if length == 0 then empty else max_by(.e) | "\(.e)\t\(.t)" end
' "$SRC")"

if [ -z "$NEWEST" ]; then
  echo "::error::no runtime-* release found — the runtime image train has never published"
  exit 1
fi

EPOCH="${NEWEST%%$'\t'*}"
TAGN="${NEWEST#*$'\t'}"
AGE_S=$((NOW - EPOCH))
AGE_DAYS=$((AGE_S / 86400))

if [ "$AGE_S" -gt "$((MAX_DAYS * 86400))" ]; then
  echo "::error::newest runtime release $TAGN is $AGE_DAYS days old — over the $MAX_DAYS-day SLO; the runtime image train is stalled"
  exit 1
fi
echo "newest runtime release $TAGN is $AGE_DAYS days old — within the $MAX_DAYS-day SLO"
