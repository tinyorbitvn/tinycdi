#!/usr/bin/env bash
# check-scan-age.test.sh — R1-09 / TM-49: a signing job refuses digests
# whose trivy gate finished longer ago than the bound, a scan leg that is
# missing, duplicated or not successful, and malformed input. A fresh,
# successful scan set passes, including one exactly at the bound.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHK="$ROOT/.github/scripts/check-scan-age.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

NOW_EPOCH="$(date -u -d 2026-10-14T12:00:00Z +%s)"; export NOW_EPOCH

job() { printf '{"name":"%s","conclusion":%s,"completed_at":%s}' "$1" "$2" "$3"; }
mk() { # mk <file> <job-json>... — a JSON array of the given jobs
  local f="$D/$1"; shift
  ( IFS=,; printf '[%s]\n' "$*" ) > "$f"
}
run() { # run <file> [max-hours] -> rc, out
  out="$(bash "$CHK" "$D/$1" "${2:-24}" 'scan linux-base' 'scan browser' 2>&1)"; rc=$?
}
pass() { run "$1" "${3:-24}"; [ "$rc" -eq 0 ] || { echo "FAIL: $2 refused: $out"; fails=1; }; }
refuse() { # refuse <file> <desc> <substr> [max-hours]
  run "$1" "${4:-24}"
  if [ "$rc" -eq 0 ]; then echo "FAIL: $2 accepted"; fails=1
  elif ! grep -qF -- "$3" <<< "$out"; then echo "FAIL: $2: want '$3' in: $out"; fails=1; fi
}

mk fresh.json  "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"success"' '"2026-10-13T13:00:00Z"')" \
               "$(job 'publish' null null)"
mk edge.json   "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"success"' '"2026-10-13T12:00:00Z"')"
mk stale.json  "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"success"' '"2026-09-15T09:00:00Z"')"
mk failed.json "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"failure"' '"2026-10-14T03:00:00Z"')"
mk missing.json "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')"
mk dup.json    "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"success"' '"2026-09-01T00:00:00Z"')"
mk nodate.json "$(job 'scan linux-base' '"success"' '"2026-10-14T03:00:00Z"')" \
               "$(job 'scan browser' '"success"' null)"
echo '{"jobs":[]}' > "$D/object.json"

pass   fresh.json "fresh successful scans"
pass   edge.json  "a scan exactly 24 h old"
refuse stale.json   "a scan finished a month before signing" "older than 24 h"
refuse failed.json  "a failed scan leg"                      "not success"
refuse missing.json "a missing scan leg"                     "found 0"
refuse dup.json     "a duplicated scan leg name"             "found 2"
refuse nodate.json  "a scan leg without completed_at"        "no valid completed_at"
refuse object.json  "a jobs object instead of an array"      "not a JSON array"
refuse fresh.json   "a zero max age"                         "not a positive integer" 0

if [ "$fails" -eq 0 ]; then echo "check-scan-age: all cases pass"; fi
exit "$fails"
