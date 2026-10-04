#!/usr/bin/env bash
# sandbox-gvisor.sh - install gVisor (runsc) into every node of a kind
# cluster and register the RuntimeClass (DEV/CI ONLY).
#
#   hack/quickstart/sandbox-gvisor.sh
#
# Downloads the pinned gVisor release tarball on the host (sha512-verified),
# copies runsc + the containerd shim + gvisor-bin/ into each node, registers
# the runsc runtime in the node's containerd config, restarts containerd and
# applies the RuntimeClass. Idempotent: a second run is a no-op.
#
# Platform: the node needs no KVM — 'auto' picks kvm only when /dev/kvm is
# visible inside the node, else ptrace (slower syscalls, works everywhere).
#
# Environment:
#   TCDI_QS_CLUSTER            kind cluster name (common.sh default)
#   TCDI_SBX_GVISOR_RELEASE    gVisor release tag (default 20260817.0)
#   TCDI_SBX_GVISOR_SHA512     sha512 of gvisor.tar.bz2 for that release
#   TCDI_SBX_PLATFORM          auto|kvm|ptrace (default auto)
#   TCDI_SBX_RUNTIMECLASS      RuntimeClass name (default gvisor, handler runsc)
set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

GVISOR_RELEASE="${TCDI_SBX_GVISOR_RELEASE:-20260817.0}"
GVISOR_SHA512="${TCDI_SBX_GVISOR_SHA512:-bd8271a7742f90e53373b2a8613f37f3ae2c765ff5e2e611a75a47167a323cab7519b149c50273307743491713525a14ad1b3e398651c93b16f3e248dfeff3dd}"
PLATFORM="${TCDI_SBX_PLATFORM:-auto}"
RUNTIMECLASS="${TCDI_SBX_RUNTIMECLASS:-gvisor}"

need docker kind kubectl tar bzip2 sha256sum sha512sum

nodes="$(kind get nodes --name "$CLUSTER" 2>/dev/null)"
[ -n "$nodes" ] || die "kind cluster $CLUSTER not found — create it first (sandbox-up.sh or up.sh)"

# common.sh keeps the quickstart kubeconfig private — export it like up.sh.
umask 077
mkdir -p "$STATE_DIR/logs"
kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null
chmod 600 "$KUBECONFIG"

# ---- 1. fetch + verify the release tarball (cached in STATE_DIR) ----------
SBX_DIR="$STATE_DIR/gvisor/$GVISOR_RELEASE"
mkdir -p "$SBX_DIR"
TARBALL="$SBX_DIR/gvisor.tar.bz2"
if [ ! -s "$TARBALL" ]; then
  log "downloading gVisor release $GVISOR_RELEASE"
  curl -fsSL --retry 3 -o "$TARBALL" \
    "https://storage.googleapis.com/gvisor/releases/release/${GVISOR_RELEASE}/x86_64/gvisor.tar.bz2"
fi
echo "${GVISOR_SHA512}  ${TARBALL}" | sha512sum -c - >/dev/null \
  || die "gVisor tarball checksum mismatch ($TARBALL)"
if [ ! -x "$SBX_DIR/runsc" ]; then
  tar -xjf "$TARBALL" -C "$SBX_DIR"
fi

# ---- 2. install into every node + containerd runtime block ----------------
# runsc resolves gvisor-bin/ relative to its own binary — keep them together.
for node in $nodes; do
  docker exec "$node" mkdir -p /usr/local/bin
  docker cp "$SBX_DIR/runsc" "$node:/usr/local/bin/runsc"
  docker cp "$SBX_DIR/containerd-shim-runsc-v1" "$node:/usr/local/bin/"
  docker exec "$node" rm -rf /usr/local/bin/gvisor-bin
  docker cp "$SBX_DIR/gvisor-bin" "$node:/usr/local/bin/"
  docker exec "$node" chmod -R 755 /usr/local/bin/runsc \
    /usr/local/bin/containerd-shim-runsc-v1 /usr/local/bin/gvisor-bin

  platform="$PLATFORM"
  if [ "$platform" = auto ]; then
    if docker exec "$node" test -e /dev/kvm; then platform=kvm; else platform=ptrace; fi
  fi

  if docker exec "$node" grep -q 'runtimes\.runsc' /etc/containerd/config.toml; then
    log "runsc already configured on $node"
  else
    log "registering the runsc runtime on $node (platform: $platform)"
    # kind's config.toml uses the grpc plugin namespace; the runsc options
    # ConfigPath hands the shim its flags file (written next).
    docker exec "$node" sh -c 'cat >> /etc/containerd/config.toml' <<EOF

# gVisor (runsc) runtime — added by sandbox-gvisor.sh
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc.options]
    TypeUrl = "io.containerd.runsc.v1.options"
    ConfigPath = "/etc/containerd/runsc.toml"
EOF
    docker exec "$node" sh -c 'cat > /etc/containerd/runsc.toml' <<EOF
# runsc flags handed to the containerd shim (ConfigPath above).
[runsc_config]
  platform = "$platform"
EOF
    # containerd restart keeps running containers (shim model); the kubelet
    # reconnects on its own. Wait for containerd + node Ready again.
    docker exec "$node" systemctl restart containerd
  fi
done

log "waiting for the node(s) to come back"
for _ in $(seq 1 60); do
  if kc get nodes -o jsonpath='{.items[*].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null | grep -q True; then
    ready="$(kc get nodes -o jsonpath='{.items[*].status.conditions[?(@.type=="Ready")].status}')"
    [ -z "$(echo "$ready" | grep -v True || true)" ] && break
  fi
  sleep 5
done

# ---- 3. RuntimeClass -------------------------------------------------------
kc apply -f - >/dev/null <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: ${RUNTIMECLASS}
handler: runsc
EOF

# ---- 4. probe: one pod under runsc ------------------------------------------
log "probing the runsc handler with a throwaway pod"
probe="runsc-probe-${RUNTIMECLASS}"
kc delete pod "$probe" --ignore-not-found --wait=false >/dev/null 2>&1 || true
kc run "$probe" --restart=Never --image=registry.k8s.io/pause:3.10 \
  --overrides="{\"spec\":{\"runtimeClassName\":\"${RUNTIMECLASS}\"}}" >/dev/null
rc=0
for _ in $(seq 1 24); do
  phase="$(kc get pod "$probe" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [ "$phase" = Running ] && break
  [ "$phase" = Failed ] && { rc=1; break; }
  sleep 5
done
[ "$phase" = Running ] || { rc=1; warn "runsc probe pod did not reach Running (phase=$phase)"; }
kc describe pod "$probe" >"$STATE_DIR/logs/runsc-probe-describe.txt" 2>&1 || true
kc delete pod "$probe" --ignore-not-found --wait=false >/dev/null 2>&1 || true
[ "$rc" -eq 0 ] || die "runsc probe failed — see $STATE_DIR/logs/runsc-probe-describe.txt"
log "RuntimeClass ${RUNTIMECLASS} (handler runsc) is live on cluster ${CLUSTER}"
