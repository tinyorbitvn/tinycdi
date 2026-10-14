# Runbook — Observability: metrics, dashboards, alerts

The platform's machine-readable signals are Prometheus metrics on a
dedicated listener, plus optional Grafana dashboards and a
`PrometheusRule` alert set in the chart. All are **off by default**; each
needs `backend.metrics.enabled: true`.

## Metrics

```yaml
backend:
  metrics:
    enabled: true
    port: 9090
serviceMonitor:
  enabled: true                 # if you run the Prometheus Operator
  labels: {release: prometheus} # your stack's serviceMonitorSelector
networkPolicy:
  prometheusPeers:              # REQUIRED when metrics are on — the
    - namespaceSelector:        #   render fails without it; scope to
        matchLabels: {kubernetes.io/metadata.name: monitoring}   # your monitoring namespace
      podSelector:
        matchLabels: {app.kubernetes.io/name: prometheus}
```

The listener serves plain HTTP on its own ClusterIP Service
`backend-metrics` (:9090), never on the public Service — the scrape path
is in-cluster only, default-deny `NetworkPolicy` included, so only the
peers in `networkPolicy.prometheusPeers` reach it.

Keep it off the edge path: the listener has no auth, so never expose
`backend-metrics` through an Ingress, HTTPRoute or a
NodePort/LoadBalancer Service, and never add the metrics port to an edge
rule (`networkPolicy.edgeIngress*`). The chart tests pin this — no
rendered policy admits edge/gateway traffic to the metrics port in any
`edgeIngress` mode (ipBlock, any or cilium).

The **operator exposes no metrics endpoint**: secure controller-runtime
metrics need cluster-scoped TokenReview/SAR RBAC the chart never grants,
and plaintext metrics are banned — `--metrics-bind-address=0` is pinned.
Watch the operator via `kubectl get lease` (leader election), its Pod
status and logs.

### Metric reference

All series carry the `tinycdi_` prefix. `tenant`, `result`, `outcome`,
`reason`, `route`, `listener`, `code_class` and `dest` labels are bounded —
unrecognized values are folded into `"other"`/curated buckets, so high
cardinality cannot leak in.

| Series | Type | Labels | Meaning |
|---|---|---|---|
| `tinycdi_http_requests_total` | counter | `listener`, `route`, `method`, `code_class` | HTTP requests by listener (app/session/internal), route template and response class |
| `tinycdi_http_request_duration_seconds` | histogram | same | request latency; p50/p95 per route |
| `tinycdi_workspace_provisioning_seconds` | histogram | `result` | end-to-end workspace provisioning latency |
| `tinycdi_workspaces_running` | gauge | `tenant` | workspaces in Running, per tenant |
| `tinycdi_workspaces_reserved` | gauge | `tenant` | slots reserved by quota, per tenant — compare with `running` for the quota picture |
| `tinycdi_quota_drift` | gauge | `tenant` | observed drift between quota reservations and actual usage — should stay `0` |
| `tinycdi_lease_failures_total` | counter | `reason` | connection-lease acquire/renew failures |
| `tinycdi_finalizers_stuck` | gauge | — | workspaces whose teardown finalizer is past deadline (see `stuck-finalizer.md`) |
| `tinycdi_pvc_leaks` | gauge | — | PVCs without an owning workspace record |
| `tinycdi_boot_deadline_exceeded_total` | counter | — | runtimes that missed their boot deadline |
| `tinycdi_sessions_active` | gauge | — | live desktop sessions this replica holds |
| `tinycdi_gateway_rehydrations_total` | counter | `result` | session-directory lookups for cookies the replica never saw (`ok`/miss/error) |
| `tinycdi_gateway_streams_fenced_total` | counter | — | streams closed because another replica claimed the stream epoch (takeovers) |
| `tinycdi_gateway_upstream_streams` | gauge | — | upstream-bound requests in flight on this replica's session gateway — websocket streams and plain proxied requests summed over workspaces; each workspace is bounded by `-session-stream-cap` (default 32, per replica). Saturation: a workspace pinned at cap is a storm or a sick runtime |
| `tinycdi_gateway_connection_tokens_dropped_total` | counter | — | client `Connection` header tokens dropped before proxying (only `upgrade`/`keep-alive`/`close` are kept) — nonzero means a client tried to name headers the hop-by-hop strip would delete, e.g. the gateway-injected `Authorization` |
| `tinycdi_session_frame_reloads_total` | counter | `dest` | session-frame document loads **re-navigating** a session whose lease already had a stream — the portal connection watch's lease-active reload fallback and user reloads/new-tab loads, same tab only. Two exclusions keep it honest: the KasmVNC client's in-frame websocket retry (`reconnect=true`) is a bare `/websockify` upgrade and never produces a document load, so it is never counted; and a load whose embedded claiming tab id (`path=websockify?tcdi_tab=<id>`) differs from the lease's recorded stream owner is a second tab taking the session over via the shared cookie — a takeover, not a reload — and is skipped (loads with no comparable id — absent/malformed tab id or no recorded owner — still count). `dest=iframe` is the portal's embedded frame, `document` a top-level load, `other` covers clients without fetch metadata. Expected: `0` at steady state; up to 1 per session per backend rollout (a few per session when a rollout's reconnect needs several watch attempts — `RECONNECT_BACKOFF_MS` allows up to 3). Sustained growth with no rollout means in-frame websocket retries are failing and the SPA is reloading the frame |
| `tinycdi_logins_total` | counter | `outcome` | completed `/v1/auth/callback` attempts |
| `tinycdi_runtime_image_age_seconds` | gauge | `family` | age of each catalog family's newest published image (refreshed every minute) |
| `tinycdi_rate_limited_total` | counter | `route` | requests refused by the limiters — a rising series means `429` responses; check `backend.trustedProxies` is set before blaming clients. `route` is a bounded route/family label: the app login family, `/v1/launch`, `session_lookup` (unseen-cookie session-directory lookups — local per-replica bound, `-session-lookup-rate`), and `upstream_streams` (the per-workspace concurrent upstream bound, `-session-stream-cap` — a refusal means `429 too_many_streams` + `Retry-After: 1`, retryable) |
| `tinycdi_rate_limit_store_errors_total` | counter | `route` | Postgres rate-limit window check failures (ADR 0006) — real store errors only: each check runs under a 500 ms deadline and a failure opens a 10 s circuit-breaker cool-down in which requests skip the store entirely, so a sustained outage shows ~one increment per 10 s per limiter, not one per request; `route` is the limiter family (`login`, `callback_ceiling`, `launch`) |
| `tinycdi_rate_limit_store_degraded` | gauge | `route` | rate-limit store circuit-breaker state — `1` while open (Postgres checks skipped; each pod's divided local bucket enforces the bound — the limited routes need Postgres to complete anyway) and `0` while closed; `route` is the limiter family (`login`, `callback_ceiling`, `launch`) |
| `tinycdi_internal_auth_failures_total` | counter | `reason` | mTLS identity rejections on the internal broker listener (`reason` is `no_cert`, `no_cn` or `spiffe_mismatch`) — the complete record of refused attempts; the matching warn line is rate-limited per reason. A rising series means bad or absent client certs are hitting :9443 — check internal cert issuance/rotation or a probing client |
| `tinycdi_retention_rows_pruned_total` | counter | `table` | terminal `connection_lease`/`launch_ticket` rows and expired `audit_event`/`workspace_boot` rows deleted by the leader's retention sweeps (`table` is `connection_lease`, `launch_ticket`, `audit_event` or `workspace_boot`). Flat zero is normal on quiet installs — pair with the last-success gauge before alarming |
| `tinycdi_retention_last_success_timestamp_seconds` | gauge | — | unix timestamp of the last completed terminal-row retention sweep. Absent until the first pass finishes on this leader; stale > ~2 h means the sweep is not running and the session tables resume growing without bound |
| `tinycdi_internal_mtls_revoked_serial_total` | counter | `listener` | internal-mTLS handshakes aborted because the verified peer leaf's (issuer, serial) is on the ADR 0008 revocation list — the complete record of refusals; the `internal_mtls_revoked_cert` WARN line (rate-limited) carries peer/serial/issuer. Nonzero means a revoked cert is still being presented — see `runbooks/mtls-cert-revocation.md` |
| `tinycdi_internal_mtls_revocation_entries` | gauge | — | entries in the currently loaded internal-mTLS revocation list (0 when the feature is off) |
| `tinycdi_internal_mtls_revocation_reload_errors_total` | counter | — | revocation-list reload failures — the last-good list kept serving; a rising series means the mounted file is unreadable/invalid and new revocations are NOT landing |


## Dashboards

```yaml
dashboards:
  enabled: true                 # requires backend.metrics.enabled
  labels: {grafana_dashboard: "1"}   # sidecar discovery labels
```

One ConfigMap per file in `deploy/helm/tinycdi/files/dashboards/` renders
in the release namespace — currently `tinycdi-overview` (request, session
and login health of the backend) and `tinycdi-capacity` (workspace load,
provisioning, leases and runtime-image freshness). The kube-prometheus-stack Grafana sidecar
picks them up automatically when the label matches (`grafana_dashboard:
"1"` is its default); set `dashboards.labels` to your sidecar's discovery
label/value otherwise. Importing the JSON files by hand into an existing
Grafana works too — the dashboards templating only asks for a Prometheus
datasource.

### Admin overview

The portal has its own health view — **Admin → Overview** (`/admin`),
backed by `GET /v1/admin/tenants/{tenant}/overview` (tenant admins
only). It shows counts only, no identities:

- live desktop sessions (active, unexpired leases);
- Ready workspaces per template;
- provisioning p95 over the last 24 hours, from the `workspace_boot`
  ledger (ledger rows are kept 7 days);
- workspaces stuck deleting for more than 10 minutes — see
  [stuck-finalizer.md](stuck-finalizer.md);
- workspace phase counts.

The answer is cached for 15 seconds per tenant. `fresh: false` in the
response means the backend's Kubernetes watch is stale and the
Kubernetes-derived counts are empty. Reading the overview is itself
audited (`admin.overview.get`).

## Alerts

```yaml
alerts:
  enabled: true                 # requires backend.metrics.enabled +
  labels: {release: prometheus} #   the Prometheus Operator CRDs
  sessionDropFloor: 5           # TinyCDISessionDropSpike fires only if at
                              # least this many sessions were live 5m ago;
                              # 0 disables the floor
```

Renders one `PrometheusRule` named `tinycdi` (group `tinycdi.platform`).
`alerts.labels` must match the operator's `ruleSelector` — the same
convention as `serviceMonitor.labels`. The ratio alerts
(`TinyCDISessionDropSpike`, `TinyCDIRehydrationFailures`,
`TinyCDILoginFailuresHigh`) put the measured ratio first in their
expression, so the alert description's `$value` is the real drop / miss /
failure percentage. Each rule below states what it means and the first
thing to check.

| Alert | Severity | Fires when | First thing to check |
|---|---|---|---|
| `TinyCDIBackendReplicaDown` | warning | fewer backend replicas report metrics than `backend.replicas` for 15m | `kubectl -n <release-ns> get pods -l app.kubernetes.io/name=backend` — a replica is down, Pending, or its metrics listener is unreachable (NetworkPolicy) |
| `TinyCDISessionDropSpike` | critical | >50 % of live sessions vanish inside 5m **and** at least `alerts.sessionDropFloor` (default 5) were live — a small install draining at night (2→0) stays quiet; `0` disables the floor | backend restarts/rollouts (`kubectl rollout history`), then Postgres health — a failover past the 30 s revoke deadline drops every stream by design (see below) |
| `TinyCDILeaseRenewFailures` | warning | lease failures sustain ≈ >1 per 50 s for 15m | the `reason` label on `tinycdi_lease_failures_total`; gateway↔broker connectivity and fencing |
| `TinyCDIRehydrationFailures` | warning | >20 % of session rehydrations return miss/error for 15m | backend pod churn — sessions should survive a restart; repeated misses mean the shared session directory is being lost |
| `TinyCDIRuntimeImageStale` | warning | a catalog family's newest revision is older than `-image-stale-after` (14 d default) for 1h | the runtime-image publish train (`.github/workflows/runtime-images.yml`) — a fresh `rt-*` tag clears it. Threshold mirrors the flag default; if you overrode it via `backend.extraArgs`, edit the rendered rule |
| `TinyCDILoginFailuresHigh` | warning | >50 % of completed logins are denied/error for 15m | IdP health and `oidc.requiredGroups`/tenant membership first; check whether the `outcome` mix looks like brute force |

## Reading the signals during a Postgres outage

Fail-closed is intentional, not a bug — measured in an outage drill
(`tests/integration/postgres_outage_test.go`): a 10 s outage kept the
session WebSocket open and echoing; a 45 s outage closed it **30.1 s**
after the outage began (`renew_deadline`, inside the 30–40 s window);
`/v1/*` answered `503 UNAVAILABLE` in ~1 ms throughout, never a hang.
With the lease row expired, reconnect takes the re-launch path. Full
table and recovery procedure: `disaster-recovery.md` → "Postgres outage
(failover)"; the same numbers in `capacity.md` → "Postgres outage /
failover timing". Expect `TinyCDISessionDropSpike` exactly then — a
failover past the 30 s revoke deadline drops every stream by design.

## Probes

For load-balancer and monitoring wiring: backend `/readyz` answers 200
only once the replica is serving **and** the Workspace informer is synced;
`/healthz` is plain liveness. Both answer on the app listener (`:8443`),
and `/healthz` also on the session listener's in-cluster control surface.
The frontend serves `/healthz` on its HTTPS port; the operator serves
`/healthz` + `/readyz` on its probe port.

## Audit events

Security-significant actions are emitted as a dedicated **audit event
stream**: one JSON object per line on the component's stdout
(`observability.JSONSink`), separate from the per-request `http_request`
structured-log record that covers every HTTP call. Ship the backend and
gateway container logs to your log backend and the audit events arrive
inline; they are distinguishable by the `action` field naming a domain
action rather than `http.request`. Audited API actions are also stored in
Postgres and readable in the portal; see [audit.md](audit.md) for what is
stored and for how long.

Every record carries:

| Field | Content |
|---|---|
| `time` | UTC emission timestamp |
| `actor` | pseudonymous principal ref — `oidc:` + truncated SHA-256 of `issuer\|subject`, stable per user; `"anonymous"` when unauthenticated; `"gateway:<id>"` for gateway-side events; `"config:tenant-quotas"` for the config-driven quota apply |
| `action` | the domain action (table below) |
| `targetUid` | the acted-on id — workspace `ws_*`, retained record `rd_*` or tenant id; create actions record the id the call just made (`workspace.create` fills the new `ws_*` once known) |
| `tenant` | the verified tenant of the caller |
| `requestId` | the request's `X-Request-Id` correlation id (`"startup"` for the config apply) |
| `outcome` | `success`, `failure` or `denied` (401/403) |
| `errorCode` | the stable API error code on refusals (e.g. `CSRF_FAILED`, `QUOTA_MANAGED_BY_CONFIG`) |
| `details` | non-secret context — attempted quota limits, `role=tenant-admin` when a shared route was used with the elevated role, `takeover` on ticket issue, `retained_data` on attach-creates, `workspace` on data attach, `leases_revoked` on sign-out |

Emitted events:

| `action` | Source | Covers |
|---|---|---|
| `admin.quota.get` / `admin.quota.set` | `GET`/`PUT /v1/admin/tenants/{t}/quota` | tenant-admin quota reads/writes — including denied attempts, `If-Match` refusals and `QUOTA_MANAGED_BY_CONFIG` rejections; `details` carry the attempted limits |
| `admin.user_limit.get` / `.set` / `.clear` / `.default.get` / `.default.set` / `.default.clear` | `GET`/`PUT`/`DELETE /v1/admin/tenants/{t}/user-limits` and `GET`/`PUT .../default` | tenant-admin per-principal limit reads/writes — including denied attempts (denied pre-decode writes record the `.set` attempt action); `details` carry the pseudonymized `owner` ref and attempted `max_running` |
| `admin.session_policy.get` / `.set` / `.clear` | `GET`/`PUT /v1/admin/tenants/{t}/session-policy` | session-ceiling reads/writes — an all-`FollowTemplate` body records `.clear` |
| `admin.session_policy.apply` | `POST /v1/admin/tenants/{t}/session-policy:apply` | apply-now runs, dry runs included — `details` carry `dryRun`, `includeActive`, `affected` and `restarted` |
| `admin.audit.get` | `GET /v1/admin/tenants/{t}/audit` | reads of the stored audit log |
| `admin.user.list` / `admin.user.get` | `GET /v1/admin/tenants/{t}/users` and `GET .../users/{ownerRef}` | user-directory reads |
| `admin.session.list` | `GET /v1/admin/tenants/{t}/sessions` | the tenant's live-lease listing |
| `admin.session.revoke` | `POST /v1/admin/tenants/{t}/users/{ownerRef}/sessions:revoke-all` | per-user sign-out — portal sessions, leases and launch tickets revoked in one transaction |
| `admin.overview.get` | `GET /v1/admin/tenants/{t}/overview` | reads of the tenant overview |
| `admin.quota.config_apply` | `-tenant-quotas` startup singleton | the platform-level quota write performed from configuration (leader replica only); a failed pass records `failure`/`apply_failed` with `changed: "0"` (the transaction rolls back) |
| `workspace.create` / `.start` / `.stop` / `.delete` | `/v1/workspaces` mutations | every lifecycle write, owner- and admin-scoped alike — `role=tenant-admin` marks elevated use |
| `data.attach` / `data.purge` | `/v1/data/{id}/attach`/`/purge` | retained-disk claims and destructive purges, incl. admin action on other owners' records |
| `file.transfer.upload` / `file.transfer.download` | `POST /v1/workspaces/{id}/file-transfers`, then the broker's claim and completion writes | grant mint, claim and completion — `details` carry `phase`, `direction`, `path` and `declared`/`bytes`, never content |
| `session.revoke_all` | `POST /v1/me/sessions:revoke-all` | the caller's own sign-out-everywhere — `details` carry `leases_revoked` |
| `session.touch` | `POST /v1/session:touch` | the portal's activity beat — log stream only, not stored |
| `connection.create` | `POST /v1/workspaces/{id}/connections` | launch-ticket issue — the ticket itself is never recorded |
| `session.logout` | `POST /v1/logout` | the sign-out request |
| `session.revoke` | sign-out + `POST /v1/control/revoke` | revocation of session-bound lease material; bearer-denied control attempts record `denied`/`unauthorized` |
| `session.list` | `GET /v1/control/session` | the operator's replica-local session listing |
| `session.host_mismatch` | gateway | a session cookie presented on the wrong workspace host |
| `session.lookup` | gateway session-listener cookie gate | unseen-cookie lookups refused by the lookup limiter (`denied`/`rate_limited`) |
| `session.stream` | gateway session-listener stream cap | upstream requests over `-session-stream-cap` (`denied`/`too_many_streams`) |
| `launch.redeem` / `launch.host_mismatch` | gateway `/v1/launch` | ticket redemption and per-workspace host binding denials |

Requests refused before authentication (no session → `401` from
`RequireAuth`) emit no dedicated event by design — the wrapper sits inside
the authenticator, so those refusals are covered by the per-request
`http_request` log record that every HTTP call produces.

Redaction is enforced in the sink, not by convention: detail keys matching
credentials (`token`, `cookie`, `session`, `authorization`, …) are replaced
with `[REDACTED]` on write, request/response headers, bodies and query
strings are never logged, and raw OIDC subjects/emails never reach the
record — only the pseudonymous ref (an unsalted hash: anyone who knows the
issuer and subject can compute it).

### Log integrity — what TinyCDI guarantees vs what you must provide

TinyCDI guarantees **emission and shape**: every audited action above
produces exactly one record with a verified (not caller-supplied) actor,
a per-process serialized write, and redaction of credential material — a
handler panic still emits its record (`outcome` `failure`, `errorCode`
`panic`). A failing stdout write never fails the request; the loss is
signalled by the `tinycdi_audit_write_errors_total{event}` counter and a
rate-limited `audit sink write failed` warning — alert on it, since a
silent gap is worse than a loud one. A failing audit-store insert on an
audited route does fail it (`500 INTERNAL`, see
[audit.md](audit.md#when-the-store-is-unavailable)).
For the log stream it does **not** guarantee delivery or tamper evidence —
it is stdout JSONL, so it inherits whatever integrity your log pipeline
gives it. For a trustworthy trail the operator must provide:

- **collection of every replica's stdout** (backend and gateway alike)
  with retention matching your compliance horizon — audit completeness
  ends where collection ends;
- **integrity protection downstream** (WORM/object-lock storage or a
  signed/log-hashed pipeline) — TinyCDI does not sign records;
- **synchronized clocks** so cross-replica ordering by `time` +
  `requestId` is meaningful;
- **Kubernetes audit logging** for platform-level admin actions that
  never pass through TinyCDI: `WorkspaceTemplate` CRD writes (the
  template/catalog ops), namespace and Helm-value changes
  (`-tenant-quotas`, tenant map). Those are recorded by the Kubernetes
  API server's audit log, not by TinyCDI.
