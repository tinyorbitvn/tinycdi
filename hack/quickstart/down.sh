#!/usr/bin/env bash
# down.sh - remove everything up.sh created (DEV ONLY). Safe to run twice.
#
#   hack/quickstart/down.sh
#
# Deletes the kind cluster (and with it every namespace, Secret, volume and
# workspace in it), the images up.sh built, and the state directory holding the
# private kubeconfig, local CA and generated passwords. Docker's build cache is
# left alone: `docker builder prune` reclaims it.
set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

need kind docker

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log "deleting kind cluster ${CLUSTER}"
  kind delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1 \
    || kind delete cluster --name "$CLUSTER" >/dev/null
else
  log "kind cluster ${CLUSTER} not found; nothing to delete"
fi

# Images built by up.sh (tcdi-qs.local/tcdi-qs/<component>:qs-<epoch>).
images="$(docker images --format '{{.Repository}}:{{.Tag}}' | grep "^${LOCAL_IMAGE_PREFIX}/" || true)"
if [ -n "$images" ]; then
  log "removing locally built images"
  # shellcheck disable=SC2086 # one reference per word is intended
  docker rmi -f $images >/dev/null 2>&1 || true
fi

if [ -d "$STATE_DIR" ]; then
  log "removing ${STATE_DIR}"
  rm -rf "${STATE_DIR:?}"
fi
rmdir "$STATE_ROOT" 2>/dev/null || true

log "done"
