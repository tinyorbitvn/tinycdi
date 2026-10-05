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
peers in `networkPolicy.prometheusPeers` reach it (SEC-33).

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
| `tinycdi_session_frame_reloads_total` | counter | `dest` | session-frame document loads **re-navigating** a session whose lease already had a stream — the portal connection watch's lease-active reload (FX-R32 fallback) and user reloads/new-tab loads, same tab only. Two exclusions keep it honest: the KasmVNC client's in-frame websocket retry (FX-R32, `reconnect=true`) is a bare `/websockify` upgrade and never produces a document load, so it is never counted; and a load whose embedded claiming tab id (`path=websockify?tcdi_tab=<id>`) differs from the lease's recorded stream owner is a second tab taking the session over via the shared cookie — a takeover, not a reload — and is skipped (loads with no comparable id — absent/malformed tab id or no recorded owner — still count). `dest=iframe` is the portal's embedded frame, `document` a top-level load, `other` covers clients without fetch metadata. Expected: `0` at steady state; up to 1 per session per backend rollout (a few per session when a rollout's reconnect needs several watch attempts — `RECONNECT_BACKOFF_MS` allows up to 3). Sustained growth with no rollout means in-frame websocket retries are failing and the SPA is reloading the frame |
| `tinycdi_logins_total` | counter | `outcome` | completed `/v1/auth/callback` attempts |
| `tinycdi_runtime_image_age_seconds` | gauge | `family` | age of each catalog family's newest published image (refreshed every minute) |
| `tinycdi_rate_limited_total` | counter | `route` | requests refused by the per-client rate limiters (E7) — a rising series means `429 RATE_LIMITED` responses; check `backend.trustedProxies` is set before blaming clients |
| `tinycdi_rate_limit_store_errors_total` | counter | `route` | Postgres rate-limit window check failures (ADR 0006) — real store errors only: each check runs under a 500 ms deadline and a failure opens a 10 s circuit-breaker cool-down in which requests skip the store entirely, so a sustained outage shows ~one increment per 10 s per limiter, not one per request; `route` is the limiter family (`login`, `callback_ceiling`, `launch`) |
| `tinycdi_rate_limit_store_degraded` | gauge | `route` | rate-limit store circuit-breaker state — `1` while open (Postgres checks skipped; each pod's divided local bucket enforces the bound — the limited routes need Postgres to complete anyway) and `0` while closed; `route` is the limiter family (`login`, `callback_ceiling`, `launch`) |

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

Fail-closed is intentional, not a bug — measured in the v0.3 outage drill
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
