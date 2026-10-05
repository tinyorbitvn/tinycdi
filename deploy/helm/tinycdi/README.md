# tinycdi — official Helm chart

TinyCDI is a Kubernetes-native Linux workspace platform: a **backend**
(public API + session gateway in one binary), a **frontend** (static
portal SPA server) and an **operator**. This chart
installs the three components, the `workspaces.cdi.tinyorbit.vn` CRDs, and the
namespaced RBAC/NetworkPolicy baseline — nothing else. It never ships
Secrets, never installs KubeVirt/CDI, a database, or a dev OIDC provider.

> **v0.2:** [ADR 0005](../../../docs/adr/0005-backend-frontend-operator.md)
> (accepted) consolidated the v0.1 `api` + `gateway` + `portal` into
> `backend` + `frontend` and replaced `sessionHost` with a wildcard
> `sessionDomain` (one host per workspace). The values below describe the
> v0.2 chart.

* Chart: `deploy/helm/tinycdi` (Helm v4, `apiVersion: v2`)
* OCI artifact: `oci://ghcr.io/tinyorbitvn/charts/tinycdi`
* Source: <https://github.com/tinyorbitvn/tinycdi> · License: MIT

## Prerequisites

| Requirement | Notes |
|---|---|
| Kubernetes | `>= 1.30` (CEL-immutable WorkspaceTemplate specs) |
| PostgreSQL | reachable from the cluster; DSN in a Secret (`database.existingSecret`); the backend runs embedded migrations at startup, so the role needs DDL rights |
| OIDC provider | a client with redirect URL `https://<portalHost>/v1/auth/callback`; client secret in a Secret (`oidc.existingSecret`) |
| Login keys | AEAD keys sealing the `__Host-tcdi_login` cookie — a Secret with `current` (and `previous` while rotating), 32 bytes each (`backend.loginKeys.existingSecret`), or `backend.loginKeys.generate=true` |
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
`ci/minimal-values.yaml`. `portalHost` and `sessionDomain` must be
**disjoint** — `portalHost` may not equal or sit inside `sessionDomain`,
because the edge routes `*.<sessionDomain>` to the session listener and a
portal host in that scope would be captured by the wildcard (enforced at
render). Every workspace session is served on its own
`<label>.<sessionDomain>` host, which needs a wildcard DNS record and a
wildcard certificate (DNS-01 — HTTP-01 cannot issue wildcards). See
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

**Kasm note (E13):** a seeded `adapter: kasm` template with
`experience: Browser` **fails the render** after this upgrade until its
image is on `kasmAdapter.browserAllowlist` or the template is re-classed
as `experience: Desktop` — the allowlist is the chart-side binding to the
catalog's tightened ≤2-major browser engine gate (non-browser templates
keep the ≤4 budget). Both options:

```yaml
# Option A — the image meets the ≤2 gate: allowlist it (it must be a
# browser-class build/kasm-catalog.txt entry or the catalog check fails).
kasmAdapter:
  browserAllowlist:
    - kasmweb/<app>@sha256:<digest>

# Option B — the image misses the gate: keep it for non-browser use.
templates:
  - name: <tpl>
    spec:
      experience: Desktop    # was: Browser
```

`kasmweb/chromium` is desktop-class today (Chromium 150 vs the native 154
pin — lag 4), so a Browser template pinned to it only has option B. See
`docs/runbooks/upgrade.md` → "Kasm browser templates — the E13 engine
gate" and `docs/kasm-images.md`.

## Upgrading to 0.3.0

Chart 0.3.0 removes the deprecated `templates[].nodeSelector` entry field
(the v0.1 knob that rendered the `workspaces.cdi.tinyorbit.vn/node-selector`
annotation — pod placement has been typed `spec.placement` since v0.2). A
values file that still sets it **fails the render**, both via the values
schema (`additionalProperties`) and an explicit check for
`--skip-schema-validation` renders. Move the selector into the template
spec before upgrading:

```yaml
templates:
  - name: <tpl>
    spec:
      placement:               # was: nodeSelector: {workload: runtime}
        nodeSelector:
          workload: runtime
```

WorkspaceTemplate objects already in the cluster keep working — the
annotation is simply ignored now; only chart-rendered values fail.

## Upgrading to 0.2.0

Chart 0.2.0 replaces the `api`, `gateway` and `portal` components with
`backend` (the public API and the session gateway merged into one
binary — `api-internal.<ns>.svc:9443` becomes `backend.<ns>.svc:9443`)
and `frontend` (the static portal SPA server). The schema still accepts
the old `api.*`/`gateway.*`/`portal.*`/`images.{api,gateway,portal}` keys
only so the render can fail with a migration hint instead of a bare
schema error. Mapping:

| 0.1.x | 0.2.0 |
|---|---|
| `api.*` | `backend.*` (`internalTLS` → `tls.internal`, `clientCA`/`extraCA`/`operatorCN`/`sessionIdle`/`extraPortalOrigins` keep their names) |
| `gateway.*` | `backend.*` (`tls` → `tls.session`, `metricsListen` → `metrics.{enabled,port}`, `id` → `gatewayID`; `mtls`/`trustedCA`/`service` removed — the gateway reaches the broker in-process and the session edge is exposed via ingress/gatewayApi) |
| `portal.*` | `frontend.*` (`tls` keeps its name; `apiUpstream`/`service` removed — the edge routes `/v1` to the backend) |
| `images.{api,gateway}` | `images.backend` |
| `images.portal` | `images.frontend` |
| `<c>.podDisruptionBudget` | `backend.pdb` / `frontend.pdb` (`operator.podDisruptionBudget` unchanged) |
| `sessionHost` | `sessionDomain` — every workspace session gets its own `<label>.<sessionDomain>` host; the edge carries ONE wildcard route and needs ONE wildcard certificate for `*.<sessionDomain>` (DNS-01). `portalHost` must not equal or sit inside `sessionDomain` |
| `backend.extraAllowedHosts` | `backend.controlHosts` — the session Host allowlist is the session domain itself plus the in-cluster Service names |

New required value: `backend.loginKeys` — the AEAD key(s) sealing the
`__Host-tcdi_login` cookie that makes OIDC logins survive replica
failover. `backend` defaults to **2 replicas** with `maxUnavailable: 0`,
`minAvailable: 1` PDB and preferred node anti-affinity — size your cluster
accordingly.

Upgrading from 0.1.x additionally requires: a wildcard DNS record and
wildcard certificate for `*.<sessionDomain>` (the session listener
terminates TLS per workspace host), a dedicated workspace node pool
(`runtime.placement.*` — `allowSharedNodes: true` opts out for kind/dev),
and the login-key Secret above. **Every open session drops once** — users
re-launch from the portal after the upgrade — and database migrations
011/012 are forward-only, so rollback means restoring the pre-upgrade
database backup together with the 0.1.x chart. The full procedure,
prerequisite checklist and post-upgrade checks are in
`docs/runbooks/upgrade.md` → "Upgrading from v0.1 to v0.2".

## Uninstall and CRD policy

```bash
helm uninstall tinycdi -n tinycdi-system
```

Removes the platform Deployments/Services/RBAC and release-namespace
objects. It **keeps**:

- CRDs — Helm installs `crds/` once and never upgrades or deletes them.
  Reconcile CRD drift by hand: `kubectl diff -f crds/` then
  `kubectl apply -f crds/` after review. Deleting a CRD deletes every CR.
  Under GitOps (Argo CD), a changed CRD needs a hard refresh before the
  app syncs CRs that use the new fields — see `docs/runbooks/upgrade.md`
  → "GitOps note".
- Managed namespaces and everything inside (Workspaces, runtime pods,
  **user PVCs**) — they carry `helm.sh/resource-policy: keep`.
- Secrets you created manually.

## Parameters

### Global

| Key | Default | Description |
|---|---|---|
| `global.imageRegistry` | `ghcr.io` | registry prepended to every `images.*.repository` (e.g. `registry.example.com`); per-image `registry` wins |
| `global.imagePullSecrets` | `[]` | pull secrets attached to every platform pod (names or `{name: ...}` maps) |
| `images.<name>.repository` | `tinyorbitvn/tinycdi-<name>` | image path (joined under the registry); names: `backend`, `operator`, `frontend`, `linuxBase`, `linuxDesktop`, `browser` (`linuxBase` is the shared base of the two runtime profiles: a release stamps its digest next to the others for custom images built on it, but **the chart never pulls it** and no template references it) |
| `images.<name>.tag` | chart `appVersion` | tag; ignored when `digest` is set |
| `images.<name>.digest` | `""` | `sha256:<64hex>` — digest pinning wins over tag |
| `images.<name>.pullPolicy` | `IfNotPresent` | per-image pull policy |
| `images.<name>.builtAt` | `""` | RFC 3339 build time from the runtime train's `runtime-images.json` → `image-built-at` annotation; an image older than `-image-block-after` (45 d default) refuses create/start with 409 IMAGE_STALE (runtime images only) |
| `images.<name>.engines.{chromium,firefox}` | unset | browser engine versions from `runtime-images.json` → `image-chromium`/`image-firefox` annotations, shown on the template's stale-image view (runtime images only) |

### Topology

| Key | Default | Description |
|---|---|---|
| `portalHost` / `sessionDomain` | `*.example.invalid` | public portal hostname / session domain — sessions run on `<label>.<sessionDomain>` behind the `*.<sessionDomain>` wildcard route; `portalHost` must not equal or sit inside `sessionDomain` (render-time guard) |
| `managedNamespaces[]` | `[]` | `{name, tenant, quota?}` — tenant namespaces created with `resource-policy: keep`; `quota` declares the tenant's limits (see "Tenant quotas"). The operator's manager-role is bound ONLY here and `--watch-namespaces` lists exactly these (never the release namespace, SEC-09); with an empty list the operator watches all namespaces, which its RBAC denies |
| `podSecurity.platformEnforce` / `.managedEnforce` | `baseline` / `restricted` | PSS labels on created namespaces; `managedEnforce=privileged` needs `dev.enabled` (CHTR-2) |

### Credentials (existing Secrets only — never values)

| Key | Secret holds | Used by |
|---|---|---|
| `database.existingSecret` / `.urlKey` | Postgres DSN (`url`) | backend `TCDI_DATABASE_URL` |
| `database.tls.caSecret.name` / `.key` | DB server CA bundle | backend `PGSSLROOTCERT` (with `database.tls.mode`, e.g. `verify-full`, exported as `PGSSLMODE`) |
| `oidc.existingSecret` / `.clientSecretKey` | OIDC client secret | backend `TCDI_OIDC_CLIENT_SECRET` |
| `backend.loginKeys.existingSecret` | `current` (+ `previous` while rotating), 32 bytes each | backend `-login-key-file` — seals the `__Host-tcdi_login` cookie; **required** unless `loginKeys.generate` |
| `backend.tls.{app,session,internal}.existingSecret` | `tls.crt`/`tls.key` | backend app listener :8443 / session listener :8444 / internal mTLS listener :9443 |
| `backend.clientCA.existingSecret` | `ca.crt` | backend verifies internal-listener client certs |
| `backend.extraCA.existingSecret` | `ca.crt` | optional outbound trust (`SSL_CERT_FILE`) |
| `backend.upstreamCA.existingSecret` | `ca.crt` | optional runtime upstream CA |
| `backend.controlToken.existingSecret` | `token` | `/v1/control/*` bearer; unset = routes closed |
| `operator.brokerClient.mtls.existingSecret` | `tls.crt`/`tls.key` | operator broker client cert — **CN must equal `backend.operatorCN` (default `operator`)** |
| `operator.brokerClient.ca.existingSecret` | `ca.crt` | operator verifies the broker cert |
| `frontend.tls.existingSecret` | `tls.crt`/`tls.key` | frontend TLS |
| `ingress.tls.existingSecret` | `tls.crt`/`tls.key` | **required** when `ingress.enabled` — edge TLS is mandatory |

### Internal mTLS via cert-manager

| Key | Default | Description |
|---|---|---|
| `certManager.enabled` | `false` | render `Certificate` resources producing the internal mTLS Secrets |
| `certManager.issuerRef.{kind,name}` | `ClusterIssuer`/`""` | existing signer; required unless `selfSigned` |
| `certManager.selfSigned` | `false` | also bootstrap selfsigned ClusterIssuer → CA Certificate → Issuer |
| `certManager.caSecretName` | `<release>-internal-ca` | bootstrap CA Secret (point `backend.clientCA`/`operator.brokerClient.ca` at it) |
| `certManager.duration` / `.renewBefore` | `2160h` / `360h` | leaf lifetime/renewal |

### Exposure — pick ONE (`ingress.enabled` XOR `gatewayApi.enabled`; both → render fails)

| Key | Default | Description |
|---|---|---|
| `ingress.enabled` / `.className` / `.annotations` | `false`/`""`/`{}` | one Ingress per host; pods terminate TLS — use a pass-through backend annotation (e.g. nginx `backend-protocol: "HTTPS"`) |
| `ingress.portalAnnotations` / `.sessionAnnotations` | `{}` | per-edge annotations |
| `gatewayApi.enabled` / `.parentRefs` / `.annotations` | `false`/`[]`/`{}` | one `HTTPRoute` per host; backends are HTTPS — gateway must re-encrypt/pass through. API-server-defaulted route fields (parentRef `group`/`kind`, rule `matches`, backendRef `group`/`kind`/`weight`) render explicitly so GitOps shows no drift |
| `gatewayApi.portalParentRefs` / `.sessionParentRefs` | `[]` | per-route `parentRefs` overrides: a non-empty list replaces `gatewayApi.parentRefs` for that HTTPRoute — e.g. different Gateway listeners for the portal host (`cdi-https`) vs the wildcard session domain (`wildcard-https`); empty falls back to the shared list |
| `gatewayApi.portalAnnotations` / `.sessionAnnotations` | `{}` | per-route annotations merged over `gatewayApi.annotations` — on a key collision the per-route value wins (e.g. a CDN proxy flag ON for the portal route, OFF for the long-lived-websocket session route) |
| `gatewayApi.portalRouteName` / `.sessionRouteName` | `portal`/`session` | HTTPRoute object names (DNS-1123 subdomain, must differ); the defaults match every earlier release — rename to pair hand-written routes by convention, e.g. a `<name>-redirect` route |
| `backend.service.annotations` / `frontend.service.annotations` | `{}` | the Services are ClusterIP-only — the edge routes `portalHost` `/v1` → `backend:8443`, `/` → `frontend:8443`, and `*.<sessionDomain>` → `backend:8444` |

### Per-component tuning (`backend`, `operator`, `frontend`)

| Key | Default | Description |
|---|---|---|
| `<c>.replicas` | `2` | `operator` runs leader-elected: one active reconciler, one standby (E4) |
| `<c>.resources` | set | requests+limits required by chart tests |
| `<c>.podAnnotations` | `{}` | pod template annotations |
| `<c>.nodeSelector` / `.tolerations` / `.affinity` | `{}`/`[]`/`{}` | platform pod placement; backend ships a preferred `kubernetes.io/hostname` anti-affinity that `backend.affinity` keys merge over |
| `<c>.podSecurityContext` / `.securityContext` | `{}` | merged **over** the hardened defaults (non-root, drop ALL, RO rootfs); keys that would WEAKEN them (privileged, allowPrivilegeEscalation, added caps or a `drop` list missing ALL, root uid/gid/fsGroup/supplementalGroups, writable rootfs, Unconfined seccomp/AppArmor, seLinuxOptions) fail the render unless `dev.enabled` |
| `<c>.pdb.{enabled,minAvailable,maxUnavailable}` | on for all three (`minAvailable: 1`) | PDB for backend/frontend; an explicit `enabled` wins, otherwise a sizing key turns it on. `operator.podDisruptionBudget` keeps the old `enabled`-required shape |
| `<c>.extraArgs` / `.extraEnv` | `[]` | escape hatch — dangerous flags (operator `--dev-allow-no-broker`/`--disable-builtin-egress-excepts`/metrics flags, backend `--dev-insecure-db`/`--required-groups`/`--metrics-listen`/the split-mode broker client flags) and `backend.extraVolumes` hostPath fail the render unless `dev.enabled` |

### Component-specific highlights

| Key | Default | Description |
|---|---|---|
| `backend.sessionIdle` | `30m` | session idle timeout |
| `backend.drainPropagationDelay` | `5s` | wait between the readiness drop and the graceful 1001 stream close on pod drain, so the endpoint removal reaches kube-proxy/the ingress before clients are told to reconnect — render-time budget check: `drainPropagationDelay` + `drainWindow` must be <= 20s (leaves >= 4 s of the 24 s shutdown deadline inside the 30 s grace period) |
| `backend.drainWindow` | `8s` | post-close drain window: the listeners keep serving reads and refuse new launches/upgrades for this long after streams are shed |
| `backend.sessionCookieMode` | `lax` | session cookie mode — `lax` (portal + session domain on one registrable domain) or `partitioned` (cross-site) |
| `backend.gatewayID` | `tinycdi-backend` | ONE gateway identity shared by all replicas — the lease directory is per-identity, so it must be a literal, never a pod name |
| `backend.loginKeys.{existingSecret,generate}` | `""`/`false` | **required** — see Credentials; `generate` mints `<release>-backend-login-keys` once via `lookup` (kept across upgrades; not for GitOps) |
| `backend.extraPortalOrigins` | `[]` | extra CSRF + launch-Origin allowlist entries and session `frame-ancestors` |
| `backend.controlHosts` / `.audience` | `[]` / `""` (=sessionDomain) | extra Hosts allowed for the session listener's in-cluster control surface (`/healthz`, `/v1/control/*`) on top of the `backend[.<ns>[.svc[.cluster.local]]]` Service names / ticket audience |
| `backend.hostAliases` | `[]` | pod `spec.hostAliases` on the backend Deployment — e.g. resolve an in-cluster OIDC issuer host to a Service ClusterIP when cluster DNS cannot; empty renders nothing |
| `backend.trustedProxies` | `[]` | CIDRs of the edge proxies whose X-Forwarded-For claims are trusted — **required behind an ingress/Gateway** or every user shares one rate-limit bucket; see [Rate limits and trusted proxies](#rate-limits-and-trusted-proxies) |
| `backend.metrics.{enabled,port}` | `false`/`9090` | metrics listener on the dedicated ClusterIP `backend-metrics` Service — never the public port (SEC-33); needs `networkPolicy.prometheusPeers` |
| `backend.operatorCN` | `""` (=`operator`) | CN required on the operator broker client cert |
| `operator.leaderElect` / `.webhookPort` | `true` / `-1` | leader election keeps a standby reconciler (E4) |
| `operator.internetExceptCIDRs` | `[]` | subtracted from runtime `InternetOnly` egress |
| `operator.clusterCIDRs` | `[]` | this cluster's pod/service/node CIDRs — appended to `--internet-except-cidrs`; **required** (render fails) when any seeded template uses `networkProfile: InternetOnly` |
| `operator.brokerClient.enabled` | `true` | internal broker wiring (teardown finalizer); `devAllowNoBroker` is dev-only — needs `dev.enabled` |
| `database.tls.mode` | `verify-full` | `PGSSLMODE` on the backend — only `verify-ca`/`verify-full` render without `dev.enabled` (the backend refuses weaker sslmodes at startup, CHTR-8); dev + insecure mode renders `--dev-insecure-db`; DSN `sslmode=` still wins |
| `database.allowedPeers` | deny-all placeholder | backend→DB NetworkPolicy peers — **required**: an empty list OR the shipped `0.0.0.0/32` placeholder fails the render |
| `oidc.requiredGroups` | `[]` | login gate — backend flag `--required-groups=<csv>`; ID-token `groups` must carry one listed group (exact match); empty = every IdP account may log in |
| `oidc.endSession` | `true` | sign-out also ends the identity provider session (RP-initiated logout) when its discovery document has `end_session_endpoint` — backend flag `--oidc-end-session`; `false` keeps sign-out local. See the install runbook, "Sign-out and the identity provider" |
| `oidc.postLogoutRedirect` | `""` | `post_logout_redirect_uri` sent at sign-out (`https` only; must be registered at the provider — Keycloak: the client's *Valid post logout redirect URIs*) — backend flag `--oidc-post-logout-redirect`; empty omits it and the provider shows its own logged-out page |
| `oidc.egressCIDRs` | `[0.0.0.0/0]` | backend→IdP egress CIDRs — **required** non-empty, narrow to your IdP |
| `dev.enabled` | `false` | dev gate: required for `operator.devAllowNoBroker`, dangerous `extraArgs`, a non-verifying `database.tls.mode`, `podSecurity.managedEnforce=privileged`, `backend.extraVolumes` hostPath, and any securityContext override that weakens the hardened defaults |
| `frontend.drainDelay` | `8s` | pre-stop drain: on SIGTERM `/healthz` fails at once and the pod keeps serving this long while the endpoint removal reaches kube-proxy/the edge, so a rollover never routes to a dead pod — render-time budget check: `drainDelay` must be <= 20s (leaves >= 10 s of the 30 s `terminationGracePeriodSeconds` for graceful shutdown) |
| `frontend.branding.configMap` | `""` | optional ConfigMap mounted read-only at `/branding` and served at `/branding/` — see Branding below |

### Branding (`frontend.branding.configMap`)

Set `frontend.branding.configMap` to the name of a ConfigMap in the release
namespace to rebrand the portal. The chart mounts it read-only at
`/branding` in the frontend pods and passes `-branding-dir=/branding`; the
frontend serves it at `https://<portalHost>/branding/` (regular files only,
no listing, `Cache-Control: no-cache`). Empty (the default) renders no flag
and no volume, and `/branding/tokens.css` answers an empty stylesheet and
`/branding/branding.json` the empty object `{}` (the portal keeps its default
branding), so neither request is ever a console error — also when the
ConfigMap is set but lacks that key.

ConfigMap layout — every key is optional:

| Key | Consumed by | Content |
|---|---|---|
| `branding.json` | `web/src/app/branding.ts` (`loadBranding`) | `{"productName": "...", "logo": "/branding/<file>", "logoDark": "/branding/<file>"}` — unknown fields ignored; `logo*` must stay under `/branding/` (same origin) |
| `tokens.css` | `index.html` stylesheet link, after the app CSS | CSS custom-property overrides (`--to-*`/`--tc-*` design tokens) |
| logo files | referenced from `branding.json` | e.g. `logo.svg` — served with their extension's content type |
| `favicon.svg`, `favicon.ico` | `index.html` `<link rel="icon">` | replaces the shipped TinyOrbit favicon of the same name at `/favicon.svg` / `/favicon.ico` (`no-cache`); either may be given alone. `favicon.ico` is binary, so put it under the ConfigMap's `binaryData` |

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: acme-branding
data:
  branding.json: |
    {"productName": "Acme Desktops", "logo": "/branding/logo.svg"}
  tokens.css: |
    :root { --to-accent: #123456; }
  logo.svg: |
    <svg xmlns="http://www.w3.org/2000/svg">…</svg>
```

Update the ConfigMap and restart the frontend Deployment to pick up changes
(`kubectl rollout restart deploy/frontend`). Files are re-read per request,
so no image rebuild is needed.

**Trademark note.** The TinyOrbit name, wordmark and mark shipped as the
default branding are trademarks of TinyOrbit and are NOT covered by the MIT
licence — see `TRADEMARKS.md`. Supplying your own `branding.json` replaces
the default product name and marks; "TinyCDI by TinyOrbit" attribution is
shown only for the default branding.

The full branding guide — field-level fallbacks, favicon override and
token reference — is `docs/branding.md`.

### Runtime pod defaults (`runtime`)

Cluster-wide defaults for workspace (runtime) pods; a template's typed `spec.placement` / `spec.linux.hostUsers` overrides them per field.

| Key | Default | Description |
|---|---|---|
| `runtime.placement.allowSharedNodes` | `false` | `false` = dedicated workspace pool: the operator gets `--runtime-node-selector`/`--runtime-tolerations` and the node-profile installer DaemonSet targets the same pool. `true` opts out (kind/dev only): no placement flags, runtime pods schedule anywhere, and install NOTES warn — node-level isolation is lost |
| `runtime.placement.nodeSelector` | `{cdi.tinyorbit.vn/workspace: "true"}` | node labels every runtime pod selects; **must be non-empty** while `allowSharedNodes=false` (render fails otherwise) |
| `runtime.placement.tolerations` | the `cdi.tinyorbit.vn/workspace` `NoSchedule` toleration | tolerations every runtime pod carries — keep matching the pool taint |
| `runtime.hostUsers` | `false` | `pod.spec.hostUsers` default for runtime pods (`--runtime-host-users`): `false` gives each pod its own user namespace (verified on the reference environment, see `docs/compatibility.md`); `null` leaves the field unset (apiserver default — host user namespace) |
| `runtime.appArmor.requireRuntimeDefault` | `true` | `true` sets an explicit `securityContext.appArmorProfile: RuntimeDefault` on runtime containers (the operator flag `--runtime-apparmor-require-default` is not rendered — it defaults to `true`). `false` (renders `--runtime-apparmor-require-default=false`, prints an install NOTES line) omits it for **nodes without AppArmor** — kind, RHEL-family/SELinux-based distributions — where the kubelet otherwise refuses the pod (`Cannot enforce AppArmor: AppArmor is not enabled on the host`). See [Nodes without AppArmor](#nodes-without-apparmor) |
| `runtime.topologySpread.enabled` | `true` | `true` adds a **soft** `topologySpreadConstraint` to every runtime pod (`--runtime-topology-spread`, not rendered at the default): maxSkew 1 over `kubernetes.io/hostname`, `whenUnsatisfiable: ScheduleAnyway`, selecting the namespace's runtime pods — it prefers an even spread of a tenant's workspace pods across pool nodes (the soak found the scheduler packing them onto a subset — see `docs/runbooks/capacity.md`). Soft means it never blocks scheduling: retained-PVC reattach on a node-pinned volume and single-node pools still work. `false` renders `--runtime-topology-spread=false` and keeps pre-v0.3.1 packed-by-scoring placement |

#### Nodes without AppArmor

Set `runtime.appArmor.requireRuntimeDefault=false` when the workspace pool runs on nodes that cannot enforce AppArmor (kind; RHEL-family and other SELinux-based distributions). This is a supported setting, not a dev escape hatch. The `apparmor` check in `hack/preflight/preflight.sh` (see [preflight README](../../../hack/preflight/README.md)) probes every node workspaces can reach and names the fitting value — run it before install.

- **What changes:** runtime containers (and the Kasm adapter init container) no longer carry `appArmorProfile: RuntimeDefault`. Nothing else changes — seccomp `RuntimeDefault`, dropped capabilities, `runAsNonRoot`/uid 1000, `allowPrivilegeEscalation=false`, the read-only root filesystem and `hostUsers` stay as configured.
- **What is lost:** the fail-closed AppArmor guarantee. On an AppArmor host the container runtime's default profile still applies to non-privileged containers even without the field, so little is lost there; on a host without AppArmor there is no AppArmor confinement at all and isolation rests on seccomp, dropped capabilities, the user namespace and SELinux.
- **Localhost profiles still apply:** a template that requests a Localhost AppArmor profile (`appArmorProfile: <name>`, e.g. the browser templates) always sets it, so those pods are refused on such nodes. Run browser templates only on AppArmor nodes (use `nodeProfiles.install` there), or use a template without a Localhost profile.

### Observability & network

| Key | Default | Description |
|---|---|---|
| `serviceMonitor.enabled` | `false` | ServiceMonitor for the backend metrics endpoint (`backend-metrics` Service). The operator exposes no metrics endpoint: secure metrics need cluster-scoped TokenReview/SAR RBAC the chart never grants, and HTTP metrics are banned — `--metrics-bind-address=0` is pinned |
| `serviceMonitor.labels` / `.interval` / `.scrapeTimeout` / `.honorLabels` | `{}`/`""`/`""`/`false` | Prometheus Operator selection + timing |
| `dashboards.enabled` | `false` | Grafana dashboard ConfigMaps from `files/dashboards/*.json` for the Grafana sidecar — requires `backend.metrics.enabled` |
| `dashboards.labels` | `{grafana_dashboard: "1"}` | sidecar discovery labels on the ConfigMaps (kube-prometheus-stack default; set your Grafana's label/value when it differs) |
| `alerts.enabled` | `false` | `PrometheusRule` with the platform alert set below — requires `backend.metrics.enabled` and the Prometheus Operator CRDs |
| `alerts.labels` | `{}` | labels on the `PrometheusRule` — the operator's ruleSelector (e.g. `{release: prometheus}`) |
| `alerts.sessionDropFloor` | `5` | minimum live sessions 5m back required for `TinyCDISessionDropSpike` to fire — a small install draining at night (2→0) stays quiet; `0` disables the floor |
| `networkPolicy.enabled` | `true` | default-deny baseline + allow rules (incl. the operator↔backend :9443 broker rule) |
| `networkPolicy.apiServerPeers` / `.apiServerPort` | `10.96.0.1/32` / `443` | apiserver egress — set your `kubernetes.default` ClusterIP |
| `networkPolicy.dnsPeers` | kube-system pods | DNS egress |
| `networkPolicy.prometheusPeers` | `[]` | metrics-scrape ingress peers — **required** (render fails) when `backend.metrics.enabled` is set; scope to your monitoring namespace/pods |
| `networkPolicy.edgeIngress` / `.edgeIngressCIDRs` | `ipBlock` / `[0.0.0.0/0]` | how edge traffic reaches the public listeners (backend :8443/:8444, frontend :8443) — `ipBlock` needs non-empty CIDRs (empty fails closed), `any` admits every source on the TLS ports, `cilium` renders `*-edge-ingress` CiliumNetworkPolicies instead |

#### Dashboards and alerts

Both are opt-in and require `backend.metrics.enabled` (the render fails
otherwise — every expression reads the `tinycdi_*` series only the
backend metrics listener serves).

`dashboards.enabled` renders one ConfigMap per file in
`files/dashboards/` (`tinycdi-overview`, `tinycdi-capacity`) carrying the
`grafana_dashboard: "1"` label — the kube-prometheus-stack Grafana
sidecar picks them up automatically; adjust `dashboards.labels` for
other sidecar configurations.

`alerts.enabled` renders one `PrometheusRule` named `tinycdi`:

| Alert | Fires when |
|---|---|
| `TinyCDIBackendReplicaDown` | fewer backend replicas report metrics than `backend.replicas`, for 15m |
| `TinyCDISessionDropSpike` | >50% of live sessions vanish inside 5m while ≥`alerts.sessionDropFloor` were live |
| `TinyCDILeaseRenewFailures` | lease acquire/renew failures sustain ≈ >1 per 50 s for 15m |
| `TinyCDIRehydrationFailures` | >20% of session rehydrations return miss/error for 15m |
| `TinyCDIRuntimeImageStale` | a catalog family's newest revision is older than `-image-stale-after` (14 d default) for 1h |
| `TinyCDILoginFailuresHigh` | >50% of completed logins are denied/error for 15m |

The image-age threshold mirrors the `-image-stale-after` default; when
the flag is overridden via `backend.extraArgs`, edit the rendered rule
(or keep the default — the alert stays advisory). Set `alerts.labels` to
match your Prometheus Operator's `ruleSelector` (e.g.
`{release: prometheus}`), the same convention as `serviceMonitor.labels`.

### Host-network gateways (Cilium Gateway API / cilium-envoy)

**Symptom.** On Cilium, with a Gateway API or Ingress edge whose envoy runs in the
host network namespace (Cilium's own Gateway API and Ingress controllers do), every
request through the edge returns `503` while the pods are Ready and
`kubectl port-forward` works. This was GitHub issue #13.

**Cause.** The default edge rule (`edgeIngress: ipBlock`, `edgeIngressCIDRs:
[0.0.0.0/0]`) is a plain `NetworkPolicy` `ipBlock` peer. Under Cilium's default
`policy-cidr-match-mode` (empty), CIDR selectors only match traffic from *outside*
the cluster. Traffic from a host-network envoy does not carry a world identity: it
carries the `ingress`, `host` or `remote-node` identity of the node it came from, so
no `ipBlock` — not even `0.0.0.0/0` — ever matches it, and the backend/frontend
default-deny drops the connection. Widening the CIDRs cannot fix it; select the
identities instead (below).

**Fix.** Set `networkPolicy.edgeIngress: cilium`. The chart then renders no edge peers
in the plain `NetworkPolicy` objects and instead renders two
`CiliumNetworkPolicy` objects in the release namespace:

| Policy | Selects | Admits |
|---|---|---|
| `backend-edge-ingress` | `app.kubernetes.io/name: backend` | `fromEntities: [ingress, host, remote-node]` on TCP `8443` (app) and `8444` (session) |
| `frontend-edge-ingress` | `app.kubernetes.io/name: frontend` | the same entities on TCP `8443` |

Neither policy ever admits the internal mTLS listener (`9443`, operator only) or the
metrics port (`9090`, `networkPolicy.prometheusPeers` only); the chart tests assert it
for every edge mode. The in-cluster control surface on `8444` (release namespace) and
the operator's `9443` rule are plain `NetworkPolicy` rules and are unchanged.

```yaml
# values.yaml — Cilium CNI with a Cilium Gateway API / Ingress edge
networkPolicy:
  edgeIngress: cilium     # edgeIngressCIDRs is ignored in this mode
gatewayApi:
  enabled: true
```

`edgeIngress: cilium` needs the `cilium.io/v2` `CiliumNetworkPolicy` CRD, which any
cluster running Cilium provides — Helm does not check for it, so applying the release
on a cluster without it fails at install time; do not select it on other CNIs. Edges
that are not host-network (an external load balancer that preserves the client source
address, for example) keep working with `ipBlock` — narrow `edgeIngressCIDRs` to the
load-balancer range. There is no separate "from host network" switch: `edgeIngress` is
the single option that decides how the edge reaches the public listeners.

### Rate limits and trusted proxies

The backend rate-limits its unauthenticated surface **per client address**:
`-login-rate` (30/min, burst 10) covers `GET /v1/login`,
`GET /v1/auth/callback` and `GET /v1/session`; `-launch-rate` (60/min, burst 20)
covers the session listener's `POST /v1/launch`. A client over its budget gets
`429 RATE_LIMITED` with `Retry-After`; `0` disables a limit
(`backend.extraArgs`, e.g. `-login-rate=0`).

Requests that prove a live session are **not** keyed on the address:
`GET /v1/session` runs on a per-session budget (a digest of the session,
never the raw value), `POST /v1/launch` likewise once the session is live
on the serving replica (a cookie only a sibling replica has seen keys by
IP — the limiter never spends a directory lookup), and
`GET /v1/auth/callback` keys on its validated OIDC state — additionally
gated by a per-IP ceiling at 10× the login limits, so minted states
cannot amplify callback throughput past that bound. `/v1/login` stays
per-IP always: it is the anonymous sign-in start. So an office of 20+
users behind one NAT keeps per-user budgets — but the *anonymous* starts
still share the IP budget: in a 9:00-style rush where N users behind one
NAT all sign in inside a minute, size `-login-rate` ≥ N (plus headroom
for signed-out probe polls); first-ever launches likewise count against
`-launch-rate` until the cookie exists. Forged, expired or unverifiable
cookies and states always fall back to the client-IP key, so they cannot
mint fresh buckets. See `docs/runbooks/capacity.md` ("Sign-in rate limits
and NAT").

The bound is enforced by a **Postgres fixed-minute window** shared by
every replica (ADR 0006): one counter per key, checked with a single
upsert per request — the flags are now the *exact* aggregate: 2 replicas
with `-login-rate` 30/min + burst 10 let one anonymous IP draw exactly
40 requests inside a window, however the traffic spreads. The
fixed-window edge admits up to ~2×(rate+burst) inside a span crossing a
minute boundary — the same overshoot class the old per-replica buckets
had. Split mode (`backend.brokerURL`, no database) keeps the divided
in-memory limiter.

Every pod additionally keeps its divided in-memory bucket — the chart
passes `-rate-limit-replicas=backend.replicas` — which now plays two
roles: a **per-replica ceiling** while Postgres is healthy (effective
bound = `min(shared window, pod share)`, so one pod can never serve more
than its `max(1, rate÷N)`/min + `max(1, burst÷N)` burst, and a locally
refused key never reaches the store) and the **fail-open fallback**
during a Postgres outage — every limited route needs Postgres to
complete anyway, so an outage degrades to per-replica limiting rather
than a lifted cap or a hard 429. Store errors count on
`tinycdi_rate_limit_store_errors_total{route}` — real failures only:
each check runs under a 500 ms deadline and a failure opens a 10 s
circuit breaker that skips the store until one probe succeeds, so a
brown-out stalls at most one request per cool-down — with the breaker
state on `tinycdi_rate_limit_store_degraded{route}` and each limiter
logging a single line on fallback entry and exit (never per request).
Expired window rows are deleted by the leader replica's periodic sweep
(15-minute retention).

Three edge cases to know:

- **Sticky load-balancing still sees the pod share.** A key pinned to
  one pod (cookie/IP-hash affinity) is bounded by that pod's 1/N ceiling
  even while the shared window has room — the ceiling is deliberate (it
  bounds what one pod may serve) and only loosens if you raise
  `-rate-limit-replicas` **downward** or spread traffic.
- **A configured rate below the replica count still clamps up.** During
  a Postgres outage (`-login-rate=2` with `backend.replicas: 3`) each
  pod's floor is 1/min — ~N/min aggregate, above the flag, never
  silently disabled by rounding to 0.
- **An external HPA should pin the divisor to its ceiling.** The chart
  does not ship an HPA for the backend; if you add one, set
  `-rate-limit-replicas=<maxReplicas>` via `backend.extraArgs` (it
  renders after the chart's value and wins) so the outage/degraded-mode
  bound holds at full scale. With Postgres healthy the shared window
  enforces the aggregate at every replica count regardless.

When reading `tinycdi_rate_limited_total` during an outage remember the
replica count — each pod's local ceiling is 1/N of the aggregate; while
Postgres is healthy the series measures the shared bound.

The client address is the socket peer — unless the peer is inside
`backend.trustedProxies`, in which case the right-most untrusted
`X-Forwarded-For` entry stands in. **Behind any ingress or Gateway the value
is required, not optional**: with it empty every user arriving through the
same edge keys on the edge's own address — one shared bucket (~30 logins and
~60 launches per minute for the whole organisation) and a self-inflicted
outage. The backend logs a startup warning while a limit is on and the list
is empty. The same list feeds the `X-Forwarded-For` / `Forwarded` /
`X-Real-IP` headers the workspace pod sees — client-supplied values are
stripped and rebuilt from the trusted chain only (S18), so a spoofed address
can never poison the runtime's brute-force blacklist.

```yaml
# Cilium Gateway API / Ingress — the edge envoy runs host-network, so the
# peer the backend sees is the NODE the request lands on. List the node
# subnet(s) (networkPolicy.edgeIngress stays "cilium" — see above).
backend:
  trustedProxies: ["10.10.0.0/24"]      # node CIDR(s) running the gateway envoy
gatewayApi:
  enabled: true
```

```yaml
# Traefik (or any ingress as pods) — the peer is the proxy pod's address;
# list the cluster pod CIDR or a tighter range covering the proxy pods.
backend:
  trustedProxies: ["10.42.0.0/16"]      # pod CIDR containing the traefik pods
ingress:
  enabled: true
```

Exposing the listeners directly (no L7 proxy) needs nothing: the socket peer
already is the client.

### Runtime catalog (`templates[]`)

Seeded `WorkspaceTemplate` objects, published as immutable `<name>-<hash8>`
revisions. Per entry:

| Field | Description |
|---|---|
| `name` / `namespace` | catalog name; must be a managed namespace |
| `image` | key into `images` (`linuxDesktop`, `browser`) or literal ref; used when `spec.linux.image` is empty — runtime images must be **digest-pinned** |
| `seccompProfile` / `appArmorProfile` | Localhost node profile names → `localhost/<name>` annotations (must be pre-loaded on nodes) |
| `storageClass` / `annotations` / `spec` | per-template SC override, verbatim annotations, verbatim spec — pod placement lives in `spec.placement` (`nodeSelector`, `tolerations`, `runtimeClassName`); the v0.1 `nodeSelector` entry field was removed in v0.3 and fails the render |

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
| `kasmAdapter.enabled` | `false` | opt-in switch. The operator gets `--kasm-adapter-image` **only** when this is `true` — a release-stamped `kasmAdapter.image.digest` alone does not turn the adapter on. `enabled: true` without a digest fails the render, and so does a seeded `adapter: kasm` template while `enabled` is `false` (the backend would reject the workspaces anyway) |
| `kasmAdapter.image.{repository,tag,digest}` | `tinyorbitvn/tinycdi-kasm-adapter` | adapter init image → operator `--kasm-adapter-image` (when enabled). **Digest is required**: the flag only carries a digest-pinned ref, and a `tag` without `digest` fails the render. Release packaging stamps the digest; set it yourself for a self-built or mirrored image |
| `kasmAdapter.browserAllowlist` | `[]` | the E13 browser gate: a seeded `adapter: kasm` template with `experience: Browser` renders only when its `spec.linux.image` is listed here; every entry must be a digest-pinned **browser-class** `build/kasm-catalog.txt` entry (≤2-major engine gate — CI enforces it). Non-Browser kasm templates ignore the list (≤4 budget). Empty today — no cataloged kasmweb browser image meets the gate |

A seeded kasm template sets `spec.linux.adapter: kasm`, an optional
`spec.linux.sessionCmd` (the session payload — for Chromium-family images
it MUST point past the image's `--no-sandbox` wrapper at the real
binary), and the runtime `image:` as a literal digest-pinned
`kasmweb/<app>@sha256:…` reference (it is pulled by the nodes straight
from Docker Hub — never mirrored/republished). The digest must be a
`build/kasm-catalog.txt` entry — CI enforces the catalog (digest pin,
trivy gate, browser-engine freshness floor). `ci/example-values.yaml`
ships a complete `kasmweb/chromium` example (a **Desktop**-experience
seed — the image is desktop-class under E13).

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

### Tenant quotas

Each `managedNamespaces[]` entry may carry a `quota` block. The chart renders
the entries that have one as the backend flag `-tenant-quotas` (JSON), and the
singleton backend leader upserts exactly those tenants' `tenant_quota` rows at
startup:

```yaml
managedNamespaces:
  - name: tinycdi-tenant-a
    tenant: tenant-a
    quota:
      runningWorkspaces: 12   # whole number >= 0
      cpu: "16"               # cores or millicores: "16", "1.5", "500m"
      memory: 64Gi            # Kubernetes quantity
      storage: 200Gi          # Kubernetes quantity
```

All four fields are required inside a `quota` block. The upsert is
idempotent (an identical row is not touched), values below current usage are
accepted (running workspaces keep running; new creates are refused with
`QUOTA_EXHAUSTED`), and a tenant without a `quota` block is left untouched.
A tenant with **no** row at all gets `409 QUOTA_NOT_CONFIGURED` on every
create, so NOTES warns about each entry that has no `quota` block.
`values.schema.json` rejects negative or unparsable quantities. Declared
values overwrite a hand-edited row for that tenant at the next backend
start. See `docs/runbooks/install.md` → "Tenant quotas" for the install
flow and `docs/runbooks/tenant-quotas.md` for day-2 operations (reading
usage, refusal codes, changing limits).

## Validation & tests

`values.schema.json` rejects malformed values at lint/template time
(https-only issuers/origins, hostname-patterned `portalHost`/`sessionDomain`,
comma-free `oidc.requiredGroups` entries, pinned installer image).
Render-time guards refuse the removed `api.*`/`gateway.*`/`portal.*`/
`sessionHost`/`backend.extraAllowedHosts` values
(with a migration hint), a portalHost equal to or inside
sessionDomain, conflicting exposure modes, a cert-manager toggle without an
issuer, missing `backend.loginKeys`, empty or placeholder-only
`database.allowedPeers`, empty
`oidc.egressCIDRs`/`networkPolicy.prometheusPeers`/`edgeIngressCIDRs`,
ingress without TLS,
HTTPRoute parentRefs without `sectionName`, InternetOnly templates without
`operator.clusterCIDRs`, an installer namespace equal to the release
namespace/a managed namespace/`kube-*`/`default`, `database.tls.mode`
`disable`/`allow`, `podSecurity.managedEnforce=privileged`, hostPath
`backend.extraVolumes`, dangerous `extraArgs`, and privilege-weakening
securityContext overrides — the dev surfaces all unless `dev.enabled`. Run
`go test ./deploy/helm/` for the full assertion suite (helm resolved via
`$TCDI_HELM`, `bin/helm`, or `PATH`); the installer behaviour is
additionally proven inside the pinned alpine image by
`deploy/helm/proof/node-profiles/run.sh` (docker-gated test).
