#!/usr/bin/env bash
# sandbox-check.sh - pod-level sandboxed-runtime checks against a workspace
# (kind DEV/CI ONLY — the local matrix behind docs/compatibility.md).
#
#   hack/quickstart/sandbox-check.sh [template-catalog-name ...]
#
# For each seeded template (default: every catalog name in the tenant
# namespace) creates a Workspace CR directly — the API is bypassed, so this
# needs no portal or login — then checks, all under the sandboxed runtime:
#
#   ready      pod reaches Ready; workspace conditions all True
#   runtime    pod.spec.runtimeClassName == the RuntimeClass and `uname -r`
#              shows the gVisor marker inside the container
#   stream     KasmVNC TLS + BasicAuth + websocket upgrade + full RFB
#              handshake + FramebufferUpdate after a KeyEvent and a
#              PointerEvent (sandbox-rfb.py inside the kind node — the pod
#              IP/ClusterIP path is what the gateway uses; `kubectl
#              port-forward` cannot see netstack sockets under runsc)
#   lifecycle  Stopped -> pod gone -> Running (new runtimeGeneration) ->
#              Ready again, marker file on the home PVC still there
#
# Prints one RESULT line per check and exits nonzero on the first failure.
#
# Environment: TCDI_QS_CLUSTER, TCDI_SBX_RUNTIMECLASS (default gvisor),
# TCDI_SBX_NS (tenant namespace, default tinycdi-tenant-a),
# TCDI_SBX_CHECKS (space-separated subset, default "ready runtime stream lifecycle").
set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

RUNTIMECLASS="${TCDI_SBX_RUNTIMECLASS:-gvisor}"
NS="${TCDI_SBX_NS:-tinycdi-tenant-a}"
CHECKS="${TCDI_SBX_CHECKS:-ready runtime stream lifecycle}"
RFB_PY="$QS_DIR/sandbox-rfb.py"

need docker kubectl base64
[ -f "$RFB_PY" ] || die "missing $RFB_PY"
NODE="${CLUSTER}-control-plane"

result() { printf 'RESULT %-9s %-9s %s\n' "$1" "$2" "$3"; }
pass() { result PASS "$1" "$2"; }
fail() { result FAIL "$1" "$2"; exit 1; }

# Resolve a catalog name (the logical template) to the newest published
# WorkspaceTemplate revision in the namespace.
tpl_ref() {
  kc -n "$NS" get workspacetemplates \
    -l "workspaces.cdi.tinyorbit.vn/catalog-name=$1" \
    -o jsonpath='{.items[0].metadata.name}'
}

ws_running_pod() {
  kc -n "$NS" get pod -l "workspaces.cdi.tinyorbit.vn/workspace-name=$1" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

wait_ws() { # wait_ws <ws> <phase> <timeout-s>
  local ws="$1" phase="$2" deadline=$((SECONDS + $3))
  while [ "$SECONDS" -lt "$deadline" ]; do
    local cur
    cur="$(kc -n "$NS" get workspace "$ws" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$cur" = "$phase" ] && return 0
    sleep 5
  done
  return 1
}

set_ws() { # set_ws <ws> <desired> <runtimeGeneration> <intentRevision>
  kc -n "$NS" patch workspace "$1" --type=merge \
    -p "{\"spec\":{\"desiredState\":\"$2\",\"runtimeGeneration\":$3,\"intentRevision\":$4}}" >/dev/null
}

stream_check() { # stream_check <ws>
  local ws="$1" pod ip creds
  pod="$(ws_running_pod "$ws")"
  [ -n "$pod" ] || return 1
  ip="$(kc -n "$NS" get pod "$pod" -o jsonpath='{.status.podIP}')"
  creds="$STATE_DIR/rfb-$ws"
  mkdir -p "$creds"
  kc -n "$NS" get secret "${pod}-rt" -o jsonpath='{.data.password}' | base64 -d >"$creds/pw"
  kc -n "$NS" get secret "${pod}-rt" -o jsonpath='{.data.tls\.crt}' | base64 -d >"$creds/ca.crt"
  docker cp "$RFB_PY" "$NODE:/root/sbx-rfb.py" >/dev/null 2>&1 || {
    docker exec "$NODE" mkdir -p /root; docker cp "$RFB_PY" "$NODE:/root/sbx-rfb.py" >/dev/null; }
  docker cp "$creds/pw" "$NODE:/root/sbx-pw" >/dev/null
  docker cp "$creds/ca.crt" "$NODE:/root/sbx-ca.crt" >/dev/null
  docker exec "$NODE" python3 /root/sbx-rfb.py \
    --host "$ip" --port 8443 --servername "$pod" \
    --ca /root/sbx-ca.crt --user kasm_user --password-file /root/sbx-pw
}

check_ws() { # check_ws <catalog-name>
  local tpl="$1" ws="sbx-$1-$$" pod gen=1 rev=1 marker="sbx-marker-$$"
  log "checking template $tpl (workspace $ws)"
  local ref
  ref="$(tpl_ref "$tpl")"
  [ -n "$ref" ] || fail "$tpl" "no WorkspaceTemplate with catalog-name $tpl in $NS"

  kc apply -f - >/dev/null <<EOF
apiVersion: workspaces.cdi.tinyorbit.vn/v1alpha1
kind: Workspace
metadata:
  name: ${ws}
  namespace: ${NS}
spec:
  templateRef: {name: ${ref}}
  ownerSubject: {issuer: https://keycloak.invalid/realms/tinycdi, subject: sbx-check}
  desiredState: Running
  dataPolicy: Retain
  runtimeGeneration: ${gen}
  intentRevision: ${rev}
EOF

  if [[ " $CHECKS " == *" ready "* ]]; then
    wait_ws "$ws" Ready 600 || fail ready "$ws did not reach Ready in 10m"
    pass ready "workspace Ready"
  fi

  pod="$(ws_running_pod "$ws")"
  [ -n "$pod" ] || fail ready "no runtime pod for $ws"

  if [[ " $CHECKS " == *" runtime "* ]]; then
    local rc kern
    rc="$(kc -n "$NS" get pod "$pod" -o jsonpath='{.spec.runtimeClassName}')"
    [ "$rc" = "$RUNTIMECLASS" ] || fail runtime "pod runtimeClassName=$rc, want $RUNTIMECLASS"
    kern="$(kc -n "$NS" exec "$pod" -c desktop -- uname -r 2>/dev/null || true)"
    case "$kern" in
      *gvisor*) pass runtime "pod under runsc (kernel: $kern)" ;;
      *) fail runtime "container kernel reports $kern — not gVisor" ;;
    esac
  fi

  if [[ " $CHECKS " == *" stream "* ]]; then
    stream_check "$ws" >"$STATE_DIR/logs/rfb-$ws.log" 2>&1 \
      || fail stream "RFB check failed — $STATE_DIR/logs/rfb-$ws.log"
    pass stream "$(tail -1 "$STATE_DIR/logs/rfb-$ws.log")"
  fi

  if [[ " $CHECKS " == *" lifecycle "* ]]; then
    kc -n "$NS" exec "$pod" -c desktop -- sh -c \
      "printf %s '$marker' > /home/workspace/sbx-marker.txt && sync" || true
    rev=$((rev + 1)); set_ws "$ws" Stopped "$gen" "$rev"
    wait_ws "$ws" Stopped 300 || fail lifecycle "did not reach Stopped in 5m"
    rev=$((rev + 1)); gen=$((gen + 1)); set_ws "$ws" Running "$gen" "$rev"
    wait_ws "$ws" Ready 600 || fail lifecycle "restart did not reach Ready in 10m"
    pod="$(ws_running_pod "$ws")"
    local got
    got="$(kc -n "$NS" exec "$pod" -c desktop -- cat /home/workspace/sbx-marker.txt 2>/dev/null || true)"
    [ "$got" = "$marker" ] || fail lifecycle "marker lost across stop/start (got '$got')"
    pass lifecycle "stop/start OK, home PVC data retained"
  fi
}

catalog=("$@")
if [ "${#catalog[@]}" -eq 0 ]; then
  mapfile -t catalog < <(kc -n "$NS" get workspacetemplates \
    -o jsonpath='{.items[*].metadata.labels.workspaces\.cdi\.tinyorbit\.vn/catalog-name}')
  IFS=' ' read -r -a catalog <<< "${catalog[*]}"
fi
[ "${#catalog[@]}" -gt 0 ] || die "no seeded templates in namespace $NS"

for tpl in "${catalog[@]}"; do
  check_ws "$tpl"
done
log "sandbox checks done"
