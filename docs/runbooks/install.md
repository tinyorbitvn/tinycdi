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

- `backend` (public API + session gateway + in-process broker — one
  binary, listeners :8443 app / :8444 session / :9443 internal mTLS),
  `frontend` (static portal SPA server) and `operator` Deployments +
  Services in the release namespace (recommend `tinycdi-system`).
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
  every managed namespace; DNS, apiserver, backend/frontend allow rules
  (including the operator→backend :9443 broker rule), plus a
  backend-pod-scoped metrics-scrape rule when `backend.metrics.enabled`
  is set — that also
  requires `networkPolicy.prometheusPeers` (empty peer lists fail the
  render; `database.allowedPeers` and `oidc.egressCIDRs` are likewise
  mandatory when `networkPolicy.enabled`, and `allowedPeers` must not be
  the shipped `0.0.0.0/32` deny-all placeholder).
- Optional exposure: `ingress.enabled` (portal Ingress + ONE wildcard
  session Ingress for `*.<sessionDomain>`) **or** `gatewayApi.enabled`
  (portal HTTPRoute + wildcard session HTTPRoute) — mutually exclusive,
  enforced at render.
- Optional cert-manager Certificates for the internal mTLS chain
  (`certManager.enabled`).
- Optional seeded `WorkspaceTemplate` CRs (digest-pinned images; per
  -template Localhost seccomp/AppArmor profile and StorageClass via
  structured fields or annotations; pod placement via `spec.placement`).
- `tinycdi-runtime` ServiceAccount per managed namespace with
  `automountServiceAccountToken: false`.

The chart **never** installs KubeVirt, CDI, a PostgreSQL database, a dev
OIDC provider, or any other bundled dependency, and **never** carries
secret values — every Secret is a pre-existing object referenced by name
(or produced by cert-manager when `certManager.enabled`).

## Prerequisites check

Run the preflight script first. It checks the Kubernetes version,
NetworkPolicy enforcement (with a throwaway probe namespace it always
deletes), a StorageClass, user-namespace support, the workspace node pool,
AppArmor on the workspace nodes (it names the fitting
`runtime.appArmor.requireRuntimeDefault` value), wildcard DNS, the session
TLS Secret, OIDC discovery and Postgres with TLS verification, and prints
`PASS`/`WARN`/`FAIL` with a one-line fix for each
(exit code 1 on any `FAIL`). It works without cluster-admin: a check it is
not permitted to run is a `WARN`. Flags and the check table are in
`hack/preflight/README.md`.

```bash
hack/preflight/preflight.sh \
  --session-domain session.example.com \
  --tls-secret tinycdi-system/tinycdi-backend-session-tls \
  --oidc-issuer https://idp.example.com/realms/tinycdi \
  --postgres-dsn-secret tinycdi-system/tinycdi-backend-db:url \
  --host-users-false          # only if runtime.hostUsers=false is wanted
# on a throwaway cluster add:  --allow-shared-nodes
```

Checks preflight does not cover (tools and cluster-wide rights):

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

1. **A portal host and a session domain** — `portal.<dom>` plus a
   `session.<dom>` *domain*: every workspace session is served on its own
   `<label>.<sessionDomain>` host, so you need a **wildcard DNS record
   and a wildcard certificate for `*.<sessionDomain>`** (HTTP-01 cannot
   issue wildcards; use DNS-01). `portalHost` must differ from
   `sessionDomain` and must not sit inside it — the `*.<sessionDomain>`
   wildcard route would capture portal traffic (render-time guard).
   Two layouts:
   - **same-site (default, `lax`)**: portal and session domain under one
     registrable domain, e.g. `portal.example.com` +
     `sessionDomain: session.example.com`. Session cookies are
     `SameSite=Lax`; `backend.sessionCookieMode: lax`.
   - **cross-site (`partitioned`)**: portal and session domain on
     different registrable domains, e.g. `portal.example.com` +
     `sessionDomain: vdi.example.net`. Session cookies are
     `SameSite=None; Secure; Partitioned` (CHIPS); set
     `backend.sessionCookieMode: partitioned`.
2. **OIDC client** at your IdP: redirect URL
   `https://<portalHost>/v1/auth/callback`.
3. **PostgreSQL** reachable from the cluster; a DSN for a role with DDL
   rights on its schema (the backend runs embedded migrations at startup).
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
5. **Images** — default `ghcr.io/tinyorbitvn/tinycdi-{backend,operator,
   frontend,linux-desktop,browser}` (plus `linux-base`, the shared base the two
   runtime profiles build from — pinned in the values, never pulled by the
   chart; cosign-verify it with the rest of the release, and carry
   `images.linuxBase.digest` into any GitOps wrapper that pins digests) tagged with the chart
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

   **Nodes without AppArmor.** Runtime pods carry an explicit
   `appArmorProfile: RuntimeDefault`, which a node that cannot enforce
   AppArmor (kind; RHEL-family and other SELinux-based distributions)
   refuses with `Cannot enforce AppArmor: AppArmor is not enabled on the
   host`. The preflight `apparmor` check probes every node workspaces can
   reach and names the fitting value. On such a pool set
   `runtime.appArmor.requireRuntimeDefault:
   false` (operator flag `--runtime-apparmor-require-default=false`; the
   install NOTES print a reminder). This is a supported setting. It omits
   only the RuntimeDefault AppArmor field — seccomp, dropped capabilities,
   non-root, no privilege escalation and `hostUsers` are unchanged. What
   you give up is the fail-closed guarantee: on an AppArmor host the
   runtime's default profile still applies to non-privileged containers,
   whereas on a host without AppArmor there is no AppArmor confinement and
   isolation rests on seccomp, the user namespace and SELinux. Templates
   that select a Localhost AppArmor profile (the browser templates) always
   keep it and therefore cannot run on such nodes.

8. **TLS material** for the portal and session edges plus the internal
   mTLS chain — either pre-created Secrets (table below) or
   `certManager.enabled` for the internal chain. The session edge cert
   (`backend.tls.session`, and the ingress TLS Secret when
   `ingress.enabled`) MUST be a wildcard covering `*.<sessionDomain>` —
   cert-manager can only issue it through a DNS-01 solver. If you use
   Gateway API exposure, the gateway listener terminates browser TLS and
   must re-encrypt/pass through to the HTTPS backends.
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
| `tinycdi-backend-db` | `url` = Postgres DSN | backend (`TCDI_DATABASE_URL`) |
| `tinycdi-oidc-client` | `client-secret` | backend (`TCDI_OIDC_CLIENT_SECRET`) |
| `tinycdi-backend-app-tls` | `tls.crt`, `tls.key` | backend app listener :8443 — certificate for `portalHost` |
| `tinycdi-backend-session-tls` | `tls.crt`, `tls.key` | backend session listener :8444 — **wildcard certificate covering `*.<sessionDomain>`** (DNS-01) |
| `tinycdi-backend-internal-tls` | `tls.crt`, `tls.key` | backend internal mTLS listener :9443 (or `certManager.enabled`) |
| `tinycdi-backend-login-keys` | `current` (+ `previous` while rotating), 32 bytes each | backend `-login-key-file` — seals the `__Host-tcdi_login` cookie (or `backend.loginKeys.generate`) |
| `tinycdi-internal-ca` | `ca.crt` | internal mTLS CA: backend verifies client certs (`backend.clientCA`), operator verifies the broker cert (`operator.brokerClient.ca`). With `certManager.selfSigned` the CA Secret is `<release>-internal-ca` |
| `tinycdi-operator-mtls` | `tls.crt`, `tls.key` | operator client cert to the broker — **CN must be `operator`** (or `backend.operatorCN`): the only identity the internal workspace revoke/drain routes accept (ADR 0003). `certManager.enabled` issues it with the right CN |
| `tinycdi-frontend-tls` | `tls.crt`, `tls.key` | frontend TLS |
| `tinycdi-backend-control-token` | `token` | backend session control endpoints — **optional**; set `backend.controlToken.existingSecret` (key `token`). Without it the `/v1/control/*` routes stay closed (fail-closed); they answer only on the in-cluster control hosts |
| `tinycdi-extra-ca` | `ca.crt` | optional: backend outbound CA (`backend.extraCA`), runtime upstream CA (`backend.upstreamCA`), DB CA (`database.tls.caSecret`) |
| `tinycdi-ingress-tls` | `tls.crt`, `tls.key` | optional: ingress TLS when `ingress.enabled` — must cover `portalHost` **and** `*.<sessionDomain>` |

Create them with `kubectl create secret tls/generic` from local files
kept outside version control — never put values in the values file or
commit them.

## Install

```bash
cp deploy/helm/tinycdi/ci/example-values.yaml my-values.yaml
# edit: portalHost/sessionDomain, managedNamespaces (with a quota each — see
# "Tenant quotas"), oidc, database,
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

## Tenant quotas

A tenant with **no quota** cannot create anything: admission fails closed,
and the API answers `409 QUOTA_NOT_CONFIGURED` ("No quota is configured for
your tenant. Ask an administrator to set one."). Declare each tenant's
limits in the chart values — no SQL needed:

```yaml
managedNamespaces:
  - name: tinycdi-tenant-a
    tenant: tenant-a
    quota:
      runningWorkspaces: 12   # whole number >= 0
      cpu: "16"               # cores or millicores: "16", "1.5", "500m"
      memory: 64Gi            # Kubernetes quantity
      storage: 200Gi          # Kubernetes quantity (retained + active disks)
```

The chart renders the entries that carry a `quota` block as the backend
flag `-tenant-quotas` (JSON). At startup the singleton leader (the replica
holding the Postgres leader lock) upserts exactly those tenants' `tenant_quota`
rows, so replicas starting together write once.

- **Idempotent.** A row that already matches is not touched; a changed value
  is updated in place on the next backend start (`helm upgrade` rolls the
  backend).
- **Listed tenants only.** A tenant without a `quota` block, or not in
  `managedNamespaces`, is left exactly as it is — an existing row (for
  example one set by SQL, see `capacity.md`) survives. `helm install` /
  `upgrade` NOTES print a warning for every tenant without a `quota` block
  because its creates are refused unless a row already exists.
- **Declared values win.** For a tenant that has a `quota` block, the chart
  value overwrites a hand-edited row at the next backend start. Change the
  value in `values.yaml`, not in the database.
- **Lowering is safe.** A limit below current usage is accepted: running
  workspaces keep running and keep their reservations, and new creates are
  refused with `QUOTA_EXHAUSTED` until usage drops under the limit.
- **Validation.** `values.schema.json` rejects negative or unparsable
  quantities at lint/template time, and the backend refuses to start on a
  malformed `-tenant-quotas`, naming the flag.
- **Sizing.** Keep the sum of the quotas inside what the cluster (and any
  namespace `ResourceQuota`) can actually schedule — see `capacity.md`.

Check the result — only the leader replica logs the pass:

```bash
$K -n tinycdi-system logs -l app.kubernetes.io/name=backend --tail=-1 | grep "tenant quotas applied"
```

Day-2 quota operations (reading usage, `QUOTA_EXHAUSTED` vs
`release_pending`, changing limits): `tenant-quotas.md`.

## Rate limits and trusted proxies

`GET /v1/login`, `GET /v1/auth/callback` and `GET /v1/session` are limited
to 60 requests/min per client address (burst 20); `POST /v1/launch` to
120/min (burst 40). Over the limit the API answers `429 RATE_LIMITED` with
`Retry-After`.

The client address is the socket peer unless the peer is inside
`backend.trustedProxies` — then the right-most untrusted `X-Forwarded-For`
entry stands in. **Behind any ingress or Gateway, set the value to the
CIDR(s) your edge sources from.** With it empty every user shares the
edge's own bucket (~60 logins/min for the whole organisation) and the
backend logs a startup warning while a limit is on. The same list feeds
the forwarded headers toward workspace pods — client-supplied values are
stripped and rebuilt from the trusted chain only.

```yaml
# Cilium Gateway API / Ingress — edge envoy runs host-network; the peer
# is the node the request lands on. List the node subnet(s); keep
# networkPolicy.edgeIngress: cilium (see the chart README).
backend:
  trustedProxies: ["10.10.0.0/24"]
```

```yaml
# Traefik or another ingress running as pods — the peer is the proxy pod
# IP; list the cluster pod CIDR (shown: 10.42.0.0/16) or a tighter range.
backend:
  trustedProxies: ["10.42.0.0/16"]
```

No edge proxy at all needs nothing — the peer already is the client.
Rates are tunable via `backend.extraArgs` (`-login-rate`, `-launch-rate`;
`0` disables a limit).

## Sign-out and the identity provider

The account menu (top right, on the user's name) has **Sign out**. It ends
the portal session (`POST /v1/logout`, CSRF-protected, session cookie
expired) and leaves for a public **Signed out** page that offers
**Sign in again** and never starts a login by itself.

**Sign out everywhere** sits next to it (ADR 0007). After a confirmation
it calls `POST /v1/me/sessions:revoke-all`, which ends **every** portal
session the user holds in the current tenant — on all browsers and
devices, including this one — in one store transaction that also revokes
the sessions' live desktop streams (they close within seconds) and any
unredeemed launch links. Sessions in other tenants are not affected. The
answer carries the same `endSessionUrl` as a plain sign-out, so the
provider session ends identically; there is no IdP back-channel logout. A
store failure answers `500` and revokes nothing — the call is idempotent
and safe to retry.

Ending only the portal session is not enough: the identity provider keeps
its own browser session, and the next visit would log the user straight
back in. So, by default, sign-out continues at the provider
(RP-initiated logout):

- The backend reads `end_session_endpoint` from the provider's discovery
  document at startup. When it is there and `oidc.endSession` is `true`
  (default), `POST /v1/logout` answers `200 {"endSessionUrl": ...}` and the
  portal navigates the browser to that URL, which ends the provider
  session. Without the endpoint, or with `oidc.endSession: false`, the
  answer is `204` and the user lands on the Signed out page; the provider
  session then survives, so **Sign in again** signs in without asking for
  credentials.
- The URL carries `client_id` (the `oidc.clientID` value) and, when the
  portal session retained the login ID token, `id_token_hint` — the hint is
  what makes the provider end its session without asking the user to
  confirm. A session without a retained token (pre-v0.3 row, rotated key)
  sends `client_id` only.
- `post_logout_redirect_uri` is sent only when `oidc.postLogoutRedirect` is
  set (default empty). The provider only redirects to URIs registered on
  the client, so register the value first, then set it, for example
  `https://<portalHost>/signed-out`. Left empty, the provider shows its own
  logged-out page, which is fine.
- The target comes only from the discovery document and the chart values.
  Nothing in the sign-out request (query, body, headers, `Host`) can change
  it, so it is not an open redirect. A discovered endpoint that is not an
  absolute `https` URL — `http` is accepted only for loopback dev hosts —
  is ignored and sign-out stays local.

```yaml
oidc:
  endSession: true            # default; false = sign-out stays local to the portal
  postLogoutRedirect: ""      # default; e.g. https://portal.example.com/signed-out
```

**Keycloak.** Discovery already has `end_session_endpoint` and
`id_token_hint` is normally sent, so Keycloak ends its session without a
confirmation page. To return to the portal afterwards, add
`https://<portalHost>/signed-out` under the client's *Valid post logout
redirect URIs* and set `oidc.postLogoutRedirect` to the same value. Do not
change the OIDC client ID for this.

**Other providers** that reject an end-session request carrying only
`client_id` (a session without a retained ID token): set
`oidc.endSession: false` there.

The discovery document is read when the backend starts, so after changing
the provider's configuration restart the backend (`helm upgrade` or a
rollout restart). Verify with a browser: sign in, choose **Sign out**, then
open the portal again. You must be asked to sign in (or land on the Signed
out page), not be signed in silently.

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
