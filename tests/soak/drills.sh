#!/usr/bin/env bash
# drills.sh — failure drills to run while a soak is in progress.
#
# Each drill prints a UTC ISO-8601 timestamp before and after it acts, so
# the non-connected spans in soak-report.json can be matched to the drill
# window by eye or by script.
#
#   drills.sh [options] delete-pod    delete one backend pod, wait for the Deployment to recover
#   drills.sh [options] rollout       rollout restart of the backend Deployment, wait for it
#   drills.sh [options] rotate-cert   re-issue the session certificate (cert-manager renew)
#
# Options may come before or after the drill name; an unknown option or a
# second drill name is an error and nothing runs.
#
#   -n, --namespace NS     namespace of the tinycdi release   (SOAK_NAMESPACE, default: tinycdi)
#       --deployment NAME  backend Deployment name            (SOAK_DEPLOYMENT, default: backend)
#       --selector SEL     pod selector for delete-pod        (SOAK_SELECTOR, default: app.kubernetes.io/name=backend)
#       --certificate NAME cert-manager Certificate to renew  (SOAK_CERTIFICATE, rotate-cert)
#       --secret NAME      TLS Secret holding the session cert (SOAK_CERT_SECRET, rotate-cert);
#                          the Certificate that owns it is looked up and renewed
#   -h, --help             print this help
#   KUBECTL                kubectl binary                     (default: kubectl)
#
# rotate-cert needs --certificate NAME, or --secret NAME naming a Secret that
# a cert-manager Certificate issues. It runs `cmctl renew` (or the kubectl
# cert-manager plugin), which re-issues the key material; a Secret that no
# Certificate owns cannot be rotated from here and the drill fails.
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

stamp() {
  date -u +'%Y-%m-%dT%H:%M:%SZ'
}

log() {
  printf '%s drill=%s %s\n' "$(stamp)" "${DRILL:-?}" "$*"
}

# The leading comment block is the help text.
usage() {
  local code=${1:-1}
  if [ "$code" -eq 0 ]; then
    awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"
  else
    awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0" >&2
  fi
  exit "$code"
}

DRILL=
while [ $# -gt 0 ]; do
  case "$1" in
    -n|--namespace|--deployment|--selector|--certificate|--secret)
      [ $# -ge 2 ] || { echo "$1 needs a value" >&2; usage 2; }
      case "$1" in
        -n|--namespace) NAMESPACE=$2 ;;
        --deployment)   DEPLOYMENT=$2 ;;
        --selector)     SELECTOR=$2 ;;
        --certificate)  CERTIFICATE=$2 ;;
        --secret)       CERT_SECRET=$2 ;;
      esac
      shift 2 ;;
    -h|--help)      usage 0 ;;
    -*)             echo "unknown flag: $1" >&2; usage 2 ;;
    *)
      if [ -n "$DRILL" ]; then
        echo "unexpected argument: $1 (drill is already $DRILL)" >&2
        usage 2
      fi
      DRILL=$1
      shift ;;
  esac
done
[ -n "$DRILL" ] || usage 2

# Prints the names of the cert-manager Certificates that issue CERT_SECRET.
# A failing kubectl is reported (with its own message) and returns non-zero:
# the caller must not mistake "could not ask" for "nobody owns it".
find_certificate_for_secret() {
  local listing
  if ! listing=$("$KUBECTL" -n "$NAMESPACE" get certificate \
    -o jsonpath='{range .items[*]}{.spec.secretName}{"\t"}{.metadata.name}{"\n"}{end}' 2>&1); then
    log "error: could not list certificates in namespace $NAMESPACE: $listing" >&2
    return 1
  fi
  printf '%s\n' "$listing" | awk -v s="$CERT_SECRET" '$1 == s { print $2 }'
}

drill_delete_pod() {
  log "begin namespace=$NAMESPACE selector=$SELECTOR deployment=$DEPLOYMENT"
  local pod
  pod=$("$KUBECTL" -n "$NAMESPACE" get pod -l "$SELECTOR" \
    --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null) || pod=
  if [ -z "$pod" ]; then
    log "error: no running backend pod matches $SELECTOR"
    return 1
  fi
  log "deleting pod $pod"
  # Blocks until the pod is gone, so the Deployment has already counted it
  # as missing when rollout status looks. `kubectl wait` on the selector
  # would match the terminating pod and return before its replacement exists.
  "$KUBECTL" -n "$NAMESPACE" delete pod "$pod"
  "$KUBECTL" -n "$NAMESPACE" rollout status "deployment/$DEPLOYMENT" --timeout=180s
  log "end: deployment/$DEPLOYMENT recovered after deleting $pod"
}

drill_rollout() {
  log "begin namespace=$NAMESPACE deployment=$DEPLOYMENT"
  "$KUBECTL" -n "$NAMESPACE" rollout restart "deployment/$DEPLOYMENT"
  "$KUBECTL" -n "$NAMESPACE" rollout status "deployment/$DEPLOYMENT" --timeout=300s
  log "end: rollout of $DEPLOYMENT complete"
}

drill_rotate_cert() {
  log "begin namespace=$NAMESPACE certificate=${CERTIFICATE:-none} secret=${CERT_SECRET:-none}"
  if [ -z "$CERTIFICATE" ] && [ -z "$CERT_SECRET" ]; then
    log "error: rotate-cert needs --certificate NAME or --secret NAME"
    return 1
  fi
  if [ -z "$CERTIFICATE" ]; then
    local owners
    owners=$(find_certificate_for_secret) || return 1
    CERTIFICATE=$(printf '%s\n' "$owners" | head -n1)
    if [ -z "$CERTIFICATE" ]; then
      log "error: no cert-manager Certificate in namespace $NAMESPACE issues secret $CERT_SECRET — re-issue it with your PKI or pass --certificate"
      return 1
    fi
    log "secret $CERT_SECRET is issued by certificate $CERTIFICATE"
  fi
  if command -v cmctl >/dev/null 2>&1; then
    cmctl renew "$CERTIFICATE" --namespace "$NAMESPACE"
  elif "$KUBECTL" cert-manager version >/dev/null 2>&1; then
    "$KUBECTL" cert-manager renew "$CERTIFICATE" --namespace "$NAMESPACE"
  else
    log "error: neither cmctl nor the kubectl cert-manager plugin is installed"
    return 1
  fi
  log "end: renewal of certificate $CERTIFICATE triggered"
}

case "$DRILL" in
  delete-pod)  drill_delete_pod ;;
  rollout)     drill_rollout ;;
  rotate-cert) drill_rotate_cert ;;
  *)           echo "unknown drill: $DRILL" >&2; usage ;;
esac
