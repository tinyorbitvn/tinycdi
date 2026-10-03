#!/usr/bin/env bash
# next-rt-tag.test.sh — the rt-YYYYMMDD.N numbering contract (backlog
# 15): N counts successful publishes only, i.e. the highest existing
# rt-<date>.* tag on the package + 1. Other dates, non-rt tags and
# lookalikes never contribute; an empty/absent tag list starts the day
# at .1; malformed input and a failed registry lookup fail closed.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GEN="$ROOT/.github/scripts/next-rt-tag.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0
rc=0; out=""

run_gen() { out="$(bash "$GEN" "$@" 2>&1)"; rc=$?; }

expect() { # expect <desc> <want-tag> <tags-json>
  local desc="$1" want="$2" json="$3"
  printf '%s' "$json" > "$D/in.json"
  run_gen --date 20261020 --tags-json "$D/in.json"
  if [ "$rc" -ne 0 ] || [ "$out" != "$want" ]; then
    echo "FAIL: $desc (rc=$rc, out='$out', want '$want')"; fails=1
  fi
}

expect "empty package starts at .1"          rt-20261020.1  '{"tags":[]}'
expect "tags:null starts at .1"              rt-20261020.1  '{"tags":null}'
expect "missing tags key starts at .1"       rt-20261020.1  '{"name":"x"}'
expect "consecutive existing tags"           rt-20261020.4  '["rt-20261020.1","rt-20261020.2","rt-20261020.3"]'
expect "max+1 over a gapped series"          rt-20261020.6  '{"tags":["rt-20261020.1","rt-20261020.5"]}'
expect "numeric not lexical (.10 beats .9)"  rt-20261020.11 '{"tags":["rt-20261020.9","rt-20261020.10"]}'
expect "other dates/prefixes ignored"        rt-20261020.2  '{"tags":["rt-20261019.7","rt-20261021.4","sha-abc","main","rt-20261020.1"]}'
expect "rt lookalikes ignored"               rt-20261020.2  '{"tags":["rt-2026102.9","rt-202610200.9","rt-20261020.x","rt-20261020.","foo","rt-20261020.1"]}'

# --- stdin source ---
out="$(printf '%s' '{"tags":["rt-20261020.2"]}' \
  | bash "$GEN" --date 20261020 --tags-json - 2>&1)"; rc=$?
{ [ "$rc" -eq 0 ] && [ "$out" = "rt-20261020.3" ]; } \
  || { echo "FAIL: --tags-json - (stdin) (rc=$rc, out='$out')"; fails=1; }

# --- failures must fail closed ---
printf '%s' '{"tags":[]}' > "$D/in.json"
run_gen --date 2026-10-20 --tags-json "$D/in.json"
[ "$rc" -ne 0 ] || { echo "FAIL: non-YYYYMMDD date accepted"; fails=1; }
run_gen --tags-json "$D/in.json"
[ "$rc" -ne 0 ] || { echo "FAIL: missing --date accepted"; fails=1; }
run_gen --date 20261020 --tags-json "$D/nope.json"
[ "$rc" -ne 0 ] || { echo "FAIL: missing tags file accepted"; fails=1; }
printf '%s' 'not json' > "$D/bad.json"
run_gen --date 20261020 --tags-json "$D/bad.json"
[ "$rc" -ne 0 ] || { echo "FAIL: malformed tags JSON accepted"; fails=1; }
run_gen --date 20261020 --bogus
[ "$rc" -ne 0 ] || { echo "FAIL: unknown argument accepted"; fails=1; }

# --- registry lookup fails closed (dead port, no guessing) ---
out="$(RT_TAG_REGISTRY=127.0.0.1:1 GH_TOKEN= \
  bash "$GEN" --date 20261020 2>&1)"; rc=$?
[ "$rc" -ne 0 ] || { echo "FAIL: unreachable registry did not fail closed"; fails=1; }

if [ "$fails" -eq 0 ]; then echo "next-rt-tag: all cases pass"; fi
exit "$fails"
