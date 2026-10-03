#!/usr/bin/env bash
# up.sh - bring up a complete, throwaway TinyCDI on kind (DEV ONLY).
#
#   hack/quickstart/up.sh
#
# Creates a kind cluster, a Traefik ingress controller, PostgreSQL (TLS) and a
# dev Keycloak realm with one test user, a local CA plus wildcard certificate,
# then installs the chart from this working tree with
# runtime.placement.allowSharedNodes: true. Prints the portal URL and the test
# login. down.sh removes everything this script created. See
# docs/quickstart.md. Re-running on an existing cluster upgrades in place.
#
# Environment: TCDI_QS_CLUSTER, TCDI_QS_DOMAIN, TCDI_QS_IMAGES (build|published),
# TCDI_QS_IMAGE_TAG, TCDI_QS_STATE_DIR - see common.sh.
set -euo pipefail

# shellcheck source-path=SCRIPTDIR source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

SECONDS=0
BG_PIDS=()

on_error() {
  local rc=$?
  warn "up.sh failed (exit $rc) after ${SECONDS}s. Logs: ${STATE_DIR}/logs"
  if [ -s "$KUBECONFIG" ]; then
    warn "pods not ready:"
    kc get pods -A --no-headers 2>/dev/null | grep -Ev 'Running|Completed' >&2 || true
  fi
  warn "tear down with: hack/quickstart/down.sh"
  exit "$rc"
}
trap on_error ERR

# ---- preflight -------------------------------------------------------------
need docker kind kubectl helm openssl curl sed awk sha256sum
docker info >/dev/null 2>&1 || die "cannot talk to the Docker daemon"
case "$IMAGES" in build | published) ;; *) die "TCDI_QS_IMAGES must be build or published, got: $IMAGES" ;; esac
[ "${DOMAIN#*.}" != "$DOMAIN" ] || die "TCDI_QS_DOMAIN must be a DNS name with at least two labels"

helm_major="$(helm version --template '{{.Version}}' | sed -E 's/^v([0-9]+)\..*/\1/')"
[ "$helm_major" -ge 4 ] || die "Helm v4 or newer is required (found $(helm version --short))"

cluster_exists() { kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; }

port_busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
if ! cluster_exists; then
  for p in 80 443; do
    if port_busy "$p"; then
      die "host port $p is already in use; the quickstart needs 80 and 443 on 127.0.0.1"
    fi
  done
fi

umask 077
mkdir -p "$STATE_DIR/logs" "$STATE_DIR/tls"
NODE="${CLUSTER}-control-plane"

# ---- 1. images + cluster (in parallel) ---------------------------------------
IMAGE_TAG_RESOLVED="$IMAGE_TAG"
if [ "$IMAGES" = build ]; then
  IMAGE_TAG_RESOLVED="qs-$(date +%s)"
  log "building backend, operator and frontend images from the working tree (tag $IMAGE_TAG_RESOLVED)"
  for c in backend operator frontend; do
    (cd "$REPO_ROOT" && docker build -q -f "build/${c}/Dockerfile" \
      -t "${LOCAL_IMAGE_PREFIX}/${c}:${IMAGE_TAG_RESOLVED}" . >"$STATE_DIR/logs/build-${c}.log" 2>&1) &
    BG_PIDS+=($!)
  done
fi

if cluster_exists; then
  log "kind cluster ${CLUSTER} already exists; reusing it"
  kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null
else
  log "creating kind cluster ${CLUSTER}"
  kind create cluster --name "$CLUSTER" --image "$KIND_NODE_IMAGE" \
    --config "$QS_DIR/kind-config.yaml" --kubeconfig "$KUBECONFIG" --wait 120s \
    >"$STATE_DIR/logs/kind-create.log" 2>&1 || { tail -n 30 "$STATE_DIR/logs/kind-create.log" >&2; die "kind create cluster failed"; }
fi
chmod 600 "$KUBECONFIG"

# Pre-pull the third-party and runtime images into the node while the rest of
# the install runs, so the first workspace does not wait for a ~1 GB pull.
prefetch() {
  local ref="$1" name
  name="$(printf '%s' "$ref" | tr -c 'A-Za-z0-9' '_' | cut -c1-60)"
  docker exec "$NODE" crictl pull "$ref" >"$STATE_DIR/logs/prefetch-${name}.log" 2>&1 || true
}
PREFETCH_PIDS=()
for ref in "$POSTGRES_IMAGE" "$KEYCLOAK_IMAGE" \
  "docker.io/library/traefik@${TRAEFIK_DIGEST}" \
  "ghcr.io/tinyorbitvn/tinycdi-browser@${BROWSER_DIGEST}" \
  "ghcr.io/tinyorbitvn/tinycdi-linux-desktop@${DESKTOP_DIGEST}"; do
  prefetch "$ref" &
  PREFETCH_PIDS+=($!)
done

if [ "$IMAGES" = build ]; then
  for pid in "${BG_PIDS[@]}"; do
    wait "$pid" || { tail -n 40 "$STATE_DIR"/logs/build-*.log >&2; die "image build failed"; }
  done
  log "loading images into kind"
  kind load docker-image --name "$CLUSTER" \
    "${LOCAL_IMAGE_PREFIX}/backend:${IMAGE_TAG_RESOLVED}" \
    "${LOCAL_IMAGE_PREFIX}/operator:${IMAGE_TAG_RESOLVED}" \
    "${LOCAL_IMAGE_PREFIX}/frontend:${IMAGE_TAG_RESOLVED}" \
    >"$STATE_DIR/logs/kind-load.log" 2>&1 || { tail -n 30 "$STATE_DIR/logs/kind-load.log" >&2; die "kind load failed"; }
fi

# ---- 2. cluster plumbing: DNS rewrite, namespaces ---------------------------
# Pods must reach https://keycloak.<domain> (the OIDC issuer the browser also
# uses). On the public DNS that name is 127.0.0.1, which inside a pod is the
# pod itself; CoreDNS answers it with the ingress controller's Service instead.
log "pointing *.${DOMAIN} at the ingress controller inside the cluster (CoreDNS rewrite)"
domain_re="${DOMAIN//./\\.}"
corefile="$(kc -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')"
rule="    # tcdi-quickstart: *.${DOMAIN} -> ingress controller (added by hack/quickstart/up.sh)"$'\n'
rule+="    rewrite stop name regex ^(.*\\.)?${domain_re}\\.?\$ traefik.${NS_INGRESS}.svc.cluster.local answer auto"
# Idempotent: drop any earlier quickstart rule, then insert the current one
# right before the kubernetes plugin.
base="$(grep -v -e 'tcdi-quickstart' -e '^[[:space:]]*rewrite stop name regex' <<<"$corefile")"
patched="$(RULE="$rule" awk '!done && /^[[:space:]]*kubernetes[[:space:]]/ {print ENVIRON["RULE"]; done=1} {print}' <<<"$base")"
if [ "$patched" != "$corefile" ]; then
  kc -n kube-system create configmap coredns --from-literal="Corefile=${patched}" \
    --dry-run=client -o yaml | kc -n kube-system replace -f - >/dev/null
  kc -n kube-system rollout restart deployment/coredns >/dev/null
fi

for ns in "$NS_SYSTEM" "$NS_DEPS" "$NS_INGRESS"; do
  kc create namespace "$ns" --dry-run=client -o yaml | kc apply -f - >/dev/null
done
# Platform namespace PSS labels, as in docs/runbooks/install.md.
kc label namespace "$NS_SYSTEM" --overwrite \
  pod-security.kubernetes.io/enforce=baseline pod-security.kubernetes.io/warn=restricted >/dev/null

# ---- 3. local CAs, certificates and secrets ---------------------------------
T="$STATE_DIR/tls"
ca() { # ca <name> <CN>
  [ -s "$T/$1.crt" ] && return 0
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
    -keyout "$T/$1.key" -out "$T/$1.crt" -days 397 -subj "/CN=$2" \
    -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
}
leaf() { # leaf <ca> <name> <CN> <eku> [subjectAltName]
  [ -s "$T/$2.crt" ] && return 0
  local ext="$T/$2.ext"
  {
    echo "basicConstraints=critical,CA:FALSE"
    echo "keyUsage=critical,digitalSignature"
    echo "extendedKeyUsage=$4"
    [ -z "${5:-}" ] || echo "subjectAltName=$5"
  } >"$ext"
  openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
    -keyout "$T/$2.key" -out "$T/$2.csr" -subj "/CN=$3" 2>/dev/null
  openssl x509 -req -in "$T/$2.csr" -CA "$T/$1.crt" -CAkey "$T/$1.key" -CAcreateserial \
    -days 397 -extfile "$ext" -out "$T/$2.crt" 2>/dev/null
  rm -f "${T:?}/$2.csr" "${T:?}/$2.ext"
}
secret_tls() { # secret_tls <ns> <secret> <name>
  kc -n "$1" create secret tls "$2" --cert "$T/$3.crt" --key "$T/$3.key" --dry-run=client -o yaml | kc apply -f - >/dev/null
}
secret_ca() { # secret_ca <ns> <secret> <ca>
  kc -n "$1" create secret generic "$2" --from-file=ca.crt="$T/$3.crt" --dry-run=client -o yaml | kc apply -f - >/dev/null
}
rand_file() { # rand_file <name>: a random hex value kept in the state dir, never printed
  [ -s "$STATE_DIR/$1" ] || (umask 077 && openssl rand -hex 24 >"$STATE_DIR/$1")
  cat "$STATE_DIR/$1"
}

log "generating the local CAs and certificates (dev only; kept in ${STATE_DIR}/tls)"
ca local-ca "TinyCDI quickstart local CA (dev only)"
ca internal-ca "TinyCDI quickstart internal mTLS CA (dev only)"
leaf local-ca edge "tcdi-quickstart-edge" serverAuth \
  "DNS:${PORTAL_HOST},DNS:${KEYCLOAK_HOST},DNS:*.${SESSION_DOMAIN}"
leaf local-ca postgres "postgres" serverAuth \
  "DNS:postgres,DNS:postgres.${NS_DEPS},DNS:postgres.${NS_DEPS}.svc,DNS:postgres.${NS_DEPS}.svc.cluster.local"
leaf internal-ca backend-internal "backend" serverAuth \
  "DNS:backend,DNS:backend.${NS_SYSTEM},DNS:backend.${NS_SYSTEM}.svc,DNS:backend.${NS_SYSTEM}.svc.cluster.local"
leaf internal-ca operator-client "operator" clientAuth

DB_PASSWORD="$(rand_file db-password)"
CLIENT_SECRET="$(rand_file oidc-client-secret)"
KC_ADMIN_PASSWORD="$(rand_file keycloak-admin-password)"

# deps namespace
kc -n "$NS_DEPS" create secret generic postgres-auth --from-literal=password="$DB_PASSWORD" \
  --dry-run=client -o yaml | kc apply -f - >/dev/null
secret_tls "$NS_DEPS" postgres-tls postgres
secret_tls "$NS_DEPS" edge-tls edge
kc -n "$NS_DEPS" create secret generic keycloak-admin --from-literal=username=admin \
  --from-literal=password="$KC_ADMIN_PASSWORD" --dry-run=client -o yaml | kc apply -f - >/dev/null
sed -e "s|@CLIENT_SECRET@|${CLIENT_SECRET}|g" -e "s|@PORTAL_HOST@|${PORTAL_HOST}|g" \
  -e "s|@DEMO_USER@|${DEMO_USER}|g" -e "s|@DEMO_PASSWORD@|${DEMO_PASSWORD}|g" \
  -e "s|@NOQUOTA_USER@|${NOQUOTA_USER}|g" \
  "$QS_DIR/realm-export.json" >"$STATE_DIR/realm.json"
chmod 600 "$STATE_DIR/realm.json"
kc -n "$NS_DEPS" create secret generic keycloak-realm --from-file=tinycdi-realm.json="$STATE_DIR/realm.json" \
  --dry-run=client -o yaml | kc apply -f - >/dev/null

# release namespace (the chart only references these by name)
db_url="postgres://tinycdi:${DB_PASSWORD}@postgres.${NS_DEPS}.svc.cluster.local:5432/tinycdi"
kc -n "$NS_SYSTEM" create secret generic tinycdi-backend-db --from-literal=url="$db_url" \
  --dry-run=client -o yaml | kc apply -f - >/dev/null
kc -n "$NS_SYSTEM" create secret generic tinycdi-oidc-client --from-literal=client-secret="$CLIENT_SECRET" \
  --dry-run=client -o yaml | kc apply -f - >/dev/null
for s in tinycdi-backend-app-tls tinycdi-backend-session-tls tinycdi-frontend-tls tinycdi-ingress-tls; do
  secret_tls "$NS_SYSTEM" "$s" edge
done
secret_tls "$NS_SYSTEM" tinycdi-backend-internal-tls backend-internal
secret_tls "$NS_SYSTEM" tinycdi-operator-mtls operator-client
secret_ca "$NS_SYSTEM" tinycdi-internal-ca internal-ca
secret_ca "$NS_SYSTEM" tinycdi-local-ca local-ca
secret_ca "$NS_SYSTEM" tinycdi-db-ca local-ca

# ---- 4. ingress controller + dependencies ------------------------------------
subst() {
  sed -e "s|@POSTGRES_IMAGE@|${POSTGRES_IMAGE}|g" -e "s|@KEYCLOAK_IMAGE@|${KEYCLOAK_IMAGE}|g" \
    -e "s|@KEYCLOAK_HOST@|${KEYCLOAK_HOST}|g" "$1"
}
log "starting PostgreSQL and the dev Keycloak"
subst "$QS_DIR/deps.yaml" | kc apply -f - >/dev/null

log "installing Traefik ${TRAEFIK_VERSION} (chart ${TRAEFIK_CHART_VERSION})"
chart_tgz="$STATE_DIR/traefik-${TRAEFIK_CHART_VERSION}.tgz"
if [ ! -s "$chart_tgz" ]; then
  curl -fsSL --retry 3 -o "$chart_tgz" "$TRAEFIK_CHART_URL"
fi
echo "${TRAEFIK_CHART_SHA256}  ${chart_tgz}" | sha256sum -c - >/dev/null || die "Traefik chart checksum mismatch"
helm upgrade --install traefik "$chart_tgz" -n "$NS_INGRESS" -f "$QS_DIR/traefik-values.yaml" \
  --set "image.digest=${TRAEFIK_DIGEST}" --set "versionOverride=${TRAEFIK_VERSION}" \
  --wait --timeout 5m >"$STATE_DIR/logs/helm-traefik.log" 2>&1 || { tail -n 30 "$STATE_DIR/logs/helm-traefik.log" >&2; die "Traefik install failed"; }

log "waiting for PostgreSQL and Keycloak"
kc -n "$NS_DEPS" rollout status statefulset/postgres --timeout=300s >/dev/null
kc -n "$NS_DEPS" rollout status deployment/keycloak --timeout=600s >/dev/null

# ---- 5. TinyCDI ----------------------------------------------------------------
# kindnet enforces NetworkPolicy after the kubernetes Service is NAT-ed, so the
# egress rule for the apiserver has to name the control-plane endpoint (and its
# port), not only the Service IP the chart defaults to.
api_ip="$(kc -n default get endpointslices -l kubernetes.io/service-name=kubernetes \
  -o jsonpath='{.items[0].endpoints[0].addresses[0]}')"
api_port="$(kc -n default get endpointslices -l kubernetes.io/service-name=kubernetes \
  -o jsonpath='{.items[0].ports[0].port}')"
if [ -z "$api_ip" ] || [ -z "$api_port" ]; then
  die "could not resolve the kubernetes apiserver endpoint"
fi

GEN="$STATE_DIR/values-generated.yaml"
{
  echo "# generated by up.sh - do not edit"
  echo "portalHost: ${PORTAL_HOST}"
  echo "sessionDomain: ${SESSION_DOMAIN}"
  echo "oidc:"
  echo "  issuer: https://${KEYCLOAK_HOST}/realms/tinycdi"
  # Back to the portal's signed-out page after the IdP session ends;
  # realm-export.json registers exactly this post-logout redirect URI.
  echo "  postLogoutRedirect: https://${PORTAL_HOST}/signed-out"
  echo "networkPolicy:"
  echo "  apiServerPort: ${api_port}"
  echo "  apiServerPeers:"
  echo "    - ipBlock: {cidr: ${api_ip}/32}"
  echo "images:"
  for c in backend operator frontend; do
    echo "  ${c}:"
    if [ "$IMAGES" = build ]; then
      echo "    registry: tcdi-qs.local"
      echo "    repository: tcdi-qs/${c}"
    fi
    [ -z "$IMAGE_TAG_RESOLVED" ] || echo "    tag: \"${IMAGE_TAG_RESOLVED}\""
  done
  echo "  linuxDesktop:"
  echo "    digest: ${DESKTOP_DIGEST}"
  echo "    builtAt: \"${DESKTOP_BUILT_AT}\""
  echo "  browser:"
  echo "    digest: ${BROWSER_DIGEST}"
  echo "    builtAt: \"${BROWSER_BUILT_AT}\""
} >"$GEN"

log "installing TinyCDI from ${REPO_ROOT}/deploy/helm/tinycdi"
helm upgrade --install "$RELEASE" "$REPO_ROOT/deploy/helm/tinycdi" -n "$NS_SYSTEM" \
  -f "$QS_DIR/values.yaml" -f "$GEN" --wait --timeout 10m \
  >"$STATE_DIR/logs/helm-tinycdi.log" 2>&1 || { tail -n 40 "$STATE_DIR/logs/helm-tinycdi.log" >&2; die "TinyCDI install failed"; }

# ---- 6. verify end to end (TLS chain, routing, OIDC discovery) -------------
curl_ca() { # curl_ca <host> <url...>: verified TLS against the local CA, host pinned to loopback
  local host="$1"
  shift
  curl -fsS --retry 30 --retry-delay 2 --retry-connrefused --retry-all-errors --max-time 10 \
    --cacert "$T/local-ca.crt" --resolve "${host}:443:127.0.0.1" "$@"
}
log "checking the portal, the OIDC issuer and a session host through https"
curl_ca "$PORTAL_HOST" -o /dev/null "https://${PORTAL_HOST}/"
curl_ca "$KEYCLOAK_HOST" -o /dev/null "https://${KEYCLOAK_HOST}/realms/tinycdi/.well-known/openid-configuration"
# Traefik picks the new Ingress up asynchronously, so retry until the session
# host answers (400 and 5xx mean the router is not there yet).
code=""
for _ in $(seq 1 60); do
  code="$(curl -sS --max-time 10 --cacert "$T/local-ca.crt" --resolve "ws-check.${SESSION_DOMAIN}:443:127.0.0.1" \
    -o /dev/null -w '%{http_code}' "https://ws-check.${SESSION_DOMAIN}/" || true)"
  case "$code" in 000 | 400 | 5??) code="" ;; esac
  [ -n "$code" ] && break
  sleep 2
done
if [ -z "$code" ]; then
  die "session host ws-check.${SESSION_DOMAIN} did not answer over https"
fi

# The runtime images were pulled in the background; give them time to finish
# so the first workspace starts quickly (a slow pull is only a warning).
for pid in "${PREFETCH_PIDS[@]}"; do wait "$pid" || true; done

cat >"$STATE_DIR/env" <<ENVEOF
TCDI_QS_PORTAL_URL=https://${PORTAL_HOST}
TCDI_QS_SESSION_DOMAIN=${SESSION_DOMAIN}
TCDI_QS_USER=${DEMO_USER}
TCDI_QS_NOQUOTA_USER=${NOQUOTA_USER}
TCDI_QS_CA_FILE=${T}/local-ca.crt
KUBECONFIG=${KUBECONFIG}
ENVEOF

cat <<MSG

TinyCDI quickstart is up after ${SECONDS}s (DEV ONLY - loopback, throwaway).

  Portal   https://${PORTAL_HOST}
  Login    ${DEMO_USER} / ${DEMO_PASSWORD}   (dev realm; exists only inside kind cluster ${CLUSTER})
  CA       ${T}/local-ca.crt   (import it, or accept the browser's certificate warning)
  kubectl  export KUBECONFIG=${KUBECONFIG}
  Remove   hack/quickstart/down.sh

MSG
