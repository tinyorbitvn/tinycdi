#!/usr/bin/env bash
# preflight.test.sh — drives hack/preflight/preflight.sh against PATH shims of
# kubectl, openssl, curl, psql and getent. Covers PASS, WARN and FAIL for every
# check, the exit code, that the probe namespace is deleted (also on failure
# and on a signal), that only probe objects are ever created, and that the DSN
# is never printed.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="$ROOT/hack/preflight/preflight.sh"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
SHIM="$D/shim"; LOG="$D/kubectl.log"; mkdir -p "$SHIM"
fails=0

# ---- shims ----------------------------------------------------------------
cat > "$SHIM/kubectl" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$STUB_LOG"
args="$*"
forbidden() { echo "Error from server (Forbidden): $1 is forbidden: User cannot" >&2; exit 1; }
case "$args" in
  *"get --raw /version"*)
    [ -n "${STUB_K8S_DOWN:-}" ] && { echo "Unable to connect to the server" >&2; exit 1; }
    [ -n "${STUB_K8S_FORBIDDEN:-}" ] && forbidden version
    echo "{\"major\":\"1\",\"gitVersion\":\"${STUB_K8S_VERSION:-v1.31.2+rke2r1}\"}" ;;
  *"auth can-i create namespaces"*)
    echo "${STUB_CANI_NS:-yes}"; [ "${STUB_CANI_NS:-yes}" = yes ] || exit 1 ;;
  *"create -f -"*)
    body="$(cat)"
    case "$body" in
      *"kind: Namespace"*) [ -n "${STUB_NS_FORBIDDEN:-}" ] && forbidden namespaces ;;
      *"kind: NetworkPolicy"*) [ -n "${STUB_NETPOL_FORBIDDEN:-}" ] && forbidden networkpolicies ;;
      *"kind: Pod"*)
        [ -n "${STUB_POD_FORBIDDEN:-}" ] && forbidden pods
        case "$body" in
          *"hostUsers: false"*) echo "userns-pod-created" >> "$STUB_LOG"
            [ -n "${STUB_USERNS_REJECT:-}" ] && { echo "unknown field hostUsers" >&2; exit 1; }
            case "$body" in *nodeSelector*) echo "userns-pod-on-pool" >> "$STUB_LOG" ;; esac ;;
        esac ;;
    esac
    echo "created" ;;
  *"wait --for=condition=Ready pod/probe-server"*)
    [ -n "${STUB_WAIT_SLEEP:-}" ] && sleep "$STUB_WAIT_SLEEP"
    exit "${STUB_SERVER_WAIT_RC:-0}" ;;
  *"wait --for=condition=Ready pod/probe-userns"*) exit "${STUB_USERNS_WAIT_RC:-0}" ;;
  *"get pod probe-server"*) echo "${STUB_POD_IP-10.1.2.3}" ;;
  *"get pod probe-client-allow"*) echo "${STUB_BASELINE_PHASE:-Succeeded}" ;;
  *"get pod probe-client-deny"*) echo "${STUB_DENY_PHASE:-Failed}" ;;
  *"get pod probe-userns"*) echo Pending ;;
  *"get storageclass"*)
    [ -n "${STUB_SC_FORBIDDEN:-}" ] && forbidden storageclasses
    printf '%b' "${STUB_SC-standard\ttrue\n}" ;;
  *"get nodes"*)
    [ -n "${STUB_NODES_FORBIDDEN:-}" ] && forbidden nodes
    printf '%b' "${STUB_NODES-n1\tcdi.tinyorbit.vn/workspace;\nn2\tcdi.tinyorbit.vn/workspace;\n}" ;;
  *"get secret"*)
    [ -n "${STUB_SECRET_FORBIDDEN:-}" ] && forbidden secrets
    case "$args" in
      *"get secret missing"*) echo 'Error from server (NotFound): secrets "missing" not found' >&2; exit 1 ;;
      *"get secret wild-tls"*)
        if [ -n "${STUB_TLS_PEM:-}" ]; then base64 -w0 "$STUB_TLS_PEM"; else printf 'FAKECERT' | base64 -w0; fi ;;
      *"get secret db-dsn"*) printf '%s' "${STUB_DSN:-postgres://app:SECRETPW@db.internal.example:5432/tcdi?sslmode=verify-full}" | base64 -w0 ;;
      *"get secret db-ca"*) printf 'FAKECA' | base64 -w0 ;;
    esac ;;
  *"delete namespace tcdi-preflight-"*) exit "${STUB_DELETE_RC:-0}" ;;
  *) echo "stub kubectl: unexpected call: $args" >&2; exit 99 ;;
esac
EOF
cat > "$SHIM/openssl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *"-checkhost"*)
    case "${STUB_TLS:-match}" in
      match) echo "Hostname $4 does match certificate" ;;
      nomatch) echo "Hostname $4 does NOT match certificate" ;;
      garbage) echo "Could not read certificate" >&2; exit 1 ;;
    esac ;;
  *"-checkend 0"*) [ -n "${STUB_TLS_EXPIRED:-}" ] && { echo "Certificate will expire"; exit 1; }; exit 0 ;;
  *"-checkend 1209600"*) [ -n "${STUB_TLS_EXPIRING:-}" ] && { echo "Certificate will expire"; exit 1; }; exit 0 ;;
  *) echo "stub openssl: unexpected call: $*" >&2; exit 99 ;;
esac
EOF
cat > "$SHIM/curl" <<'EOF'
#!/usr/bin/env bash
echo "curl ${*: -1}" >> "$STUB_LOG"
[ -n "${STUB_OIDC_FAIL:-}" ] && exit 22
echo "${STUB_OIDC_BODY:-{\"issuer\":\"https://idp.example.com/realms/tcdi\",\"jwks_uri\":\"x\"\}}"
EOF
cat > "$SHIM/psql" <<'EOF'
#!/usr/bin/env bash
echo "psql PGSSLMODE=${PGSSLMODE:-unset} PGSSLROOTCERT=${PGSSLROOTCERT:+set}" >> "$STUB_LOG"
printf '%s\n' "$@" > "$STUB_PSQL_ARGV"
env | grep '^PG' > "$STUB_PSQL_ARGV.env" || true
case "${STUB_PSQL:-ok}" in
  ok) echo 1 ;;
  cert) echo 'psql: error: connection to server failed: SSL error: certificate verify failed' >&2; exit 2 ;;
  nossl) echo 'psql: error: server does not support SSL, but SSL was required' >&2; exit 2 ;;
  auth) echo 'psql: error: FATAL: password authentication failed for user "app"' >&2; exit 2 ;;
  down) echo 'psql: error: connection to server failed: Connection timed out' >&2; exit 2 ;;
esac
EOF
cat > "$SHIM/getent" <<'EOF'
#!/usr/bin/env bash
[ "${STUB_DNS:-ok}" = ok ] || exit 2
echo "10.0.0.7 $2"
EOF
chmod +x "$SHIM"/*

# ---- harness ----------------------------------------------------------------
OUT="" RC=0
# run [VAR=val ...] -- [flags]
run() {
  local envs=()
  while [ "$1" != -- ]; do envs+=("$1"); shift; done; shift
  : > "$LOG"; rm -f "$D/dsn-seen" "$D/dsn-seen.env"
  OUT="$(env -i PATH="$SHIM:/usr/bin:/bin" HOME="$D" STUB_LOG="$LOG" STUB_PSQL_ARGV="$D/dsn-seen" \
    TCDI_PREFLIGHT_POLL=0.05 TCDI_PREFLIGHT_PROPAGATE=0 TCDI_PREFLIGHT_POD_TIMEOUT=2 \
    "${envs[@]}" bash "$SCRIPT" "$@" 2>&1)"
  RC=$?
}
ok()   { echo "ok:   $1"; }
bad()  { echo "FAIL: $1"; echo "----- output:"; echo "$OUT"; echo "----- kubectl log:"; cat "$LOG"; echo "-----"; fails=1; }
expect() { # expect <desc> <STATUS> <check>
  if printf '%s\n' "$OUT" | grep -Eq "^$2 +$3( |$)"; then ok "$1"; else bad "$1 (want $2 $3)"; fi
}
expect_rc() { if [ "$RC" = "$2" ]; then ok "$1"; else bad "$1 (rc=$RC, want $2)"; fi; }
expect_out() { if printf '%s\n' "$OUT" | grep -Eq -- "$2"; then ok "$1"; else bad "$1 (no match for $2)"; fi; }
expect_no_out() { if printf '%s\n' "$OUT" | grep -Eq -- "$2"; then bad "$1 (found $2)"; else ok "$1"; fi; }
expect_log() { if grep -Eq -- "$2" "$LOG"; then ok "$1"; else bad "$1 (log lacks $2)"; fi; }
expect_no_log() { if grep -Eq -- "$2" "$LOG"; then bad "$1 (log has $2)"; else ok "$1"; fi; }
expect_fix() { # every WARN/FAIL line is followed by a fix line
  local n w
  n="$(printf '%s\n' "$OUT" | grep -Ec '^(WARN|FAIL) ')"
  w="$(printf '%s\n' "$OUT" | grep -Ec '^      fix: .+')"
  if [ "$n" = "$w" ]; then ok "$1"; else bad "$1 ($n WARN/FAIL lines, $w fix lines)"; fi
}

ALL=(--context stub --session-domain cdi.example.com --tls-secret ns/wild-tls
     --oidc-issuer https://idp.example.com/realms/tcdi --postgres-dsn-secret ns/db-dsn:url)

echo "== usage"
run -- --help;            expect_rc "--help exits 0" 0
run -- --bogus;           expect_rc "unknown flag exits 2" 2
run -- --min-k8s one;     expect_rc "bad --min-k8s exits 2" 2
run -- --node-label foo;  expect_rc "--node-label without = exits 2" 2

echo "== all green"
run -- "${ALL[@]}" --host-users-false
expect_rc "all green exits 0" 0
for c in k8s-version netpol storageclass host-users node-pool dns tls-secret oidc postgres; do
  expect "all green: $c" PASS "$c"
done
expect_out "summary line" '^preflight: 9 PASS, 0 WARN, 0 FAIL$'
expect_log "probe namespace deleted" 'delete namespace tcdi-preflight-[0-9a-f]{6}'
expect_log "userns probe pinned to the workspace pool" 'userns-pod-on-pool'
# Only probe objects are created or deleted; nothing is applied/patched/labeled.
if grep -Ev '(^| )(get|auth|create|wait|delete namespace tcdi-preflight-)|^userns-pod|^curl|^psql' "$LOG" | grep -q .; then
  bad "unexpected kubectl verbs: $(grep -Ev '(^| )(get|auth|create|wait|delete namespace tcdi-preflight-)|^userns-pod|^curl|^psql' "$LOG" | head -3)"
else ok "only get/auth/create/wait and probe-namespace delete are used"; fi
expect_no_out "DSN password never printed" 'SECRETPW|app:'
expect_no_out "DSN host never printed" 'db\.internal'
psql_argv_clean() { # psql_argv_clean <desc> <fixed string that must not be on argv>
  if grep -qF -- "$2" "$D/dsn-seen" 2>/dev/null; then bad "$1 (argv has $2)"; else ok "$1"; fi
}
psql_env_has() { # psql_env_has <desc> <NAME=value, exact>
  if grep -qFx -- "$2" "$D/dsn-seen.env" 2>/dev/null; then ok "$1"; else bad "$1 (psql env lacks $2)"; fi
}
psql_env_lacks() { # psql_env_lacks <desc> <NAME>
  if grep -q "^$2=" "$D/dsn-seen.env" 2>/dev/null; then bad "$1 ($2 is set)"; else ok "$1"; fi
}
if [ "$(cat "$D/dsn-seen" 2>/dev/null)" = "-X
-w
-Atq
-c
select 1" ]; then ok "psql argv is only its fixed flags"; else bad "psql argv is not the fixed flags: $(cat "$D/dsn-seen" 2>/dev/null)"; fi
psql_argv_clean "DSN password not on psql argv" 'SECRETPW'
psql_argv_clean "DSN host not on psql argv" 'db.internal'
psql_argv_clean "no DSN scheme on psql argv" '://'
psql_env_has "PGPASSWORD carries the exact password" 'PGPASSWORD=SECRETPW'
psql_env_has "PGHOST from the URL" 'PGHOST=db.internal.example'
psql_env_has "PGPORT from the URL" 'PGPORT=5432'
psql_env_has "PGUSER from the URL" 'PGUSER=app'
psql_env_has "PGDATABASE from the URL" 'PGDATABASE=tcdi'
psql_env_has "PGSSLMODE from the URL query" 'PGSSLMODE=verify-full'

echo "== 1. Kubernetes version"
run STUB_K8S_VERSION=v1.30.0 -- "${ALL[@]}";            expect "1.30 passes" PASS k8s-version
run STUB_K8S_VERSION=v1.29.9+rke2r1 -- "${ALL[@]}";     expect "1.29 fails" FAIL k8s-version; expect_rc "FAIL exits 1" 1; expect_fix "fix lines"
run STUB_K8S_VERSION=v2.0.0 -- "${ALL[@]}";             expect "2.0 passes" PASS k8s-version
run STUB_K8S_VERSION=garbage -- "${ALL[@]}";            expect "unparseable warns" WARN k8s-version
run STUB_K8S_DOWN=1 -- "${ALL[@]}";                     expect "unreachable apiserver fails" FAIL k8s-version
run STUB_K8S_FORBIDDEN=1 -- "${ALL[@]}";                expect "forbidden /version warns" WARN k8s-version
run -- "${ALL[@]}" --min-k8s 1.40;                      expect "--min-k8s raises the floor" FAIL k8s-version

echo "== 2. NetworkPolicy enforcement"
run STUB_DENY_PHASE=Failed -- "${ALL[@]}";              expect "enforced passes" PASS netpol
run STUB_DENY_PHASE=Succeeded -- "${ALL[@]}";           expect "not enforced fails" FAIL netpol; expect_rc "exits 1" 1
expect_log "probe namespace deleted after a FAIL" 'delete namespace tcdi-preflight-'
expect_fix "every WARN/FAIL has a fix"
run STUB_BASELINE_PHASE=Failed -- "${ALL[@]}";          expect "baseline traffic broken warns" WARN netpol; expect_rc "WARN alone exits 0" 0
run STUB_SERVER_WAIT_RC=1 -- "${ALL[@]}";               expect "server never Ready warns" WARN netpol
run STUB_CANI_NS=no -- "${ALL[@]}";                     expect "cannot create namespace warns" WARN netpol
expect_no_log "no namespace create attempted without permission" 'create -f -'
run STUB_NS_FORBIDDEN=1 -- "${ALL[@]}";                 expect "namespace create forbidden warns" WARN netpol
run STUB_POD_FORBIDDEN=1 -- "${ALL[@]}";                expect "pod create forbidden warns" WARN netpol
run STUB_NETPOL_FORBIDDEN=1 -- "${ALL[@]}";             expect "policy create forbidden warns" WARN netpol
run -- "${ALL[@]}" --no-probe;                          expect "--no-probe warns" WARN netpol
expect_no_log "--no-probe creates nothing" 'create -f -'
expect_no_log "--no-probe deletes nothing" 'delete namespace'
run STUB_DELETE_RC=1 -- "${ALL[@]}";                    expect_rc "failed cleanup exits 1" 1
expect_out "failed cleanup names the namespace" 'could not delete probe namespace tcdi-preflight-'

echo "== 3. StorageClass"
run -- "${ALL[@]}";                                     expect "default class passes" PASS storageclass
run STUB_SC='fast\t\nslow\t\n' -- "${ALL[@]}";          expect "no default fails" FAIL storageclass
run STUB_SC= -- "${ALL[@]}";                            expect "no classes fails" FAIL storageclass
run STUB_SC='fast\t\n' -- "${ALL[@]}" --storage-class fast;    expect "named class passes" PASS storageclass
run -- "${ALL[@]}" --storage-class absent;              expect "named class missing fails" FAIL storageclass
run STUB_SC_FORBIDDEN=1 -- "${ALL[@]}";                 expect "list forbidden warns" WARN storageclass

echo "== 4. user namespaces"
run -- "${ALL[@]}";                                     expect "not requested passes" PASS host-users
expect_no_log "not requested creates no userns pod" 'userns-pod'
run STUB_USERNS_WAIT_RC=1 -- "${ALL[@]}" --host-users-false;   expect "pod does not start fails" FAIL host-users
run STUB_USERNS_REJECT=1 -- "${ALL[@]}" --host-users-false;     expect "pod rejected fails" FAIL host-users
run STUB_POD_FORBIDDEN=1 -- "${ALL[@]}" --host-users-false;     expect "pod forbidden warns" WARN host-users
run STUB_CANI_NS=no -- "${ALL[@]}" --host-users-false;         expect "no namespace permission warns" WARN host-users
run -- "${ALL[@]}" --host-users-false --no-probe;       expect "--no-probe warns" WARN host-users
run -- "${ALL[@]}" --host-users-false --allow-shared-nodes
expect "shared nodes: probe passes" PASS host-users
expect_no_log "shared nodes: probe not pinned to a pool" 'userns-pod-on-pool'

echo "== 5. workspace node pool"
run -- "${ALL[@]}";                                     expect "labeled + tainted passes" PASS node-pool
run STUB_NODES= -- "${ALL[@]}";                         expect "no labeled node fails" FAIL node-pool
run STUB_NODES='n1\tcdi.tinyorbit.vn/workspace;\nn2\t\n' -- "${ALL[@]}";  expect "untainted node warns" WARN node-pool
expect_out "untainted node is named" 'not tainted .*: n2'
run STUB_NODES= -- "${ALL[@]}" --allow-shared-nodes;    expect "shared-node opt-out passes" PASS node-pool
expect_no_log "opt-out does not list nodes" 'get nodes'
run STUB_NODES_FORBIDDEN=1 -- "${ALL[@]}";               expect "list forbidden warns" WARN node-pool
run STUB_NODES='n1\tpool/gpu;\n' -- "${ALL[@]}" --node-label pool=gpu --node-taint pool/gpu
expect "custom label and taint pass" PASS node-pool
expect_log "custom label is the selector" 'get nodes -l pool=gpu'

echo "== 6. wildcard DNS"
run -- "${ALL[@]}";                                     expect "resolves passes" PASS dns
run STUB_DNS=none -- "${ALL[@]}";                       expect "does not resolve fails" FAIL dns
run GETENT=/nonexistent/getent -- "${ALL[@]}";          expect "no resolver tool warns" WARN dns
run -- --context stub;                                  expect "no session domain warns" WARN dns

echo "== 7. TLS secret"
run -- "${ALL[@]}";                                     expect "wildcard covered passes" PASS tls-secret
run STUB_TLS=nomatch -- "${ALL[@]}";                    expect "not covering fails" FAIL tls-secret
run STUB_TLS=garbage -- "${ALL[@]}";                    expect "unreadable PEM fails" FAIL tls-secret
run STUB_TLS_EXPIRED=1 -- "${ALL[@]}";                  expect "expired fails" FAIL tls-secret
run STUB_TLS_EXPIRING=1 -- "${ALL[@]}";                 expect "expiring soon warns" WARN tls-secret
run -- "${ALL[@]}" --tls-secret ns/missing;             expect "missing secret fails" FAIL tls-secret
run STUB_SECRET_FORBIDDEN=1 -- "${ALL[@]}";             expect "secret forbidden warns" WARN tls-secret
run -- "${ALL[@]}" --tls-secret nonamespace;            expect "bad ref fails" FAIL tls-secret
run OPENSSL=/nonexistent/openssl -- "${ALL[@]}";        expect "no openssl warns" WARN tls-secret
run -- --context stub --session-domain cdi.example.com; expect "no secret ref warns" WARN tls-secret

echo "== 8. OIDC discovery"
run -- "${ALL[@]}";                                     expect "answers passes" PASS oidc
expect_log "discovery URL" 'curl https://idp.example.com/realms/tcdi/.well-known/openid-configuration'
run -- "${ALL[@]}" --oidc-issuer https://idp.example.com/realms/tcdi/; expect "trailing slash tolerated" PASS oidc
run STUB_OIDC_FAIL=1 -- "${ALL[@]}";                    expect "no answer fails" FAIL oidc
run STUB_OIDC_BODY='{"issuer":"https://other.example.com"}' -- "${ALL[@]}";  expect "issuer mismatch fails" FAIL oidc
run STUB_OIDC_BODY='{"issuer":"http://idp.example.com/realms/tcdi"}' -- "${ALL[@]}" --oidc-issuer http://idp.example.com/realms/tcdi
expect "plain http warns" WARN oidc
run CURL=/nonexistent/curl -- "${ALL[@]}";              expect "no curl warns" WARN oidc
run -- --context stub;                                  expect "no issuer warns" WARN oidc

echo "== 9. Postgres"
run -- "${ALL[@]}";                                     expect "verified passes" PASS postgres
run STUB_DSN='postgres://app:SECRETPW@db/tcdi' -- "${ALL[@]}"; expect "silent DSN verifies" PASS postgres
expect_log "verify-full is forced when the DSN is silent" 'psql PGSSLMODE=verify-full'
run STUB_DSN='postgres://app:SECRETPW@db/tcdi?sslmode=require' -- "${ALL[@]}"; expect "unverified TLS warns" WARN postgres
run STUB_DSN='host=db user=app password=SECRETPW sslmode=disable' -- "${ALL[@]}"; expect "sslmode=disable warns" WARN postgres
run STUB_PSQL=cert -- "${ALL[@]}";                      expect "TLS verify failure fails" FAIL postgres
run STUB_PSQL=nossl -- "${ALL[@]}";                     expect "server without TLS fails" FAIL postgres
run STUB_PSQL=auth -- "${ALL[@]}";                      expect "auth failure fails" FAIL postgres
run STUB_PSQL=down -- "${ALL[@]}";                      expect "unreachable from this host warns" WARN postgres
expect_no_out "DSN not echoed on failure" 'SECRETPW|app:'
run PSQL=/nonexistent/psql -- "${ALL[@]}";              expect "no psql warns" WARN postgres
run STUB_SECRET_FORBIDDEN=1 -- "${ALL[@]}";             expect "DSN secret forbidden warns" WARN postgres
run -- "${ALL[@]}" --postgres-dsn-secret ns/missing;    expect "DSN secret missing fails" FAIL postgres
run -- "${ALL[@]}" --postgres-ca-secret ns/db-ca;      expect "CA secret passes" PASS postgres
expect_log "CA from the secret reaches psql" 'PGSSLROOTCERT=set'
run -- --context stub;                                  expect "no DSN source warns" WARN postgres
run MYDSN='postgres://app:SECRETPW@db/tcdi' -- --context stub --postgres-dsn-env MYDSN
expect "DSN from env passes" PASS postgres
expect_no_out "env DSN never printed" 'SECRETPW'

echo "== 9b. DSN stays off psql's argument list"
PWSPECIAL='p@ss:w/rd% '"'"'q"x'
# URL form with a percent-encoded password, key=value form with a quoted one.
run STUB_DSN='postgresql://app:p%40ss%3Aw%2Frd%25%20%27q%22x@db.example:6543/tcdi?sslmode=verify-ca&sslrootcert=%2Fetc%2Fca%20x.pem' -- "${ALL[@]}"
expect "special-character URL password verifies" PASS postgres
psql_argv_clean "URL: special password not on argv" 'rd%'
psql_argv_clean "URL: decoded password not on argv" 'p@ss'
psql_argv_clean "URL: encoded password not on argv" 'p%40ss'
psql_argv_clean "URL: host not on argv" 'db.example'
psql_env_has "URL: PGPASSWORD is the decoded password" "PGPASSWORD=$PWSPECIAL"
psql_env_has "URL: PGHOST" 'PGHOST=db.example'
psql_env_has "URL: PGPORT" 'PGPORT=6543'
psql_env_has "URL: PGUSER" 'PGUSER=app'
psql_env_has "URL: PGDATABASE" 'PGDATABASE=tcdi'
psql_env_has "URL: PGSSLMODE" 'PGSSLMODE=verify-ca'
psql_env_has "URL: PGSSLROOTCERT from the query string, decoded" 'PGSSLROOTCERT=/etc/ca x.pem'
expect_no_out "URL: password not printed" 'rd%|p@ss|p%40ss|app:'
run STUB_DSN="host=db.example port=6543 user=app dbname=tcdi password='p@ss:w/rd% \\'q\"x' sslmode=verify-full" -- "${ALL[@]}"
expect "key=value DSN verifies" PASS postgres
psql_argv_clean "kv: password not on argv" 'p@ss'
psql_argv_clean "kv: host not on argv" 'db.example'
psql_env_has "kv: PGPASSWORD is the exact password" "PGPASSWORD=$PWSPECIAL"
psql_env_has "kv: PGHOST" 'PGHOST=db.example'
psql_env_has "kv: PGPORT" 'PGPORT=6543'
psql_env_has "kv: PGUSER" 'PGUSER=app'
psql_env_has "kv: PGDATABASE" 'PGDATABASE=tcdi'
psql_env_has "kv: PGSSLMODE" 'PGSSLMODE=verify-full'
expect_no_out "kv: password not printed" 'p@ss|rd%|SECRETPW'
run STUB_DSN='host=db user=app password=plain sslmode=verify-full' -- "${ALL[@]}"
psql_env_has "kv unquoted: PGPASSWORD" 'PGPASSWORD=plain'
run STUB_DSN='postgres://u:pw@[2001:db8::1]:5433,other:5434/d' -- "${ALL[@]}"
psql_env_has "IPv6 literal host is unbracketed" 'PGHOST=2001:db8::1,other'
psql_env_has "per-host ports are comma separated" 'PGPORT=5433,5434'
run STUB_DSN='postgres://app@db/tcdi' -- "${ALL[@]}"
psql_env_lacks "no password in the DSN sets no PGPASSWORD" PGPASSWORD
run STUB_DSN='postgres://app:SECRETPW@db/tcdi?sslmode=verify-full&sslnegotiation=direct' -- "${ALL[@]}"
expect "a parameter with no PG* variable skips the check" WARN postgres
expect_no_out "skip message does not leak the DSN" 'SECRETPW|app:'
psql_argv_clean "skipped check never ran psql with the DSN" 'SECRETPW'
run STUB_DSN='postgres://app:bad%zz@db/tcdi' -- "${ALL[@]}"
expect "a malformed escape fails" FAIL postgres
expect_no_out "parse failure does not leak the DSN" 'bad%zz|app:'
run STUB_DSN="host=db password='unterminated" -- "${ALL[@]}"
expect "an unterminated quote fails" FAIL postgres
expect_no_out "kv parse failure does not leak the password" 'unterminated'
run MYDSN='postgres://app:SECRETPW@db.env/tcdi' -- --context stub --postgres-dsn-env MYDSN
psql_argv_clean "env DSN: password not on argv" 'SECRETPW'
psql_env_has "env DSN: PGPASSWORD" 'PGPASSWORD=SECRETPW'
psql_env_has "env DSN: PGHOST" 'PGHOST=db.env'
run -- "${ALL[@]}" --postgres-ca-secret ns/db-ca
if grep -Eq '^PGSSLROOTCERT=/.*/db-ca\.crt$' "$D/dsn-seen.env"; then ok "CA secret goes through PGSSLROOTCERT"; else bad "CA secret goes through PGSSLROOTCERT"; fi

echo "== cleanup on signal"
: > "$LOG"
env -i PATH="$SHIM:/usr/bin:/bin" HOME="$D" STUB_LOG="$LOG" STUB_PSQL_ARGV="$D/dsn-seen" STUB_WAIT_SLEEP=2 \
  TCDI_PREFLIGHT_POLL=0.05 TCDI_PREFLIGHT_PROPAGATE=0 TCDI_PREFLIGHT_POD_TIMEOUT=2 \
  bash "$SCRIPT" --context stub >"$D/sig.out" 2>&1 &
pid=$!
for _ in $(seq 1 100); do grep -q 'wait --for=condition=Ready pod/probe-server' "$LOG" && break; sleep 0.1; done
kill -TERM "$pid"
wait "$pid"; RC=$?; OUT="$(cat "$D/sig.out")"
expect_rc "SIGTERM exits 143" 143
expect_log "probe namespace deleted on SIGTERM" 'delete namespace tcdi-preflight-'

echo "== real openssl against a generated certificate"
if command -v openssl >/dev/null 2>&1; then
  mkdir -p "$D/pki"
  mk() { openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days "$1" \
           -keyout "$D/pki/key" -out "$D/pki/$2" -subj "/CN=$3" -addext "subjectAltName=$4" >/dev/null 2>&1; }
  mk 90 wild.pem wild "DNS:*.cdi.example.com"
  mk 90 other.pem other "DNS:*.other.example.com"
  mk 90 exact.pem exact "DNS:cdi.example.com"
  mk 5 soon.pem soon "DNS:*.cdi.example.com"
  REAL="$(command -v openssl)"
  run OPENSSL="$REAL" STUB_TLS_PEM="$D/pki/wild.pem" -- "${ALL[@]}";  expect "real openssl: wildcard passes" PASS tls-secret
  run OPENSSL="$REAL" STUB_TLS_PEM="$D/pki/other.pem" -- "${ALL[@]}"; expect "real openssl: other domain fails" FAIL tls-secret
  run OPENSSL="$REAL" STUB_TLS_PEM="$D/pki/exact.pem" -- "${ALL[@]}"; expect "real openssl: apex-only cert fails" FAIL tls-secret
  run OPENSSL="$REAL" STUB_TLS_PEM="$D/pki/soon.pem" -- "${ALL[@]}";  expect "real openssl: 5-day cert warns" WARN tls-secret
else
  echo "skip: openssl not installed"
fi

echo "== shellcheck"
if command -v shellcheck >/dev/null 2>&1; then
  if shellcheck -x "$SCRIPT" "$0"; then ok "shellcheck clean"; else bad "shellcheck findings"; fi
else
  echo "skip: shellcheck not installed"
fi

if [ "$fails" = 0 ]; then echo "PASS: preflight.test.sh"; else echo "FAILED: preflight.test.sh"; fi
exit "$fails"
