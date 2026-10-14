#!/usr/bin/env bash
# legacy-shims.test.sh — C1 gate: no compatibility shim, "legacy"/
# "deprecated" handling, v1alpha1 reference or internal-version comment in
# the Go, TS and chart sources, except the reviewed allowlist entries in
# .github/tests/legacy-shims.allow (each with a reason). Also self-tests the
# shared allowlist engine (.github/scripts/allowlist-grep.sh).
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENGINE="$ROOT/.github/scripts/allowlist-grep.sh"
fails=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ---- engine fixtures ---------------------------------------------------
FX="$TMP/fx"; mkdir -p "$FX/sub"
git -C "$FX" init -q
printf 'clean\na legacy branch\n' > "$FX/a.go"
printf 'upstream legacy quirk\n' > "$FX/sub/b.go"
printf 'legacy in docs\n' > "$FX/c.md"
git -C "$FX" add -A

expect() { # expect <name> <want-rc> <want-grep-or-empty> <allowlist-body> [-s]
  local name="$1" want="$2" grepfor="$3" body="$4" extra="${5:-}"
  printf '%s\n' "$body" > "$TMP/allow"
  out="$(bash "$ENGINE" -i $extra -p 'legacy' -a "$TMP/allow" -r "$FX" -- '*.go' 2>&1)"; rc=$?
  if [ "$rc" -ne "$want" ]; then echo "FAIL: engine/$name: rc=$rc want $want: $out"; fails=1; return; fi
  if [ -n "$grepfor" ] && ! grep -qF -- "$grepfor" <<< "$out"; then
    echo "FAIL: engine/$name: output lacks '$grepfor': $out"; fails=1
  fi
}
expect "unallowed hit fails" 1 "a.go:2" \
  'sub/*.go :: upstream legacy quirk :: upstream quirk name kept verbatim'
expect "all allowed passes" 0 "" \
  'sub/*.go :: upstream legacy quirk :: upstream quirk name kept verbatim
a.go :: legacy branch :: fixture entry with a long enough reason'
expect "stale entry fails" 1 "stale allowlist entry" \
  'sub/*.go :: upstream legacy quirk :: upstream quirk name kept verbatim
a.go :: legacy branch :: fixture entry with a long enough reason
*.go :: never matches :: fixture entry that covers nothing'
expect "stale ignored with -s" 0 "" \
  'sub/*.go :: upstream legacy quirk :: upstream quirk name kept verbatim
a.go :: legacy branch :: fixture entry with a long enough reason
*.go :: never matches :: fixture entry that covers nothing' -s
expect "short reason fails" 1 "malformed allowlist entry" \
  'sub/*.go :: upstream legacy quirk :: too short
a.go :: legacy branch :: fixture entry with a long enough reason'
expect "glob star crosses slash" 0 "" \
  '*.go :: legacy :: one entry for every Go file in the fixture'
printf 'a legacy line in a new file\n' > "$FX/new.go"   # untracked: never git-added
expect "untracked file is checked" 1 "new.go:1" \
  'sub/*.go :: upstream legacy quirk :: upstream quirk name kept verbatim
a.go :: legacy branch :: fixture entry with a long enough reason'

# ---- live gate ---------------------------------------------------------
# One allowlist serves both patterns, so the two pattern runs skip the
# stale check (-s) and a combined third run enforces it.
SCOPE=('*.go' '*.ts' '*.tsx' 'deploy/helm/**' 'internal/api/openapi.yaml' ':(exclude)*.md')
ALLOW="$ROOT/.github/tests/legacy-shims.allow"
SHIMS='legacy|deprecated|v1alpha1'
# VERS: v0.x not glued to a module path/tag (so controller-runtime@v0.25.0
# and image tags are not hits); v1.1+; pre-vN / pre-1.0; "until|since|before
# vN"; rc.N; and test names carrying a version (TestX_V040Budgets).
VERS='(^|[^0-9A-Za-z.@/-])v0\.[0-9]|\bv1\.[1-9]([^0-9]|$)|\bpre-(v[0-9]|1\.0)|\b(until|since|before) v[0-9]|\brc\.[0-9]|\bTest[A-Za-z0-9_]*V0[0-9]'
out="$(bash "$ENGINE" -i -s -p "$SHIMS" -a "$ALLOW" -r "$ROOT" -- "${SCOPE[@]}" 2>&1)" \
  || { echo "$out"; echo "FAIL: legacy/deprecated/v1alpha1 references outside the allowlist"; fails=1; }
out="$(bash "$ENGINE" -s -p "$VERS" -a "$ALLOW" -r "$ROOT" -- "${SCOPE[@]}" 2>&1)" \
  || { echo "$out"; echo "FAIL: internal version references in code comments"; fails=1; }
out="$(bash "$ENGINE" -i -p "$SHIMS|$VERS" -a "$ALLOW" -r "$ROOT" -- "${SCOPE[@]}" 2>&1)"
if grep -qE 'stale allowlist entry|malformed allowlist entry' <<< "$out"; then
  grep -E 'stale allowlist entry|malformed allowlist entry' <<< "$out"
  echo "FAIL: every allowlist entry must be well formed and cover a live hit"; fails=1
fi

# The screenshot harness stages every key the operator writes into a
# workspace Secret: the keys of generateCredentials' returned map.
keys="$(awk '/^func generateCredentials\(/{f=1} f&&/^}/{exit} f' "$ROOT/internal/runtime/linux/backend.go" \
  | grep -oE '^[[:space:]]*"[a-z0-9.-]+":' | tr -d ' \t":')"
[ -n "$keys" ] || { echo "FAIL: no Secret keys read from generateCredentials in internal/runtime/linux/backend.go"; fails=1; }
for key in $keys; do
  grep -qF "\"$key\"" "$ROOT/web/scripts/screenshots.mjs" \
    || { echo "FAIL: web/scripts/screenshots.mjs stageSecret lacks the $key Secret key"; fails=1; }
done

[ "$fails" -eq 0 ] && echo "legacy-shims: engine fixtures and live gate pass"
exit "$fails"
