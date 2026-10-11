#!/usr/bin/env bash
# e2e-smoke.test.sh — regression coverage for hack/e2e/smoke.sh: the script
# must lint clean, validate its environment (missing and non-numeric),
# emit PASS/FAIL/SKIP lines, exit non-zero on the first failed check, keep
# credentials out of argv, join a relative IdP form action preserving the
# issuer's port, parse headers without a space after the colon, and pass
# the anonymous + authenticated suites against a stubbed HTTP surface
# (a PATH curl shim — no cluster).
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

# ---- 3. numeric validation -----------------------------------------------------
out="$(TCDI_E2E_PORTAL_URL=https://portal.example.test \
  TCDI_E2E_SESSION_HOST=ws-00000000.session.example.test \
  TCDI_E2E_READY_TIMEOUT=soon "$SMOKE" --anon-only 2>&1)" && rc=0 || rc=$?
[ "$rc" != 0 ] || bad "non-numeric timeout should exit non-zero"
case "$out" in
  "FAIL config: TCDI_E2E_READY_TIMEOUT must be a positive integer"*) ;;
  *) bad "numeric validation line wrong: $out" ;;
esac
out="$(TCDI_E2E_PORTAL_URL=https://portal.example.test \
  TCDI_E2E_SESSION_HOST=ws-00000000.session.example.test \
  TCDI_E2E_MAX_WORKSPACES=0 "$SMOKE" --anon-only 2>&1)" && rc=0 || rc=$?
[ "$rc" != 0 ] || bad "zero MAX_WORKSPACES should exit non-zero"
case "$out" in "FAIL config: TCDI_E2E_MAX_WORKSPACES"*) ;; *) bad "zero-count validation wrong: $out" ;; esac
ok "numeric validation"

# ---- 4. unreachable portal fails fast ------------------------------------------
out="$(TCDI_E2E_PORTAL_URL=https://127.0.0.1:1 TCDI_E2E_SESSION_HOST=ws-00000000.example.test \
  TCDI_E2E_HTTP_TIMEOUT=3 "$SMOKE" --anon-only 2>&1)" && rc=0 || rc=$?
[ "$rc" != 0 ] || bad "unreachable portal should exit non-zero"
case "$out" in FAIL\ portal-root*) ;; *) bad "expected FAIL portal-root, got: $out" ;; esac
ok "first-check failure exits non-zero"

# ---- the stub -------------------------------------------------------------------
# One shim answers the whole surface: portal (anonymous + cookie-bearing),
# session hosts, and an IdP on a non-443 port whose login form action is
# root-relative — the join must keep the port. Auth state, workspace phases
# and the argv log live in $STUB_DIR so sequential curl processes share it.
mkdir -p "$D/shim" "$D/stub"
cat > "$D/shim/curl" <<'SHIM'
#!/usr/bin/env bash
SD="${STUB_DIR:?}"
printf '%s\n' "$*" >>"$SD/argv.log" 2>/dev/null || true
body=""; hdrs=""; wfmt=""; url=""; method=GET; jar=0
while [ $# -gt 0 ]; do
  case "$1" in
    -o) body="$2"; shift 2 ;;
    -D) hdrs="$2"; shift 2 ;;
    -w) wfmt="$2"; shift 2 ;;
    -X) method="$2"; shift 2 ;;
    -b) jar=1; shift 2 ;;
    -c) shift 2 ;;
    -H | -d | --cacert | --resolve | --max-time | --data-urlencode) shift 2 ;;
    -sS) shift ;;
    http*) url="$1"; shift ;;
    *) shift ;;
  esac
done
state() { [ -f "$SD/state" ] && cat "$SD/state" || echo out; }
auth() { [ "$jar" = 1 ] && [ "$(state)" = in ]; }
code=404; rbody='{"code":"NOT_FOUND","message":"x","retryable":false,"requestId":"req_stub"}'
redir=""; sec=0
case "$url" in
  https://portal.example.test/) code=200; sec=1 ;;
  https://portal.example.test/healthz | https://portal.example.test/readyz) code=200; rbody='ok' ;;
  https://portal.example.test/v1/login)
    code=302
    redir="https://idp.example.test:8443/realms/x/protocol/openid-connect/auth?client_id=tinycdi&state=s1" ;;
  https://idp.example.test:8443/realms/x/protocol/openid-connect/auth*)
    code=200
    rbody='<html><body><form id="kc-form-login" method="post" action="/realms/x/login-actions/authenticate?session_code=a&amp;execution=b&amp;client_id=tinycdi&amp;tab_id=t"><input name="username"><input name="password"></form></body></html>' ;;
  https://idp.example.test:8443/realms/x/login-actions/*)
    code=302; redir="https://portal.example.test/v1/auth/callback?code=c1&state=s1" ;;
  https://portal.example.test/v1/auth/callback*)
    code=302; redir="/"; echo in >"$SD/state" ;;
  https://portal.example.test/v1/session)
    code=200
    if auth; then rbody='{"authenticated":true}'; else rbody='{"authenticated":false}'; fi ;;
  https://portal.example.test/v1/me)
    if auth; then
      code=200
      rbody='{"subject":"u1","displayName":"Stub","tenant":"tenant-a","roles":["user"],"csrfToken":"csrf-stub","sessionDomain":"session.example.test"}'
    else code=401; rbody='{"code":"UNAUTHENTICATED","message":"x","retryable":false,"requestId":"req_stub"}'; fi ;;
  https://portal.example.test/v1/templates)
    if auth; then code=200; rbody='{"items":[{"id":"tpl_stub001","name":"browser"}]}'
    else code=401; rbody='{"code":"UNAUTHENTICATED","message":"x","retryable":false,"requestId":"req_stub"}'; fi ;;
  https://portal.example.test/v1/metrics) code=404 ;;
  https://portal.example.test/v1/quota)
    code=200
    rbody='{"tenant":"tenant-a","configured":true,"limits":{"runningWorkspaces":'"${STUB_TENANTLIMIT:-2}"',"cpuMillicores":4000,"memoryMib":8192,"storageGib":20},"usage":{"workspaces":0,"runningWorkspaces":0,"cpuMillicores":0,"memoryMib":0,"storageGib":0},"users":[]'
    [ -z "${STUB_USERLIMIT:-}" ] || rbody+=',"userLimits":{"runningWorkspaces":'"${STUB_USERLIMIT}"',"cpuMillicores":0,"memoryMib":0,"storageGib":0}'
    rbody+='}' ;;
  https://portal.example.test/v1/workspaces)
    n=$(( $(wc -l <"$SD/created" 2>/dev/null || echo 0) + 1 ))
    if [ -n "${STUB_NOREFUSE:-}" ] || [ "$n" -le 2 ]; then
      code=201
      id="ws_stub000$n"
      echo "$id" >>"$SD/created"
      rbody="{\"id\":\"$id\",\"phase\":\"Ready\",\"desiredState\":\"Running\"}"
    else
      code=409
      rbody='{"code":"QUOTA_EXHAUSTED","message":"x","retryable":false,"requestId":"req_stub"'
      [ -z "${STUB_REASON:-}" ] || rbody+=",\"details\":{\"reason\":\"$STUB_REASON\"}"
      rbody+='}'
    fi ;;
  https://portal.example.test/v1/workspaces/*/stop)
    echo Stopped >"$SD/phase"; code=200; rbody='{"phase":"Stopped"}' ;;
  https://portal.example.test/v1/workspaces/*/start)
    echo Ready >"$SD/phase"; code=200; rbody='{"phase":"Ready"}' ;;
  https://portal.example.test/v1/workspaces/ws_*)
    id="${url##*/v1/workspaces/}"
    if [ "$method" = DELETE ]; then
      echo "$id" >>"$SD/deleted"
      code=202; rbody='{"phase":"Terminating"}'
    elif grep -qx "$id" "$SD/deleted" 2>/dev/null; then
      code=404; rbody='{"code":"NOT_FOUND","message":"x","retryable":false,"requestId":"req_stub"}'
    else
      code=200; phase="$(cat "$SD/phase" 2>/dev/null || echo Ready)"
      rbody="{\"id\":\"$id\",\"phase\":\"$phase\",\"desiredState\":\"Running\"}"
    fi ;;
  https://portal.example.test/v1/logout) code=204; echo out >"$SD/state"; rbody='' ;;
  https://portal.example.test/v1/me/sessions:revoke-all) code=204; echo out >"$SD/state"; rbody='' ;;
  https://ws-00000000.session.example.test/) code=401; rbody='{"error":"unauthorized"}' ;;
  https://ws-00000000.session.example.test/metrics) code=404; rbody='{"error":"not_found"}' ;;
  https://e2e-foreign.session.example.test/) code="${STUB_FOREIGN_CODE:-421}"; rbody='{"error":"bad_host"}' ;;
esac
[ -n "$body" ] && printf '%s' "$rbody" >"$body"
if [ -n "$hdrs" ]; then
  {
    printf 'HTTP/2 %s\r\n' "$code"
    [ -z "$redir" ] || printf 'location: %s\r\n' "$redir"
    if [ "$sec" = 1 ]; then
      # Deliberately no space after the colon: hdr() must still parse.
      printf 'content-security-policy:default-src '"'"'self'"'"'; frame-ancestors '"'"'none'"'"'\r\n'
      printf 'x-frame-options:DENY\r\n'
      printf 'cross-origin-opener-policy:same-origin\r\n'
      printf 'referrer-policy:strict-origin\r\n'
      printf 'x-content-type-options:nosniff\r\n'
      printf 'strict-transport-security:max-age=63072000; includeSubDomains\r\n'
    fi
  } >"$hdrs"
fi
case "$wfmt" in
  *'%{http_code} %{redirect_url}'*) printf '%s %s' "$code" "$redir" ;;
  *'%{http_code}'*) printf '%s' "$code" ;;
  *'%{redirect_url}'*) printf '%s' "$redir" ;;
esac
exit 0
SHIM
chmod +x "$D/shim/curl"
: > "$D/stub/argv.log"; echo Ready > "$D/stub/phase"

# ---- 5. anonymous suite against the stub ----------------------------------------
out="$(PATH="$D/shim:$PATH" STUB_DIR="$D/stub" \
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
ok "anonymous suite (stubbed, no-space headers)"

# ---- 6. a wrong answer fails the check -----------------------------------------
out="$(PATH="$D/shim:$PATH" STUB_DIR="$D/stub" \
  TCDI_E2E_PORTAL_URL=https://portal.example.test \
  TCDI_E2E_SESSION_HOST=ws-00000000.session.example.test \
  STUB_FOREIGN_CODE=200 \
  "$SMOKE" --anon-only 2>&1)" && rc=0 || rc=$?
[ "$rc" != 0 ] || bad "foreign session host should fail the suite"
case "$out" in *FAIL\ session-host-foreign-421*) ;; *) bad "expected FAIL session-host-foreign-421, got: $out" ;; esac
ok "unexpected session-host code fails"

# ---- 7. full authenticated flow: login (relative action, port), lifecycle, -------
# ---- quota refusal, logout — plus the password never appears in argv. -----------
rm -f "$D/stub/state" "$D/stub/created" "$D/stub/deleted"
out="$(PATH="$D/shim:$PATH" STUB_DIR="$D/stub" \
  TCDI_E2E_PORTAL_URL=https://portal.example.test \
  TCDI_E2E_SESSION_HOST=ws-00000000.session.example.test \
  TCDI_E2E_IDP_ISSUER=https://idp.example.test:8443/realms/x \
  TCDI_E2E_USER=e2e-user TCDI_E2E_PASSWORD='s3cr3t-p4ss' \
  TCDI_E2E_TEMPLATE=browser \
  "$SMOKE" 2>&1)" && rc=0 || rc=$?
[ "$rc" = 0 ] || bad "full flow should pass under the stub (rc=$rc): $out"
for want in login me-authenticated templates workspace-create workspace-ready \
  workspace-stop workspace-start quota-running-limit workspace-delete logout; do
  grep -q "^PASS $want" <<<"$out" || bad "missing 'PASS $want' in: $out"
done
grep -q '^FAIL ' <<<"$out" && bad "unexpected FAIL line in: $out"
grep -qF 's3cr3t-p4ss' "$D/stub/argv.log" \
  && bad "password leaked into curl argv: $(grep -nF 's3cr3t-p4ss' "$D/stub/argv.log")"
grep -q 'idp.example.test:8443/realms/x/login-actions' "$D/stub/argv.log" \
  || bad "relative form action not joined to the issuer's :8443 origin"
ok "authenticated flow + no credential in argv + port-preserving action join"

# ---- 8. per-principal limit configured but tenant quota bound first -> SKIP ------
# userLimits.runningWorkspaces=5 > the 409 refusal the tenant quota produces on
# the 3rd create: a plain (no-details) over-limit is a documented alternate.
full_env() {
  env PATH="$D/shim:$PATH" STUB_DIR="$D/stub" \
    TCDI_E2E_PORTAL_URL=https://portal.example.test \
    TCDI_E2E_SESSION_HOST=ws-00000000.session.example.test \
    TCDI_E2E_IDP_ISSUER=https://idp.example.test:8443/realms/x \
    TCDI_E2E_USER=e2e-user TCDI_E2E_PASSWORD='s3cr3t-p4ss' "$@"
}
rm -f "$D/stub/state" "$D/stub/created" "$D/stub/deleted"; echo Ready >"$D/stub/phase"
out="$(full_env STUB_USERLIMIT=5 "$SMOKE" 2>&1)" && rc=0 || rc=$?
[ "$rc" = 0 ] || bad "user-limit variant should still exit 0 (rc=$rc): $out"
case "$out" in *"SKIP quota-running-limit"*) ;; *) bad "expected SKIP quota-running-limit, got: $out" ;; esac
grep -q '^FAIL quota-running-limit' <<<"$out" && bad "false-FAIL on non-user-limit refusal: $out"
ok "quota plain-tenant refusal + userLimits -> SKIP, not FAIL"

# ---- 9. release_pending is the other documented alternate -> SKIP; unknown -> FAIL --
rm -f "$D/stub/state" "$D/stub/created" "$D/stub/deleted"
out="$(full_env STUB_USERLIMIT=5 STUB_REASON=release_pending "$SMOKE" 2>&1)" && rc=0 || rc=$?
[ "$rc" = 0 ] || bad "release_pending variant should still exit 0 (rc=$rc): $out"
case "$out" in *"SKIP quota-running-limit"*) ;; *) bad "expected SKIP on release_pending, got: $out" ;; esac
rm -f "$D/stub/state" "$D/stub/created" "$D/stub/deleted"
out="$(full_env STUB_USERLIMIT=5 STUB_REASON=bogus_reason "$SMOKE" 2>&1)" && rc=0 || rc=$?
[ "$rc" != 0 ] || bad "an undocumented details.reason should fail"
case "$out" in *"FAIL quota-running-limit: refusal details.reason 'bogus_reason' is undocumented"*) ;; \
  *) bad "expected undocumented-reason FAIL, got: $out" ;; esac
ok "documented alternates SKIP; unknown reason FAILs"

# ---- 10. no-refusal: provable never-bound limit -> FAIL; equal-to-limit -> SKIP ----
# Tenant limit 10 keeps it out of the way; only the per-principal limit binds.
rm -f "$D/stub/state" "$D/stub/created" "$D/stub/deleted"
out="$(full_env STUB_NOREFUSE=1 STUB_TENANTLIMIT=10 STUB_USERLIMIT=2 "$SMOKE" 2>&1)" && rc=0 || rc=$?
[ "$rc" != 0 ] || bad "held above the configured limit must fail"
case "$out" in *"FAIL quota-running-limit: held 4 Running with no refusal — above the configured running limit (per-principal 2)"*) ;; \
  *) bad "expected never-bound FAIL, got: $out" ;; esac
rm -f "$D/stub/state" "$D/stub/created" "$D/stub/deleted"
out="$(full_env STUB_NOREFUSE=1 STUB_TENANTLIMIT=10 STUB_USERLIMIT=4 "$SMOKE" 2>&1)" && rc=0 || rc=$?
[ "$rc" = 0 ] || bad "held==limit is a legitimate SKIP (rc=$rc): $out"
case "$out" in *"SKIP quota-running-limit: no refusal holding 4 Running; the binding create is limit+1"*) ;; \
  *) bad "expected no-refusal SKIP with the limit+1 rationale, got: $out" ;; esac
# Tenant-side provable violation is caught too.
rm -f "$D/stub/state" "$D/stub/created" "$D/stub/deleted"
out="$(full_env STUB_NOREFUSE=1 "$SMOKE" 2>&1)" && rc=0 || rc=$?
[ "$rc" != 0 ] || bad "held above the tenant limit must fail"
case "$out" in *"FAIL quota-running-limit: held 4 Running with no refusal — above the configured running limit (tenant 2)"*) ;; \
  *) bad "expected tenant never-bound FAIL, got: $out" ;; esac
ok "never-bound limit FAILs; budget-bound SKIP states why"

[ "$fails" = 0 ] || exit 1
echo "e2e-smoke.test.sh: all cases passed"
