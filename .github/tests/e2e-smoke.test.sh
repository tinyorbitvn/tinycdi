#!/usr/bin/env bash
# e2e-smoke.test.sh — regression coverage for hack/e2e/smoke.sh: the script
# must lint clean, validate its environment, emit PASS/FAIL lines, exit
# non-zero on the first failed check, and pass the anonymous suite against
# a stubbed HTTP surface (a PATH curl shim — no cluster).
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SMOKE="$ROOT/hack/e2e/smoke.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
fails=0

bad() { echo "FAIL: $1"; fails=1; }
ok() { echo "ok: $1"; }

[ -x "$SMOKE" ] || bad "smoke.sh missing or not executable"

# ---- 1. lint ------------------------------------------------------------------
bash -n "$SMOKE" || bad "bash -n"
command -v shellcheck >/dev/null 2>&1 || bad "shellcheck not installed"
shellcheck -x "$SMOKE" || bad "shellcheck"
ok "lint"

# ---- 2. env validation ---------------------------------------------------------
out="$(env -u TCDI_E2E_PORTAL_URL -u TCDI_E2E_SESSION_HOST "$SMOKE" 2>&1)" && rc=0 || rc=$?
[ "$rc" != 0 ] || bad "missing env should exit non-zero"
case "$out" in
  *"FAIL config: missing env: TCDI_E2E_PORTAL_URL,TCDI_E2E_SESSION_HOST"*) ;;
  *) bad "missing-env line wrong: $out" ;;
esac
out="$(TCDI_E2E_PORTAL_URL=https://portal.example.test \
  TCDI_E2E_SESSION_HOST=ws-00000000.session.example.test "$SMOKE" 2>&1)" && rc=0 || rc=$?
case "$out" in
  *"TCDI_E2E_IDP_ISSUER"*TCDI_E2E_USER*TCDI_E2E_PASSWORD*) ;;
  *) bad "auth-env validation wrong: $out" ;;
esac
ok "env validation"

# ---- 3. unreachable portal fails fast ------------------------------------------
out="$(TCDI_E2E_PORTAL_URL=https://127.0.0.1:1 TCDI_E2E_SESSION_HOST=ws-00000000.example.test \
  TCDI_E2E_HTTP_TIMEOUT=3 "$SMOKE" --anon-only 2>&1)" && rc=0 || rc=$?
[ "$rc" != 0 ] || bad "unreachable portal should exit non-zero"
case "$out" in FAIL\ portal-root*) ;; *) bad "expected FAIL portal-root, got: $out" ;; esac
ok "first-check failure exits non-zero"

# ---- 4. anonymous suite against a stubbed surface -------------------------------
mkdir -p "$D/shim"
cat > "$D/shim/curl" <<'SHIM'
#!/usr/bin/env bash
# Stub curl for hack/e2e/smoke.sh --anon-only: answers the canned
# portal/session-host surface by request URL.
body=""; hdrs=""; wfmt=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) body="$2"; shift 2 ;;
    -D) hdrs="$2"; shift 2 ;;
    -w) wfmt="$2"; shift 2 ;;
    -X | -H | -d | -b | -c | --cacert | --resolve | --max-time | --data-urlencode) shift 2 ;;
    -sS) shift ;;
    http*) url="$1"; shift ;;
    *) shift ;;
  esac
done
code=404; rbody='{"code":"NOT_FOUND","message":"x","retryable":false,"requestId":"req_stub"}'
sec=0
case "$url" in
  https://portal.example.test/) code=200; sec=1 ;;
  https://portal.example.test/healthz | https://portal.example.test/readyz) code=200; rbody='ok' ;;
  https://portal.example.test/v1/session) code=200; rbody='{"authenticated":false}' ;;
  https://portal.example.test/v1/me | https://portal.example.test/v1/templates)
    code=401; rbody='{"code":"UNAUTHENTICATED","message":"x","retryable":false,"requestId":"req_stub"}' ;;
  https://portal.example.test/v1/metrics) code=404 ;;
  https://ws-00000000.session.example.test/) code=401; rbody='{"error":"unauthorized"}' ;;
  https://ws-00000000.session.example.test/metrics) code=404; rbody='{"error":"not_found"}' ;;
  https://e2e-foreign.session.example.test/) code="${STUB_FOREIGN_CODE:-421}"; rbody='{"error":"bad_host"}' ;;
esac
[ -n "$body" ] && printf '%s' "$rbody" >"$body"
if [ -n "$hdrs" ]; then
  {
    printf 'HTTP/2 %s\r\n' "$code"
    if [ "$sec" = 1 ]; then
      printf 'content-security-policy: default-src '"'"'self'"'"'; frame-ancestors '"'"'none'"'"'\r\n'
      printf 'x-frame-options: DENY\r\n'
      printf 'cross-origin-opener-policy: same-origin\r\n'
      printf 'referrer-policy: strict-origin\r\n'
      printf 'x-content-type-options: nosniff\r\n'
      printf 'strict-transport-security: max-age=63072000; includeSubDomains\r\n'
    fi
  } >"$hdrs"
fi
case "$wfmt" in
  *'%{http_code} %{redirect_url}'*) printf '%s ' "$code" ;;
  *'%{http_code}'*) printf '%s' "$code" ;;
  *'%{redirect_url}'*) printf '' ;;
esac
exit 0
SHIM
chmod +x "$D/shim/curl"

out="$(PATH="$D/shim:$PATH" \
  TCDI_E2E_PORTAL_URL=https://portal.example.test \
  TCDI_E2E_SESSION_HOST=ws-00000000.session.example.test \
  "$SMOKE" --anon-only 2>&1)" && rc=0 || rc=$?
[ "$rc" = 0 ] || bad "anon suite should pass under the stub (rc=$rc): $out"
for want in portal-root security-headers healthz readyz session-probe \
  me-unauthenticated templates-unauthenticated metrics-not-public \
  session-host-no-cookie session-host-foreign-421 session-metrics-not-public; do
  grep -q "^PASS $want" <<<"$out" || bad "missing 'PASS $want' in: $out"
done
grep -q '^FAIL ' <<<"$out" && bad "unexpected FAIL line in: $out"
ok "anonymous suite (stubbed)"

# ---- 5. a wrong answer fails the check -----------------------------------------
out="$(PATH="$D/shim:$PATH" \
  TCDI_E2E_PORTAL_URL=https://portal.example.test \
  TCDI_E2E_SESSION_HOST=ws-00000000.session.example.test \
  STUB_FOREIGN_CODE=200 \
  "$SMOKE" --anon-only 2>&1)" && rc=0 || rc=$?
[ "$rc" != 0 ] || bad "foreign session host should fail the suite"
case "$out" in *FAIL\ session-host-foreign-421*) ;; *) bad "expected FAIL session-host-foreign-421, got: $out" ;; esac
ok "unexpected session-host code fails"

[ "$fails" = 0 ] || exit 1
echo "e2e-smoke.test.sh: all cases passed"
