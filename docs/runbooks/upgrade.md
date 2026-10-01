# Runbook — Upgrade the TinyCDI platform (operator / api / gateway / portal / CRDs)

Companion to install.md §Upgrade. Covers moving a Helm release from one
release candidate to the next, the ordering that keeps running sessions
alive, and what "rollback" does and does not mean.

## What is safe to upgrade while sessions run

| Component | Effect of a restart/upgrade | Session impact |
|---|---|---|
| `portal` | static SPA + /v1 proxy | page reloads; no session loss |
| `gateway` | holds live WebSocket relays | **connected streams drop** — users reconnect inside their live lease via the session cookie (no new ticket); broker fencing still binds the lease to the same runtime incarnation |
| `api` (broker) | tickets/leases/quota in Postgres survive | in-flight API calls retry; leases renew on their next 10 s tick — a broker outage shorter than the 30 s lease TTL is invisible; longer outages fail closed: gateways drop sessions whose leases expire (by design) |
| `operator` | reconcile resumes from persisted annotations (`applied-intent`, `template-snapshot`, `finalizer-progress`) | running pods untouched; in-flight teardown continues after restart |

Upgrade order that minimizes user-visible impact: **CRDs → api → operator →
gateway → portal**. Gateway last-among-stateful so live streams die once,
after the control plane they renew against is already up.

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
4. Check `tinycdi_workspaces_running` — decide whether the maintenance
   window tolerates one gateway stream drop, or drain users first (stop
   issuing tickets; running sessions still die at gateway restart — only
   *schedule* the restart, there is no connection draining in MVP).

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
#    images.{api,operator,gateway,portal}.digest: "sha256:..."

# 3. Render and diff before applying. Seeded WorkspaceTemplates are
#    published as immutable revision objects "<name>-<hash8>": a template
#    change diffs as a create+delete pair, never a spec mutation.
$HELM template tinycdi deploy/helm/tinycdi -n <release-ns> \
  -f my-values.yaml | $K diff -f -

# 4. Upgrade.
$HELM upgrade tinycdi deploy/helm/tinycdi -n <release-ns> -f my-values.yaml

# 5. Watch rollouts in order.
$K -n <release-ns> rollout status deployment/api deployment/operator
$K -n <release-ns> rollout status deployment/gateway deployment/portal
```

Post-checks: `tinycdi_lease_failures_total` back to baseline, a synthetic
create→connect→delete round-trip, and `tinycdi_quota_drift == 0`.

### Hardening notes (chart)

When upgrading to a chart that includes the SEC-* hardening set, note the
fail-closed changes — `helm template`/`upgrade` fails until values comply:

- `operator.metricsBindAddress`/`metricsSecure` are removed: the operator
  has no metrics endpoint (secure metrics need cluster-scoped authz RBAC
  the chart never grants). Drop the keys from your values.
- `networkPolicy.prometheusPeers` is **required** (non-empty, scoped to
  your monitoring pods) when `gateway.metricsListen` is set, and gateway
  metrics moved off the public Service onto the ClusterIP
  `gateway-metrics` Service.
- `database.allowedPeers` and `oidc.egressCIDRs` must be non-empty — and
  `allowedPeers` must also differ from the shipped `ipBlock: 0.0.0.0/32`
  **deny-all placeholder**: the render now fails on it, because upgrading
  with the placeholder in place silently cut the api off from its
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
  metrics flags, api `--dev-insecure-db`/`--required-groups`, gateway
  `--metrics-listen`), `capabilities.drop` lists that drop less than ALL,
  `appArmorProfile: Unconfined`, `seLinuxOptions`, root
  `fsGroup`/`supplementalGroups`, `hostPath` in `gateway.extraVolumes`,
  a non-verifying `database.tls.mode`, and
  `podSecurity.managedEnforce=privileged`. `hostUsers: false` is a
  hardening and stays allowed (CHTR-3).
- **DB TLS is now mandatory by default (breaking):** the api refuses to
  start on a non-verifying sslmode (CHTR-8) — `disable`, `allow`,
  `prefer`, `require` and unset all fail. `database.tls.mode` therefore
  defaults to `verify-full`; databases with a private CA need
  `database.tls.caSecret` (mounted as `PGSSLROOTCERT`), databases with a
  publicly-trusted certificate need nothing extra (pgx falls back to the
  system CA pool). An explicit `sslmode=` in the DSN still wins over
  `PGSSLMODE`. A dev-only escape exists: `dev.enabled=true` + a weaker
  mode renders `--dev-insecure-db`.
- New optional login gate: `oidc.requiredGroups` (list, default `[]`)
  renders `--required-groups=<csv>` on the api — an ID-token `groups`
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

`db.Migrate` runs at `api` startup (`cmd/api/main.go`) and applies embedded
migrations in filename order, idempotently via `schema_migrations`
(`internal/store/migrate.go`). Deploy the **new api before or together
with** anything that writes new-shaped rows; never run an old api against a
newer schema it cannot read — treat "old binary + new schema" as
unsupported, and "new binary + old schema" as the supported direction.

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
- The internal mTLS chain (`tinycdi-api-internal-tls`,
  `tinycdi-internal-ca`, `tinycdi-gateway-mtls`,
  `tinycdi-operator-mtls`) is operator-managed PKI or cert-manager
  (`certManager.enabled`, which also handles renewal). When rotating
  manually: replace the CA bundle first (so both old and new client certs
  verify), then the client certs, then the server cert — in that order,
  or every component loses its peer at once. The operator's client cert
  **CN must stay `operator`** (or match `api.operatorCN`) — the broker's
  workspace revoke/drain routes accept no other identity (ADR 0003).
- `devAllowNoBroker` exists to run the operator without a broker in dev —
  it must never appear in a release values file; the operator fails fast
  if the broker client is unconfigured. The render rejects
  `operator.devAllowNoBroker` (and any securityContext override that
  weakens the hardened defaults) unless `dev.enabled: true` — an explicit
  dev-only escape hatch.
