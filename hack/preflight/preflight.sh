#!/usr/bin/env bash
# Copyright (c) 2026 TinyOrbit
# SPDX-License-Identifier: MIT
#
# preflight.sh — check a target cluster before installing the TinyCDI chart.
#
# Every check prints PASS, WARN or FAIL with a one-line fix; the exit code is
# 1 when any check FAILs (2 on a usage error). A check that cannot run for
# lack of permissions or a missing local tool is a WARN, never a FAIL.
#
# The only objects the script creates are one throwaway probe namespace
# (tcdi-preflight-<random>) with its probe pods and a deny-ingress
# NetworkPolicy. A trap deletes the namespace on exit, error and interrupt.
# Secret values (DSN, TLS keys) are never printed. The Postgres DSN is split
# into PG* variables and handed to psql through its environment only, never
# as an argument, so the password is not visible in the process list.
#
# See hack/preflight/README.md for the flags and the check list.

set -uo pipefail

KUBECTL="${KUBECTL:-kubectl}"
OPENSSL="${OPENSSL:-openssl}"
CURL="${CURL:-curl}"
PSQL="${PSQL:-psql}"
GETENT="${GETENT:-getent}"

CONTEXT="${TCDI_PREFLIGHT_CONTEXT:-}"
SESSION_DOMAIN="${TCDI_PREFLIGHT_SESSION_DOMAIN:-}"
TLS_SECRET="${TCDI_PREFLIGHT_TLS_SECRET:-}"
OIDC_ISSUER="${TCDI_PREFLIGHT_OIDC_ISSUER:-}"
DSN_SECRET="${TCDI_PREFLIGHT_DSN_SECRET:-}"
DSN_ENV="${TCDI_PREFLIGHT_DSN_ENV:-TCDI_PREFLIGHT_DSN}"
CA_SECRET="${TCDI_PREFLIGHT_DB_CA_SECRET:-}"
NODE_LABEL="${TCDI_PREFLIGHT_NODE_LABEL:-cdi.tinyorbit.vn/workspace=true}"
NODE_TAINT="${TCDI_PREFLIGHT_NODE_TAINT:-cdi.tinyorbit.vn/workspace}"
STORAGE_CLASS="${TCDI_PREFLIGHT_STORAGE_CLASS:-}"
MIN_K8S="${TCDI_PREFLIGHT_MIN_K8S:-1.30}"
PROBE_IMAGE="${TCDI_PREFLIGHT_PROBE_IMAGE:-registry.k8s.io/e2e-test-images/agnhost:2.53}"
ALLOW_SHARED=0
HOST_USERS_FALSE=0
NO_PROBE=0
POLL="${TCDI_PREFLIGHT_POLL:-2}"          # seconds between probe-pod polls
POD_TIMEOUT="${TCDI_PREFLIGHT_POD_TIMEOUT:-90}"
PROPAGATE="${TCDI_PREFLIGHT_PROPAGATE:-5}" # NetworkPolicy propagation wait

usage() {
  cat <<'EOF'
Usage: preflight.sh [flags]

  --context NAME            kubeconfig context (default: current)
  --session-domain DOMAIN   session domain; checks wildcard DNS and the TLS secret
  --tls-secret NS/NAME      kubernetes.io/tls Secret that must cover *.<sessionDomain>
  --oidc-issuer URL         OIDC issuer URL; discovery document must answer
  --postgres-dsn-secret NS/NAME[:KEY]
                            Secret holding the Postgres DSN (default key: url)
  --postgres-dsn-env VAR    environment variable holding the DSN
                            (default VAR: TCDI_PREFLIGHT_DSN)
  --postgres-ca-secret NS/NAME[:KEY]
                            Secret with the Postgres CA (default key: ca.crt)
  --node-label KEY=VALUE    workspace node-pool label (default cdi.tinyorbit.vn/workspace=true)
  --node-taint KEY          workspace node-pool taint key (default cdi.tinyorbit.vn/workspace)
  --allow-shared-nodes      shared-node opt-out is declared; skip the pool check
  --host-users-false        hostUsers: false is wanted; probe user namespaces
  --storage-class NAME      require this StorageClass (default: any default class)
  --min-k8s X.Y             minimum Kubernetes version (default 1.30)
  --probe-image IMAGE       image for the probe pods (default registry.k8s.io/e2e-test-images/agnhost:2.53)
  --no-probe                create nothing: NetworkPolicy and user-namespace checks WARN
  -h, --help                this help

Every flag also has a TCDI_PREFLIGHT_* environment variable
(see hack/preflight/README.md). Exit code 1 on any FAIL.
EOF
}

die_usage() { echo "preflight: $*" >&2; usage >&2; exit 2; }

while [ $# -gt 0 ]; do
  case "$1" in
    --context) CONTEXT="${2:?--context needs a value}"; shift 2 ;;
    --session-domain) SESSION_DOMAIN="${2:?--session-domain needs a value}"; shift 2 ;;
    --tls-secret) TLS_SECRET="${2:?--tls-secret needs a value}"; shift 2 ;;
    --oidc-issuer) OIDC_ISSUER="${2:?--oidc-issuer needs a value}"; shift 2 ;;
    --postgres-dsn-secret) DSN_SECRET="${2:?--postgres-dsn-secret needs a value}"; shift 2 ;;
    --postgres-dsn-env) DSN_ENV="${2:?--postgres-dsn-env needs a value}"; shift 2 ;;
    --postgres-ca-secret) CA_SECRET="${2:?--postgres-ca-secret needs a value}"; shift 2 ;;
    --node-label) NODE_LABEL="${2:?--node-label needs a value}"; shift 2 ;;
    --node-taint) NODE_TAINT="${2:?--node-taint needs a value}"; shift 2 ;;
    --allow-shared-nodes) ALLOW_SHARED=1; shift ;;
    --host-users-false) HOST_USERS_FALSE=1; shift ;;
    --storage-class) STORAGE_CLASS="${2:?--storage-class needs a value}"; shift 2 ;;
    --min-k8s) MIN_K8S="${2:?--min-k8s needs a value}"; shift 2 ;;
    --probe-image) PROBE_IMAGE="${2:?--probe-image needs a value}"; shift 2 ;;
    --no-probe) NO_PROBE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die_usage "unknown argument: $1" ;;
  esac
done

case "$MIN_K8S" in
  [0-9]*.[0-9]*) ;;
  *) die_usage "--min-k8s must look like 1.30" ;;
esac
case "$NODE_LABEL" in
  *=*) ;;
  *) die_usage "--node-label must be KEY=VALUE" ;;
esac

TMP="$(mktemp -d)"
RAND="$(printf '%06x' $(( (RANDOM << 15 | RANDOM) & 0xffffff )))"
PROBE_NS="tcdi-preflight-$RAND"
NS_CREATED=0
NPASS=0 NWARN=0 NFAIL=0
CLEANUP_FAILED=0

# ---- cleanup: the probe namespace and temp files always go away ----------
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [ "$NS_CREATED" = 1 ]; then
    if ! k delete namespace "$PROBE_NS" --ignore-not-found --wait=true --timeout=120s >"$TMP/del.out" 2>&1; then
      echo "FAIL  cleanup  could not delete probe namespace $PROBE_NS" >&2
      echo "      fix: kubectl delete namespace $PROBE_NS" >&2
      CLEANUP_FAILED=1
    fi
  fi
  rm -rf "$TMP"
  if [ "$CLEANUP_FAILED" = 1 ] && [ "$rc" = 0 ]; then rc=1; fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

k() {
  if [ -n "$CONTEXT" ]; then
    "$KUBECTL" --context "$CONTEXT" --request-timeout=30s "$@"
  else
    "$KUBECTL" --request-timeout=30s "$@"
  fi
}

# ---- result reporting -----------------------------------------------------
report() { # report STATUS CHECK DETAIL [FIX]
  local st="$1" check="$2" detail="$3" fix="${4:-}"
  printf '%-5s %-14s %s\n' "$st" "$check" "$detail"
  if [ -n "$fix" ] && [ "$st" != PASS ]; then printf '      fix: %s\n' "$fix"; fi
  case "$st" in
    PASS) NPASS=$((NPASS + 1)) ;;
    WARN) NWARN=$((NWARN + 1)) ;;
    FAIL) NFAIL=$((NFAIL + 1)) ;;
  esac
}

# kq <args...>: run kubectl, capture stdout in KOUT, stderr in KERR, status in KRC.
# KFORBID=1 when the apiserver answered Forbidden/Unauthorized.
KOUT="" KERR="" KRC=0 KFORBID=0
kq() {
  KOUT="$(k "$@" 2>"$TMP/err")"
  KRC=$?
  KERR="$(cat "$TMP/err" 2>/dev/null)"
  KFORBID=0
  if [ "$KRC" != 0 ] && printf '%s' "$KERR" | grep -qiE 'forbidden|unauthorized|cannot (create|get|list)'; then
    KFORBID=1
  fi
  return "$KRC"
}

# can_i <verb> <resource>: 0 yes, 1 no, 2 unknown
can_i() {
  local out
  out="$(k auth can-i "$1" "$2" 2>/dev/null)"
  case "$out" in
    yes) return 0 ;;
    no) return 1 ;;
    *) return 2 ;;
  esac
}

split_ref() { # split_ref NS/NAME[:KEY] DEFAULTKEY -> REF_NS REF_NAME REF_KEY
  local ref="$1" defkey="$2" nsname
  nsname="${ref%%:*}"
  REF_KEY="$defkey"
  case "$ref" in *:*) REF_KEY="${ref#*:}" ;; esac
  REF_NS="${nsname%%/*}"
  REF_NAME="${nsname#*/}"
  [ "$REF_NS" != "$nsname" ] && [ -n "$REF_NS" ] && [ -n "$REF_NAME" ] && [ -n "$REF_KEY" ]
}

# secret_value NS NAME KEY -> stdout (decoded); status 0 ok / 1 failed (see KFORBID)
secret_value() {
  local ns="$1" name="$2" key="$3" b64 jp
  jp="{.data.${key//./\\.}}"
  kq -n "$ns" get secret "$name" -o "jsonpath=$jp" || return 1
  b64="$KOUT"
  KOUT=""
  if [ -z "$b64" ]; then KERR="key $key not found in secret $ns/$name"; return 1; fi
  printf '%s' "$b64" | base64 -d 2>/dev/null
}

# ---- 1. Kubernetes version -----------------------------------------------
check_k8s_version() {
  local ver major minor want_major want_minor
  want_major="${MIN_K8S%%.*}"
  want_minor="${MIN_K8S#*.}"
  if ! kq get --raw /version; then
    if [ "$KFORBID" = 1 ]; then
      report WARN k8s-version "not permitted to read /version" "grant get on the /version non-resource URL, or check the version by hand"
    else
      report FAIL k8s-version "cannot reach the API server" "check --context / KUBECONFIG and network access to the apiserver"
    fi
    return
  fi
  ver="$(printf '%s' "$KOUT" | sed -n 's/.*"gitVersion"[[:space:]]*:[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/p' | head -n1)"
  major="${ver%%.*}"
  minor="${ver#*.}"; minor="${minor%%[!0-9]*}"
  case "$major$minor" in
    ''|*[!0-9]*) report WARN k8s-version "could not parse the server version" "check the Kubernetes version by hand (>= $MIN_K8S)"; return ;;
  esac
  if [ "$major" -gt "$want_major" ] || { [ "$major" -eq "$want_major" ] && [ "$minor" -ge "$want_minor" ]; }; then
    report PASS k8s-version "server v$ver (>= $MIN_K8S)"
  else
    report FAIL k8s-version "server v$ver is older than $MIN_K8S" "upgrade the cluster to Kubernetes $MIN_K8S or newer"
  fi
}

# ---- probe namespace (lazy) ----------------------------------------------
PROBE_STATE=""   # "" not tried, ok, noperm, noprobe, error
PROBE_REASON=""
ensure_probe_ns() {
  [ -n "$PROBE_STATE" ] && return
  if [ "$NO_PROBE" = 1 ]; then PROBE_STATE=noprobe; PROBE_REASON="skipped by --no-probe"; return; fi
  can_i create namespaces; local rc=$?
  if [ "$rc" = 1 ]; then PROBE_STATE=noperm; PROBE_REASON="not permitted to create a probe namespace"; return; fi
  NS_CREATED=1   # set before the call: an interrupted create is still cleaned up
  if kq create -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $PROBE_NS
  labels:
    app.kubernetes.io/managed-by: tcdi-preflight
    pod-security.kubernetes.io/enforce: restricted
EOF
  then
    PROBE_STATE=ok
  elif [ "$KFORBID" = 1 ]; then
    NS_CREATED=0; PROBE_STATE=noperm; PROBE_REASON="not permitted to create a probe namespace"
  else
    PROBE_STATE=error; PROBE_REASON="could not create probe namespace"
  fi
}

# probe_pod NAME ARGS_YAML [EXTRA_SPEC] [EXTRA_CONTAINER_SC] — restricted-PSS
# pod; $2 is the args yaml list body, $3 spec-level yaml, $4 is inserted into
# the container's securityContext block.
probe_pod() { # probe_pod NAME ARGS_YAML [EXTRA_SPEC] [EXTRA_CONTAINER_SC]
  local name="$1" args="$2" extra="${3:-}" csc="${4:-}"
  kq -n "$PROBE_NS" create -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  labels:
    app: $name
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
${extra}
  securityContext:
    runAsNonRoot: true
    runAsUser: 65534
    runAsGroup: 65534
    seccompProfile:
      type: RuntimeDefault
  containers:
  - name: probe
    image: $PROBE_IMAGE
    args:
${args}
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
${csc}
      capabilities:
        drop: [ALL]
    resources:
      limits: {cpu: 100m, memory: 64Mi}
EOF
}

pod_phase() { kq -n "$PROBE_NS" get pod "$1" -o 'jsonpath={.status.phase}' && printf '%s' "$KOUT"; }

# wait_done POD: poll until Succeeded/Failed; echoes the phase, or "Timeout".
wait_done() {
  local pod="$1" deadline=$((SECONDS + POD_TIMEOUT)) ph
  while :; do
    ph="$(pod_phase "$pod")"
    case "$ph" in Succeeded|Failed) printf '%s' "$ph"; return ;; esac
    [ "$SECONDS" -ge "$deadline" ] && { printf 'Timeout'; return; }
    sleep "$POLL"
  done
}

# ---- 2. NetworkPolicy enforcement ----------------------------------------
check_netpol() {
  local ip ph attempt
  ensure_probe_ns
  case "$PROBE_STATE" in
    noprobe|noperm)
      report WARN netpol "NetworkPolicy enforcement not checked: $PROBE_REASON" \
        "run preflight with rights to create a namespace and pods, or verify the CNI enforces NetworkPolicy by hand"
      return ;;
    error)
      report WARN netpol "NetworkPolicy enforcement not checked: $PROBE_REASON" "see kubectl error output; retry or verify by hand"
      return ;;
  esac
  if ! probe_pod probe-server "    - netexec
    - --http-port=8080"; then
    if [ "$KFORBID" = 1 ]; then
      report WARN netpol "NetworkPolicy enforcement not checked: not permitted to create pods" "grant create on pods in a throwaway namespace"
    else
      report WARN netpol "NetworkPolicy enforcement not checked: probe pod rejected" "check pod security admission and --probe-image"
    fi
    return
  fi
  if ! kq -n "$PROBE_NS" wait --for=condition=Ready pod/probe-server --timeout="${POD_TIMEOUT}s"; then
    report WARN netpol "NetworkPolicy enforcement not checked: probe server did not become Ready" \
      "make --probe-image ($PROBE_IMAGE) pullable from this cluster, then re-run"
    return
  fi
  kq -n "$PROBE_NS" get pod probe-server -o 'jsonpath={.status.podIP}'
  ip="$KOUT"
  if [ -z "$ip" ]; then
    report WARN netpol "NetworkPolicy enforcement not checked: probe server has no pod IP" "re-run; check the CNI"
    return
  fi
  # Baseline: with no policy the client must reach the server.
  probe_pod probe-client-allow "    - connect
    - --timeout=5s
    - $ip:8080" >/dev/null
  ph="$(wait_done probe-client-allow)"
  if [ "$ph" != Succeeded ]; then
    report WARN netpol "NetworkPolicy enforcement not checked: baseline pod-to-pod traffic failed" \
      "pod networking is broken or the probe image cannot run; fix pod networking, then re-run"
    return
  fi
  # Deny all ingress to the server; the same connection must now fail.
  if ! kq -n "$PROBE_NS" create -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: probe-deny-ingress
spec:
  podSelector: {}
  policyTypes: [Ingress]
EOF
  then
    if [ "$KFORBID" = 1 ]; then
      report WARN netpol "NetworkPolicy enforcement not checked: not permitted to create NetworkPolicy" "grant create on networkpolicies in a throwaway namespace"
    else
      report WARN netpol "NetworkPolicy enforcement not checked: policy rejected" "see kubectl error output"
    fi
    return
  fi
  for attempt in 1 2 3; do
    sleep "$PROPAGATE"   # the CNI programs policies asynchronously
    probe_pod "probe-client-deny-$attempt" "    - connect
    - --timeout=5s
    - $ip:8080" >/dev/null
    ph="$(wait_done "probe-client-deny-$attempt")"
    case "$ph" in
      Failed) report PASS netpol "deny-ingress policy blocked the probe connection"; return ;;
      Succeeded) ;;
      *) report WARN netpol "NetworkPolicy enforcement not checked: deny probe pod did not finish" "re-run; check the node and the probe image"; return ;;
    esac
  done
  report FAIL netpol "a deny-all-ingress policy did not block traffic: NetworkPolicy is not enforced" \
    "install a CNI that enforces NetworkPolicy (Cilium, Calico, canal); the chart's default-deny baseline is otherwise inert"
}

# ---- 3. StorageClass ------------------------------------------------------
check_storage_class() {
  local name def have_default=0 found=0
  if ! kq get storageclass -o 'jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}{"\n"}{end}'; then
    if [ "$KFORBID" = 1 ]; then
      report WARN storageclass "not permitted to list StorageClasses" "grant list on storageclasses.storage.k8s.io, or verify by hand"
    else
      report WARN storageclass "could not list StorageClasses" "see kubectl error output; verify by hand"
    fi
    return
  fi
  while IFS=$'\t' read -r name def; do
    [ -z "$name" ] && continue
    [ "$def" = true ] && have_default=1
    [ -n "$STORAGE_CLASS" ] && [ "$name" = "$STORAGE_CLASS" ] && found=1
  done <<<"$KOUT"
  if [ -n "$STORAGE_CLASS" ]; then
    if [ "$found" = 1 ]; then
      report PASS storageclass "StorageClass $STORAGE_CLASS exists"
    else
      report FAIL storageclass "StorageClass $STORAGE_CLASS not found" "create it, or set the chart's storageClass / template storage-class annotation to an existing class"
    fi
  elif [ "$have_default" = 1 ]; then
    report PASS storageclass "a default StorageClass exists"
  else
    report FAIL storageclass "no default StorageClass" \
      "mark one default (storageclass.kubernetes.io/is-default-class=true) or pass --storage-class and set the chart's storageClass"
  fi
}

# ---- 4. user namespaces ---------------------------------------------------
check_host_users() {
  local extra="" ph
  if [ "$HOST_USERS_FALSE" != 1 ]; then
    report PASS host-users "not requested (pass --host-users-false to probe user namespaces)"
    return
  fi
  ensure_probe_ns
  case "$PROBE_STATE" in
    noprobe|noperm|error)
      report WARN host-users "user namespaces not checked: $PROBE_REASON" \
        "run preflight with rights to create a namespace and pods, or set runtime.hostUsers=false only after verifying by hand"
      return ;;
  esac
  # Probe on the workspace pool when there is one: that is where workspaces run.
  if [ "$ALLOW_SHARED" != 1 ]; then
    extra="  nodeSelector:
    ${NODE_LABEL%%=*}: \"${NODE_LABEL#*=}\"
  tolerations:
  - key: ${NODE_TAINT%%=*}
    operator: Exists"
  fi
  extra="  hostUsers: false${extra:+
$extra}"
  if ! probe_pod probe-userns "    - netexec
    - --http-port=8080" "$extra"; then
    if [ "$KFORBID" = 1 ]; then
      report WARN host-users "user namespaces not checked: not permitted to create pods" "grant create on pods in a throwaway namespace"
    else
      report FAIL host-users "a pod with hostUsers: false was rejected" \
        "enable the UserNamespacesSupport feature gate on the apiserver and kubelets, or install with runtime.hostUsers=false off"
    fi
    return
  fi
  if kq -n "$PROBE_NS" wait --for=condition=Ready pod/probe-userns --timeout="${POD_TIMEOUT}s"; then
    report PASS host-users "a pod with hostUsers: false started"
  else
    ph="$(pod_phase probe-userns)"
    report FAIL host-users "a pod with hostUsers: false did not start (phase ${ph:-unknown})" \
      "needs Kubernetes >= 1.33 (or the UserNamespacesSupport gate), a runtime with user-namespace support and idmapped mounts on the workspace nodes"
  fi
}

# ---- 5. workspace node pool ----------------------------------------------
check_node_pool() {
  local lines n taints name tk
  if [ "$ALLOW_SHARED" = 1 ]; then
    report PASS node-pool "shared-node opt-out declared (runtime.placement.allowSharedNodes: true)"
    return
  fi
  if ! kq get nodes -l "$NODE_LABEL" -o 'jsonpath={range .items[*]}{.metadata.name}{"\t"}{range .spec.taints[*]}{.key}{";"}{end}{"\n"}{end}'; then
    if [ "$KFORBID" = 1 ]; then
      report WARN node-pool "not permitted to list nodes" "grant list on nodes, or verify the pool label and taint by hand"
    else
      report WARN node-pool "could not list nodes" "see kubectl error output; verify by hand"
    fi
    return
  fi
  lines="$(printf '%s\n' "$KOUT" | sed '/^$/d')"
  if [ -z "$lines" ]; then
    report FAIL node-pool "no node has the label $NODE_LABEL" \
      "kubectl label node <node> $NODE_LABEL (and taint it), or declare --allow-shared-nodes / runtime.placement.allowSharedNodes=true"
    return
  fi
  n=0; local untainted=""
  tk="${NODE_TAINT%%=*}"; tk="${tk%%:*}"
  while IFS=$'\t' read -r name taints; do
    n=$((n + 1))
    case ";$taints" in *";$tk;"*) ;; *) untainted="$untainted $name" ;; esac
  done <<<"$lines"
  if [ -z "$untainted" ]; then
    report PASS node-pool "$n node(s) labeled $NODE_LABEL and tainted $tk"
  else
    report WARN node-pool "$n labeled node(s); not tainted $tk:$untainted" \
      "kubectl taint node <node> $tk:NoSchedule so unrelated workloads stay off the pool"
  fi
}

# ---- 5b. AppArmor on the workspace nodes ----------------------------------
# The chart sets securityContext.appArmorProfile=RuntimeDefault on runtime
# containers (runtime.appArmor.requireRuntimeDefault, default true); a node
# without AppArmor refuses such pods, so the value must fit the nodes that
# will run workspaces. Node status reports no AppArmor signal, so each target
# node gets one short-lived probe pod, pinned to it with spec.nodeName and
# tolerating every taint, that asks for the same RuntimeDefault profile. The
# kubelet/container-runtime refusal on a host without AppArmor ("Cannot
# enforce AppArmor") is the exact failure a workspace pod would hit — the
# definitive read-only signal — and it lands in the pod status or events.
AA_EVENTS=""
AA_EVENTS_FETCHED=0
aa_event_apparmor() { # aa_event_apparmor POD — 0 when POD's events mention AppArmor
  local pod="$1"
  if [ "$AA_EVENTS_FETCHED" = 0 ]; then
    AA_EVENTS_FETCHED=1
    kq -n "$PROBE_NS" get events -o 'jsonpath={range .items[*]}{.involvedObject.name}{"|"}{.reason}{"|"}{.message}{";"}{end}' && AA_EVENTS="$KOUT"
  fi
  printf '%s' "$AA_EVENTS" | tr ';' '\n' | grep -iF "$pod|" | grep -qi apparmor
}

# aa_pod_state POD -> ok | noaa | failed | pending
aa_pod_state() {
  local pod="$1" out ph
  if ! kq -n "$PROBE_NS" get pod "$pod" -o 'jsonpath={.status.phase}{"\t"}{.status.reason}{"|"}{.status.message}{"\t"}{range .status.containerStatuses[*]}{.state.waiting.reason}{"|"}{.state.waiting.message}{";"}{end}'; then
    printf 'pending'; return
  fi
  out="$KOUT"; ph="${out%%$'\t'*}"
  case "$ph" in Succeeded|Running) printf 'ok'; return ;; esac
  case "$(printf '%s' "$out" | tr '[:upper:]' '[:lower:]')" in
    *apparmor*) printf 'noaa'; return ;;
  esac
  case "$ph" in
    Failed) printf 'failed'; return ;;
  esac
  # A container-create refusal without the word AppArmor stays inconclusive:
  # the cause may only sit in the pod's events (checked once at the end).
  case "$out" in
    *CreateContainerError*|*CreatePodSandboxError*|*RunContainerError*) printf 'failed'; return ;;
  esac
  printf 'pending'
}

check_apparmor() {
  local lines name os unsched taints tk tkey teff taint skip i s
  local -a nodes=() pods=() st=()
  tk="${NODE_TAINT%%=*}"; tk="${tk%%:*}"
  # The target set is the placement the chart would use: with a dedicated
  # pool the labeled nodes (runtime.placement.nodeSelector), skipping nodes
  # a workspace pod could not reach (unschedulable, or a NoSchedule/NoExecute
  # taint the runtime tolerations do not cover); with --allow-shared-nodes
  # every schedulable untainted Linux node, since runtime pods then carry no
  # tolerations at all.
  # Field separator is |, not a tab: empty unschedulable/taint fields must not
  # collapse, which IFS whitespace would do.
  if [ "$ALLOW_SHARED" = 1 ]; then
    kq get nodes -o 'jsonpath={range .items[*]}{.metadata.name}{"|"}{.status.nodeInfo.operatingSystem}{"|"}{.spec.unschedulable}{"|"}{range .spec.taints[*]}{.key}{":"}{.effect}{";"}{end}{"\n"}{end}'
  else
    kq get nodes -l "$NODE_LABEL" -o 'jsonpath={range .items[*]}{.metadata.name}{"|"}{.status.nodeInfo.operatingSystem}{"|"}{.spec.unschedulable}{"|"}{range .spec.taints[*]}{.key}{":"}{.effect}{";"}{end}{"\n"}{end}'
  fi
  if [ "$KRC" != 0 ]; then
    if [ "$KFORBID" = 1 ]; then
      report WARN apparmor "not permitted to list nodes" "grant list on nodes, or check AppArmor on each workspace node by hand"
    else
      report WARN apparmor "could not list nodes" "see kubectl error output; check AppArmor on each workspace node by hand"
    fi
    return
  fi
  lines="$(printf '%s\n' "$KOUT" | sed '/^$/d')"
  while IFS='|' read -r name os unsched taints; do
    [ -z "$name" ] && continue
    [ "$os" = linux ] || continue
    [ "$unsched" = true ] && continue
    skip=0
    for taint in ${taints//;/ }; do
      tkey="${taint%%:*}"; teff="${taint##*:}"
      case "$teff" in
        # Only the workspace taint key's NoSchedule effect is covered by the
        # chart's default runtime.placement.tolerations; shared-node pods
        # carry no tolerations at all, and NoExecute is never covered.
        NoSchedule)
          if [ "$ALLOW_SHARED" = 1 ] || [ "$tkey" != "$tk" ]; then skip=1; fi ;;
        NoExecute) skip=1 ;;
      esac
    done
    [ "$skip" = 0 ] && nodes+=("$name")
  done <<<"$lines"
  if [ "${#nodes[@]}" = 0 ]; then
    if [ "$ALLOW_SHARED" = 1 ]; then
      report WARN apparmor "no schedulable untainted Linux node found" "check the nodes by hand: cat /sys/module/apparmor/parameters/enabled"
    else
      report WARN apparmor "no schedulable node carries $NODE_LABEL" "fix the node pool first (see node-pool), or pass --allow-shared-nodes"
    fi
    return
  fi
  ensure_probe_ns
  case "$PROBE_STATE" in
    noprobe|noperm|error)
      report WARN apparmor "AppArmor not probed on ${#nodes[@]} workspace node(s): $PROBE_REASON" \
        "check each workspace node by hand: cat /sys/module/apparmor/parameters/enabled; if any lacks AppArmor set runtime.appArmor.requireRuntimeDefault=false"
      return ;;
  esac
  for name in "${nodes[@]}"; do
    if ! probe_pod "probe-aa-${#pods[@]}" "    - entrypoint-tester" \
"  nodeName: $name
  tolerations:
  - operator: Exists" \
"      appArmorProfile:
        type: RuntimeDefault"; then
      if [ "$KFORBID" = 1 ]; then
        report WARN apparmor "not permitted to create probe pods" "grant create on pods in a throwaway namespace, or check AppArmor per node by hand"
      else
        case "$KERR" in
          *appArmorProfile*|*apparmor*)
            report WARN apparmor "the API server rejected securityContext.appArmorProfile (the field needs Kubernetes >= 1.30)" \
              "check AppArmor on each workspace node by hand: cat /sys/module/apparmor/parameters/enabled" ;;
          *)
            report WARN apparmor "AppArmor probe pod rejected" "see kubectl error output; check AppArmor per node by hand" ;;
        esac
      fi
      return
    fi
    pods+=("probe-aa-${#pods[@]}")
  done
  # Poll all pods in parallel until each resolves or the deadline passes.
  local deadline=$((SECONDS + POD_TIMEOUT)) pending=1
  while [ "$pending" = 1 ] && [ "$SECONDS" -lt "$deadline" ]; do
    pending=0
    for i in "${!pods[@]}"; do
      [ -n "${st[$i]:-}" ] && continue
      s="$(aa_pod_state "${pods[$i]}")"
      if [ "$s" = pending ]; then pending=1; else st[i]="$s"; fi
    done
    [ "$pending" = 1 ] && [ "$SECONDS" -lt "$deadline" ] && sleep "$POLL"
  done
  local ok="" noaa="" unknown=""
  for i in "${!pods[@]}"; do
    case "${st[$i]:-timeout}" in
      ok)   ok="$ok ${nodes[$i]}" ;;
      noaa) noaa="$noaa ${nodes[$i]}" ;;
      *)
        if aa_event_apparmor "${pods[$i]}"; then noaa="$noaa ${nodes[$i]}"; else unknown="$unknown ${nodes[$i]}"; fi ;;
    esac
  done
  ok="${ok# }"; noaa="${noaa# }"; unknown="${unknown# }"
  if [ -z "$noaa" ] && [ -z "$unknown" ]; then
    report PASS apparmor "AppArmor on all ${#nodes[@]} workspace node(s): runtime.appArmor.requireRuntimeDefault=true (the chart default) fits"
    return
  fi
  local detail=""
  [ -n "$noaa" ] && detail="no AppArmor: $noaa"
  [ -n "$unknown" ] && detail="${detail:+$detail; }AppArmor not determined: $unknown"
  report WARN apparmor "$detail" \
    "set runtime.appArmor.requireRuntimeDefault=false (nodes without AppArmor refuse the RuntimeDefault profile — see the chart README); when every workspace node has AppArmor, true fits"
}

# ---- 6. wildcard DNS ------------------------------------------------------
check_dns() {
  if [ -z "$SESSION_DOMAIN" ]; then
    report WARN dns "no session domain given; wildcard DNS not checked" "pass --session-domain <domain>"
    return
  fi
  if ! command -v "$GETENT" >/dev/null 2>&1; then
    report WARN dns "no resolver tool (getent) on this host" "check by hand: dig +short preflight-$RAND.$SESSION_DOMAIN"
    return
  fi
  if "$GETENT" hosts "preflight-$RAND.$SESSION_DOMAIN" >/dev/null 2>&1; then
    report PASS dns "*.$SESSION_DOMAIN resolves"
  else
    report FAIL dns "preflight-<random>.$SESSION_DOMAIN does not resolve" \
      "create a wildcard record *.$SESSION_DOMAIN (A or CNAME) pointing at the ingress/gateway"
  fi
}

# ---- 7. TLS secret covers the wildcard ------------------------------------
check_tls() {
  local host="preflight-$RAND.${SESSION_DOMAIN}" out
  if [ -z "$TLS_SECRET" ] || [ -z "$SESSION_DOMAIN" ]; then
    report WARN tls-secret "no TLS secret or session domain given; certificate not checked" "pass --tls-secret <ns>/<name> and --session-domain <domain>"
    return
  fi
  if ! split_ref "$TLS_SECRET" tls.crt; then
    report FAIL tls-secret "bad --tls-secret value (want NAMESPACE/NAME)" "use --tls-secret <namespace>/<name>"
    return
  fi
  if ! command -v "$OPENSSL" >/dev/null 2>&1; then
    report WARN tls-secret "openssl not found on this host" "install openssl, or check the SAN list by hand"
    return
  fi
  if ! secret_value "$REF_NS" "$REF_NAME" "$REF_KEY" >"$TMP/tls.crt"; then
    if [ "$KFORBID" = 1 ]; then
      report WARN tls-secret "not permitted to read secret $REF_NS/$REF_NAME" "grant get on that secret, or verify the certificate by hand"
    else
      report FAIL tls-secret "cannot read secret $REF_NS/$REF_NAME ($REF_KEY)" "create the kubernetes.io/tls Secret with a certificate for *.$SESSION_DOMAIN"
    fi
    return
  fi
  out="$("$OPENSSL" x509 -noout -checkhost "$host" -in "$TMP/tls.crt" 2>&1)"
  case "$out" in
    *"does match certificate"*) ;;
    *"does NOT match"*)
      report FAIL tls-secret "certificate in $REF_NS/$REF_NAME does not cover *.$SESSION_DOMAIN" \
        "issue a wildcard certificate for *.$SESSION_DOMAIN (DNS-01 with cert-manager) into that Secret"
      return ;;
    *)
      report FAIL tls-secret "$REF_NS/$REF_NAME/$REF_KEY is not a readable PEM certificate" "store the PEM chain in the $REF_KEY key"
      return ;;
  esac
  if ! "$OPENSSL" x509 -noout -checkend 0 -in "$TMP/tls.crt" >/dev/null 2>&1; then
    report FAIL tls-secret "certificate in $REF_NS/$REF_NAME has expired" "renew the certificate"
  elif ! "$OPENSSL" x509 -noout -checkend 1209600 -in "$TMP/tls.crt" >/dev/null 2>&1; then
    report WARN tls-secret "certificate covers *.$SESSION_DOMAIN but expires within 14 days" "renew the certificate (or check cert-manager)"
  else
    report PASS tls-secret "certificate in $REF_NS/$REF_NAME covers *.$SESSION_DOMAIN"
  fi
}

# ---- 8. OIDC discovery ----------------------------------------------------
check_oidc() {
  local base body got
  if [ -z "$OIDC_ISSUER" ]; then
    report WARN oidc "no issuer given; discovery not checked" "pass --oidc-issuer <url>"
    return
  fi
  if ! command -v "$CURL" >/dev/null 2>&1; then
    report WARN oidc "curl not found on this host" "install curl, or fetch <issuer>/.well-known/openid-configuration by hand"
    return
  fi
  base="${OIDC_ISSUER%/}"
  if body="$("$CURL" -fsSL --max-time 10 -- "$base/.well-known/openid-configuration" 2>/dev/null)"; then :; else
    report FAIL oidc "discovery document did not answer at the issuer URL" \
      "check --oidc-issuer (Keycloak: https://<host>/realms/<realm>) and that it is reachable"
    return
  fi
  got="$(printf '%s' "$body" | tr -d '\n' | sed -n 's/.*"issuer"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
  if [ "${got%/}" != "$base" ]; then
    report FAIL oidc "discovery answered but its issuer does not match $base" \
      "set oidc.issuer exactly to the issuer the IdP reports (scheme, host, path)"
    return
  fi
  case "$base" in
    http://*) report WARN oidc "discovery works but the issuer is plain http" "serve the IdP over https" ;;
    *) report PASS oidc "discovery document answers and the issuer matches" ;;
  esac
}

# ---- Postgres DSN splitting ---------------------------------------------------
# The DSN holds the password, so it must never reach psql's argument list (any
# user on the host can read that from the process list). It is split into
# PG* variables instead, which are exported only inside the subshell that runs
# psql. PGENV holds "NAME=value" entries; nothing here runs an external command.
PGENV=()
DSN_BADKEY=""

# dsn_set KEY VALUE: queue the PG* variable for a libpq connection parameter.
# Returns 1 (and records the key name, never the value) for a parameter that
# has no environment variable.
dsn_set() {
  local v
  case "$1" in
    host) v=PGHOST ;;
    hostaddr) v=PGHOSTADDR ;;
    port) v=PGPORT ;;
    dbname) v=PGDATABASE ;;
    user) v=PGUSER ;;
    password) v=PGPASSWORD ;;
    passfile) v=PGPASSFILE ;;
    sslmode) v=PGSSLMODE ;;
    sslrootcert) v=PGSSLROOTCERT ;;
    sslcert) v=PGSSLCERT ;;
    sslkey) v=PGSSLKEY ;;
    sslcrl) v=PGSSLCRL ;;
    sslcrldir) v=PGSSLCRLDIR ;;
    sslsni) v=PGSSLSNI ;;
    sslcompression) v=PGSSLCOMPRESSION ;;
    ssl_min_protocol_version) v=PGSSLMINPROTOCOLVERSION ;;
    ssl_max_protocol_version) v=PGSSLMAXPROTOCOLVERSION ;;
    channel_binding) v=PGCHANNELBINDING ;;
    connect_timeout) v=PGCONNECT_TIMEOUT ;;
    application_name) v=PGAPPNAME ;;
    options) v=PGOPTIONS ;;
    service) v=PGSERVICE ;;
    target_session_attrs) v=PGTARGETSESSIONATTRS ;;
    gssencmode) v=PGGSSENCMODE ;;
    krbsrvname) v=PGKRBSRVNAME ;;
    requirepeer) v=PGREQUIREPEER ;;
    *) DSN_BADKEY="$1"; return 1 ;;
  esac
  PGENV+=("$v=$2")
}

# urldecode STRING: percent-decode into $REPLY; status 1 on a malformed escape.
urldecode() {
  local s="$1" out="" c
  while [[ "$s" == *%* ]]; do
    [[ "$s" =~ ^([^%]*)%([0-9A-Fa-f]{2}) ]] || return 1
    out+="${BASH_REMATCH[1]}"
    printf -v c '%b' "\\x${BASH_REMATCH[2]}"
    out+="$c"
    s="${s:$((${#BASH_REMATCH[1]} + 3))}"
  done
  REPLY="$out$s"
}

# dsn_from_url URL: postgres[ql]://[user[:password]@]host[:port][,host2[:port2]][/db][?k=v&...]
dsn_from_url() {
  local rest="${1#*://}" query="" path="" auth userinfo="" hostpart user pass item host port
  local hosts="" ports="" anyport="" pair k v
  local -a items pairs
  case "$rest" in *\?*) query="${rest#*\?}"; rest="${rest%%\?*}" ;; esac
  case "$rest" in */*) path="${rest#*/}"; auth="${rest%%/*}" ;; *) auth="$rest" ;; esac
  case "$auth" in
    *@*) userinfo="${auth%@*}"; hostpart="${auth##*@}" ;;
    *) hostpart="$auth" ;;
  esac
  if [ -n "$userinfo" ]; then
    user="${userinfo%%:*}"
    if [ "$user" != "$userinfo" ]; then
      pass="${userinfo#*:}"
      urldecode "$pass" || return 1
      dsn_set password "$REPLY" || return 1
    fi
    if [ -n "$user" ]; then
      urldecode "$user" || return 1
      dsn_set user "$REPLY" || return 1
    fi
  fi
  if [ -n "$hostpart" ]; then
    IFS=, read -ra items <<<"$hostpart"
    for item in "${items[@]}"; do
      case "$item" in
        \[*\]*) host="${item%%]*}"; host="${host#[}"; port="${item#*]}"; port="${port#:}" ;;
        *) host="${item%%:*}"; port=""; [ "$host" != "$item" ] && port="${item#*:}" ;;
      esac
      urldecode "$host" || return 1
      hosts+="${hosts:+,}$REPLY"
      urldecode "$port" || return 1
      ports+="${ports:+,}$REPLY"
      [ -n "$port" ] && anyport=1
    done
    [ -n "${hosts//,/}" ] && { dsn_set host "$hosts" || return 1; }
    [ -n "$anyport" ] && { dsn_set port "$ports" || return 1; }
  fi
  if [ -n "$path" ]; then
    urldecode "$path" || return 1
    dsn_set dbname "$REPLY" || return 1
  fi
  if [ -n "$query" ]; then
    IFS='&' read -ra pairs <<<"$query"
    for pair in "${pairs[@]}"; do
      [ -n "$pair" ] || continue
      k="${pair%%=*}"; v=""
      [ "$k" != "$pair" ] && v="${pair#*=}"
      urldecode "$k" || return 1; k="$REPLY"
      urldecode "$v" || return 1
      dsn_set "$k" "$REPLY" || return 1
    done
  fi
}

# dsn_from_kv STRING: libpq "key=value key2='quoted \' value'" form.
dsn_from_kv() {
  local s="$1" key val c closed
  while :; do
    s="${s#"${s%%[![:space:]]*}"}"
    [ -n "$s" ] || break
    key="${s%%=*}"
    [ "$key" != "$s" ] || return 1
    key="${key%"${key##*[![:space:]]}"}"
    s="${s#*=}"
    s="${s#"${s%%[![:space:]]*}"}"
    val=""
    if [ "${s:0:1}" = "'" ]; then
      s="${s:1}"; closed=""
      while [ -n "$s" ]; do
        c="${s:0:1}"; s="${s:1}"
        case "$c" in
          \\) val+="${s:0:1}"; s="${s:1}" ;;
          "'") closed=1; break ;;
          *) val+="$c" ;;
        esac
      done
      [ -n "$closed" ] || return 1
    else
      while [ -n "$s" ]; do
        c="${s:0:1}"
        case "$c" in
          [[:space:]]) break ;;
          \\) s="${s:1}"; val+="${s:0:1}"; s="${s:1}" ;;
          *) val+="$c"; s="${s:1}" ;;
        esac
      done
    fi
    dsn_set "$key" "$val" || return 1
  done
}

# dsn_split DSN: fill PGENV from a URL, a key=value string or a bare database name.
dsn_split() {
  DSN_BADKEY=""
  case "$1" in
    postgres://*|postgresql://*) dsn_from_url "$1" ;;
    *=*) dsn_from_kv "$1" ;;
    *) dsn_set dbname "$1" ;;
  esac
}

# pgenv_get NAME: value of the last PGENV entry for NAME in $REPLY; status 1 if unset.
pgenv_get() {
  local e i
  for ((i = ${#PGENV[@]} - 1; i >= 0; i--)); do
    e="${PGENV[i]}"
    if [ "${e%%=*}" = "$1" ]; then REPLY="${e#*=}"; return 0; fi
  done
  return 1
}

# ---- 9. Postgres with TLS verification ------------------------------------
check_postgres() {
  local dsn="" mode="" err rc e
  if [ -n "$DSN_SECRET" ]; then
    if ! split_ref "$DSN_SECRET" url; then
      report FAIL postgres "bad --postgres-dsn-secret value (want NAMESPACE/NAME[:KEY])" "use --postgres-dsn-secret <namespace>/<name>"
      return
    fi
    # Into a file, not $(...): KFORBID must survive in this shell. $TMP is 0700.
    if secret_value "$REF_NS" "$REF_NAME" "$REF_KEY" >"$TMP/dsn"; then
      dsn="$(cat "$TMP/dsn")"
      rm -f "$TMP/dsn"
    else
      if [ "$KFORBID" = 1 ]; then
        report WARN postgres "not permitted to read the DSN secret $REF_NS/$REF_NAME" "grant get on that secret, or pass the DSN via env (--postgres-dsn-env)"
      else
        report FAIL postgres "cannot read the DSN secret $REF_NS/$REF_NAME ($REF_KEY)" "create it with the DSN under key $REF_KEY"
      fi
      return
    fi
  elif [ -n "${!DSN_ENV:-}" ]; then
    dsn="${!DSN_ENV}"
  else
    report WARN postgres "no DSN source given; database not checked" "pass --postgres-dsn-secret <ns>/<name> or set \$$DSN_ENV"
    return
  fi
  if ! command -v "$PSQL" >/dev/null 2>&1; then
    report WARN postgres "psql not found on this host" "install the PostgreSQL client, or check connectivity and TLS by hand"
    return
  fi
  PGENV=("PGCONNECT_TIMEOUT=10")
  if ! dsn_split "$dsn"; then
    dsn=""; PGENV=()
    if [ -n "$DSN_BADKEY" ]; then
      report WARN postgres "the DSN sets '$DSN_BADKEY', which preflight cannot hand to psql without putting the DSN on its command line; database not checked" \
        "check connectivity and TLS by hand, or drop that parameter from the DSN used for preflight"
    else
      report FAIL postgres "the DSN is not a valid Postgres URL or key=value string" "fix the DSN (a URL needs percent-encoded special characters in the password)"
    fi
    return
  fi
  dsn=""
  if ! pgenv_get PGSSLMODE; then
    PGENV+=("PGSSLMODE=${PGSSLMODE:-verify-full}")
    pgenv_get PGSSLMODE
  fi
  mode="$REPLY"
  if [ -n "$CA_SECRET" ]; then
    if ! split_ref "$CA_SECRET" ca.crt; then
      report FAIL postgres "bad --postgres-ca-secret value (want NAMESPACE/NAME[:KEY])" "use --postgres-ca-secret <namespace>/<name>"
      return
    fi
    if secret_value "$REF_NS" "$REF_NAME" "$REF_KEY" >"$TMP/db-ca.crt"; then
      PGENV+=("PGSSLROOTCERT=$TMP/db-ca.crt")
    elif [ "$KFORBID" = 1 ]; then
      report WARN postgres "not permitted to read the CA secret $REF_NS/$REF_NAME" "grant get on that secret, or set PGSSLROOTCERT yourself"
      return
    else
      report FAIL postgres "cannot read the CA secret $REF_NS/$REF_NAME ($REF_KEY)" "create it with the CA under key $REF_KEY"
      return
    fi
  fi
  # The connection parameters, password included, travel in psql's environment
  # only: the exports happen inside this subshell, and psql gets no DSN argument.
  # shellcheck disable=SC2163  # "$e" is a NAME=value entry on purpose
  err="$(for e in "${PGENV[@]}"; do export "$e"; done; "$PSQL" -X -w -Atq -c 'select 1' 2>&1 >/dev/null)"
  rc=$?
  PGENV=()
  if [ "$rc" = 0 ]; then
    case "$mode" in
      verify-full|verify-ca) report PASS postgres "reachable with TLS verification ($mode)" ;;
      *) report WARN postgres "reachable, but sslmode=$mode does not verify the server certificate" \
           "use sslmode=verify-full (database.tls.mode) with the CA in database.tls.caSecret" ;;
    esac
    return
  fi
  case "$err" in
    *"certificate verify failed"*|*"root certificate file"*|*"server certificate"*)
      report FAIL postgres "TLS verification failed" "pass the CA with --postgres-ca-secret (chart: database.tls.caSecret) and check the server certificate name" ;;
    *"does not support SSL"*|*"SSL error"*|*"SSL is not enabled"*)
      report FAIL postgres "the server does not accept TLS ($mode)" "enable TLS on PostgreSQL, or lower database.tls.mode on a link you trust" ;;
    *"authentication failed"*)
      report FAIL postgres "reachable with TLS but authentication failed" "fix the role/password in the DSN" ;;
    *)
      # A cluster-internal database is normally unreachable from the operator's
      # workstation, so a connection-level failure is a WARN, not a FAIL.
      report WARN postgres "not reachable from this host (connection failed before TLS or authentication)" \
        "if the database is cluster-internal, run preflight from a pod or through kubectl port-forward; otherwise check host, port and firewall" ;;
  esac
}

echo "TinyCDI preflight — probe namespace (if needed): $PROBE_NS"
check_k8s_version
check_netpol
check_storage_class
check_host_users
check_node_pool
check_apparmor
check_dns
check_tls
check_oidc
check_postgres

echo
echo "preflight: $NPASS PASS, $NWARN WARN, $NFAIL FAIL"
[ "$NFAIL" -eq 0 ]
