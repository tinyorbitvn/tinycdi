# Runbook — Install TinyCDI from the official Helm chart

Chart: `deploy/helm/tinycdi` (Helm v4, `apiVersion: v2`), also published
as `oci://ghcr.io/tinyorbitvn/charts/tinycdi` by the release workflow.
Requires Helm v4 (`helm` on `PATH`, or `$TCDI_HELM` / `bin/helm` — the
same resolution order `deploy/helm/chart_test.go` uses).

> **Authorization gate (shared cluster).** The chart ships CRDs under `crds/`.
> Helm applies them cluster-wide on first `helm install`. On a shared
> cluster this is a cluster-wide mutation and **requires the user's
> explicit authorization before running `helm install`**. If you do not
> have that authorization, stop at `helm template` / `helm lint`.

## What the chart installs

- `api`, `operator`, `gateway`, `portal` Deployments + Services in the
  release namespace (recommend `tinycdi-system`).
- Managed (tenant) namespaces from `managedNamespaces`, with
  `helm.sh/resource-policy: keep`, the tenant label
  `workspaces.cdi.tinyorbit.vn/tenant=<tenant>` and a `restricted` PSS
  enforcement label.
- RBAC: two ClusterRoles (`<release>-manager-role`, `<release>-api-role`)
  bound **only** via RoleBindings in the release + managed namespaces; a
  leader-election Role/RoleBinding in the release namespace. The chart
  never creates a ClusterRoleBinding; the operator's manager-role is bound
  ONLY in the managed namespaces and it runs with
  `--watch-namespaces=<managed ns>` so its informer cache matches the RBAC
  allowlist exactly (no Secrets/Pods grant in the platform namespace).
- NetworkPolicy baseline: default-deny (Ingress+Egress) in the release and
  every managed namespace; DNS, apiserver, api/portal/gateway allow rules
  (including the operator↔api :9443 broker rule), plus a gateway-pod-scoped
  metrics-scrape rule when `gateway.metricsListen` is set — that also
  requires `networkPolicy.prometheusPeers` (empty peer lists fail the
  render; `database.allowedPeers` and `oidc.egressCIDRs` are likewise
  mandatory when `networkPolicy.enabled`, and `allowedPeers` must not be
  the shipped `0.0.0.0/32` deny-all placeholder).
- Optional exposure: `ingress.enabled` (one Ingress per host) **or**
  `gatewayApi.enabled` (one HTTPRoute per host) — mutually exclusive,
  enforced at render. Otherwise expose the `portal`/`gateway` Services
  directly (`service.type`).
- Optional cert-manager Certificates for the internal mTLS chain
  (`certManager.enabled`).
- Optional seeded `WorkspaceTemplate` CRs (digest-pinned images; per
  -template Localhost seccomp/AppArmor profile, nodeSelector and
  StorageClass via structured fields or annotations).
- `tinycdi-runtime` ServiceAccount per managed namespace with
  `automountServiceAccountToken: false`.

The chart **never** installs KubeVirt, CDI, a PostgreSQL database, a dev
OIDC provider, or any other bundled dependency, and **never** carries
secret values — every Secret is a pre-existing object referenced by name
(or produced by cert-manager when `certManager.enabled`).

## Prerequisites check

```bash
HELM=helm
K=kubectl
$HELM version            # >= v4.x
$K version               # server >= 1.30 (chart kubeVersion floor)
$K auth can-i create namespace            # platform namespace
$K auth can-i create customresourcedefinitions.apiextensions.k8s.io
# ^ cluster-wide — on a shared cluster this needs the user's authorization.
```

Also gather:

1. **Two public hostnames** — `portal.<dom>` and `session.<dom>`, different
   registrable hosts, DNS pointing at your ingress/LB.
2. **OIDC client** at your IdP: redirect URL
   `https://<portalHost>/v1/auth/callback`.
3. **PostgreSQL** reachable from the cluster; a DSN for a role with DDL
   rights on its schema (the api runs embedded migrations at startup).
   The chart does NOT bundle Postgres. Enforce TLS on the DSN with
   `database.tls.mode` (exported as `PGSSLMODE`; `verify-full`
   recommended) plus `database.tls.caSecret` (mounted read-only as
   `PGSSLROOTCERT`) — an explicit `sslmode=`/`sslrootcert=` in the DSN
   still wins. An in-the-clear `sslmode=disable` is acceptable only for a
   private link you trust.
4. **apiserver Service IP** for NetworkPolicy egress:
   `$K -n default get svc kubernetes -o jsonpath='{.spec.clusterIP}'`
   (kind: `10.96.0.1`, RKE2/canal: usually `10.43.0.1`) →
   `networkPolicy.apiServerPeers`/`apiServerPort`.
5. **Images** — default `ghcr.io/tinyorbitvn/tinycdi-{api,operator,
   gateway,portal,linux-desktop,browser}` tagged with the chart
   appVersion. To pull from a mirror registry set
   `global.imageRegistry: registry.example.com` (optionally per-image
   `registry`/`repository` overrides) plus `global.imagePullSecrets`.
   Digest-pin production images via `images.<name>.digest`; runtime
   (template) images MUST be digest-pinned — the CRD rejects tag-only
   references.
6. **A StorageClass** for retained home PVCs
   (`workspaces.cdi.tinyorbit.vn/storage-class` per template, or global
   `storageClass`).
7. **Node security profiles** for Browser templates: the chart's
   `seccompProfile`/`appArmorProfile` template fields select **Localhost**
   node profiles (`localhost/<name>`). They must be pre-loaded on the
   nodes that run browser sandboxes — seccomp JSON under
   `<kubelet-root>/seccomp/profiles/` and the AppArmor profile via
   `apparmor_parser`. The shipped profiles and install/verify/rollback
   steps live under `deploy/node-profiles/`
   (`seccomp/chromium-userns.json`, `apparmor/tinycdi-browser` — see
   `deploy/node-profiles/README.md`).
8. **TLS material** for the portal and session edges plus the internal
   mTLS chain — either pre-created Secrets (table below) or
   `certManager.enabled` for the internal chain. If you use Gateway API
   exposure, the gateway listener terminates browser TLS and must
   re-encrypt/pass through to the HTTPS backends.
9. **Workspace node pool** — by default (`runtime.placement.
   allowSharedNodes: false`) every runtime pod, and the node-profile
   installer DaemonSet, target a dedicated pool: nodes labeled
   `cdi.tinyorbit.vn/workspace=true` and tainted
   `cdi.tinyorbit.vn/workspace:NoSchedule`. Label and taint the pool
   BEFORE workspaces launch:

   ```bash
   for n in <pool-node-1> <pool-node-2>; do
     $K label node "$n" cdi.tinyorbit.vn/workspace=true
     $K taint node "$n" cdi.tinyorbit.vn/workspace:NoSchedule
   done
   ```

   Without the label, runtime pods stay Pending forever; without the
   taint, unrelated workloads keep landing on the pool. Point
   `runtime.placement.nodeSelector` / `runtime.placement.tolerations` at
   different values when your pool already exists under another label or
   taint — but never leave the selector empty (the render fails: a
   dedicated pool with no selector would schedule nowhere).

   **Shared nodes (opt-out).** `runtime.placement.allowSharedNodes: true`
   passes no placement flags at all: runtime pods schedule anywhere
   schedulable, and the install NOTES print a warning. That is the right
   choice for throwaway kind/dev clusters; in production it gives up
   node-level isolation — user desktops then share nodes (and their
   seccomp/AppArmor profile loading, memory pressure and image pulls)
   with unrelated cluster workloads.

### Secrets (create BEFORE install — the chart only references names)

All in the release namespace unless noted:

| Secret | Keys | Used by |
|---|---|---|
| `tinycdi-api-db` | `url` = Postgres DSN | api (`TCDI_DATABASE_URL`) |
| `tinycdi-oidc-client` | `client-secret` | api (`TCDI_OIDC_CLIENT_SECRET`) |
| `tinycdi-api-internal-tls` | `tls.crt`, `tls.key` | api internal mTLS listener (or `certManager.enabled`) |
| `tinycdi-internal-ca` | `ca.crt` | internal mTLS CA: api verifies client certs (`api.clientCA`), gateway verifies the broker cert (`gateway.trustedCA`), operator verifies it (`operator.brokerClient.ca`); split values if the two differ. With `certManager.selfSigned` the CA Secret is `<release>-internal-ca` |
| `tinycdi-gateway-tls` | `tls.crt`, `tls.key` | gateway public TLS |
| `tinycdi-gateway-mtls` | `tls.crt`, `tls.key` | gateway client cert to the broker (or `certManager.enabled`) |
| `tinycdi-operator-mtls` | `tls.crt`, `tls.key` | operator client cert to the broker — **CN must be `operator`** (or `api.operatorCN`): the only identity the internal workspace revoke/drain routes accept (ADR 0003). `certManager.enabled` issues it with the right CN |
| `tinycdi-portal-tls` | `tls.crt`, `tls.key` | portal TLS |
| `tinycdi-gateway-control-token` | `token` | gateway control endpoints — **optional**; set `gateway.controlToken.existingSecret` (key `token`). Without it the `/v1/control/*` routes stay closed (fail-closed) |
| `tinycdi-extra-ca` | `ca.crt` | optional: api outbound CA (`api.extraCA`), gateway broker CA (`gateway.trustedCA`), upstream CA |
| `tinycdi-ingress-tls` | `tls.crt`, `tls.key` | optional: ingress TLS when `ingress.enabled` |

Create them with `kubectl create secret tls/generic` from local files
kept outside version control — never put values in the values file or
commit them.

## Install

```bash
cp deploy/helm/tinycdi/ci/example-values.yaml my-values.yaml
# edit: portalHost/sessionHost, managedNamespaces, oidc, database,
# images digests, networkPolicy.apiServerPeers, storageClass, templates

# dry-run render first (no cluster access needed):
$HELM template tinycdi deploy/helm/tinycdi \
  -n tinycdi-system --include-crds -f my-values.yaml > rendered.yaml

# create the platform namespace with PSS labels (Helm does not label it):
# baseline is enough — nothing privileged runs here. Only the OPTIONAL
# nodeProfiles.install.namespace (chart-created as enforce=privileged when
# createNamespace=true, never the release namespace) needs privileged.
$K create namespace tinycdi-system
$K label namespace tinycdi-system \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/warn=restricted

# create the Secrets listed above, then:
$HELM install tinycdi deploy/helm/tinycdi \
  -n tinycdi-system -f my-values.yaml
```

First install applies the CRDs from `crds/` (cluster-wide — authorization
gate above). Verify:

```bash
$K -n tinycdi-system get deploy,po
$K get crd | grep workspaces.cdi.tinyorbit.vn
$K get netpol -A | grep default-deny
```

## Upgrade

```bash
$HELM upgrade tinycdi deploy/helm/tinycdi -n tinycdi-system -f my-values.yaml
```

**CRD policy:** Helm never upgrades and never deletes CRDs installed from
`crds/` — it applies them on first install only. To upgrade a CRD version
after reviewing the diff (`kubectl diff -f deploy/helm/tinycdi/crds/`),
apply it manually with `$K apply`. No `helm` flag changes this; deleting a
CRD deletes every CR of that kind — never do it on a cluster holding user
Workspaces. See `docs/runbooks/upgrade.md` for ordering and rollback.

## Uninstall (keeps CRDs and user data)

```bash
$HELM uninstall tinycdi -n tinycdi-system
```

This removes the platform Deployments/Services/RBAC and the objects Helm
created in the release namespace. It **keeps**:

- the CRDs (Helm never deletes them),
- the managed namespaces and everything inside — Workspace CRs, runtime
  Pods/Services/Secrets, and **user PVCs** (the namespaces carry
  `helm.sh/resource-policy: keep`),
- Secrets you created manually (and cert-manager Secrets — the
  Certificates are removed but their issued Secrets remain; delete them
  if you want the keys gone),
- the selfsigned bootstrap ClusterIssuer is cluster-scoped and IS removed
  with the release (Helm owns it).

If a full teardown is ever wanted: `kubectl delete ns tinycdi-tenant-*`
(this deletes user PVCs — confirm backups per
`docs/runbooks/backup-restore.md`), then `kubectl delete crd
workspaces.workspaces.cdi.tinyorbit.vn
workspacetemplates.workspaces.cdi.tinyorbit.vn` (deletes all CRs).

## Verifying the chart without a cluster

```bash
go test ./deploy/helm/ -count=1        # renders + asserts all invariants
$HELM lint --strict deploy/helm/tinycdi
```
