#!/usr/bin/env bash
# adr-refs.test.sh — the ADR set is numbered without gaps (0001..N), every
# "ADR NNNN" reference anywhere in the repo names an existing ADR, and every
# docs/adr/NNNN-<slug>.md path reference names the real file.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
fails=0
mapfile -t files < <(cd "$ROOT/docs/adr" && ls | grep -E '^[0-9]{4}-[a-z0-9-]+\.md$' | sort)
[ "${#files[@]}" -gt 0 ] || { echo "FAIL: no ADRs found"; exit 1; }
i=1
for f in "${files[@]}"; do
  want="$(printf '%04d' "$i")"
  [ "${f:0:4}" = "$want" ] || { echo "FAIL: ADR numbering gap or duplicate at $f (want $want)"; fails=1; }
  head -n 1 "$ROOT/docs/adr/$f" | grep -qE "^# ADR ${f:0:4} — " \
    || { echo "FAIL: docs/adr/$f: first line must be '# ADR ${f:0:4} — <title>'"; fails=1; }
  grep -qE '^Status: Accepted$' "$ROOT/docs/adr/$f" \
    || { echo "FAIL: docs/adr/$f: needs the line 'Status: Accepted'"; fails=1; }
  i=$((i + 1))
done
max="$(printf '%04d' $((i - 1)))"
while IFS= read -r ref; do
  n="$(grep -oE '[0-9]{4}' <<< "$ref")"
  ls "$ROOT/docs/adr/$n-"*.md > /dev/null 2>&1 \
    || { echo "FAIL: reference '$ref' names no ADR (highest is $max)"; fails=1; }
done < <(git -C "$ROOT" grep -ohE 'ADR[ _-]?[0-9]{4}' -- ':(exclude,glob)docs/superpowers/**' | sort -u)
while IFS= read -r ref; do
  [ -f "$ROOT/docs/$ref" ] || { echo "FAIL: path reference '$ref' does not exist"; fails=1; }
done < <(git -C "$ROOT" grep -ohE 'adr/[0-9]{4}-[a-z0-9-]+\.md' -- ':(exclude,glob)docs/superpowers/**' | sort -u)
[ "$fails" -eq 0 ] && echo "adr-refs: $((i - 1)) ADRs, gap-free, every reference resolves"
exit "$fails"
