#!/usr/bin/env bash
# next-rt-tag.sh — print the next rt-YYYYMMDD.N tag for the runtime
# image train (D27).
#
# N counts successful publishes only (backlog 15): it is one plus the
# highest N found among the rt-<date>.* tags already promoted on the
# train's linux-base package, so failed builds, scan-gate rejections
# and non-publishing rehearsals leave no gaps in the per-day series.
#
# Race safety: runtime-images.yml serializes every publishing run on
# refs/heads/main through its concurrency group (cancel-in-progress:
# false), so this count cannot interleave with another promote. The
# meta job derives the tag once for the OCI labels; the publish job
# derives it again immediately before promotion so a "re-run failed
# jobs" of an old run can never repoint a tag a later train shipped.
#
# Usage: next-rt-tag.sh --date YYYYMMDD [--tags-json FILE|-]
#   --tags-json feeds a registry tags/list body ({"tags":[...]}, a bare
#   array, or {"tags":null}) from a file or stdin instead of querying
#   the registry — for tests and offline mirrors.
#
# Env (registry lookup only):
#   RT_TAG_REGISTRY — registry host (default ghcr.io)
#   RT_TAG_REPO     — repository whose tags define the series (default
#                     tinyorbitvn/tinycdi-linux-base; every publish
#                     promotes all three runtime images under one tag)
#   GH_TOKEN        — token used to mint the registry pull token; unset
#                     asks for an anonymous grant (public packages)
#   GITHUB_ACTOR    — basic-auth user for GH_TOKEN (Actions sets it)
#
# Output: the rt-YYYYMMDD.N tag on stdout. Any lookup failure is fatal
# (fail closed — never guess a number that may already be taken).
set -euo pipefail

DATE="" SRC=""
while [ $# -gt 0 ]; do
  case "$1" in
    --date)      DATE="${2:?--date needs YYYYMMDD}"; shift 2 ;;
    --tags-json) SRC="${2:?--tags-json needs a path or -}"; shift 2 ;;
    *) echo "::error::unknown argument: $1" >&2; exit 2 ;;
  esac
done
[[ "$DATE" =~ ^[0-9]{8}$ ]] \
  || { echo "::error::--date must be YYYYMMDD (got '$DATE')" >&2; exit 2; }

# one tags/list body on stdout; arg 1 = registry token
tags_page() {
  curl -fsSL -D "$HDRS" -H "Authorization: Bearer $1" "$2"
}

if [ -n "$SRC" ]; then
  if [ "$SRC" = "-" ]; then BODY="$(cat)"; else BODY="$(cat "$SRC")"; fi
else
  REG="${RT_TAG_REGISTRY:-ghcr.io}"
  REPO="${RT_TAG_REPO:-tinyorbitvn/tinycdi-linux-base}"
  AUTH=()
  [ -n "${GH_TOKEN:-}" ] && AUTH=(-u "${GITHUB_ACTOR:-x}:$GH_TOKEN")
  TOKEN="$(curl -fsSL "${AUTH[@]}" \
    "https://$REG/token?scope=repository:$REPO:pull" | jq -r '.token // empty')"
  [ -n "$TOKEN" ] \
    || { echo "::error::no pull token from https://$REG/token" >&2; exit 1; }

  HDRS="$(mktemp)"; trap 'rm -f "$HDRS"' EXIT
  URL="https://$REG/v2/$REPO/tags/list?n=1000"
  MERGED="[]" pages=0
  while [ -n "$URL" ]; do
    pages=$((pages + 1))
    [ "$pages" -le 50 ] \
      || { echo "::error::tags/list exceeded 50 pages" >&2; exit 1; }
    PAGE="$(tags_page "$TOKEN" "$URL")"
    MERGED="$(jq --argjson prev "$MERGED" '$prev + (.tags // [])' <<<"$PAGE")"
    # RFC 5988 Link header: </v2/<repo>/tags/list?last=<tag>&n=1000>; rel="next".
    # sed (not grep) does the rel="next" match: the last page carries no
    # Link header, and grep's no-match exit 1 would kill the loop under
    # pipefail. An absent match must mean "done", not "error" — real
    # failures (curl, bad JSON) above still die via set -e.
    NEXT="$(tr -d '\r' < "$HDRS" | awk 'tolower($0) ~ /^link:/' \
      | tr ',' '\n' | sed -n '/rel="next"/ s/.*<\(.*\)>.*/\1/p' | head -1)"
    case "$NEXT" in
      "") URL="" ;;
      /*) URL="https://$REG$NEXT" ;;
      http*) URL="$NEXT" ;;
      *) echo "::error::unparseable Link header in tags/list" >&2; exit 1 ;;
    esac
  done
  BODY="$MERGED"
fi

N="$(jq -r --arg d "$DATE" '
  (if type == "array" then . else (.tags // []) end)
  | [ .[] | capture("^rt-\($d)\\.(?<n>[0-9]+)$").n | tonumber ]
  | (max // 0) + 1
' <<<"$BODY")"
echo "rt-$DATE.$N"
