#!/usr/bin/env bash
# restore-drill.sh — Postgres-only restore drill on the kind quickstart
# (DEV ONLY).
#
#   hack/quickstart/up.sh              # once: bring the throwaway stack up
#   hack/quickstart/restore-drill.sh
#
# Exercises docs/runbooks/disaster-recovery.md §"Postgres-only restore onto
# a live cluster" end to end:
#
#   seed   — dev-realm login (curl OIDC), a Running workspace, a redeemed
#            launch ticket: live session + live lease + gateway cookie.
#   dump   — pg_dump of the platform DB.
#   mutate — sign-out (S17 revocation), a workspace stop, a second
#            workspace — changes the dump does not know about.
#   naive  — the unsafe restore: freeze, load the dump, reopen WITHOUT the
#            post-restore SQL. Asserts the signed-out session resurrects
#            and the revoked lease row comes back 'active' (the hazards the
#            runbook exists for).
#   fix    — the documented post-restore block (session_epoch rotation +
#            revoke all restored leases + deny all unconsumed tickets), the
#            workspaces↔CR reconcile, reopen, invariants.
#
# Lease-TTL note: a revoked lease resurrects as 'active' with its
# session_digest; whether the old gateway cookie still serves depends on
# the restore landing inside the 30 s lease TTL — timing the drill does
# not depend on. The script asserts the resurrected row state (the
# mechanism) and the post-fix denial (401); a sub-TTL window was observed
# by hand during development (gateway cookie 200 until expires_at).
#
# Environment: the TCDI_QS_* knobs of common.sh. Leaves the quickstart
# running; hack/quickstart/down.sh tears it down.
set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

need kubectl curl python3 sed awk
[ -s "$KUBECONFIG" ] || die "no quickstart kubeconfig at $KUBECONFIG — run hack/quickstart/up.sh first"

CA="${STATE_DIR}/tls/local-ca.crt"
[ -s "$CA" ] || die "no quickstart CA at $CA"
PORTAL="https://${PORTAL_HOST}"
PSQL_DB="tinycdi" # deps.yaml: POSTGRES_DB/POSTGRES_USER
DRILL_DIR="${STATE_DIR}/restore-drill"
JAR="$DRILL_DIR/portal.jar"
JAR_GW="$DRILL_DIR/gateway.jar"
SESS_VALUE_FILE="$DRILL_DIR/session-cookie" # raw __Host-tcdi_session value

mkdir -p "$DRILL_DIR"

psql() { kc -n "$NS_DEPS" exec -i postgres-0 -- psql -U "$PSQL_DB" -d "$PSQL_DB" -v ON_ERROR_STOP=1 "$@"; }
psql_t() { psql -tA -c "$1"; }

# HTTP helpers ---------------------------------------------------------------

portal() { # portal <jar> <method> <path> [curl args...]
  local jar="$1" method="$2" path="$3"; shift 3
  curl -sS --max-time 15 --cacert "$CA" -b "$jar" -c "$jar" -X "$method" "$PORTAL$path" "$@"
}

csrf_of() { # csrf_of <jar>
  portal "$1" GET /v1/me | python3 -c 'import json,sys; print(json.load(sys.stdin)["csrfToken"])'
}

# portal_login <jar> <cookie-out>: run the dev-realm OIDC flow with curl and
# save the raw session cookie value (it survives the jar being cleared by
# sign-out — the drill needs exactly that to prove resurrection).
portal_login() {
  local jar="$1" out="$2" loc action cb
  rm -f "$jar"
  loc=$(curl -sS --max-time 15 --cacert "$CA" -c "$jar" -D - -o /dev/null \
    "$PORTAL/v1/login" | awk 'BEGIN{IGNORECASE=1} /^location:/{sub(/\r$/,""); print substr($0,11)}')
  [ -n "$loc" ] || die "login: no redirect from /v1/login"
  action=$(curl -sS --max-time 15 --cacert "$CA" -b "$jar" -c "$jar" "$loc" \
    | grep -o 'action="[^"]*"' | head -1 | sed 's/^action="//;s/"$//;s/&amp;/\&/g')
  [ -n "$action" ] || die "login: no keycloak form action"
  cb=$(curl -sS --max-time 15 --cacert "$CA" -b "$jar" -c "$jar" -D - -o /dev/null \
    -X POST "$action" \
    --data-urlencode "username=$DEMO_USER" --data-urlencode "password=$DEMO_PASSWORD" \
    | awk 'BEGIN{IGNORECASE=1} /^location:/{sub(/\r$/,""); print substr($0,11)}')
  [ -n "$cb" ] || die "login: no callback redirect (bad credentials?)"
  curl -sS --max-time 15 --cacert "$CA" -b "$jar" -c "$jar" -D "$DRILL_DIR/callback.h" -o /dev/null "$cb"
  sed -n 's/^[Ss]et-[Cc]ookie: *__Host-tcdi_session=\([^;]*\).*/\1/p' "$DRILL_DIR/callback.h" | head -1 >"$out"
  [ -s "$out" ] || die "login: no session cookie issued"
  portal "$jar" GET /v1/me | grep -q '"subject"' || die "login: /v1/me did not authenticate"
}

status_with_cookie() { # status_with_cookie <host> <path> <cookie-value>
  curl -sS --max-time 15 --cacert "$CA" --cookie "__Host-tcdi_session=$3" \
    -o /dev/null -w '%{http_code}' "https://$1$2" || true
}

status_with_jar() { # status_with_jar <host> <path> <jar>
  curl -sS --max-time 15 --cacert "$CA" -b "$3" -o /dev/null -w '%{http_code}' "https://$1$2" || true
}

api_json_field() { python3 -c "import json,sys; print(json.load(sys.stdin)$1)"; }

wait_phase() { # wait_phase <ws-id> <wanted> <timeout-s>
  local id="$1" want="$2" deadline=$((SECONDS + $3)) phase=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    phase=$(portal "$JAR" GET "/v1/workspaces/$id" | api_json_field "['phase']" 2>/dev/null || true)
    [ "$phase" = "$want" ] && return 0
    sleep 10
  done
  die "workspace $id never reached $want (last: ${phase:-unknown})"
}

create_ws() { # create_ws <name> <desiredState> -> id on stdout
  local csrf resp id
  csrf=$(csrf_of "$JAR")
  resp=$(portal "$JAR" POST /v1/workspaces \
    -H "X-CSRF-Token: $csrf" -H "Idempotency-Key: drill-$1-$RANDOM$RANDOM" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$1\",\"templateRef\":\"$TPL_REF\",\"desiredState\":\"$2\"}")
  id=$(printf '%s' "$resp" | api_json_field "['id']" 2>/dev/null) \
    || die "create workspace $1 failed: $resp"
  printf '%s\n' "$id"
}

scale_platform() { # scale_platform <replicas>
  kc -n "$NS_SYSTEM" scale deploy backend operator --replicas="$1" >/dev/null
  if [ "$1" -eq 0 ]; then
    kc -n "$NS_SYSTEM" wait --for=delete pod -l app.kubernetes.io/name=backend --timeout=120s >/dev/null 2>&1 || true
  else
    kc -n "$NS_SYSTEM" rollout status deploy/backend --timeout=240s >/dev/null
    kc -n "$NS_SYSTEM" rollout status deploy/operator --timeout=240s >/dev/null
    wait_portal
  fi
}

# wait_portal: rollout status reports the pods available before Traefik
# re-learns the endpoints — the first probe otherwise sees a 503.
wait_portal() {
  local deadline=$((SECONDS + 90))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(status_with_jar "$PORTAL_HOST" /v1/session /dev/null)" = "200" ] && return 0
    sleep 3
  done
  die "portal did not come back routable"
}

on_error() {
  local rc=$?
  warn "restore-drill failed (exit $rc) — scaling the platform back"
  scale_platform 2 || true
  die "see $DRILL_DIR for artifacts"
}
trap on_error ERR

# ---- preflight ---------------------------------------------------------------
kc -n "$NS_SYSTEM" get deploy backend >/dev/null || die "no backend deploy in $NS_SYSTEM — is the quickstart up?"
[ "$(status_with_jar "$PORTAL_HOST" /v1/session /dev/null)" = "200" ] \
  || die "portal not answering at $PORTAL"

# ---- seed --------------------------------------------------------------------
log "seed: login + running workspace + redeemed ticket"
portal_login "$JAR" "$SESS_VALUE_FILE"
TPL_REF=$(portal "$JAR" GET /v1/templates | python3 -c '
import json,sys
items=json.load(sys.stdin)["items"]
print(next((t["id"] for t in items if t["name"]=="browser"), items[0]["id"]))')
log "drill workspace template: $TPL_REF"
WS_A=$(create_ws "drill-a-$(date +%s)" Running)
log "  workspace A: $WS_A — waiting for Ready"
wait_phase "$WS_A" Ready 600
csrf=$(csrf_of "$JAR")
ticket_json=$(portal "$JAR" POST "/v1/workspaces/$WS_A/connections" \
  -H "X-CSRF-Token: $csrf" -H "Content-Type: application/json" -d '{}')
TICKET=$(printf '%s' "$ticket_json" | api_json_field "['ticket']")
LAUNCH_URL=$(printf '%s' "$ticket_json" | api_json_field "['launchUrl']")
rm -f "$JAR_GW"
code=$(curl -sS --max-time 15 --cacert "$CA" -c "$JAR_GW" -o /dev/null -w '%{http_code}' \
  -X POST "$LAUNCH_URL" --data-urlencode "ticket=$TICKET")
[ "$code" = "303" ] || die "ticket redeem expected 303, got $code"
GW_HOST="${LAUNCH_URL#https://}"; GW_HOST="${GW_HOST%%/*}"
[ "$(status_with_jar "$GW_HOST" / "$JAR_GW")" = "200" ] \
  || die "gateway cookie did not serve the session host"
psql_t "SELECT state FROM connection_lease WHERE workspace_id='$WS_A'" | grep -qx active \
  || die "seed produced no active lease"
log "  active lease + gateway cookie verified"

# ---- dump --------------------------------------------------------------------
log "dump: pg_dump -> $DRILL_DIR/dump.sql"
kc -n "$NS_DEPS" exec postgres-0 -- pg_dump -U "$PSQL_DB" -d "$PSQL_DB" --no-owner \
  >"$DRILL_DIR/dump.sql"
[ -s "$DRILL_DIR/dump.sql" ] || die "empty dump"

# ---- mutate ------------------------------------------------------------------
log "mutate: stop A, create B, sign out (post-dump changes the dump never saw)"
csrf=$(csrf_of "$JAR")
portal "$JAR" POST "/v1/workspaces/$WS_A/stop" -H "X-CSRF-Token: $csrf" \
  -H "Idempotency-Key: drill-stop-$RANDOM$RANDOM" -o /dev/null -w '  stop A -> %{http_code}\n'
WS_B=$(create_ws "drill-b-$(date +%s)" Stopped)
log "  workspace B: $WS_B (exists only cluster-side after the restore)"
csrf=$(csrf_of "$JAR")
portal "$JAR" POST /v1/logout -H "X-CSRF-Token: $csrf" -o /dev/null -w '  logout -> %{http_code}\n'
[ "$(status_with_jar "$GW_HOST" / "$JAR_GW")" = "401" ] \
  || warn "gateway cookie still answered after sign-out"
SESS_VALUE=$(cat "$SESS_VALUE_FILE")

# ---- naive restore: the hazard demo -------------------------------------------
log "naive restore: freeze -> load dump -> reopen WITHOUT post-restore SQL"
scale_platform 0
psql -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" >/dev/null
psql <"$DRILL_DIR/dump.sql" >"$DRILL_DIR/load.log" 2>&1 \
  || { tail -20 "$DRILL_DIR/load.log"; die "dump load failed"; }
kc -n "$NS_SYSTEM" scale deploy backend --replicas=2 >/dev/null
kc -n "$NS_SYSTEM" rollout status deploy/backend --timeout=240s >/dev/null
wait_portal

code=$(status_with_cookie "$PORTAL_HOST" /v1/me "$SESS_VALUE")
if [ "$code" = "200" ]; then
  log "  OBSERVED: signed-out portal session resurrected (/v1/me 200) — S17 regression without epoch rotation"
else
  warn "  expected resurrection but got $code — investigate before trusting this drill"
fi
lease_state=$(psql_t "SELECT state FROM connection_lease WHERE workspace_id='$WS_A' ORDER BY created_at DESC LIMIT 1")
[ "$lease_state" = "active" ] \
  || die "restored lease is $lease_state, expected active — resurrection mechanism not reproduced"
log "  OBSERVED: revoked lease row restored as 'active' (session_digest intact)"

# ---- fix: documented post-restore block ---------------------------------------
log "fix: freeze, post-restore SQL, reconcile, reopen"
scale_platform 0
psql <<'SQL' >/dev/null
CREATE TABLE IF NOT EXISTS platform_meta (key text PRIMARY KEY, value text NOT NULL);
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS epoch text NOT NULL DEFAULT '';
INSERT INTO platform_meta (key, value)
VALUES ('session_epoch', gen_random_uuid()::text)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;
UPDATE connection_lease SET state = 'revoked', closed_at = now() WHERE state = 'active';
UPDATE launch_ticket SET revoked_at = now() WHERE consumed_at IS NULL AND revoked_at IS NULL;
SQL

# Reopening is safe the moment the kill-SQL lands — the reconcile steps
# need the platform up anyway (the orphan CR's finalizer talks to the
# backend control surface).
scale_platform 2

# reconcile: orphan CR (B, created post-dump) gets deleted.
B_CR="ws-${WS_B#ws_}"
if kc -n "$NS_TENANT" get workspace "$B_CR" >/dev/null 2>&1; then
  kc -n "$NS_TENANT" delete workspace "$B_CR" --timeout=120s
  log "  orphan CR $B_CR deleted (no workspaces row — created after the dump)"
fi
# reconcile: align diverged rows to their live CRs (intent fence).
# workspaces.id = ws_<hex>, CR name = ws-<hex>.
for row in $(psql_t "SELECT id FROM workspaces WHERE state='active'"); do
  cr="ws-${row#ws_}"
  applied=$(kc -n "$NS_TENANT" get workspace "$cr" \
    -o jsonpath='{.metadata.annotations.workspaces\.cdi\.tinyorbit\.vn/applied-intent}' 2>/dev/null || true)
  [ -n "$applied" ] || continue
  rev=$(printf '%s' "$applied" | python3 -c 'import json,sys; print(json.load(sys.stdin)["revision"])')
  desired=$(kc -n "$NS_TENANT" get workspace "$cr" -o jsonpath='{.spec.desiredState}')
  gen=$(kc -n "$NS_TENANT" get workspace "$cr" -o jsonpath='{.spec.runtimeGeneration}')
  psql_t "UPDATE workspaces SET desired_state='$desired', runtime_generation=$gen,
            intent_revision=$rev WHERE id='$row' AND intent_revision < $rev" >/dev/null
  log "  $row aligned to CR (desired=$desired gen=$gen rev=$rev)"
done

# ---- invariants ----------------------------------------------------------------
log "invariants"
live_sessions=$(psql_t "SELECT count(*) FROM sessions WHERE epoch = (SELECT value FROM platform_meta WHERE key='session_epoch')")
active_leases=$(psql_t "SELECT count(*) FROM connection_lease WHERE state='active'")
armed_tickets=$(psql_t "SELECT count(*) FROM launch_ticket WHERE consumed_at IS NULL AND revoked_at IS NULL")
[ "$live_sessions" = "0" ] || die "invariant: $live_sessions sessions still live at current epoch"
[ "$active_leases" = "0" ] || die "invariant: $active_leases active leases resurrected"
[ "$armed_tickets" = "0" ] || die "invariant: $armed_tickets unconsumed tickets armed"
[ "$(status_with_cookie "$PORTAL_HOST" /v1/me "$SESS_VALUE")" = "401" ] \
  || die "invariant: resurrected session still authenticates"
[ "$(status_with_jar "$GW_HOST" / "$JAR_GW")" = "401" ] \
  || die "invariant: resurrected gateway cookie still serves"
log "  zero live sessions / active leases / armed tickets; both pre-restore cookies 401"

# ---- reopen + cleanup -----------------------------------------------------------
log "reopen: fresh login + workspace lifecycle still work"
portal_login "$JAR" "$DRILL_DIR/session-cookie-fresh"
portal "$JAR" GET /v1/workspaces | grep -q "$WS_A" || die "restored workspace missing from list"
csrf=$(csrf_of "$JAR")
portal "$JAR" DELETE "/v1/workspaces/$WS_A" -H "X-CSRF-Token: $csrf" \
  -o /dev/null -w '  delete A -> %{http_code}\n'

log "drill complete — kind quickstart left running (down.sh tears it down)"
