#!/usr/bin/env bash
# workflow-expressions.test.sh — regression test for FX-R16: a workflow
# value holding GitHub expression syntax outside ${{ }} is passed through
# as literal text (the runtime train's first publish run died on
# `outputs: format('type=registry,...', env.IMAGE_REF)`).
# check-workflow-expressions.sh must flag every such value, leave the
# legitimate free-text keys alone, and every real workflow must be clean.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHECK="$ROOT/.github/scripts/check-workflow-expressions.sh"
fails=0

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

expect() { # expect <desc> <want-rc> <fixture-body>
  local desc="$1" want="$2" body="$3" out rc
  printf '%s\n' "$body" > "$WORK/wf.yml"
  out="$(bash "$CHECK" "$WORK/wf.yml" 2>&1)"; rc=$?
  if [ "$rc" -ne "$want" ]; then
    echo "FAIL: $desc (rc=$rc, want $want)"; echo "$out" | sed 's/^/    /'
    fails=1
  fi
}

# --- the exact pre-fix shape: folded scalar, bare format(...) ----------
expect "bare format() in a folded with: scalar is flagged" 1 \
"jobs:
  b:
    steps:
      - uses: docker/build-push-action@x
        with:
          outputs: >-
            format('type=registry,name={0},push-by-digest=true', env.IMAGE_REF)
          labels: \${{ env.OCI_LABELS }}"

expect "bare env. in a literal | with: scalar is flagged" 1 \
"jobs:
  b:
    steps:
      - uses: x/y@z
        with:
          path: |
            out/
            env.OUT_DIR"

for ctx in github.sha env.X inputs.platforms matrix.image steps.build.outputs.digest needs.meta.outputs.t secrets.T vars.V; do
  expect "bare $ctx as an inline with: value is flagged" 1 \
"jobs:
  b:
    steps:
      - uses: x/y@z
        with:
          thing: $ctx"
done

expect "bare hashFiles()/contains() in an env: value is flagged" 1 \
"jobs:
  b:
    env:
      A: hashFiles('x')
    steps:
      - run: echo"

# --- must NOT be flagged ------------------------------------------------
expect "wrapped single-line expression is clean" 0 \
"jobs:
  b:
    steps:
      - uses: x/y@z
        with:
          outputs: \${{ format('type=registry,name={0}', env.IMAGE_REF) }}"

expect "wrapped multi-line folded expression is clean" 0 \
"jobs:
  b:
    steps:
      - uses: x/y@z
        with:
          outputs: >-
            \${{ env.PUBLISH == 'true'
                && format('type=registry,name={0}', env.IMAGE_REF)
                || 'type=docker,dest=/tmp/x.tar' }}"

expect "if:, run:, name: and description: carry free text" 0 \
"jobs:
  b:
    name: scan \${{ matrix.image }} (env.X in prose)
    if: >-
      always()
      && env.SCAN_MODE == 'registry'
    steps:
      - name: use steps.build.outputs.digest
        run: |
          echo format(a) \$GITHUB_ENV env.X steps.y
      - uses: x/y@z
        with:
          url: https://github.com/tinyorbitvn/tinycdi/blob/main/NOTICE
          image: ghcr.io/tinyorbitvn/tinycdi-browser
on:
  workflow_dispatch:
    inputs:
      platforms:
        description: >-
          Platforms to build; see inputs.platforms and env.X in the docs."

expect "comments are ignored" 0 \
"jobs:
  b:
    steps:
      - uses: x/y@z
        with:
          # SEC-16: no cache — env.GHA_CACHE would be wrong here
          context: .   # not github.workspace"

# --- the real workflows (all of them) must be clean ----------------------
if ! out="$(bash "$CHECK" 2>&1)"; then
  echo "FAIL: a workflow under .github/workflows has expression syntax outside \${{ }}"
  echo "$out" | sed 's/^/    /'
  fails=1
fi

# the default scan must really cover the workflows (guards a glob typo)
n="$(ls "$ROOT"/.github/workflows/*.yml | wc -l)"
[ "$n" -ge 5 ] || { echo "FAIL: only $n workflows found"; fails=1; }

[ "$fails" -eq 0 ] && echo "workflow-expressions: ok"
exit "$fails"
