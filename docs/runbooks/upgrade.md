# Runbook — Upgrade the TinyCDI platform (operator / backend / frontend / CRDs)

Companion to install.md §Upgrade. Covers moving a Helm release from one
release candidate to the next, the ordering that keeps running sessions
alive, and what "rollback" does and does not mean.

## Upgrading from v0.2 to v0.3

v0.3 ("Operate") keeps the v0.2 topology — the same `backend`,
`operator` and `frontend` Deployments, the same session-domain routing —
and upgrades **without dropping sessions**. What is new is fail-closed
values: the render refuses the upgrade while a legacy
`templates[].nodeSelector` entry or a non-allowlisted Kasm Browser
template remains, `backend.trustedProxies` is required wherever an edge
fronts the backend, four forward-only database migrations run, and both
CRDs change. Read "Required values" before touching the release.

### Prerequisites

1. **Take the coordinated backup set** (`docs/runbooks/backup-restore.md`)
   — the `pg_dump` specifically. Four embedded migrations run at backend
   startup, all forward-only:
   - `015_sessions_id_token` — adds `sessions.id_token`: the AEAD-sealed
     OIDC ID token used as `id_token_hint` on logout (the raw JWT is
     never stored; NULL rows just fall back to a `client_id`-only
     end-session URL).
   - `016_launch_ticket_clipboard` — adds `launch_ticket.clipboard_policy`:
     the template's clipboard policy recorded at issue so the gateway
     redirect can re-assert it.
   - `017_sessions_drop_csrf` — drops `NOT NULL` on the dead v0.1 column
     `sessions.csrf_token`. The column itself **stays** so a still-running
     v0.2 replica can keep naming it; it drops in v0.4.
   - `018_lease_stream_owner` — adds `stream_owner_tab`,
     `stream_owner_epoch` and `portal_session_digest` to
     `connection_lease` plus `portal_session_digest` to `launch_ticket`
     (per-tab stream ownership — see "What happens to running
     sessions").
   All are additive/expand: new nullable columns plus one dropped
   constraint, so a v0.2 replica still runs against the migrated schema
   during the rolling window. Past the window, "old binary + new schema"
   remains the unsupported direction — see "Schema migrations" below.
2. **Apply the CRDs first.** v0.3 changes both `workspaces` and
   `workspacetemplates` under `deploy/helm/tinycdi/crds/`: templates gain
   the optional `lifecycle.imageUpdate` (`OnStart` default, `Pinned`)
   and the Workspace `templateRef` CEL rule widens — the platform API
   may re-point a workspace to a newer revision of its template family
   only while `desiredState` is `Stopped`. Both changes are write-path
   only; existing objects stay valid. Under Argo CD the cached schemas
   must be refreshed before the app writes the new fields — see the
   GitOps note under "Procedure".
3. **No node or workload changes** — unless your nodes lack AppArmor
   (`runtime.appArmor` under "Required values").

### Required values

`backend.trustedProxies` is an operational requirement (the backend
warns, it does not fail); the Kasm and `nodeSelector` items fail the
render; the rest are changed defaults you may want to pin back.

- **`backend.trustedProxies`** — required whenever an ingress or Gateway
  fronts the backend. v0.3 rate-limits the public API per client: the
  anonymous surface (login start, unauthenticated session probes) keys
  on the client IP, while signed-in traffic keys per validated session
  and each OIDC callback on its validated state — so an office behind
  one NAT keeps per-user budgets everywhere except the anonymous
  sign-in start itself (`docs/runbooks/capacity.md`, "Sign-in rate
  limits and NAT"). Leave it empty and every user collapses into the
  edge's own IP bucket; the backend only logs a startup warning, it
  does not fail.
- **Kasm Browser templates** — a seeded `adapter: kasm` template with
  `experience: Browser` fails the render until its image is on
  `kasmAdapter.browserAllowlist` or the template is re-classed
  `experience: Desktop`. The allowlist accepts digest-pinned
  browser-class `build/kasm-catalog.txt` entries only and ships empty —
  see "Kasm browser templates — the E13 engine gate (fail-closed)"
  below.
- **`templates[].nodeSelector` is removed** — the v0.1 entry field now
  fails the render; move the map to the typed
  `templates[].spec.placement.nodeSelector`.
- **Operator HA is on by default** — `operator.replicas: 2`,
  `operator.leaderElect: true`, `operator.podDisruptionBudget.enabled:
  true`, `frontend.pdb.enabled: true`. Size the cluster for two
  operator pods; a single-replica install keeps `operator.replicas: 1`
  (leader election with one replica is fine — it just holds the Lease).
- **GitOps users: drop stale `ignoreDifferences`.** The chart now
  renders every API-server-defaulted HTTPRoute field (`parentRefs`
  group/kind, `rules[].matches`, `backendRefs` group/kind/weight) and
  `spec.linux.adapter` explicitly — suppressions you kept for those
  paths are dead config that can mask real drift.
- **Nodes without AppArmor** (kind, SELinux-family distros): runtime
  pods now carry `appArmorProfile: RuntimeDefault` by default
  (`runtime.appArmor.requireRuntimeDefault: true`); set it to `false`
  or the kubelet refuses runtime pods there. Templates with a Localhost
  AppArmor profile (browser templates) cannot run on such nodes either
  way — install.md "Nodes without AppArmor".

### Values migration

Only real changes; everything not listed keeps its name and meaning.

| v0.2 | v0.3 |
|---|---|
| `templates[].nodeSelector` | `templates[].spec.placement.nodeSelector` — the old entry field fails the render |
| `operator.replicas: 1`, `operator.leaderElect: false` | defaults `2` and `true` — pin `replicas: 1` to stay single-replica |
| `operator.podDisruptionBudget.enabled: false`, `frontend.pdb.enabled: false` | both default `true` |
| — | `backend.trustedProxies` — see "Required values" |
| — | `kasmAdapter.browserAllowlist` — see the E13 gate section below |
| — | `images.linuxBase` — the shared base layer the runtime profiles build FROM; pin its digest from the release's `runtime-images.json` alongside the profiles' (the chart never pulls it) |
| — | `images.{linuxDesktop,browser}.engines` — optional; render as the templates' `image-chromium`/`image-firefox` annotations for the stale-image view |
| — | `dashboards.*`, `alerts.*` — opt-in Grafana ConfigMaps and a PrometheusRule; require `backend.metrics.enabled` (observability.md) |
| — | `runtime.appArmor.requireRuntimeDefault` — default `true`; set `false` on nodes without AppArmor |

### What happens to running sessions

**Sessions survive the upgrade.** The schema changes are expand-only,
the two backend replicas roll one at a time (`maxUnavailable: 0`), and a
terminating pod's pre-stop drain holds both listeners for the drain
window (`-drain-window`, 8 s default): readiness drops at once, open
reads keep being served, and only new launches and new stream upgrades
get a retryable 503. A tab whose stream dies reconnects to a sibling
inside its live lease — same session cookie, no re-launch. v0.3 also
records the claiming portal tab on the lease, so a same-tab reconnect
after a rollout resumes cleanly instead of flashing "open in another
tab"; only an actual second tab raises that prompt.

The one case that still costs sessions is every backend replica down
past the 30 s lease TTL — the same exposure as any v0.2 rollout. A
maintenance window is optional; pick a low-traffic one if one reconnect
per user matters.

### Procedure

1. Translate values per the migration table (`trustedProxies`,
   `browserAllowlist` if you run Kasm Browser templates,
   `templates[].spec.placement.nodeSelector`).
2. Take the backup set — the `pg_dump` is the rollback artefact.
3. `kubectl diff -f deploy/helm/tinycdi/crds/`, review, then
   `kubectl apply -f deploy/helm/tinycdi/crds/` — before the release,
   not with it: Helm never upgrades objects in `crds/`.
4. Pin digests: `images.{backend,frontend,operator}.digest` for the
   platform release; `images.{linuxDesktop,browser}.digest` (with
   `builtAt`/`engines`) and `images.linuxBase.digest` from
   `runtime-images.json` for the runtime train.
5. `helm template | kubectl diff`, then `helm upgrade` — the shared
   "Procedure" below has the full commands (its steps 3–4).
6. Watch the rollouts in order — `deployment/backend` (each pod's
   streams drop and reconnect), `deployment/operator` (the standby
   acquires the leader-election Lease when the leader pod goes),
   `deployment/frontend`.

Argo CD variant: sync `crds/` outside the application (or in an earlier
sync wave), hard-refresh — `argocd app get <app> --hard-refresh` — so
the cached OpenAPI schemas pick up `lifecycle.imageUpdate` and the wider
`templateRef` rule, then sync the app (see "GitOps note — the Argo CD
schema cache on CRD changes" below). Seeded templates diff as a
delete+create of `<name>-<hash8>` revision objects, as always.

### Post-upgrade checks

- `SELECT version, name FROM schema_migrations ORDER BY version;` shows
  015–018.
- `kubectl -n <release-ns> get deploy` — `backend` and `operator` at
  2/2, `frontend` ready; `kubectl -n <release-ns> get pdb` shows the
  operator and frontend budgets.
- `kubectl -n <release-ns> get lease` — the `b6b73984.cdi.tinyorbit.vn`
  leader-election Lease names a holder.
- The backend startup log carries no trusted-proxies warning once
  `backend.trustedProxies` is set; a quick sign-in flood answers
  `429 RATE_LIMITED` instead of queueing.
- A synthetic login → launch → connect round-trip; reload the session
  tab through a backend rollout and confirm it resumes without the
  "open in another tab" prompt.
- `tinycdi_lease_failures_total` back to baseline and
  `tinycdi_quota_drift == 0`, as after any upgrade. With `dashboards` /
  `alerts` enabled, the `tinycdi-overview` / `tinycdi-capacity`
  ConfigMaps and the `tinycdi` PrometheusRule exist and panels show
  data.

### Rollback across v0.2 → v0.3

Migrations 015–018 do not roll back with `helm rollback`. The clean
downgrade is a **restore**: bring back the previous chart release
(`helm upgrade` with the v0.2 chart and the pre-upgrade values file)
**and** restore the pre-upgrade `pg_dump` per
`docs/runbooks/backup-restore.md`. What is safe without a restore:

- **Binary rollback inside the additive window.** 015/016/018 add only
  nullable columns the v0.2 binaries never name, and 017 drops a
  constraint on a column they still write — so `helm rollback` is a
  workable short-term downgrade. It is still "old binary + new schema":
  treat it as a bridge to the restore, not a steady state. Rows written
  by v0.3 (sealed `id_token`s, recorded clipboard policies,
  stream-owner evidence) are simply ignored under v0.2.
- **Re-pointed workspaces.** `imageUpdate: OnStart` may have moved
  stopped workspaces onto a newer `<name>-<hash8>` revision; a rollback
  deletes revisions the old chart did not render. Before a start under
  v0.2, check `spec.templateRef` still names an object that exists
  (`kubectl -n <tenant-ns> get workspacetemplates`).
- **The CRDs stay.** Do not "un-apply" the v0.3 CRDs — the wider CEL
  rule and the new optional field harm nothing under v0.2, and a
  `kubectl replace` of the older schema can prune stored data.

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
  and the portal sets only `__Host-`-prefixed cookies.
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
   and reports them closed, then keeps both listeners serving for the rest
   of the drain window (`-drain-window`, default 8 s) — late reads still
   answer and new launches/upgrades get a retryable 503 while endpoint
   removal propagates; clients then reconnect inside their live lease.

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

### Kasm browser templates — the E13 engine gate (fail-closed)

A seeded `adapter: kasm` template with `experience: Browser` **fails the
render** after this upgrade until its image is on
`kasmAdapter.browserAllowlist` or the template is re-classed as
`experience: Desktop`. The allowlist is the chart-side binding to the
catalog's tightened ≤2-major browser engine gate — only browser-class
`build/kasm-catalog.txt` entries may be listed
(`.github/scripts/check-kasm-catalog.sh` enforces it); non-browser kasm
templates keep the ≤4 budget and ignore the list.

```yaml
# Option A — the image meets the ≤2 gate: allowlist the digest-pinned ref.
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
pin — lag 4), so a Browser template pinned to it only has option B. If no
cataloged browser image meets the gate the allowlist stays empty — see
`docs/kasm-images.md`.

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

v0.2 → v0.3 keeps `sessions.csrf_token` as a **nullable** dead column
(migration 017 only drops its `NOT NULL`): a still-running v0.2 replica
names it in every session `SELECT`/`INSERT`, so it survives the
rolling-upgrade window. Once every backend is v0.3 the column carries
only NULLs; **v0.4 drops `sessions.csrf_token`** — do not roll back to
v0.2 after the v0.4 upgrade without restoring the pre-upgrade dump.

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
