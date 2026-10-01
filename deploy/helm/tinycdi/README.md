# tinycdi — official Helm chart

TinyCDI is a Kubernetes-native Linux workspace platform: an **API**, an
**operator**, a session **gateway** and a web **portal**. This chart
installs all four components, the `workspaces.cdi.tinyorbit.vn` CRDs, and the
namespaced RBAC/NetworkPolicy baseline — nothing else. It never ships
Secrets, never installs KubeVirt/CDI, a database, or a dev OIDC provider.

* Chart: `deploy/helm/tinycdi` (Helm v4, `apiVersion: v2`)
* OCI artifact: `oci://ghcr.io/tinyorbitvn/charts/tinycdi`
* Source: <https://github.com/tinyorbitvn/tinycdi> · License: MIT

## Prerequisites

| Requirement | Notes |
|---|---|
| Kubernetes | `>= 1.30` (CEL-immutable WorkspaceTemplate specs) |
| PostgreSQL | reachable from the cluster; DSN in a Secret (`database.existingSecret`); the api runs embedded migrations at startup, so the role needs DDL rights |
| OIDC provider | a client with redirect URL `https://<portalHost>/v1/auth/callback`; client secret in a Secret (`oidc.existingSecret`) |
| TLS | portal + session certificates (pods terminate TLS themselves) and the internal mTLS chain — either pre-created Secrets or `certManager.enabled` |
| Node profiles (browser sandbox) | Browser templates reference **Localhost** seccomp/AppArmor profiles that must be pre-loaded on nodes (`<kubelet-root>/seccomp/profiles/`, `apparmor_parser`). Either set `nodeProfiles.install.enabled` (chart-managed DaemonSet — see [Node profiles](#node-profiles)) or provision them yourself per `deploy/node-profiles/` |
| Release namespace PSS | The release namespace stays **baseline/restricted** — it never needs privileged pods. When `nodeProfiles.install.enabled`, only the DEDICATED installer namespace (`nodeProfiles.install.namespace`, default `tinycdi-node-profiles`) must allow privileged pods; the chart creates it labelled `pod-security.kubernetes.io/enforce=privileged` (`createNamespace: false` = label it yourself) |
| Storage | a StorageClass if you use `Retain` dataPolicy (per-template `storageClass` or top-level `storageClass`) |

## Install

```bash
helm install tinycdi oci://ghcr.io/tinyorbitvn/charts/tinycdi \
  -n tinycdi-system --create-namespace -f my-values.yaml
# or from a checkout:
helm install tinycdi deploy/helm/tinycdi -n tinycdi-system -f my-values.yaml
```

Start from `ci/example-values.yaml`; the minimal set is
`ci/minimal-values.yaml`. `portalHost` and `sessionHost` must be
**different registrable hosts** (enforced at render). See
`docs/runbooks/install.md` for the full procedure including secrets and
the apiserver NetworkPolicy peers.

## Upgrade

```bash
helm upgrade tinycdi deploy/helm/tinycdi -n tinycdi-system -f my-values.yaml
```

Ordering guidance and rollback semantics: `docs/runbooks/upgrade.md`.
Seeded WorkspaceTemplates are immutable `<name>-<hash8>` revision objects —
a spec change creates a new revision and removes the superseded one, so
upgrades and rollbacks never hit the CEL immutability wall.

## Uninstall and CRD policy

```bash
helm uninstall tinycdi -n tinycdi-system
```

Removes the platform Deployments/Services/RBAC and release-namespace
objects. It **keeps**:

- CRDs — Helm installs `crds/` once and never upgrades or deletes them.
  Reconcile CRD drift by hand: `kubectl diff -f crds/` then
  `kubectl apply -f crds/` after review. Deleting a CRD deletes every CR.
- Managed namespaces and everything inside (Workspaces, runtime pods,
  **user PVCs**) — they carry `helm.sh/resource-policy: keep`.
- Secrets you created manually.

## Parameters

### Global

| Key | Default | Description |
|---|---|---|
| `global.imageRegistry` | `ghcr.io` | registry prepended to every `images.*.repository` (e.g. `registry.example.com`); per-image `registry` wins |
| `global.imagePullSecrets` | `[]` | pull secrets attached to every platform pod (names or `{name: ...}` maps) |
| `images.<name>.repository` | `tinyorbitvn/tinycdi-<name>` | image path (joined under the registry); names: `api`, `operator`, `gateway`, `portal`, `linuxDesktop`, `browser` |
| `images.<name>.tag` | chart `appVersion` | tag; ignored when `digest` is set |
| `images.<name>.digest` | `""` | `sha256:<64hex>` — digest pinning wins over tag |
| `images.<name>.pullPolicy` | `IfNotPresent` | per-image pull policy |

### Topology

| Key | Default | Description |
|---|---|---|
| `portalHost` / `sessionHost` | `*.example.invalid` | public hostnames — must differ (render-time guard) |
| `managedNamespaces[]` | `[]` | `{name, tenant}` — tenant namespaces created with `resource-policy: keep`. The operator's manager-role is bound ONLY here and `--watch-namespaces` lists exactly these (never the release namespace, SEC-09); with an empty list the operator watches all namespaces, which its RBAC denies |
| `podSecurity.platformEnforce` / `.managedEnforce` | `baseline` / `restricted` | PSS labels on created namespaces; `managedEnforce=privileged` needs `dev.enabled` (CHTR-2) |

### Credentials (existing Secrets only — never values)

| Key | Secret holds | Used by |
|---|---|---|
| `database.existingSecret` / `.urlKey` | Postgres DSN (`url`) | api `TCDI_DATABASE_URL` |
| `database.tls.caSecret.name` / `.key` | DB server CA bundle | api `PGSSLROOTCERT` (with `database.tls.mode`, e.g. `verify-full`, exported as `PGSSLMODE`) |
| `oidc.existingSecret` / `.clientSecretKey` | OIDC client secret | api `TCDI_OIDC_CLIENT_SECRET` |
| `api.internalTLS.existingSecret` | `tls.crt`/`tls.key` | api internal mTLS listener :9443 |
| `api.clientCA.existingSecret` | `ca.crt` | api verifies broker client certs |
| `api.extraCA.existingSecret` | `ca.crt` | optional outbound trust (`SSL_CERT_FILE`) |
| `gateway.tls.existingSecret` | `tls.crt`/`tls.key` | session edge TLS |
| `gateway.mtls.existingSecret` | `tls.crt`/`tls.key` | gateway broker client cert |
| `gateway.trustedCA.existingSecret` | `ca.crt` | gateway verifies the broker cert (`-broker-ca`, required) |
| `gateway.upstreamCA.existingSecret` | `ca.crt` | optional runtime upstream CA |
| `gateway.controlToken.existingSecret` | `token` | `/v1/control/*` bearer; unset = routes closed |
| `operator.brokerClient.mtls.existingSecret` | `tls.crt`/`tls.key` | operator broker client cert — **CN must equal `api.operatorCN` (default `operator`)** |
| `operator.brokerClient.ca.existingSecret` | `ca.crt` | operator verifies the broker cert |
| `portal.tls.existingSecret` | `tls.crt`/`tls.key` | portal TLS |
| `ingress.tls.existingSecret` | `tls.crt`/`tls.key` | **required** when `ingress.enabled` — edge TLS is mandatory |

### Internal mTLS via cert-manager

| Key | Default | Description |
|---|---|---|
| `certManager.enabled` | `false` | render `Certificate` resources producing the internal mTLS Secrets |
| `certManager.issuerRef.{kind,name}` | `ClusterIssuer`/`""` | existing signer; required unless `selfSigned` |
| `certManager.selfSigned` | `false` | also bootstrap selfsigned ClusterIssuer → CA Certificate → Issuer |
| `certManager.caSecretName` | `<release>-internal-ca` | bootstrap CA Secret (point `*.clientCA`/`trustedCA`/`brokerClient.ca` at it) |
| `certManager.duration` / `.renewBefore` | `2160h` / `360h` | leaf lifetime/renewal |

### Exposure — pick ONE (`ingress.enabled` XOR `gatewayApi.enabled`; both → render fails)

| Key | Default | Description |
|---|---|---|
| `ingress.enabled` / `.className` / `.annotations` | `false`/`""`/`{}` | one Ingress per host; pods terminate TLS — use a pass-through backend annotation (e.g. nginx `backend-protocol: "HTTPS"`) |
| `ingress.portalAnnotations` / `.sessionAnnotations` | `{}` | per-edge annotations |
| `gatewayApi.enabled` / `.parentRefs` / `.annotations` | `false`/`[]`/`{}` | one `HTTPRoute` per host; backends are HTTPS — gateway must re-encrypt/pass through |
| `<component>.service.{type,nodePort,loadBalancerIP,loadBalancerSourceRanges,annotations}` | `ClusterIP` | direct exposure option for `portal`/`gateway` |

### Per-component tuning (`api`, `operator`, `gateway`, `portal`)

| Key | Default | Description |
|---|---|---|
| `<c>.replicas` | `1` | |
| `<c>.resources` | set | requests+limits required by chart tests |
| `<c>.podAnnotations` | `{}` | pod template annotations |
| `<c>.nodeSelector` / `.tolerations` / `.affinity` | `{}`/`[]`/`{}` | platform pod placement |
| `<c>.podSecurityContext` / `.securityContext` | `{}` | merged **over** the hardened defaults (non-root, drop ALL, RO rootfs); keys that would WEAKEN them (privileged, allowPrivilegeEscalation, added caps or a `drop` list missing ALL, root uid/gid/fsGroup/supplementalGroups, writable rootfs, Unconfined seccomp/AppArmor, seLinuxOptions) fail the render unless `dev.enabled` |
| `<c>.podDisruptionBudget.{enabled,minAvailable,maxUnavailable}` | disabled | PDB per component |
| `<c>.extraArgs` / `.extraEnv` | `[]` | escape hatch — dangerous flags (operator `--dev-allow-no-broker`/`--disable-builtin-egress-excepts`/metrics flags, api `--dev-insecure-db`/`--required-groups`, gateway `--metrics-listen`) and `gateway.extraVolumes` hostPath fail the render unless `dev.enabled` |

### Component-specific highlights

| Key | Default | Description |
|---|---|---|
| `api.sessionIdle` | `30m` | session idle timeout |
| `api.extraPortalOrigins` | `[]` | extra CSRF Origin allowlist entries |
| `api.operatorCN` | `""` (=`operator`) | CN required on the operator broker client cert |
| `operator.leaderElect` / `.webhookPort` | `false` / `-1` | |
| `operator.internetExceptCIDRs` | `[]` | subtracted from runtime `InternetOnly` egress |
| `operator.clusterCIDRs` | `[]` | this cluster's pod/service/node CIDRs — appended to `--internet-except-cidrs`; **required** (render fails) when any seeded template uses `networkProfile: InternetOnly` |
| `operator.brokerClient.enabled` | `true` | internal broker wiring (teardown finalizer); `devAllowNoBroker` is dev-only — needs `dev.enabled` |
| `database.tls.mode` | `verify-full` | `PGSSLMODE` on the api — only `verify-ca`/`verify-full` render without `dev.enabled` (the api refuses weaker sslmodes at startup, CHTR-8); dev + insecure mode renders `--dev-insecure-db`; DSN `sslmode=` still wins |
| `database.allowedPeers` | deny-all placeholder | api→DB NetworkPolicy peers — **required**: an empty list OR the shipped `0.0.0.0/32` placeholder fails the render |
| `oidc.requiredGroups` | `[]` | login gate — api flag `--required-groups=<csv>`; ID-token `groups` must carry one listed group (exact match); empty = every IdP account may log in |
| `oidc.egressCIDRs` | `[0.0.0.0/0]` | api→IdP egress CIDRs — **required** non-empty, narrow to your IdP |
| `dev.enabled` | `false` | dev gate: required for `operator.devAllowNoBroker`, dangerous `extraArgs`, a non-verifying `database.tls.mode`, `podSecurity.managedEnforce=privileged`, `gateway.extraVolumes` hostPath, and any securityContext override that weakens the hardened defaults |
| `gateway.metricsListen` | `""` | e.g. `":9090"` — served on the dedicated ClusterIP `gateway-metrics` Service, never on the public gateway port |
| `gateway.extraAllowedHosts` / `.extraPortalOrigins` | `[]` | Host-header / launch-Origin extras |

### Observability & network

| Key | Default | Description |
|---|---|---|
| `serviceMonitor.enabled` | `false` | ServiceMonitor for the gateway metrics endpoint (`gateway-metrics` Service). The operator exposes no metrics endpoint: secure metrics need cluster-scoped TokenReview/SAR RBAC the chart never grants, and HTTP metrics are banned — `--metrics-bind-address=0` is pinned |
| `serviceMonitor.labels` / `.interval` / `.scrapeTimeout` / `.honorLabels` | `{}`/`""`/`""`/`false` | Prometheus Operator selection + timing |
| `networkPolicy.enabled` | `true` | default-deny baseline + allow rules (incl. the operator↔api :9443 broker rule) |
| `networkPolicy.apiServerPeers` / `.apiServerPort` | `10.96.0.1/32` / `443` | apiserver egress — set your `kubernetes.default` ClusterIP |
| `networkPolicy.dnsPeers` | kube-system pods | DNS egress |
| `networkPolicy.prometheusPeers` | `[]` | metrics-scrape ingress peers — **required** (render fails) when `gateway.metricsListen` is set; scope to your monitoring namespace/pods |

### Runtime catalog (`templates[]`)

Seeded `WorkspaceTemplate` objects, published as immutable `<name>-<hash8>`
revisions. Per entry:

| Field | Description |
|---|---|
| `name` / `namespace` | catalog name; must be a managed namespace |
| `image` | key into `images` (`linuxDesktop`, `browser`) or literal ref; used when `spec.linux.image` is empty — runtime images must be **digest-pinned** |
| `seccompProfile` / `appArmorProfile` | Localhost node profile names → `localhost/<name>` annotations (must be pre-loaded on nodes) |
| `nodeSelector` | map → `workspaces.cdi.tinyorbit.vn/node-selector` JSON annotation (runtime pod placement; tolerations are not supported by the backend) |
| `storageClass` / `annotations` / `spec` | per-template SC override, verbatim annotations, verbatim spec |

### Kasm workspace images

Templates may run **unmodified `kasmweb/*` workspace images** (the public
kasmtech/workspaces-images catalog) by setting `spec.linux.adapter: kasm`.
The operator then injects the TinyCDI adapter into the pod — an
initContainer (the `tinycdi-kasm-adapter` image) copies the adapter
scripts into a shared volume and the runtime container starts them
instead of the image's own Kasm startup; see `docs/kasm-images.md` for
the adapter contract and the per-image onboarding checklist.

| Key | Default | Description |
|---|---|---|
| `kasmAdapter.image.{repository,tag,digest}` | `tinyorbitvn/tinycdi-kasm-adapter` | adapter init image → operator `--kasm-adapter-image`. **Digest is required**: the flag renders only for a digest-pinned ref, a `tag` without `digest` fails the render, and a seeded `adapter: kasm` template without the digest fails the render (the backend would reject the workspaces anyway) |

A seeded kasm template sets `spec.linux.adapter: kasm`, an optional
`spec.linux.sessionCmd` (the session payload — for Chromium-family images
it MUST point past the image's `--no-sandbox` wrapper at the real
binary), and the runtime `image:` as a literal digest-pinned
`kasmweb/<app>@sha256:…` reference (it is pulled by the nodes straight
from Docker Hub — never mirrored/republished). `ci/example-values.yaml`
ships a complete `kasmweb/chromium` example.

### Node-profile installer (`nodeProfiles.install`) — default OFF

| Key | Default | Description |
|---|---|---|
| `nodeProfiles.install.enabled` | `false` | render the installer DaemonSet (privileged initContainer + least-privilege verifier) + its ServiceAccount + immutable ConfigMap |
| `nodeProfiles.install.namespace` / `.createNamespace` | `tinycdi-node-profiles` / `true` | dedicated PSS-privileged installer namespace — the render refuses the release namespace, managed namespaces, `kube-*` and `default`; `false` = you create it labelled `pod-security.kubernetes.io/enforce=privileged` |
| `nodeProfiles.install.mode` | `install` | `install` writes+loads profiles; `remove` unloads+deletes them (see [Node profiles](#node-profiles)) |
| `nodeProfiles.install.image.{repository,tag,digest}` | `alpine` / `3.21` / digest-pinned | minimal publicly-pullable image (busybox nsenter); digest wins over tag — keep it pinned |
| `nodeProfiles.install.kubeletRoot` | `/var/lib/kubelet` | kubelet `--root-dir` on the nodes — the Localhost seccomp base |
| `nodeProfiles.install.nodeSelector` | `{}` | merged over `kubernetes.io/os=linux`; a built-in nodeAffinity always excludes control-plane/master |
| `nodeProfiles.install.tolerations` | `[]` | installer pod tolerations (control-plane taints NOT included by default) |
| `nodeProfiles.install.priorityClassName` | `""` | optional |
| `nodeProfiles.install.resources` | small defaults | resources for BOTH installer initContainer and verifier |

## Node profiles

Browser workspaces (`experience: Browser` templates with
`seccompProfile: localhost/profiles/chromium-userns.json` and
`appArmorProfile: tinycdi-browser`) need both profiles **pre-loaded on the
node** — Kubernetes never distributes them. Two ways to get them there:

- **Chart-managed (recommended for GitOps):** set
  `nodeProfiles.install.enabled=true`. The chart renders a `node-profiles`
  DaemonSet in its own `nodeProfiles.install.namespace` (default
  `tinycdi-node-profiles`, PSS `privileged`, **no RBAC**) that on every
  matched node (all Linux
  nodes except control-plane by default) writes
  `<nodeProfiles.install.kubeletRoot>/seccomp/profiles/chromium-userns.json`
  atomically, resolves the AppArmor template's `peer=<daemon>` placeholder
  to the node's real containerd AppArmor label, installs
  `/etc/apparmor.d/tinycdi-browser`, and loads it with the host's
  `apparmor_parser -r -K` via `nsenter` into PID 1's mount namespace — so it
  **survives reboots** (host apparmor init reloads `/etc/apparmor.d`, and
  the pod re-runs on every restart). It refuses with a clear log and stays
  NotReady on nodes without AppArmor/`apparmor_parser`.
- **Externally provisioned:** follow `deploy/node-profiles/README.md` and
  leave `nodeProfiles.install.enabled=false` (the default).

Least privilege on the DaemonSet: the privileged + hostPID work is an
**initContainer** (`install.sh`, one-shot) — the long-running container is
a **least-privilege verifier**: uid 0 with **every capability dropped**,
`allowPrivilegeEscalation: false`, read-only rootfs and read-only hostPath
mounts. uid 0 is not decorative — the kernel serves
`/sys/kernel/security/apparmor/profiles` only to root (the 0444 mode is a
decoy: a non-root reader always gets EACCES), so uid 0 + an empty
capability set is the minimum that still verifies the loaded-profiles
list; the read-only mounts and null capset keep it strictly an observer.
Scripts and profiles ship in an
**immutable, content-hash-named** ConfigMap; a dedicated
`node-profiles` ServiceAccount has `automountServiceAccountToken: false`
and **zero RBAC** — nothing in the installer namespace can write
ConfigMaps or pods (asserted by the chart tests). Writable hostPaths are
limited to `<kubeletRoot>/seccomp/profiles` and `/etc/apparmor.d` — the
whole kubelet root (which holds every pod's projected secrets) is never
mounted; the verifier reads the host's AppArmor loaded list through a
read-only bind-mount of the host's securityfs (`/sys/kernel/security` —
a plain `/sys` bind would not carry it, it is a separate filesystem).
The pod template carries a
`checksum/node-profiles` annotation, so any profile change rolls the
installer and re-runs it idempotently.

**Rollout race:** the DaemonSet and browser `WorkspaceTemplate`s land in the
same `helm install`. A browser pod scheduled onto a node whose installer
pod is not yet Ready **fails to start** (missing Localhost profile) and is
retried — it recovers on its own, but gate bulk browser rollouts on:

```bash
kubectl -n <installer-ns> get ds node-profiles   # wait for READY == DESIRED
kubectl -n <installer-ns> logs ds/node-profiles -c install  # per-node install report
```

**Removing the profiles:** deleting the DaemonSet does **not** unload
anything — AppArmor profile removal needs the host parser, and the seccomp
file stays behind. To clean nodes first drain browser workloads (or drop
the `seccompProfile`/`appArmorProfile` keys from the template — unset falls
back to the RuntimeDefault/containerd profile; do **not** set
`appArmorProfile: unconfined` — it renders as `localhost/unconfined`, which
the runtime accepts as the kernel's unconfined pseudo-profile: it strips
confinement instead of restoring the default), then upgrade with
`nodeProfiles.install.mode=remove`; once the DaemonSet reports Ready on all
nodes (removal verified), set `nodeProfiles.install.enabled=false` (or
uninstall) to drop the DaemonSet.

### Tenant quota

The API has **no quota-config endpoint** — `tenant_quota` rows live in
Postgres and are seeded by SQL (see `docs/runbooks/capacity.md`). The
chart intentionally does not model quota values.

## Validation & tests

`values.schema.json` rejects malformed values at lint/template time
(https-only issuers/origins, hostname-patterned `portalHost`/`sessionHost`,
comma-free `oidc.requiredGroups` entries, pinned installer image).
Render-time guards refuse equal portal/session
hosts, conflicting exposure modes, a cert-manager toggle without an
issuer, empty or placeholder-only `database.allowedPeers`, empty
`oidc.egressCIDRs`/`networkPolicy.prometheusPeers`, ingress without TLS,
HTTPRoute parentRefs without `sectionName`, InternetOnly templates without
`operator.clusterCIDRs`, an installer namespace equal to the release
namespace/a managed namespace/`kube-*`/`default`, `database.tls.mode`
`disable`/`allow`, `podSecurity.managedEnforce=privileged`, hostPath
`gateway.extraVolumes`, dangerous `extraArgs`, and privilege-weakening
securityContext overrides — the dev surfaces all unless `dev.enabled`. Run
`go test ./deploy/helm/` for the full assertion suite (helm resolved via
`$TCDI_HELM`, `bin/helm`, or `PATH`); the installer behaviour is
additionally proven inside the pinned alpine image by
`deploy/helm/proof/node-profiles/run.sh` (docker-gated test).
