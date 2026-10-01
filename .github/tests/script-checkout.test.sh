#!/usr/bin/env bash
# script-checkout.test.sh — every workflow job that runs a repo script
# (.github/scripts/*) must check the repository out before that step;
# otherwise the job fails at runtime with "No such file or directory".
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
fails=0
for wf in "$ROOT"/.github/workflows/*.yml; do
  out="$(awk '
    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { job=$1; co=0; next }
    /uses: actions\/checkout@/        { co=1 }
    /^[[:space:]]*#/                  { next }
    /\.github\/scripts\//             { if (job != "" && !co) print job " line " NR }
  ' "$wf")"
  if [ -n "$out" ]; then
    while IFS= read -r l; do echo "FAIL: $(basename "$wf"): job $l runs a repo script before actions/checkout"; done <<< "$out"
    fails=1
  fi
done
[ "$fails" -eq 0 ] && echo "script-checkout: all jobs check out before running repo scripts"
exit "$fails"
