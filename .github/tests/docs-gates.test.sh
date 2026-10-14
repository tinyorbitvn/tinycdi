#!/usr/bin/env bash
# docs-gates.test.sh — the docs gates: (1) every relative Markdown link
# resolves, anchors included; (2) nothing outside the planning folder
# (docs/superpowers/) refers into it, because that folder is removed before
# release; (3) the published docs carry no internal version history or PR
# numbers (check-doc-versions.sh, allowlist .github/tests/docs-versions.allow).
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
LINKS="$ROOT/.github/scripts/check-doc-links.py"
VERSIONS="$ROOT/.github/scripts/check-doc-versions.sh"
SP_DIR="docs/superpowers"
fails=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ---- link checker fixtures ---------------------------------------------
FX="$TMP/links"; mkdir -p "$FX/docs/sub" "$FX/$SP_DIR"
git -C "$FX" init -q
cat > "$FX/docs/a.md" <<'MD'
# Title — with dash
## Same
## Same
## `code` heading
[ok](sub/b.md#section-one) [ok2](#same-1) [ok3](#title--with-dash) [ok4](#code-heading)
[bad file](sub/missing.md) [bad anchor](sub/b.md#nope)
[fenced-off](superpowers/plan.md) [web](https://example.com/x#y)
~~~
[inside a fence](nowhere.md)
~~~
`[inside a code span](nowhere.md)`
[ref]: sub/b.md#section-one
MD
printf '# B\n## Section one\n' > "$FX/docs/sub/b.md"
printf '# P\n[back](../a.md)\n' > "$FX/$SP_DIR/plan.md"
git -C "$FX" add -A
out="$(python3 -I "$LINKS" --root "$FX" 2>&1)"; rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL: link fixtures: rc=$rc want 1: $out"; fails=1; }
for want in 'sub/missing.md — docs/sub/missing.md does not exist' \
            'sub/b.md#nope — no heading #nope' \
            "links into $SP_DIR/"; do
  grep -qF -- "$want" <<< "$out" || { echo "FAIL: link fixtures: missing '$want' in: $out"; fails=1; }
done
[ "$(grep -c '^FAIL:' <<< "$out")" -eq 3 ] \
  || { echo "FAIL: link fixtures: want exactly 3 findings (valid links, fences, code spans and URLs pass): $out"; fails=1; }

# ---- planning-folder fence ---------------------------------------------
# A reference into $SP_DIR/ from outside it breaks when the folder is
# removed. A pathspec that EXCLUDES the folder (":(exclude,glob)$SP_DIR/**",
# ":!$SP_DIR/**", rg's "!$SP_DIR/**") is not a reference: it is stripped
# before the check. Untracked files are searched too, so a new file is
# caught before it is committed.
sp_refs() { # sp_refs <repo root> -> offending lines, empty when clean
  git -C "$1" grep -n -I --untracked -e "$SP_DIR" -- ":(exclude,glob)$SP_DIR/**" \
    ':(exclude).github/tests/docs-gates.test.sh' ':(exclude).github/scripts/check-doc-links.py' \
    | sed -E 's#(:\(exclude[^)]*\)|:?!)docs/superpowers/\*\*##g' | grep -F -- "$SP_DIR"
}
mkdir -p "$FX/tools"
printf '%s\n' "git grep -e x -- ':(exclude,glob)$SP_DIR/**' ':!$SP_DIR/**'" "rg x -g '!$SP_DIR/**'" > "$FX/tools/excludes.sh"
printf '%s\n' "cat $SP_DIR/plan.md" > "$FX/tools/reads.sh"
out="$(sp_refs "$FX")"
grep -q '^tools/reads.sh:1:' <<< "$out" || { echo "FAIL: fence fixtures: a real reference is not caught: $out"; fails=1; }
grep -q '^tools/excludes.sh:' <<< "$out" && { echo "FAIL: fence fixtures: a pathspec exclusion is reported: $out"; fails=1; }

# ---- version gate fixtures ---------------------------------------------
FV="$TMP/vers"; mkdir -p "$FV/docs"
git -C "$FV" init -q
cat > "$FV/docs/x.md" <<'MD'
Since v0.3 the budget doubled.
See PR #117 for details.
Tag v1.0.0-rc.1 was cut.
The CRD is v1alpha1.
Upgrade from v1.2 first.
kind v0.33.0 or newer
[tags](#10-finding-tag-index) and color #123456 and TLSv1.2 and v1.0.0-rc.N
MD
git -C "$FV" add -A
printf '%s\n' 'docs/x.md :: kind v0\.33\.0 :: third-party tool version (kind), not TinyCDI history' > "$TMP/allow"
out="$(TCDI_DOCS_ROOT="$FV" TCDI_DOCS_ALLOW="$TMP/allow" bash "$VERSIONS" 2>&1)"; rc=$?
[ "$rc" -eq 1 ] || { echo "FAIL: version fixtures: rc=$rc want 1: $out"; fails=1; }
for n in 1 2 3 4 5; do
  grep -q "^FAIL: docs/x.md:$n:" <<< "$out" || { echo "FAIL: version fixtures: line $n not flagged: $out"; fails=1; }
done
for n in 6 7; do
  grep -q "^FAIL: docs/x.md:$n:" <<< "$out" && { echo "FAIL: version fixtures: line $n must pass: $out"; fails=1; }
done

# ---- live: links ---------------------------------------------------------
out="$(python3 -I "$LINKS" --root "$ROOT" 2>&1)" || { echo "$out"; echo "FAIL: broken documentation links"; fails=1; }

# ---- live: nothing outside the planning folder references it -------------
refs="$(sp_refs "$ROOT")"
[ -z "$refs" ] || { echo "$refs"; echo "FAIL: references into $SP_DIR/ outside it (it is removed before release)"; fails=1; }

[ "$fails" -eq 0 ] && echo "docs-gates: fixtures, links and the $SP_DIR fence pass"
exit "$fails"
