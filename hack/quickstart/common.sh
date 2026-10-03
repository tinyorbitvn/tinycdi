#!/usr/bin/env bash
# shellcheck shell=bash
# shellcheck disable=SC2034 # variables are consumed by the scripts that source this file
#
# Shared settings for up.sh / down.sh (sourced, never executed).
#
# DEV ONLY. The quickstart builds a throwaway kind cluster with a dev
# Keycloak realm, a self-made CA and loopback-only hostnames. Nothing here is
# meant for a shared or production cluster.

QS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$QS_DIR/../.." && pwd)"

# ---- knobs (environment) ---------------------------------------------------
CLUSTER="${TCDI_QS_CLUSTER:-tcdi-quickstart}"
# Every name below *.${DOMAIN} resolves to 127.0.0.1 on the public DNS
# (localtest.me): portal.<d>, keycloak.<d> and every ws-<hex>.session.<d>.
DOMAIN="${TCDI_QS_DOMAIN:-tcdi.localtest.me}"
# build: build backend/operator/frontend from this working tree and load them
# into kind. published: pull ghcr.io/tinyorbitvn/tinycdi-* at TCDI_QS_IMAGE_TAG
# (default: the chart appVersion) - use it on a release checkout to skip the build.
IMAGES="${TCDI_QS_IMAGES:-build}"
IMAGE_TAG="${TCDI_QS_IMAGE_TAG:-}"
STATE_ROOT="${TCDI_QS_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/tcdi-quickstart}"

case "$CLUSTER" in
  '' | */* | .*)
    printf 'ERROR: TCDI_QS_CLUSTER must be a plain kind cluster name\n' >&2
    exit 1
    ;;
esac

PORTAL_HOST="portal.${DOMAIN}"
SESSION_DOMAIN="session.${DOMAIN}"
KEYCLOAK_HOST="keycloak.${DOMAIN}"

STATE_DIR="${STATE_ROOT}/${CLUSTER}"
# A private kubeconfig keeps ~/.kube/config untouched; down.sh deletes it with
# the rest of the state.
export KUBECONFIG="${STATE_DIR}/kubeconfig"

# Names of everything up.sh creates inside the cluster.
NS_SYSTEM="tinycdi-system"
NS_TENANT="tinycdi-tenant-a"
NS_DEPS="tcdi-qs-deps"
NS_INGRESS="tcdi-qs-ingress"
RELEASE="tinycdi"
# Locally built image names (never pushed anywhere).
LOCAL_IMAGE_PREFIX="tcdi-qs.local/tcdi-qs"

# ---- pinned third-party inputs ---------------------------------------------
# Everything pulled from the network is pinned by version and, where the
# registry/repo offers one, by digest. Bump together and re-run CI.
KIND_NODE_IMAGE="kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5" # kind v0.33.0 release
TRAEFIK_CHART_VERSION="41.6.1"
TRAEFIK_CHART_SHA256="1e65d46bae0ba0baef460a1d82686b50f156372865a3d7d8a85b51c3855b2ef9"
TRAEFIK_CHART_URL="https://traefik.github.io/charts/traefik/traefik-${TRAEFIK_CHART_VERSION}.tgz"
TRAEFIK_VERSION="v3.7.13"
TRAEFIK_DIGEST="sha256:24841fe2de7304c149343d877d2923b4c8800a38ba015dea9174c23b20e344a0"
KEYCLOAK_IMAGE="quay.io/keycloak/keycloak:26.8.0@sha256:b0f60d489d51c5d113390bdf5461d4c06e6051be026c05549f2e1e10ec352bcc"
POSTGRES_IMAGE="docker.io/library/postgres:18.0@sha256:41fc5342eefba6cc2ccda736aaf034bbbb7c3df0fdb81516eba1ba33f360162c"
# Runtime images: the published runtime release train (runtime-2026.10.02),
# digest-pinned exactly like a production values file would.
BROWSER_DIGEST="sha256:b586ae0e271fa28226b6da3d9e54e1171eee2ed0d4afbd0d011424b7ab6cdc6d"
BROWSER_BUILT_AT="2026-10-02T11:02:50Z"
DESKTOP_DIGEST="sha256:5f14b9e80b1de58d8eb962c1e11c7211684dc51e6d7586e5331a088f7c1fab14"
DESKTOP_BUILT_AT="2026-10-02T11:02:50Z"

# ---- dev-only demo login (loopback-only, throwaway cluster) ------------------
# These credentials exist ONLY inside the kind cluster's dev Keycloak realm.
DEMO_USER="demo"
DEMO_PASSWORD="tcdi-demo-dev-only"
# A second dev user whose tenant has no quota row (B5.4 refusal check).
NOQUOTA_USER="${NOQUOTA_USER:-demo-noquota}"

log() { printf '==> %s\n' "$*" >&2; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

need() {
  local t
  for t in "$@"; do
    command -v "$t" >/dev/null 2>&1 || die "required tool not found on PATH: $t"
  done
}

# kc: kubectl against the quickstart cluster only (private kubeconfig).
kc() { kubectl --kubeconfig "$KUBECONFIG" "$@"; }
