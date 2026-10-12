#!/usr/bin/env bash
# quickstart-env.test.sh — regression coverage for the env file up.sh
# writes at install time and the smoke specs source verbatim
# (`set -a; . "$STATE_DIR/env"`). upgrade-test.sh sets
# TCDI_QS_CLIPBOARD_ROUNDTRIP_OPTOUT to a multi-word reason; an unquoted
# KEY=value line makes the source run the tail words as a command (the
# upgrade job's exit-127 failure). common.sh's qs_env_kv is the
# serializer — pin its quoting contract and up.sh's use of it.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

bad() { echo "FAIL: $1"; fails=1; }
ok() { echo "ok: $1"; }

# common.sh defines qs_env_kv (sourcing it only sets quickstart defaults —
# nothing executes a workload).
# shellcheck source=/dev/null
. "$ROOT/hack/quickstart/common.sh"

[ "$(declare -F qs_env_kv)" = "qs_env_kv" ] || bad "common.sh must define qs_env_kv"

# ---- 1. serializer round-trip ---------------------------------------------
# A value that breaks unquoted serialization — whitespace, quotes, dollar
# signs and backticks — must source back to the exact same string.
for v in \
  "v0.3.1 runtime train predates the entrypoint clipboard render" \
  'a "quoted" $dollar `backtick` ; semi' \
  "" \
  "single"; do
  f="$D/env-$(printf %s "$v" | sha256sum | cut -c1-8)"
  qs_env_kv TCDI_QS_CLIPBOARD_ROUNDTRIP_OPTOUT "$v" >"$f"
  ( set -a; . "$f"; set +a; [ "$TCDI_QS_CLIPBOARD_ROUNDTRIP_OPTOUT" = "$v" ] ) \
    || bad "qs_env_kv output does not round-trip value: $v"
done
ok "qs_env_kv round-trips arbitrary values through 'set -a; . env'"

# ---- 2. the failure mode being guarded ------------------------------------
# Prove the guard matters: the same value written UNQUOTED must not source
# cleanly — the tail words run as a command. The check runs in a child
# bash under its own set -e: `-e` is inert inside if/||/&& contexts, and a
# plain `.` only reports the file's LAST line (the trailing CLEANUP=ok
# would mask the bad one).
printf 'TCDI_QS_CLIPBOARD_ROUNDTRIP_OPTOUT=v0.3.1 runtime train predates the entrypoint clipboard render\nCLEANUP=ok\n' \
  >"$D/bad-env"
if bash -c 'set -ea; . "$1"' _ "$D/bad-env" 2>/dev/null; then
  bad "unquoted multi-word env line sourced cleanly — the regression cannot come back"
else
  ok "unquoted multi-word env line fails to source (the bug qs_env_kv prevents)"
fi

# ---- 3. wiring: up.sh serializes + self-checks -----------------------------
grep -q 'qs_env_kv TCDI_QS_CLIPBOARD_ROUNDTRIP_OPTOUT' "$ROOT/hack/quickstart/up.sh" \
  || bad "up.sh must serialize TCDI_QS_CLIPBOARD_ROUNDTRIP_OPTOUT via qs_env_kv"
grep -q "bash -c 'set -ea" "$ROOT/hack/quickstart/up.sh" \
  || bad "up.sh must source-check the generated env file under set -e before finishing"
ok "up.sh serializes the opt-out and self-checks the env file"

# ---- 4. upgrade-test.sh passes the named opt-out ---------------------------
grep -q 'TCDI_QS_CLIPBOARD_ROUNDTRIP_OPTOUT=' "$ROOT/hack/quickstart/upgrade-test.sh" \
  || bad "upgrade-test.sh must set the named clipboard round-trip opt-out for the previous-release install"
ok "upgrade-test.sh sets the named opt-out"

# ---- 5. lint ----------------------------------------------------------------
bash -n "$ROOT/hack/quickstart/common.sh" || bad "bash -n common.sh"
bash -n "$ROOT/hack/quickstart/up.sh" || bad "bash -n up.sh"
command -v shellcheck >/dev/null 2>&1 || bad "shellcheck not installed"
shellcheck -x "$ROOT/hack/quickstart/common.sh" "$ROOT/hack/quickstart/up.sh" \
  || bad "shellcheck"
ok "lint"

echo
if [ "$fails" = 0 ]; then echo "quickstart-env.test.sh: PASS"; else echo "quickstart-env.test.sh: FAIL"; fi
exit "$fails"
