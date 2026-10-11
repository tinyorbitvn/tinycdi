#!/usr/bin/env bash
# smoke.sh — env-driven API-level e2e smoke for a TinyCDI release candidate.
#
#   hack/e2e/smoke.sh                      # anonymous + authenticated checks
#   hack/e2e/smoke.sh --anon-only          # anonymous checks only (no login)
#   hack/e2e/smoke.sh --sign-out-everywhere
#
# Pointed at a deployment purely through the environment — nothing about a
# cluster, namespace or kubeconfig is needed. Bash + curl + jq, matching the
# hack/ toolchain: no build step and no new dependencies, and curl gives the
# precise TLS/redirect control the scripted OIDC code flow needs. Prints one
# "PASS|FAIL|SKIP <check>[: reason]" line per check and exits non-zero on the
# first failure. Credentials, cookies, CSRF tokens and launch material are
# never printed; error reasons carry only the API's error code/requestId.
#
# Environment:
#   TCDI_E2E_PORTAL_URL     portal base URL, e.g. https://portal.example.test   (required)
#   TCDI_E2E_SESSION_HOST   a workspace session host name — any well-formed
#                           ws-<suffix>.<session-domain> name; no workspace
#                           needs to exist for it                                    (required)
#   TCDI_E2E_IDP_ISSUER     expected OIDC issuer URL (its host must be where
#                           /v1/login redirects)                       (required unless --anon-only)
#   TCDI_E2E_USER           test user login                            (required unless --anon-only)
#   TCDI_E2E_PASSWORD       test user password                         (required unless --anon-only)
#   TCDI_E2E_TEMPLATE       template name to instantiate               (default: browser)
#   TCDI_E2E_CA_FILE        CA bundle for TLS verification             (default: system roots)
#   TCDI_E2E_RESOLVE        comma-separated curl --resolve entries
#                           (host:port:addr) for hosts without public DNS
#   TCDI_E2E_HTTP_TIMEOUT   per-request timeout, seconds               (default: 15)
#   TCDI_E2E_READY_TIMEOUT  workspace Ready deadline, seconds          (default: 600)
#   TCDI_E2E_DELETE_TIMEOUT workspace delete deadline, seconds         (default: 300)
#   TCDI_E2E_MAX_WORKSPACES cap on concurrently Running workspaces the
#                           quota check may hold (incl. the lifecycle
#                           one); the check SKIP-s when the limit is
#                           above this probe budget                    (default: 4)
#   TCDI_E2E_NAME_PREFIX    workspace name prefix                      (default: e2e)
#
# The OIDC login is scripted the same way hack/quickstart/restore-drill.sh
# drives the dev realm: GET /v1/login -> IdP login form -> credential POST ->
# portal callback -> session cookie. It needs an IdP that serves an HTML
# username/password form (the quickstart Keycloak realm is exactly that).
set -euo pipefail

# ---- flags -------------------------------------------------------------------
ANON_ONLY=0
SIGN_OUT_EVERYWHERE=0
for arg in "$@"; do
  case "$arg" in
    --anon-only) ANON_ONLY=1 ;;
    --sign-out-everywhere) SIGN_OUT_EVERYWHERE=1 ;;
    -h | --help)
      sed -n '2,40p' "$0"
      exit 0
      ;;
    *) printf 'FAIL config: unknown flag %s\n' "$arg" >&2; exit 1 ;;
  esac
done

# ---- configuration -----------------------------------------------------------
PORTAL="${TCDI_E2E_PORTAL_URL:-}"
SESSION_HOST="${TCDI_E2E_SESSION_HOST:-}"
ISSUER="${TCDI_E2E_IDP_ISSUER:-}"
E2E_USER="${TCDI_E2E_USER:-}"
E2E_PASSWORD="${TCDI_E2E_PASSWORD:-}"
TEMPLATE="${TCDI_E2E_TEMPLATE:-browser}"
CA_FILE="${TCDI_E2E_CA_FILE:-}"
RESOLVE="${TCDI_E2E_RESOLVE:-}"
HTTP_TIMEOUT="${TCDI_E2E_HTTP_TIMEOUT:-15}"
READY_TIMEOUT="${TCDI_E2E_READY_TIMEOUT:-600}"
DELETE_TIMEOUT="${TCDI_E2E_DELETE_TIMEOUT:-300}"
MAX_WORKSPACES="${TCDI_E2E_MAX_WORKSPACES:-4}"
NAME_PREFIX="${TCDI_E2E_NAME_PREFIX:-e2e}"

PORTAL="${PORTAL%/}"
SESSION_HOST="${SESSION_HOST#https://}"; SESSION_HOST="${SESSION_HOST#http://}"; SESSION_HOST="${SESSION_HOST%%/*}"
ISSUER="${ISSUER%/}"

missing=()
[ -n "$PORTAL" ] || missing+=(TCDI_E2E_PORTAL_URL)
[ -n "$SESSION_HOST" ] || missing+=(TCDI_E2E_SESSION_HOST)
if [ "$ANON_ONLY" -eq 0 ]; then
  [ -n "$ISSUER" ] || missing+=(TCDI_E2E_IDP_ISSUER)
  [ -n "$E2E_USER" ] || missing+=(TCDI_E2E_USER)
  [ -n "$E2E_PASSWORD" ] || missing+=(TCDI_E2E_PASSWORD)
fi
if [ "${#missing[@]}" -gt 0 ]; then
  printf 'FAIL config: missing env: %s\n' "$(IFS=,; echo "${missing[*]}")"
  exit 1
fi
case "$PORTAL" in https://*) ;; *) printf 'FAIL config: TCDI_E2E_PORTAL_URL must be an https URL\n'; exit 1 ;; esac
for v in HTTP_TIMEOUT READY_TIMEOUT DELETE_TIMEOUT MAX_WORKSPACES; do
  case "${!v}" in
    '' | *[!0-9]* | 0)
      printf 'FAIL config: TCDI_E2E_%s must be a positive integer (seconds/count), got "%s"\n' "$v" "${!v}"
      exit 1
      ;;
  esac
done

need() { for t in "$@"; do command -v "$t" >/dev/null 2>&1 || { printf 'FAIL config: required tool not on PATH: %s\n' "$t"; exit 1; }; done; }
need curl jq sed awk mktemp dirname

# ---- plumbing ------------------------------------------------------------------
W="$(mktemp -d)"; chmod 700 "$W"
RESP="$W/body"; HDRS="$W/headers"
JAR="$W/portal.jar"; JAR_B="$W/portal-b.jar"
declare -a WS_IDS=()
CHECKS=0
STAMP="$(date +%s)"

TLS_ARGS=(--max-time "$HTTP_TIMEOUT")
[ -z "$CA_FILE" ] || TLS_ARGS+=(--cacert "$CA_FILE")
if [ -n "$RESOLVE" ]; then
  IFS=',' read -ra _rs <<<"$RESOLVE"
  for r in "${_rs[@]}"; do TLS_ARGS+=(--resolve "$r"); done
fi

pass() { CHECKS=$((CHECKS + 1)); printf 'PASS %s\n' "$1"; }
skip() { printf 'SKIP %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; exit 1; }
note() { printf '    %s\n' "$*" >&2; }

cleanup() {
  local id csrf=""
  for id in "${WS_IDS[@]:-}"; do
    [ -n "$id" ] || continue
    if [ -s "$JAR" ]; then
      api "$JAR" GET /v1/me >/dev/null || true
      csrf="$(jq -r '.csrfToken // empty' "$RESP" 2>/dev/null || true)"
      [ -n "$csrf" ] && { api "$JAR" DELETE "/v1/workspaces/$id" -H "X-CSRF-Token: $csrf" >/dev/null || true; }
    fi
  done
  rm -rf "$W"
}
trap cleanup EXIT

# req <url> [curl args...] — body to $RESP, echo the http_code ("000" on transport failure).
req() {
  local url="$1"; shift
  curl -sS "${TLS_ARGS[@]}" -o "$RESP" -w '%{http_code}' "$@" "$url" 2>/dev/null || true
}
# reqh — same, but response headers land in $HDRS.
reqh() {
  local url="$1"; shift
  curl -sS "${TLS_ARGS[@]}" -D "$HDRS" -o "$RESP" -w '%{http_code}' "$@" "$url" 2>/dev/null || true
}
# api <jar> <method> <path> [curl args...] — session-cookie call on the portal.
api() {
  local jar="$1" m="$2" path="$3"; shift 3
  curl -sS "${TLS_ARGS[@]}" -b "$jar" -c "$jar" -X "$m" \
    -o "$RESP" -w '%{http_code}' "$@" "$PORTAL$path" 2>/dev/null || true
}
hdr() {
  if [ -s "$HDRS" ]; then
    awk -v h="$1" 'BEGIN{IGNORECASE=1} index(tolower($0), tolower(h)":")==1 {sub(/\r$/,""); sub(/^[^:]*:[ \t]*/,""); print; exit}' "$HDRS"
  fi
}
host_of() { sed -E 's|^[a-z]+://([^/:?]+).*|\1|' <<<"$1"; }
# origin_of keeps scheme://host[:port] — a non-443 IdP port must survive.
origin_of() { sed -E 's|^([a-z]+://[^/?]+).*|\1|' <<<"$1"; }
# path_of returns the URL path ("" when absent) without query/fragment.
path_of() { sed -E 's|^[a-z]+://[^/]*||; s|[?#].*$||' <<<"$1"; }
err_code() { jq -r '.code // "-"' "$RESP" 2>/dev/null || echo "-"; }

expect_code() { # expect_code <check> <url> <want> [curl args...]
  local check="$1" url="$2" want="$3"; shift 3
  local c; c="$(req "$url" "$@")"
  if [ "$c" = "$want" ]; then pass "$check"; else fail "$check: GET $url -> $c (want $want)"; fi
}

# ---- 1. anonymous surface ------------------------------------------------------
c="$(reqh "$PORTAL/")"
if [ "$c" = "200" ]; then pass "portal-root"; else fail "portal-root: GET / -> $c (want 200)"; fi

missing_hdrs=""
h_csp="$(hdr Content-Security-Policy)"; [[ "$h_csp" == *"default-src 'self'"* ]] || missing_hdrs+="CSP(default-src 'self') "
[ "$(hdr X-Frame-Options)" = "DENY" ] || missing_hdrs+="X-Frame-Options "
[ "$(hdr Cross-Origin-Opener-Policy)" = "same-origin" ] || missing_hdrs+="Cross-Origin-Opener-Policy "
case "$(hdr Referrer-Policy)" in strict-origin | strict-origin-when-cross-origin) ;; *) missing_hdrs+="Referrer-Policy " ;; esac
[ "$(hdr X-Content-Type-Options)" = "nosniff" ] || missing_hdrs+="X-Content-Type-Options "
[[ "$(hdr Strict-Transport-Security)" == *max-age=* ]] || missing_hdrs+="Strict-Transport-Security "
if [ -z "$missing_hdrs" ]; then pass "security-headers"; else fail "security-headers: missing/weak: $missing_hdrs"; fi

expect_code "healthz" "$PORTAL/healthz" 200
expect_code "readyz" "$PORTAL/readyz" 200

c="$(req "$PORTAL/v1/session")"
auth_flag="$(jq -r 'if .authenticated == true then "true" elif .authenticated == false then "false" else "?" end' "$RESP" 2>/dev/null)"
if [ "$c" = "200" ] && [ "$auth_flag" = "false" ]; then
  pass "session-probe"
else
  fail "session-probe: GET /v1/session -> $c authenticated=$auth_flag (want 200 false)"
fi

c="$(req "$PORTAL/v1/me")"
if [ "$c" = "401" ]; then pass "me-unauthenticated"; else fail "me-unauthenticated: GET /v1/me -> $c (want 401)"; fi

c="$(req "$PORTAL/v1/templates")"
if [ "$c" = "401" ]; then pass "templates-unauthenticated"; else fail "templates-unauthenticated: GET /v1/templates -> $c (want 401)"; fi

expect_code "metrics-not-public" "$PORTAL/v1/metrics" 404

# The session listener: a well-formed ws-<suffix> host is the cookie-gated
# desktop proxy (no cookie -> 401); a label that is not a workspace host is
# misdirected (421 bad_host); /metrics does not exist there (404).
# (internal/gateway/proxy.go)
c="$(req "https://$SESSION_HOST/")"
if [ "$c" = "401" ]; then pass "session-host-no-cookie"; else fail "session-host-no-cookie: GET https://$SESSION_HOST/ -> $c (want 401)"; fi

SESSION_DOMAIN="${SESSION_HOST#*.}"
FOREIGN_HOST="e2e-foreign.$SESSION_DOMAIN"
c="$(req "https://$FOREIGN_HOST/")"
if [ "$c" = "421" ]; then pass "session-host-foreign-421"; else fail "session-host-foreign-421: GET https://$FOREIGN_HOST/ -> $c (want 421 bad_host)"; fi

expect_code "session-metrics-not-public" "https://$SESSION_HOST/metrics" 404

if [ "$ANON_ONLY" -eq 1 ]; then
  printf '%d checks passed\n' "$CHECKS"
  exit 0
fi

# ---- 2. scripted OIDC login ----------------------------------------------------
LOGIN_ERR=""
oidc_login() { # oidc_login <jar>: leaves an authenticated session cookie in jar.
  local jar="$1" loc action cb code out
  rm -f "$jar"
  code="$(curl -sS "${TLS_ARGS[@]}" -c "$jar" -D "$HDRS" -o /dev/null \
    -w '%{http_code}' "$PORTAL/v1/login" 2>/dev/null || true)"
  if [ "$code" != "302" ]; then LOGIN_ERR="/v1/login -> $code (want 302)"; return 1; fi
  loc="$(hdr Location)"
  if [ -z "$loc" ]; then LOGIN_ERR="/v1/login answered 302 without Location"; return 1; fi
  if [ "$(host_of "$loc")" != "$(host_of "$ISSUER")" ]; then
    LOGIN_ERR="/v1/login redirects to host $(host_of "$loc"), not the TCDI_E2E_IDP_ISSUER host $(host_of "$ISSUER")"
    return 1
  fi
  # The IdP either shows the credential form (200) or — with a live SSO
  # session — bounces straight back to the portal callback (302).
  out="$(curl -sS "${TLS_ARGS[@]}" -b "$jar" -c "$jar" -o "$W/idp.html" \
    -w '%{http_code} %{redirect_url}' "$loc" 2>/dev/null || true)"
  code="${out%% *}"; cb="${out#* }"; [ "$cb" = "$code" ] && cb=""
  if [ "$code" = "200" ]; then
    action="$(sed -n 's/.*<form[^>]*action="\([^"]*\)".*/\1/p' "$W/idp.html" | head -1 | sed 's/&amp;/\&/g')"
    if [ -z "$action" ]; then LOGIN_ERR="IdP login page carried no form action"; return 1; fi
    case "$action" in
      *://*) ;;                                    # absolute — use as-is
      /*) action="$(origin_of "$loc")$action" ;;   # root-relative — keep scheme+host+port
      *) action="$(origin_of "$loc")$(dirname "$(path_of "$loc")")/$action" ;;
    esac
    # The credentials must never appear in argv: field values come from
    # 0600 files inside the private work dir (removed by the exit trap).
    printf '%s' "$E2E_USER" >"$W/idp-user"; printf '%s' "$E2E_PASSWORD" >"$W/idp-pass"
    chmod 600 "$W/idp-user" "$W/idp-pass"
    cb="$(curl -sS "${TLS_ARGS[@]}" -b "$jar" -c "$jar" -o /dev/null \
      -w '%{redirect_url}' -X POST "$action" \
      --data-urlencode "username@$W/idp-user" \
      --data-urlencode "password@$W/idp-pass" 2>/dev/null || true)"
    if [ -z "$cb" ]; then LOGIN_ERR="IdP credential POST gave no redirect (bad credentials?)"; return 1; fi
  elif [ "$code" != "302" ] && [ "$code" != "303" ]; then
    LOGIN_ERR="IdP login page -> $code (want 200 form or 302)"; return 1
  fi
  # Follow any further IdP redirects until the portal callback shows up.
  for _ in 1 2 3 4 5; do
    case "$cb" in */v1/auth/callback*) break ;; esac
    [ -n "$cb" ] || break
    cb="$(curl -sS "${TLS_ARGS[@]}" -b "$jar" -c "$jar" -o /dev/null \
      -w '%{redirect_url}' "$cb" 2>/dev/null || true)"
  done
  case "$cb" in */v1/auth/callback*) ;; *)
    LOGIN_ERR="login never reached the portal /v1/auth/callback"; return 1 ;;
  esac
  code="$(curl -sS "${TLS_ARGS[@]}" -b "$jar" -c "$jar" -o /dev/null \
    -w '%{http_code}' "$cb" 2>/dev/null || true)"
  case "$code" in 200 | 301 | 302 | 303 | 307 | 308) return 0 ;; esac
  LOGIN_ERR="portal auth callback -> $code (want a redirect into the portal)"
  return 1
}

csrf_of() { api "$1" GET /v1/me >/dev/null && jq -r '.csrfToken // empty' "$RESP"; }

oidc_login "$JAR" || fail "login: $LOGIN_ERR"
c="$(api "$JAR" GET /v1/me)"
[ "$c" = "200" ] || fail "login: session established but GET /v1/me -> $c"
CSRF="$(jq -r '.csrfToken // empty' "$RESP")"
TENANT="$(jq -r '.tenant // empty' "$RESP")"
[ -n "$CSRF" ] || fail "login: /v1/me carried no csrfToken"
pass "login"

sd="$(jq -r '.sessionDomain // empty' "$RESP")"
if [ "$c" = "200" ] && [ -n "$sd" ] && [ -n "$TENANT" ]; then
  pass "me-authenticated"
else
  fail "me-authenticated: /v1/me -> $c, missing sessionDomain/tenant"
fi
[ ".$SESSION_DOMAIN" = ".$sd" ] || [ "$SESSION_DOMAIN" = "$sd" ] \
  || note "me.sessionDomain ($sd) does not match TCDI_E2E_SESSION_HOST's domain"

c="$(api "$JAR" GET /v1/templates)"
[ "$c" = "200" ] || fail "templates: GET /v1/templates -> $c"
TPL_REF="$(jq -r --arg n "$TEMPLATE" '([.items[] | select(.name == $n)] | first | .id) // empty' "$RESP")"
if [ -n "$TPL_REF" ]; then pass "templates"; else fail "templates: no template named '$TEMPLATE' in the caller's catalog"; fi

# ---- 3. workspace lifecycle ----------------------------------------------------
create_ws() { # create_ws <name> <desiredState> -> http_code; body in $RESP.
  api "$JAR" POST /v1/workspaces \
    -H "X-CSRF-Token: $CSRF" -H "Idempotency-Key: $NAME_PREFIX-$STAMP-$1" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$1\",\"templateRef\":\"$TPL_REF\",\"desiredState\":\"$2\"}"
}

wait_phase() { # wait_phase <ws-id> <wanted> <timeout-s>; FAILS via caller.
  local id="$1" want="$2" deadline=$((SECONDS + $3)) phase="" c
  while [ "$SECONDS" -lt "$deadline" ]; do
    c="$(api "$JAR" GET "/v1/workspaces/$id")"
    phase="$(jq -r '.phase // empty' "$RESP" 2>/dev/null)"
    [ "$phase" = "$want" ] && return 0
    [ "$phase" = "Failed" ] && return 2
    sleep 5
  done
  return 1
}

delete_ws() { # delete_ws <ws-id>: DELETE then wait until the record is gone.
  local id="$1" c deadline=$((SECONDS + DELETE_TIMEOUT))
  api "$JAR" DELETE "/v1/workspaces/$id" -H "X-CSRF-Token: $CSRF" >/dev/null || true
  while [ "$SECONDS" -lt "$deadline" ]; do
    c="$(api "$JAR" GET "/v1/workspaces/$id")"
    [ "$c" = "404" ] && return 0
    sleep 5
  done
  return 1
}

ws1="$NAME_PREFIX-$STAMP-1"
c="$(create_ws "$ws1" Running)"
WS1_ID="$(jq -r '.id // empty' "$RESP" 2>/dev/null)"
if [ "$c" != "201" ] || [ -z "$WS1_ID" ]; then
  fail "workspace-create: POST /v1/workspaces -> $c $(err_code)"
fi
WS_IDS+=("$WS1_ID")
pass "workspace-create"

if wait_phase "$WS1_ID" Ready "$READY_TIMEOUT"; then
  pass "workspace-ready"
elif [ "$?" = 2 ]; then
  fail "workspace-ready: workspace went Failed ($(jq -r '.failureReason // "unknown"' "$RESP"))"
else
  fail "workspace-ready: not Ready after ${READY_TIMEOUT}s"
fi

c="$(api "$JAR" POST "/v1/workspaces/$WS1_ID/stop" -H "X-CSRF-Token: $CSRF" -H "Idempotency-Key: $NAME_PREFIX-$STAMP-stop1")"
[ "$c" = "200" ] || fail "workspace-stop: POST stop -> $c $(err_code)"
if wait_phase "$WS1_ID" Stopped "$DELETE_TIMEOUT"; then
  pass "workspace-stop"
else
  fail "workspace-stop: not Stopped after ${DELETE_TIMEOUT}s"
fi

c="$(api "$JAR" POST "/v1/workspaces/$WS1_ID/start" -H "X-CSRF-Token: $CSRF" -H "Idempotency-Key: $NAME_PREFIX-$STAMP-start1")"
[ "$c" = "200" ] || fail "workspace-start: POST start -> $c $(err_code)"
if wait_phase "$WS1_ID" Ready "$READY_TIMEOUT"; then
  pass "workspace-start"
elif [ "$?" = 2 ]; then
  fail "workspace-start: workspace went Failed ($(jq -r '.failureReason // "unknown"' "$RESP"))"
else
  fail "workspace-start: not Ready after ${READY_TIMEOUT}s"
fi

# ---- 4. quota: the next Running intent must be refused -------------------------
# A create reserves the full vector (the running slot included) up front, so
# the refusal lands on the create call itself — the documented 409
# QUOTA_EXHAUSTED, with details.reason UserLimitReached when a per-principal
# running limit is the binding one.
c="$(api "$JAR" GET /v1/quota)"
[ "$c" = "200" ] || fail "quota-running-limit: GET /v1/quota -> $c"
cp "$RESP" "$W/quota.json"
quota_note="" reason="" qid=""
user_limit="$(jq -r '.userLimits.runningWorkspaces // empty' "$W/quota.json")"
q_i=2
quota_result="skip"
while [ "$q_i" -le "$MAX_WORKSPACES" ]; do
  c="$(create_ws "$NAME_PREFIX-$STAMP-$q_i" Running)"
  qid="$(jq -r '.id // empty' "$RESP" 2>/dev/null || true)"
  if [ "$c" = "201" ] && [ -n "$qid" ]; then
    WS_IDS+=("$qid"); q_i=$((q_i + 1)); continue
  fi
  if [ "$c" = "409" ] && [ "$(err_code)" = "QUOTA_EXHAUSTED" ]; then
    reason="$(jq -r '.details.reason // "-"' "$RESP" 2>/dev/null || echo "-")"
    quota_note="refused QUOTA_EXHAUSTED reason=$reason"
    quota_result="pass"
  else
    fail "quota-running-limit: create $q_i -> $c $(err_code) (want 201 or 409 QUOTA_EXHAUSTED)"
  fi
  break
done
if [ "$quota_result" = "skip" ]; then
  skip "quota-running-limit: no refusal after $((q_i - 1)) Running (limit above TCDI_E2E_MAX_WORKSPACES=$MAX_WORKSPACES probe budget)"
elif [ -n "$user_limit" ] && [ "$reason" != "UserLimitReached" ]; then
  skip "quota-running-limit: 409 QUOTA_EXHAUSTED held, but reason '$reason' means a different quota bound first — the configured per-principal limit is unproven"
else
  pass "quota-running-limit ($quota_note)"
fi

# ---- 5. delete everything the smoke created -------------------------------------
gone=1
for id in "${WS_IDS[@]}"; do
  delete_ws "$id" || { note "workspace $id still visible after ${DELETE_TIMEOUT}s"; gone=0; }
done
if [ "$gone" = "1" ]; then pass "workspace-delete"; else fail "workspace-delete: a workspace is still visible"; fi

# ---- 6. sign out ----------------------------------------------------------------
c="$(api "$JAR" POST /v1/logout -H "X-CSRF-Token: $CSRF")"
case "$c" in
  200) ;; # {endSessionUrl} — visit it so the IdP session ends too
  204) ;;
  *) fail "logout: POST /v1/logout -> $c $(err_code)" ;;
esac
if [ "$c" = "200" ]; then
  end_url="$(jq -r '.endSessionUrl // empty' "$RESP")"
  # Best-effort RP-initiated logout: the jar still holds the IdP SSO cookie.
  [ -z "$end_url" ] || req "$end_url" -b "$JAR" -c "$JAR" >/dev/null
fi
c="$(api "$JAR" GET /v1/me)"
if [ "$c" = "401" ]; then pass "logout"; else fail "logout: /v1/me still -> $c after sign-out"; fi

# ---- 7. optional: sign out everywhere -------------------------------------------
if [ "$SIGN_OUT_EVERYWHERE" -eq 1 ]; then
  oidc_login "$JAR" || fail "revoke-all: first re-login failed ($LOGIN_ERR)"
  CSRF="$(csrf_of "$JAR")"
  oidc_login "$JAR_B" || fail "revoke-all: second re-login failed ($LOGIN_ERR)"
  cr="$(api "$JAR" POST /v1/me/sessions:revoke-all -H "X-CSRF-Token: $CSRF")"
  case "$cr" in 200 | 204) ;; *) fail "revoke-all: POST /v1/me/sessions:revoke-all -> $cr $(err_code)" ;; esac
  c1="$(api "$JAR" GET /v1/me)"
  c2="$(api "$JAR_B" GET /v1/me)"
  if [ "$c1" = "401" ] && [ "$c2" = "401" ]; then
    pass "revoke-all"
  else
    fail "revoke-all: after revoke-all /v1/me -> $c1 (self) / $c2 (other session) (want 401/401)"
  fi
fi

printf '%d checks passed\n' "$CHECKS"
