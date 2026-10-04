#!/usr/bin/env bash
# sandbox-up.sh - bring up the quickstart WITH the runtime under gVisor
# (DEV/CI ONLY — the sandboxed-runtime matrix, docs/compatibility.md).
#
#   hack/quickstart/sandbox-up.sh
#
# 1. Creates the kind cluster (reusing it when it already exists),
# 2. installs gVisor into the node(s) and registers RuntimeClass "gvisor"
#    (sandbox-gvisor.sh),
# 3. runs the regular up.sh with TCDI_QS_VALUES=values-sandbox.yaml — the
#    seeded templates then carry spec.placement.runtimeClassName: gvisor,
#    so every workspace created afterwards runs under runsc.
#
# Environment: everything up.sh honours, plus
#   TCDI_SBX_HOST_PORTS=0  create the cluster WITHOUT the 80/443 host-port
#                          mappings — for a second cluster on a machine where
#                          the ports are taken; the portal is then unreachable
#                          and up.sh's host-side verify step fails AFTER the
#                          install completes (pod-level checks still work —
#                          see sandbox-check.sh).
#   TCDI_SBX_RUNTIMECLASS  RuntimeClass name the templates must use (default
#                          gvisor) — must match sandbox-gvisor.sh.
set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

HOST_PORTS="${TCDI_SBX_HOST_PORTS:-1}"
RUNTIMECLASS="${TCDI_SBX_RUNTIMECLASS:-gvisor}"
VALUES="$QS_DIR/values-sandbox.yaml"
[ -f "$VALUES" ] || die "missing $VALUES"
# values-sandbox.yaml hardcodes runtimeClassName: gvisor — swap in the
# requested class when it differs.
if [ "$RUNTIMECLASS" != gvisor ]; then
  mkdir -p "$STATE_DIR"
  sed "s/runtimeClassName: gvisor/runtimeClassName: ${RUNTIMECLASS}/g" \
    "$VALUES" >"$STATE_DIR/values-sandbox.yaml"
  VALUES="$STATE_DIR/values-sandbox.yaml"
fi

need docker kind

# ---- 1. cluster ------------------------------------------------------------
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "kind cluster ${CLUSTER} already exists; reusing it"
else
  if [ "$HOST_PORTS" = 0 ]; then
    # Same single-node shape as kind-config.yaml, minus the host port
    # mappings — the ingress then stays cluster-internal only.
    KIND_CFG="$STATE_DIR/kind-config-noports.yaml"
    mkdir -p "$STATE_DIR"
    sed '/extraPortMappings:/,/protocol: TCP}/d' "$QS_DIR/kind-config.yaml" >"$KIND_CFG"
  else
    KIND_CFG="$QS_DIR/kind-config.yaml"
  fi
  log "creating kind cluster ${CLUSTER} (host ports: $([ "$HOST_PORTS" = 0 ] && echo off || echo on))"
  kind create cluster --name "$CLUSTER" --image "$KIND_NODE_IMAGE" \
    --config "$KIND_CFG" --wait 120s >/dev/null
fi

# ---- 2. gVisor + RuntimeClass -----------------------------------------------
"$QS_DIR/sandbox-gvisor.sh"

# ---- 3. TinyCDI with the gVisor template ------------------------------------
log "installing TinyCDI via up.sh (templates carry runtimeClassName: ${RUNTIMECLASS})"
if ! TCDI_QS_CLUSTER="$CLUSTER" TCDI_QS_VALUES="$VALUES" "$QS_DIR/up.sh"; then
  if [ "$HOST_PORTS" = 0 ] && [ -s "$STATE_DIR/env" ]; then
    warn "up.sh failed at the host-side verify step — expected without host ports"
    warn "the in-cluster install completed; run pod-level checks with sandbox-check.sh"
    exit 0
  fi
  exit 1
fi
