#!/usr/bin/env bash
# drills.sh — failure drills to run while a soak is in progress.
#
# Each drill prints a UTC ISO-8601 timestamp before and after it acts, so
# the non-connected spans in soak-report.json can be matched to the drill
# window by eye or by script.
#
#   drills.sh delete-pod   delete one backend pod, wait for a ready replacement
#   drills.sh rollout      rollout restart of the backend Deployment, wait for it
#   drills.sh rotate-cert  trigger renewal of the session certificate
#
# Configuration (env or flags; flags win):
#   SOAK_NAMESPACE     namespace of the tinycdi release   (default: tinycdi)
#   SOAK_DEPLOYMENT    backend Deployment name            (default: backend)
#   SOAK_SELECTOR      pod selector for delete-pod        (default: app.kubernetes.io/name=backend)
#   SOAK_CERTIFICATE   cert-manager Certificate name for the session cert (rotate-cert)
#   SOAK_CERT_SECRET   TLS Secret holding the session cert (rotate-cert fallback)
#   KUBECTL            kubectl binary                     (default: kubectl)
#
# rotate-cert needs one of:
#   --certificate NAME  -> `cmctl renew` / `kubectl cert-manager renew`
#   --secret NAME       -> if a cert-manager Certificate owns the Secret it is
#                          renewed the same way; otherwise --touch only bumps an
#                          annotation to force a kubelet re-mount, which
#                          exercises the hot-reload path without changing key
#                          material (not a real rotation).
#
# Nothing here is safe to point at production blindly; it exists for the
# v0.2 RKE2 drill runs (T5.2).

set -euo pipefail

KUBECTL=${KUBECTL:-kubectl}
NAMESPACE=${SOAK_NAMESPACE:-tinycdi}
DEPLOYMENT=${SOAK_DEPLOYMENT:-backend}
SELECTOR=${SOAK_SELECTOR:-app.kubernetes.io/name=backend}
CERTIFICATE=${SOAK_CERTIFICATE:-}
CERT_SECRET=${SOAK_CERT_SECRET:-}
TOUCH_ONLY=0

stamp() {
  date -u +'%Y-%m-%dT%H:%M:%SZ'
}

log() {
  printf '%s drill=%s %s\n' "$(stamp)" "${DRILL:-?}" "$*"
}

usage() {
  sed -n '2,32p' "$0"
  exit "${1:-1}"
}

while [ $# -gt 0 ]; do
  case "$1" in
    -n|--namespace) NAMESPACE=$2; shift 2 ;;
    --deployment)   DEPLOYMENT=$2; shift 2 ;;
    --selector)     SELECTOR=$2; shift 2 ;;
    --certificate)  CERTIFICATE=$2; shift 2 ;;
    --secret)       CERT_SECRET=$2; shift 2 ;;
    --touch)        TOUCH_ONLY=1; shift ;;
    -h|--help)      usage 0 ;;
    -*)             echo "unknown flag: $1" >&2; usage ;;
    *)              break ;;
  esac
done

DRILL=${1:-}
[ -n "$DRILL" ] || usage
shift || true

# Certificate name that owns CERT_SECRET, if cert-manager manages it.
find_certificate_for_secret() {
  "$KUBECTL" -n "$NAMESPACE" get certificate \
    -o jsonpath='{range .items[*]}{.spec.secretName}{"\t"}{.metadata.name}{"\n"}{end}' \
    2>/dev/null | awk -v s="$CERT_SECRET" '$1 == s { print $2 }'
}

drill_delete_pod() {
  log "begin namespace=$NAMESPACE selector=$SELECTOR"
  local pod
  pod=$("$KUBECTL" -n "$NAMESPACE" get pod -l "$SELECTOR" \
    --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name}')
  if [ -z "$pod" ]; then
    log "error: no running backend pod matches $SELECTOR"
    return 1
  fi
  log "deleting pod $pod"
  "$KUBECTL" -n "$NAMESPACE" delete pod "$pod" --wait=false
  "$KUBECTL" -n "$NAMESPACE" wait --for=condition=Ready pod -l "$SELECTOR" --timeout=120s
  log "end: replacement ready after deleting $pod"
}

drill_rollout() {
  log "begin namespace=$NAMESPACE deployment=$DEPLOYMENT"
  "$KUBECTL" -n "$NAMESPACE" rollout restart "deployment/$DEPLOYMENT"
  "$KUBECTL" -n "$NAMESPACE" rollout status "deployment/$DEPLOYMENT" --timeout=300s
  log "end: rollout of $DEPLOYMENT complete"
}

drill_rotate_cert() {
  log "begin namespace=$NAMESPACE certificate=${CERTIFICATE:-none} secret=${CERT_SECRET:-none}"
  if [ -z "$CERTIFICATE" ] && [ -n "$CERT_SECRET" ] && [ "$TOUCH_ONLY" -eq 0 ]; then
    CERTIFICATE=$(find_certificate_for_secret | head -n1)
    [ -n "$CERTIFICATE" ] && log "secret $CERT_SECRET is issued by certificate $CERTIFICATE"
  fi
  if [ -n "$CERTIFICATE" ]; then
    if command -v cmctl >/dev/null 2>&1; then
      cmctl renew "$CERTIFICATE" --namespace "$NAMESPACE"
    elif "$KUBECTL" cert-manager version >/dev/null 2>&1; then
      "$KUBECTL" cert-manager renew "$CERTIFICATE" --namespace "$NAMESPACE"
    else
      log "error: neither cmctl nor the kubectl cert-manager plugin is installed"
      return 1
    fi
    log "end: renewal of certificate $CERTIFICATE triggered"
  elif [ -n "$CERT_SECRET" ] && [ "$TOUCH_ONLY" -eq 1 ]; then
    "$KUBECTL" -n "$NAMESPACE" annotate secret "$CERT_SECRET" \
      "cdi.tinyorbit.vn/rotated-at=$(stamp)" --overwrite
    log "end: touched secret $CERT_SECRET (hot-reload path only — not a real rotation)"
  else
    log "error: rotate-cert needs --certificate NAME, or --secret NAME [--touch]"
    return 1
  fi
}

case "$DRILL" in
  delete-pod)  drill_delete_pod ;;
  rollout)     drill_rollout ;;
  rotate-cert) drill_rotate_cert ;;
  *)           echo "unknown drill: $DRILL" >&2; usage ;;
esac
