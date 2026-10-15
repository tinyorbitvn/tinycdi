#!/usr/bin/env bash
# push-sign-chart.sh — push the packaged Helm chart to GHCR and sign its
# digest keyless; never leave an unsigned chart version tagged (R1-40,
# SUPR-13).
#
# `helm push` uploads the chart and creates the <chart>:<version> tag in
# one call, so the chart cannot be signed before it is tagged. This script
# closes that window from the other side: when anything fails once the
# push has started (the push itself, the digest lookup, `cosign sign`), it
# deletes the GHCR package version tagged <version> that THIS run created
# (the tag goes with it) and exits non-zero. A version created before the
# run (created_at < RUN_STARTED_AT) is never deleted. With the version
# gone, the prepare job's existence check lets a re-run publish it again.
#
# Usage: push-sign-chart.sh <chart.tgz>
# Env:   HELM            helm binary
#        CHART_NAME      chart name (Chart.yaml name:)
#        CHART_VERSION   X.Y.Z — the tag helm push creates
#        CHART_OCI       oci://ghcr.io/<org>/<path>
#        RUN_STARTED_AT  ISO 8601 start of this run (deletion bound)
#        GH_TOKEN        token with packages: write (list + delete versions)
# Stdout: exactly one line, ref=ghcr.io/<org>/<path>/<chart>@sha256:<64hex>,
#         for $GITHUB_OUTPUT. Every tool's own output goes to stderr.
set -euo pipefail

PKG="${1:?usage: push-sign-chart.sh <chart.tgz>}"
: "${HELM:?}" "${CHART_NAME:?}" "${CHART_VERSION:?}" "${CHART_OCI:?}" "${RUN_STARTED_AT:?}"
[ -f "$PKG" ] || { echo "::error::chart package '$PKG' not found" >&2; exit 1; }
[[ "$CHART_OCI" =~ ^oci://ghcr\.io/([a-z0-9][a-z0-9-]*)/([a-z0-9][a-z0-9/_.-]*)$ ]] \
  || { echo "::error::CHART_OCI '$CHART_OCI' is not oci://ghcr.io/<org>/<path>" >&2; exit 1; }
ORG="${BASH_REMATCH[1]}"
PKG_PATH="${BASH_REMATCH[2]}/$CHART_NAME"
REPO="ghcr.io/$ORG/$PKG_PATH"
PKG_ENC="${PKG_PATH//\//%2F}"

state="start"   # start -> pushing -> signed
rollback() {
  [ "$state" = "pushing" ] || return 0
  echo "::error::$REPO:$CHART_VERSION is not signed — deleting the version this run pushed" >&2
  local ids id
  if ! ids="$(gh api --paginate "orgs/$ORG/packages/container/$PKG_ENC/versions?per_page=100" \
      --jq ".[] | select((.metadata.container.tags // []) | index(\"$CHART_VERSION\"))
                | select(.created_at >= \"$RUN_STARTED_AT\") | .id")"; then
    echo "::error::could not list $PKG_PATH versions — delete $REPO:$CHART_VERSION by hand" >&2
    return 0
  fi
  if [ -z "$ids" ]; then
    echo "::warning::no $REPO:$CHART_VERSION version created by this run — nothing to delete" >&2
    return 0
  fi
  for id in $ids; do
    if gh api -X DELETE "orgs/$ORG/packages/container/$PKG_ENC/versions/$id" >/dev/null; then
      echo "deleted unsigned chart version $id ($REPO:$CHART_VERSION)" >&2
    else
      echo "::error::could not delete version $id — delete $REPO:$CHART_VERSION by hand" >&2
    fi
  done
}
trap rollback EXIT

state="pushing"
"$HELM" push "$PKG" "$CHART_OCI" >&2
# SUPR-1: helm 4 writes Pushed:/Digest: to stderr — resolve the digest
# from the registry by tag instead of parsing streams.
digest="$(docker buildx imagetools inspect "$REPO:$CHART_VERSION" | awk '/^Digest:/ {print $2; exit}')"
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] \
  || { echo "::error::could not resolve pushed chart digest from the registry" >&2; exit 1; }
# SEC-17: sign the pushed chart manifest by digest (keyless).
cosign sign --yes "$REPO@$digest" >&2
state="signed"
echo "ref=$REPO@$digest"
