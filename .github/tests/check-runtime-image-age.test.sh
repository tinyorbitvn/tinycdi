#!/usr/bin/env bash
# check-runtime-image-age.test.sh — the 14-day runtime-image SLO (D28):
# the newest runtime-* GitHub Release may never be older than 14 days,
# and a repo with no runtime release at all fails too (the train never
# ran or the release was deleted — either way the SLO is unverifiable).
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHK="$ROOT/.github/scripts/check-runtime-image-age.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

rc=0; out=""
run_check() { out="$(bash "$CHK" --releases-json "$1" 2>&1)"; rc=$?; }

iso() { date -u -d "$1" +%Y-%m-%dT%H:%M:%SZ; }
tag() { date -u -d "$1" +runtime-%Y.%m.%d; }

# --- fresh: newest runtime release is 3 days old ---
cat > "$D/fresh.json" <<EOF
[{"tagName":"$(tag '-3 days')","createdAt":"$(iso '-3 days')"},
 {"tagName":"$(tag '-10 days')","createdAt":"$(iso '-10 days')"},
 {"tagName":"v0.1.0","createdAt":"$(iso '-30 days')"}]
EOF
run_check "$D/fresh.json"
[ "$rc" -eq 0 ] \
  || { echo "FAIL: 3-day-old release should pass (rc=$rc): $out"; fails=1; }

# --- stale: 15 days old -> exit 1, message names the age ---
cat > "$D/stale.json" <<EOF
[{"tagName":"$(tag '-15 days')","createdAt":"$(iso '-15 days')"},
 {"tagName":"v9.9.9","createdAt":"$(iso 'now')"}]
EOF
run_check "$D/stale.json"
if [ "$rc" -eq 0 ]; then
  echo "FAIL: 15-day-old release should fail"; fails=1
elif ! grep -q '15' <<< "$out"; then
  echo "FAIL: stale message should name the age (15): $out"; fails=1
fi

# --- none: no runtime-* release at all -> exit 1 ---
cat > "$D/none.json" <<EOF
[{"tagName":"v0.1.0","createdAt":"$(iso '-1 day')"}]
EOF
run_check "$D/none.json"
if [ "$rc" -eq 0 ]; then
  echo "FAIL: missing runtime-* release should fail"; fails=1
fi

# --- boundary: exactly 14 days old still passes (not older than 14) ---
cat > "$D/edge.json" <<EOF
[{"tagName":"$(tag '-13 days -23 hours')","createdAt":"$(iso '-13 days -23 hours')"}]
EOF
run_check "$D/edge.json"
[ "$rc" -eq 0 ] \
  || { echo "FAIL: <14-day release should pass (rc=$rc): $out"; fails=1; }

if [ "$fails" -eq 0 ]; then echo "check-runtime-image-age: all cases pass"; fi
exit "$fails"
