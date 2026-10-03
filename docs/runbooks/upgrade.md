# Runbook — Upgrade the TinyCDI platform (operator / backend / frontend / CRDs)

Companion to install.md §Upgrade. Covers moving a Helm release from one
release candidate to the next, the ordering that keeps running sessions
alive, and what "rollback" does and does not mean.

## Upgrading from v0.1 to v0.2

v0.2 replaces the three v0.1 control-plane Deployments (`api`, `gateway`,
`portal`) with two (`backend` — the public API and the session gateway
merged into one binary — and `frontend` — the static portal SPA server)
and moves every session onto a per-workspace host
`<label>.<sessionDomain>` behind one wildcard edge route. It is a
**breaking upgrade**: the values schema changed, two database migrations
run, and every open session drops once. Read this whole section before
touching the release.

### Prerequisites

All of these must be in place before `helm upgrade`. The chart's render
checks cover only part of them: it fails closed without `sessionDomain`, with
a `portalHost` that equals or sits inside it, without the login-key Secret
(3) and with `runtime.placement.allowSharedNodes: false` and an empty
`nodeSelector`. It does **not** check that the wildcard DNS record or the
wildcard certificate exist (1) — `backend.tls.session.existingSecret`
defaults to `tinycdi-backend-session-tls`, and a missing or non-wildcard
Secret only shows up as a backend pod stuck in `ContainerCreating` or as
browser certificate errors on the session hosts — nor that the labelled and
tainted node pool exists (2): without it workspace pods stay `Pending`.
Verify items 1, 2 and 4 yourself.

1. **Wildcard DNS record and wildcard certificate for the session
   domain.** The edge carries one route `*.<sessionDomain>` and the
   backend session listener terminates TLS for every
   `<label>.<sessionDomain>` host. Create the wildcard DNS record,
   issue a wildcard certificate (DNS-01 — HTTP-01 cannot issue
   wildcards) and store it in the Secret that
   `backend.tls.session.existingSecret` names. `portalHost` must not
   equal or sit inside `sessionDomain` — the render refuses an overlap.
2. **Dedicated workspace node pool** — the v0.2 default
   (`runtime.placement.allowSharedNodes: false`). Label the pool nodes
   `cdi.tinyorbit.vn/workspace=true` and taint them
   `cdi.tinyorbit.vn/workspace:NoSchedule` (install.md "Workspace node
   pool"), or point `runtime.placement.nodeSelector` / `.tolerations` at
   your existing pool — a dedicated pool needs a non-empty selector.
   The explicit opt-out `runtime.placement.allowSharedNodes: true`
   exists for kind/dev clusters only and gives up node-level isolation.
3. **Login-key Secret.** `backend.loginKeys.existingSecret` must name a
   Secret holding a 32-byte `current` key (plus `previous` while
   rotating) — it seals the `__Host-tcdi_login` cookie that lets an OIDC
   login started on one replica finish on another.
   `backend.loginKeys.generate: true` mints
   `<release>-backend-login-keys` once via `lookup` instead (kept across
   upgrades; unusable under GitOps/`helm template` against no cluster).
4. **The coordinated backup set** from `docs/runbooks/backup-restore.md`
   — not optional here: rollback crosses a schema migration (see "Database migrations" and
   "Rollback across v0.1 → v0.2" below).

### Values migration

The 0.2.0 schema still accepts the old `api.*` / `gateway.*` /
`portal.*` / `sessionHost` keys only so the render can fail with a
migration hint instead of a bare schema error. Translate your values
file first:

| v0.1 | v0.2 |
|---|---|
| `api.*` | `backend.*` — `internalTLS` → `tls.internal`; `clientCA`, `extraCA`, `operatorCN`, `sessionIdle`, `extraPortalOrigins` keep their names |
| `gateway.*` | `backend.*` — `tls` → `tls.session`, `metricsListen` → `metrics.{enabled,port}`, `id` → `gatewayID`, `extraAllowedHosts` → `controlHosts`; `audience`, `upstreamCA`, `controlToken` keep their names; `mtls`, `trustedCA`, `service` are removed (the gateway reaches the broker in-process and the session edge is an ingress/gatewayApi route, not a Service) |
| `portal.*` | `frontend.*` — `tls` keeps its name; `apiUpstream`, `webRoot`, `service` are removed (the edge routes `/v1` to the backend) |
| `images.api`, `images.gateway` | `images.backend` (the `tinycdi-api`/`tinycdi-gateway` images are no longer built) |
| `images.portal` | `images.frontend` |
| `api.podDisruptionBudget`, `portal.podDisruptionBudget` | `backend.pdb`, `frontend.pdb` (`operator.podDisruptionBudget` is unchanged) |
| `sessionHost` | `sessionDomain` |

New or newly-required values beyond the rename: `backend.loginKeys`
(prerequisite 3 above), `backend.tls.{app,session,internal}` (one certificate per
listener — `tls.session` must be the wildcard), and
`runtime.placement.*` (prerequisite 2 above). `backend` defaults to 2 replicas with
`maxUnavailable: 0`, a `minAvailable: 1` PDB and preferred node
anti-affinity — size the cluster for two backend pods.

Example migrated values (renders as-is against the 0.2.0 chart):

```yaml
managedNamespaces:
  - name: tinycdi-tenant-a
    tenant: tenant-a

portalHost: portal.example.com
sessionDomain: session.example.com    # was: sessionHost

oidc:
  issuer: https://idp.example.com/realms/tinycdi
  clientID: tinycdi
  existingSecret: tinycdi-oidc-client

database:
  existingSecret: tinycdi-backend-db
  allowedPeers:
    - ipBlock: {cidr: 10.20.30.40/32}

backend:                              # was: api.* + gateway.*
  loginKeys:
    existingSecret: tinycdi-backend-login-keys   # NEW — 32-byte "current"
  tls:
    session:
      existingSecret: tinycdi-backend-session-tls # MUST cover *.session.example.com

frontend:                             # was: portal.*
  tls:
    existingSecret: tinycdi-frontend-tls

# Dedicated workspace pool (v0.2 default) — label + taint the nodes per
# install.md "Workspace node pool" BEFORE the upgrade, or set
# runtime.placement.allowSharedNodes: true (kind/dev only).
runtime:
  placement:
    allowSharedNodes: false
```

### What happens to running sessions

**Every session drops once.** The v0.1 `gateway` Deployment is deleted,
the v0.1 session cookies name the old single session host, and the lease
rows gain new columns — none of that carries a live v0.1 stream into
v0.2. Schedule a maintenance window, tell users their desktops close,
and have them re-launch from the portal after the upgrade. From then on
the restart-safe behaviour in the table under "What is safe to upgrade while
sessions run" (further down) applies: a backend rollout
no longer costs a session.

### Database migrations — forward-only

The new backend runs two embedded migrations at startup
(`schema_migrations` is idempotent, so replicas racing is fine):

- `011_lease_session` — adds `session_digest` and `stream_epoch` to
  `connection_lease` (cookie-digest lookups and the cross-replica stream
  fence).
- `012_principal_directory` — adds `display_name`/`email` to `sessions`
  and creates `principal_directory` for portal display names.

Both are forward-only and additive. Deploy the new chart once; do not
run the v0.1 binaries against the migrated schema — "old binary + new
schema" is unsupported in both directions.

### Procedure

Follow the steps under "Before you start" and "Procedure" further down (the
sections after "Upgrading from v0.1 to v0.2") with the
migrated values file: CRD diff → pin digests → `helm template | kubectl
diff` → `helm upgrade` → watch `deployment/backend`, then
`deployment/operator`, then `deployment/frontend`. The `api`, `gateway`
and `portal` Deployments/Services disappear in the same release.

### Post-upgrade checks

- `kubectl -n <release-ns> get deploy` shows exactly `backend` (2/2),
  `frontend`, `operator` — no `api`/`gateway`/`portal` objects remain.
- `kubectl -n <release-ns> get ingress` (or `httproute`) shows the
  `*.<sessionDomain>` wildcard route next to the portal route.
- A synthetic login → launch → connect round-trip: the session opens on
  a `<label>.<sessionDomain>` host presenting the wildcard certificate,
  and the portal sets only `__Host-`-prefixed cookies (the backend
  expires the removed v0.1 `tcdi_csrf`/`tcdi_session_origin` cookies on
  first login).
- `tinycdi_lease_failures_total` back to baseline and
  `tinycdi_quota_drift == 0`, as after any upgrade.
- `kubectl get pods -o wide` shows new workspace pods on the dedicated
  pool (the labelled/tainted nodes), not on shared nodes.

### Rollback across v0.1 → v0.2

`helm rollback` alone is NOT safe: migrations 011 and 012 are
forward-only and the v0.1 `api`/`gateway`/`portal` values shape cannot
drive the 0.2.0 chart. Rollback means **restore**: bring the v0.1 chart
release back (`helm upgrade` with the v0.1 chart and the pre-upgrade
values file) **and** restore the pre-upgrade database dump per
`docs/runbooks/backup-restore.md`. Re-run the post-checks afterwards.

## Workspaces stuck by a pre-fix max-duration stop (release candidates up to rc.5)

Up to rc.5, `status.startedAt` kept the time of a workspace's **first** start.
Once that was older than the template's `maxDuration` (8 h), starting the
workspace again made the operator stop it at once, and the platform database
never learned of that stop. The symptom is a workspace that shows
`Starting`/`Provisioning` for ever while its Workspace object says
`Stopped`, with its quota reservation still held.

From the release that carries this fix (`startedAt` belongs to the running
incarnation) nothing needs to be done: the operator clears the stale value on
its first reconcile, and within one expiry sweep (`-expiry-interval`, default
30 s) the backend brings the database row in line with the Workspace
(`desiredState` becomes `Stopped`, the events list shows `MaxDurationReached`),
the recovery pass releases the reservation once the runtime is proven gone,
and **Start** then works and gives the new run a fresh 8 h cap.

If a user presses Start before the sweep has reached the row (Start is a no-op
on a row the database still thinks is Running), the **manual fallback** is to
press **Stop**, wait for the state to read Stopped, and then press **Start**.
Operators can confirm a row has converged with:

```sql
SELECT name, desired_state, phase FROM workspaces WHERE state = 'active';
SELECT workspace_id, state FROM quota_reservation WHERE state = 'held';
```

A stopped workspace that keeps `held` after a few recovery intervals is a
different case: see "Stopped Retain workspaces and quota" in
`docs/runbooks/capacity.md`.

### Disconnect timing

The disconnect stop runs `disconnectTimeout` after the gateway reports the
last stream closed (the closing tab or a dropped connection), plus up to one
expiry sweep (30 s) — not from the last lease renewal. The timeout is the
template's `lifecycle.disconnectTimeout` (the seeded templates use 5 m). Up to
rc.5 the expiry planner looked a template up by the workspace's catalog base
name only and, missing the `<name>-<hash8>` revision objects, applied the
platform default instead (10 m): a 5 m template stopped about 10 m after the
tab closed. The planner now resolves the base name to the newest revision, as
the operator does.

## Upgrading to 0.2.0-rc.6

Migration 014 adds three nullable columns to `quota_reservation`
(`restart_slots`, `restart_cpu_millis`, `restart_memory_bytes`). It is
additive and idempotent. Take a `pg_dump` first
(`docs/runbooks/backup-restore.md`).

On the first recovery pass after the upgrade, every stopped **Retain**
workspace whose pod is gone converts to a disk-only quota hold: its running
slot, CPU and memory are released with no user action, and its disk stays
held. A start re-acquires the admitted compute from the stored vector
(`docs/runbooks/capacity.md` → "Stopped Retain workspaces and quota").

**No rollback below rc.6 after the first recovery pass; restore from the
pg_dump instead.** An older binary would see held rows with a zero compute
vector and `reserveForStart` would re-reserve zero.

## What is safe to upgrade while sessions run

| Component | Effect of a restart/upgrade | Session impact |
|---|---|---|
| `frontend` | static SPA, no proxy state | page reloads; no session loss |
| `backend` | public API + in-process broker + session gateway; tickets, leases and quota live in Postgres | a restart or rollout **closes the streams on that replica** — clients reconnect to another replica with the same session cookie inside their live lease (no new launch ticket); the lease row survives and the session is rebuilt from it |
| `operator` | reconcile resumes from persisted annotations (`applied-intent`, `template-snapshot`, `finalizer-progress`) | running pods untouched; in-flight teardown continues after restart |

Upgrade order that minimizes user-visible impact: **CRDs → backend →
operator → frontend**. The backend hosts the broker and the session gateway
in one process, so live streams drop only when the backend rolls; the
operator follows so its teardown calls hit the new internal listener; the
frontend is stateless and can go last (or first — it only needs the API it
calls to be compatible).

With the default `backend.replicas: 2` and `maxUnavailable: 0` a rollout
replaces one pod at a time while a peer keeps serving: a client whose
stream dies reconnects with the same cookie and resumes within seconds —
no re-launch, no new ticket. The one case that still needs a re-launch is
**every backend replica down for longer than the 30 s lease TTL**: nothing
renews the leases, they expire, and each user must start a fresh session.

## Before you start

1. Read the diff of `deploy/helm/tinycdi/crds/` since the last applied
   revision — CRD changes are the irreversible part.
2. Take the coordinated backup set (DB dump + CR export + volume state)
   from `docs/runbooks/backup-restore.md`. An upgrade is exactly the moment
   you might need it.
3. Record current digests:
   `helm get values tinycdi -n <release-ns>` plus
   `kubectl -n <release-ns> get deploy -o jsonpath='{..image}'`
   (values file = the one used at install — install.md creates
   `my-values.yaml`).
4. Verify the release you are about to pin: `cosign verify` (and
   `verify-attestation`) every image in the release, **including
   `tinycdi-linux-base`** — the release carries one more image than the chart
   deploys (`.github/README.md`, "Verifying a release"). If a GitOps wrapper
   or umbrella chart pins digests from the release, copy `images.linuxBase.digest`
   too (the packaged chart's values carry it; the chart never pulls it, so
   leaving it stale breaks nothing but misleads anyone building a custom
   desktop image on the base).
5. Check `tinycdi_workspaces_running` — decide whether the maintenance
   window tolerates one stream drop per backend pod, or drain users first
   (stop issuing tickets). A graceful backend stop drains its open streams
   and reports them closed; clients then reconnect inside their live lease.

## Procedure

```bash
HELM=helm
K=kubectl

# 1. CRDs — Helm never upgrades objects in crds/ (install.md §Upgrade).
$K diff -f deploy/helm/tinycdi/crds/
$K apply -f deploy/helm/tinycdi/crds/   # only if the diff is reviewed
# Additive schema (new optional fields, new CEL on writes) is the only
# class that is safe to apply under running CRs. A change that would
# invalidate existing specs needs a migration plan first — stop here.

# 2. Pin the new images by digest in values — never upgrade to a tag.
#    images.{backend,frontend,operator}.digest: "sha256:..."
#    Runtime images move on their own train (docs/images.md): pin
#    images.{linuxDesktop,browser}.digest from runtime-images.json, and
#    images.linuxBase.digest alongside (the chart never pulls linuxBase).

# 3. Render and diff before applying. Seeded WorkspaceTemplates are
#    published as immutable revision objects "<name>-<hash8>": a template
#    change diffs as a create+delete pair, never a spec mutation.
$HELM template tinycdi deploy/helm/tinycdi -n <release-ns> \
  -f my-values.yaml | $K diff -f -

# 4. Upgrade.
$HELM upgrade tinycdi deploy/helm/tinycdi -n <release-ns> -f my-values.yaml

# 5. Watch rollouts in order.
$K -n <release-ns> rollout status deployment/backend deployment/operator
$K -n <release-ns> rollout status deployment/frontend
```

Post-checks: `tinycdi_lease_failures_total` back to baseline, a synthetic
create→connect→delete round-trip, and `tinycdi_quota_drift == 0`.

### GitOps note — the Argo CD schema cache on CRD changes

Argo CD validates and diffs CRs against the CRD OpenAPI schemas it has
**cached**; a CRD update is not picked up automatically. After applying a
changed CRD (step 1 — v0.3 updates both `workspaces` and
`workspacetemplates`), a sync that writes the new fields can fail or diff
them as "unknown field" until the cache is refreshed. Apply `crds/` first
(outside the app, or in an earlier sync wave), then hard-refresh the
application — `argocd app get <app> --hard-refresh`, or Refresh → Hard in
the UI — before letting it reconcile the CRs.

### Hardening notes (chart)

When upgrading to a chart that includes the SEC-* hardening set, note the
fail-closed changes — `helm template`/`upgrade` fails until values comply:

- `operator.metricsBindAddress`/`metricsSecure` are removed: the operator
  has no metrics endpoint (secure metrics need cluster-scoped authz RBAC
  the chart never grants). Drop the keys from your values.
- `networkPolicy.prometheusPeers` is **required** (non-empty, scoped to
  your monitoring pods) when `backend.metrics.enabled` is set, and backend
  metrics are served off the public Service on the ClusterIP
  `backend-metrics` Service.
- `database.allowedPeers` and `oidc.egressCIDRs` must be non-empty — and
  `allowedPeers` must also differ from the shipped `ipBlock: 0.0.0.0/32`
  **deny-all placeholder**: the render now fails on it, because upgrading
  with the placeholder in place silently cut the backend off from its
  database (CHTR-7).
- `ingress.enabled` requires `ingress.tls.existingSecret`; every
  `gatewayApi.parentRefs` entry needs a `sectionName` (the HTTPS
  listener).
- Seeded `networkProfile: InternetOnly` templates require
  `operator.clusterCIDRs` (this cluster's pod/service/node ranges).
- `operator.devAllowNoBroker` and privilege-weakening securityContext
  overrides require `dev.enabled: true` — do not set it in production.
  The hardening gate additionally refuses: dangerous `extraArgs`
  (operator `--dev-allow-no-broker`/`--disable-builtin-egress-excepts`/
  metrics flags, backend `--dev-insecure-db`/`--required-groups`/
  `--metrics-listen` and the split-mode broker flags), `capabilities.drop`
  lists that drop less than ALL,
  `appArmorProfile: Unconfined`, `seLinuxOptions`, root
  `fsGroup`/`supplementalGroups`, `hostPath` in `backend.extraVolumes`,
  a non-verifying `database.tls.mode`, and
  `podSecurity.managedEnforce=privileged`. `hostUsers: false` is a
  hardening and stays allowed (CHTR-3).
- **DB TLS is now mandatory by default (breaking):** the backend refuses to
  start on a non-verifying sslmode (CHTR-8) — `disable`, `allow`,
  `prefer`, `require` and unset all fail. `database.tls.mode` therefore
  defaults to `verify-full`; databases with a private CA need
  `database.tls.caSecret` (mounted as `PGSSLROOTCERT`), databases with a
  publicly-trusted certificate need nothing extra (pgx falls back to the
  system CA pool). An explicit `sslmode=` in the DSN still wins over
  `PGSSLMODE`. A dev-only escape exists: `dev.enabled=true` + a weaker
  mode renders `--dev-insecure-db`.
- New optional login gate: `oidc.requiredGroups` (list, default `[]`)
  renders `--required-groups=<csv>` on the backend — an ID-token `groups`
  claim must carry at least one listed group (exact match; empty = every
  IdP account may log in).
- The operator's manager-role is no longer bound in the release namespace
  (the binding is pruned automatically) and `--watch-namespaces` drops it;
  nothing of the operator's lives there.
- The node-profile installer moved out of the release namespace into
  `nodeProfiles.install.namespace` (default `tinycdi-node-profiles`,
  PSS `privileged`, no RBAC): the old release-namespace DaemonSet,
  ServiceAccount and ConfigMap are pruned by the upgrade; the privileged
  work is an initContainer and the verifier now runs as **uid 0 with all
  capabilities dropped** (no privilege escalation, read-only mounts) —
  uid 0 is required because the kernel serves
  `/sys/kernel/security/apparmor/profiles` only to root; a non-root
  verifier could never report Ready and remove-mode falsely reported
  clean (CHTR-1). The installer namespace must now be dedicated — the
  render refuses the release namespace, managed namespaces, `kube-*` and
  `default` (CHTR-2). You may
  revert the release namespace PSS exemption if you added one — baseline
  suffices.

## Running workspaces and images — read this before bumping template images

- A Workspace records an **immutable template snapshot** at first admit
  (`workspaces.cdi.tinyorbit.vn/template-snapshot` annotation, spec JSON +
  sha256 — `internal/operator/workspace_controller.go`). The template
  object is never re-read for that workspace.
- Therefore **upgrading the platform or replacing a WorkspaceTemplate does
  not change the image of any already-created workspace.** There is no
  rolling update of desktops in use — by design (plan global constraints).
- To move users to a new runtime image: publish a new template revision
  (a values change in `templates[]` renders a new `<name>-<hash8>` object),
  then have users create replacement workspaces (attach retained disks
  where applicable — `docs/runbooks/retained-data.md`). The old template
  revision can stay until every workspace on it is deleted.

## Schema migrations

`db.Migrate` runs at `backend` startup (`internal/backend/wire.go`) and
applies embedded migrations in filename order, idempotently via
`schema_migrations` (`internal/store/migrate.go`). Deploy the **new
backend before or together with** anything that writes new-shaped rows;
never run an old backend against a newer schema it cannot read — treat
"old binary + new schema" as unsupported, and "new binary + old schema" as the supported direction.

## Rollback

- **Binary rollback is supported:** `helm rollback tinycdi -n <release-ns>`
  (or re-`helm upgrade` with the previous values file) restores the
  previous Deployment images. Safe when the DB schema is unchanged or the
  old binary can still read the migrated schema.
- **Seeded WorkspaceTemplates roll back cleanly.** Seeded templates are
  immutable revision objects named `<name>-<hash8>`; a rollback (or any
  spec change) deletes the superseded revision and creates the restored
  one — no manual `delete workspacetemplate` step. Superseded revisions
  disappearing is safe for running workloads: every Workspace pinned its
  template at first admit (`template-snapshot` annotation).
- **Do NOT rollback across a schema migration blindly.** Postgres
  migrations are forward-only; `helm rollback` will not undo them. If the
  new release migrated the schema, restore the pre-upgrade DB dump per
  `docs/runbooks/backup-restore.md` — that is a restore, not a rollback.
- **CRDs do not roll back.** `kubectl apply` of an older CRD manifest does
  not remove fields already written to etcd, and `kubectl replace -f crds/`
  with a pruned schema can delete stored data. CRD rollback = restore from
  the CR export, reviewed by hand.
- After any rollback, re-run the post-checks and a synthetic lifecycle
  round-trip.

## Certificate and credential rotation during upgrades

- Runtime upstream TLS certs are per-workspace, self-signed by the
  operator, **365-day** validity: workspaces running longer than that need
  a cert-rotation story — plan one before a release that ships long-lived
  desktops.
- The internal mTLS chain (`tinycdi-backend-internal-tls`,
  `tinycdi-internal-ca`, `tinycdi-operator-mtls`) is operator-managed PKI or cert-manager
  (`certManager.enabled`, which also handles renewal). When rotating
  manually: replace the CA bundle first (so both old and new client certs
  verify), then the client certs, then the server cert — in that order,
  or every component loses its peer at once. The operator's client cert
  **CN must stay `operator`** (or match `backend.operatorCN`) — the broker's
  workspace revoke/drain routes accept no other identity (ADR 0003).
- Rotation needs **no restart**: the operator hot-reloads its broker
  client certificate and the backend hot-reloads the internal listener's
  client-CA bundle plus all three listener certs — the updated Secret
  material lands on the next handshake. Mounts refresh within the kubelet
  sync period (~1 min), so a CA-swap rollout should still follow the order
  above rather than racing the reload.
- `devAllowNoBroker` exists to run the operator without a broker in dev —
  it must never appear in a release values file; the operator fails fast
  if the broker client is unconfigured. The render rejects
  `operator.devAllowNoBroker` (and any securityContext override that
  weakens the hardened defaults) unless `dev.enabled: true` — an explicit
  dev-only escape hatch.
