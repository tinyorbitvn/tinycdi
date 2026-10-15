#!/usr/bin/env bash
# check-scan-age.sh — refuse to sign digests whose trivy gate finished too
# long ago (R1-09, TM-49). The signing job waits on a manual environment
# approval, which GitHub keeps pending for up to 30 days, and "re-run
# failed jobs" of an old run re-enters the signing job without re-running
# its scan legs. Signing then would vouch for a scan days or weeks old.
#
# Completion times come from the GitHub jobs API (latest attempt of each
# job: a leg carried over from an earlier attempt keeps its original
# completed_at), never from an artifact the run produced. Scanners stay
# out of the signing job (SEC-05); this check is the freshness bound.
#
# Usage: check-scan-age.sh <jobs.json> <max-age-hours> <scan job name>...
#   jobs.json — a JSON array of job objects, e.g.
#     gh api --paginate "repos/$R/actions/runs/$ID/jobs?filter=latest&per_page=100" \
#       --jq '.jobs[]' | jq -s .
# Env: NOW_EPOCH — override "now" (tests only).
set -euo pipefail

JOBS="${1:?usage: check-scan-age.sh <jobs.json> <max-age-hours> <scan job name>...}"
MAX_H="${2:?usage: check-scan-age.sh <jobs.json> <max-age-hours> <scan job name>...}"
shift 2
[ "$#" -gt 0 ] || { echo "::error::no scan job names given"; exit 1; }
[[ "$MAX_H" =~ ^[1-9][0-9]*$ ]] \
  || { echo "::error::max age '$MAX_H' is not a positive integer"; exit 1; }
jq -e 'type == "array"' "$JOBS" >/dev/null 2>&1 \
  || { echo "::error::$JOBS is not a JSON array of jobs"; exit 1; }

now="${NOW_EPOCH:-$(date -u +%s)}"
fail=0
for name in "$@"; do
  n="$(jq --arg n "$name" '[.[] | select(.name == $n)] | length' "$JOBS")"
  if [ "$n" -ne 1 ]; then
    echo "::error::expected exactly one job named '$name', found $n"; fail=1; continue
  fi
  concl="$(jq -r --arg n "$name" '.[] | select(.name == $n) | .conclusion // ""' "$JOBS")"
  done_at="$(jq -r --arg n "$name" '.[] | select(.name == $n) | .completed_at // ""' "$JOBS")"
  if [ "$concl" != "success" ]; then
    echo "::error::scan job '$name' concluded '$concl', not success"; fail=1; continue
  fi
  if ! t="$(date -u -d "$done_at" +%s 2>/dev/null)" || [ -z "$done_at" ]; then
    echo "::error::scan job '$name' has no valid completed_at ('$done_at')"; fail=1; continue
  fi
  age_s=$(( now - t ))
  if [ "$age_s" -gt $(( MAX_H * 3600 )) ]; then
    echo "::error::scan job '$name' completed $done_at ($(( age_s / 3600 )) h ago) — older than $MAX_H h; start a fresh run (rebuild + rescan) instead of approving or re-running this one"
    fail=1
  else
    echo "scan '$name' completed $done_at ($(( age_s / 3600 )) h ago)"
  fi
done
exit "$fail"
