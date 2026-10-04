# Runbook — Capacity planning and tenant quota

Design: `docs/architecture.md` §8 (quota). Covers how much a session costs,
how tenant quota is enforced, what to watch, and how to read the measured
load numbers. The measured numbers (a 20-concurrent-session load run,
2026-09-30) are in the last section; contract/sizing figures are marked
as such.

## Cost model

A workspace consumes resources at two layers:

1. **Runtime pod** — sized by the template's `ResourceProfile`
   (`cpu`, `memory`, `storage` in `api/v1alpha1/workspacetemplate_types.go`),
   applied as pod requests/limits plus a PVC of `storage` size when the
   lifecycle `dataPolicy` keeps data. The dev templates used
   500 mCPU / 1 GiB / 1 GiB per session on the old openbox desktop; the
   XFCE desktop profile (`tcdi/linux-desktop`, V3.26) is
   1 CPU / 2 GiB / 5 GiB
   (`deploy/helm/tinycdi/ci/example-values.yaml`). Measured on that image: a
   fresh session idles at 14 mCPU / 98 MiB; with a terminal, Thunar and
   Firefox (three tabs) open it settles at 20–40 mCPU / 555 MiB (peak
   888 MiB incl. page cache) after a ~2-CPU, ~40 s Firefox start-up burst;
   scripted mouse+keyboard input alone (the soak driver) is p95 52 mCPU,
   max 112 MiB. Light terminal/file use fits 500 mCPU / 1 GiB; the **soak
   profile** — sessions driven with synthetic input, no browser launched — is
   its own number: **250 mCPU / 512 MiB / 1 GiB** (4× the measured memory and
   p95 CPU). `docs/images.md` has the full before/after budgets.
2. **Control-plane share** — backend (API, session gateway and broker in
   one process), frontend, operator and Postgres. Chart default *requests*
   (`deploy/helm/tinycdi/values.yaml`): backend 50 mCPU / 64 MiB × 2
   replicas, frontend 20 mCPU / 32 MiB × 2, operator 50 mCPU / 64 MiB × 1 —
   ≈190 mCPU / 256 MiB in total, before Postgres. The backend's session
   listener carries the steady-state cost of live sessions (TLS + WebSocket
   relay); API and operator cost is concentrated at provisioning and
   teardown bursts.
   **Measured at 20 live streams on the v0.1 topology** (separate api,
   gateway and portal Deployments; not re-measured on v0.2): gateway
   peaked at 60 mCPU / 18 MiB, api 49 mCPU / 25 MiB, operator
   31 mCPU / 25 MiB, portal 24 mCPU / 10 MiB, Postgres 42 mCPU / 88 MiB —
   the whole control plane stayed under ~200 mCPU aggregate, i.e. noise at
   this scale (see §The 20-session load gate).
3. **Network** — KasmVNC WebSocket streaming; dominated by screen-change
   rate and client bandwidth, not by the platform. The load gate measures
   gateway throughput directly.

Cold-start cost is dominated by **image pull**, not CPU: `tcdi/linux-desktop`
≈ 1.25 GB (302 MB compressed; the old openbox image was ≈ 0.83 GB) and
`tcdi/browser` ≈ 1.86 GB on disk (uncompressed; the `tcdi/linux-base` layers
they share are pulled once per node). Keep runtime
images resident on the worker nodes that may schedule workspace pods, or
accept the pull time inside `bootDeadline`.

## Tenant quota — the hard gate

Quota is enforced in Postgres, not by cluster capacity:

- `tenant_quota` holds per-tenant limits (`max_running_slots`,
  `max_cpu_millis`, `max_memory_bytes`, `max_disk_bytes` —
  `internal/store/migrations/001_control_plane.sql`).
- `Reserve()` takes a `FOR UPDATE` lock on the tenant's `tenant_quota` row
  inside the create transaction (`internal/provisioning/quota.go`), so
  concurrent creates serialize and at most the configured limit succeeds —
  proven by `TestQuotaConcurrentRequests` (100 concurrent creates → ≤ limit
  reservations). Over-limit creates fail `409 QUOTA_EXHAUSTED` and create
  **no** workload.
- A workspace charges `running_slots: 1` plus the template's cpu/memory/disk
  vector (`internal/provisioning/k8sapplier.go`). Retain-policy disks keep
  their `disk_bytes` reservation `held` until the record reaches `Purged`
  — a retained disk still counts against disk quota
  (`docs/runbooks/retained-data.md`).
- Reservations release only after the teardown finalizer proves the runtime
  is actually gone — quota is never released early
  (`internal/provisioning/recovery.go`).

**Quota model.** Running slots, CPU and memory are held exactly while a
runtime incarnation exists (from the start intent to the proven absence of
the pod); disk is held while the volume exists, whatever the workspace
state.

### Setting quota

Declare quota in the chart: `managedNamespaces[].quota` (`runningWorkspaces`,
`cpu`, `memory`, `storage`) — the backend leader upserts those tenants' rows
at startup (`docs/runbooks/install.md` → "Tenant quotas"). There is **no
admin API** for quota; for a tenant the chart does not declare, set the row
with SQL against the platform DB. Do not hand-edit a tenant that has a
`quota` block in the chart: the declared values overwrite it at the next
backend start.

```sql
INSERT INTO tenant_quota
  (tenant_id, max_running_slots, max_cpu_millis, max_memory_bytes, max_disk_bytes)
VALUES ('tenant-a', 10, 20000, 42949672960, 1099511627776)   -- 10 slots, 20 CPU, 40 GiB, 1 TiB
ON CONFLICT (tenant_id) DO UPDATE SET
  max_running_slots = EXCLUDED.max_running_slots,
  max_cpu_millis    = EXCLUDED.max_cpu_millis,
  max_memory_bytes  = EXCLUDED.max_memory_bytes,
  max_disk_bytes    = EXCLUDED.max_disk_bytes;
```

A tenant with no `tenant_quota` row gets `ErrNoQuota` — creates fail closed,
never open, and the API reports `409 QUOTA_NOT_CONFIGURED` (distinct from
`QUOTA_EXHAUSTED`, which means a configured limit has no headroom). Lowering a limit below current usage does not kill running
workspaces; it only blocks new reservations until held < limit.

### Stopped Retain workspaces and quota

The absence proof is "no pod for the workspace"; volumes never block it. When
a **Retain** workspace stops and its pod is gone, Recovery converts its
reservation in place into a **disk-only hold**: running slot, CPU and memory
are released, the home disk's `disk_bytes` stay held, and the admitted compute
vector is kept in `quota_reservation.restart_*` (migration 014). A start
re-acquires exactly that vector on the same row (and fails
`409 QUOTA_EXHAUSTED` if the compute no longer fits, leaving the disk hold
untouched). Existing stopped Retain rows convert on the next recovery pass
with no manual write. A stray second home volume labelled to the workspace
does not delay the release.

**Delete after stop.** The disk hold moves to the retained-data hold once:
the converted row is released and the retained dataset's real size is held
against it, so the admitted disk and the dataset are never counted together.
**Ephemeral** workspaces are unchanged: the whole reservation is released on
the absence proof. Check the hold with:

```sql
SELECT w.name, q.running_slots, q.cpu_millis, q.disk_bytes, q.restart_slots
FROM workspaces w JOIN quota_reservation q ON q.workspace_id = w.id
WHERE w.desired_state = 'Stopped' AND w.data_policy = 'Retain' AND q.state = 'held';
-- a converted row shows running_slots = 0 and restart_slots >= 0
```

## Cluster sizing

Size compute workers for the session profile; the worked examples below
use 16 CPU / 64 GiB workers (the tested environment is described in
`docs/compatibility.md`). Sizing rules:

- **Schedulable sessions per worker** ≈ worker allocatable CPU ÷ template
  cpu, and memory likewise — take the tighter of the two, then subtract
  daemonset/system overhead. At 1 CPU / 2 GiB per session a 16C/64GiB
  worker fits ~14 sessions on CPU and ~30 on memory → **CPU-bound**.
- **Browser workspaces must pin to profiled nodes.** Chromium sessions need
  the Localhost seccomp + AppArmor pair
  (`docs/compatibility.md`, "Node security profile"). Pin browser
  sessions to profiled workers only; before widening
  browser capacity, roll the profile pair to every candidate worker —
  unprofiled nodes *silently degrade* the sandbox rather than fail.
- **Headroom:** keep one compute worker's worth of capacity free if you
  promise sessions survive a node loss — there is no workspace HA; a node
  loss kills its runtime pods and users must reconnect (new incarnation,
  new ticket). Reserve quota accordingly.

## Sign-in rate limits and NAT

The backend throttles its unauthenticated surface **per client IP**:
`-login-rate` (30/min, burst 10) covers `GET /v1/login`,
`GET /v1/auth/callback` and `GET /v1/session`; `-launch-rate`
(60/min, burst 20) covers `POST /v1/launch`. Authenticated requests get
their own keys instead — `GET /v1/session` keys on a digest of the
*validated* session cookie, `/v1/launch` likewise once the session is
live on the serving replica (a cookie issued by a sibling and not yet
rehydrated keys by IP — the limiter never spends a directory lookup), and
`/v1/auth/callback` keys on its *validated* OIDC state. Anonymous traffic
— login starts on `/v1/login`, unauthenticated probes, forged or expired
cookies, unvalidated states — always keys on the client IP, so a whole
office behind one NAT address keeps per-user budgets everywhere except
the anonymous sign-in start itself.

One bound to know: a validated state is *mintable* — it costs its holder
one IP-limited `/v1/login` — so callback keys are additionally gated by a
per-IP ceiling at **10× the login limits** (300/min, burst 100 by
default). A spray of minted states cannot amplify callback throughput
past that multiplier, and a NAT'd org needs >100 concurrent OIDC
callbacks from one address before the ceiling even engages.

What that means for sizing:

- **Sign-in bursts share the IP budget only until the cookie exists.** In
  a 9:00 rush every user's *first* `GET /v1/login` is anonymous, so N
  users behind one NAT who all click sign-in inside a minute need
  `-login-rate` ≥ N **plus** headroom for the anonymous
  `GET /v1/session` probes signed-out tabs poll (`-login-rate=0` disables
  the limit entirely). Once the session exists, probes run on the
  per-session budget, and each OIDC callback on its own state (within the
  10× ceiling) — the sustained rates need no NAT multiplier.
- **Launches:** `POST /v1/launch` re-launches carrying a session cookie
  that is live on the serving replica are per-session; a *first* launch
  (no cookie yet, or a cookie only a sibling replica knows) is per-IP —
  20 users' simultaneous first connects need `-launch-rate` ≥ 20/min.
- **Budgets multiply by replica.** The limiter is in-memory per backend
  replica (`internal/ratelimit`) — no shared counter — so one key's
  effective budget is up to ~`backend.replicas` × the flag on an even
  spread: 2 replicas × `-login-rate` 30/min + burst 10 ≈ 80/min
  aggregate for one anonymous IP. Size the flag as aggregate-need ÷
  replicas; a same-IP ramp trips N buckets, not one.
- The per-key limits and the limiter's key-space bound are unchanged;
  `backend.trustedProxies` must still name the edge's CIDRs or every user
  collapses into the edge's own IP bucket regardless.

Set the flags through `backend.extraArgs`
(`deploy/helm/tinycdi/README.md`, "Rate limits and trusted proxies").

## What to watch (metrics)

`internal/observability/metrics.go` defines the `tinycdi_*` series
(bounded labels only — no UIDs/emails; `route` is always a mux template,
never a concrete path). Where they are served: the **backend** registers
the full set and serves `/metrics` on its dedicated metrics listener when
`backend.metrics.enabled` is set (port `backend.metrics.port`, reached
through the ClusterIP `backend-metrics` Service and never the public
ports — `/metrics` answers 404 on the app and session listeners). All
three serving listeners are instrumented: the app mux
(`listener="app"`), the session gateway (`listener="session"`) and the
internal mTLS broker API (`listener="internal"`). The **operator**
exposes no metrics endpoint (the chart pins `--metrics-bind-address=0`).

| Signal | Metric | Alert when |
|---|---|---|
| Provisioning latency | `tinycdi_workspace_provisioning_seconds` (histogram by `result`) | p95 approaches `bootDeadline` |
| Load | `tinycdi_workspaces_running{tenant}`, `tinycdi_workspaces_reserved{tenant}` | reserved ≈ tenant limit (saturation) |
| Live sessions | `tinycdi_sessions_active` (per replica) | drops while streams stay open |
| Lease health | `tinycdi_lease_failures_total{reason}` | any sustained increase — gateway↔broker or fencing issue |
| Rehydration | `tinycdi_gateway_rehydrations_total{result}` | `miss`/`error` growth — restarts losing sessions or directory trouble |
| Stream fencing | `tinycdi_gateway_streams_fenced_total` | spikes — replicas fighting over one lease's stream |
| Logins | `tinycdi_logins_total{outcome}` | `denied`/`error` growth — IdP or gate misconfiguration, brute force |
| Image age | `tinycdi_runtime_image_age_seconds{family}` | approaches `-image-stale-after` (default 14 d) — publish train overdue |
| Rate limits | `tinycdi_rate_limited_total{route}` | sustained refusals — attack or too-tight limits |
| Stuck teardown | `tinycdi_finalizers_stuck` | > 0 — see `docs/runbooks/stuck-finalizer.md` |
| Quota drift | `tinycdi_quota_drift{tenant}` | ≠ 0 — reservation/actual disagreement; run recovery reconcile |
| PVC leaks | `tinycdi_pvc_leaks` | > 0 — orphaned volumes cost disk quota/$$ |
| Boot failures | `tinycdi_boot_deadline_exceeded_total` | rising — image pull or scheduling trouble |
| API health | `tinycdi_http_requests_total` / `tinycdi_http_request_duration_seconds` (by `listener`) | 5xx-class growth, p95 on `/v1/workspaces` |

Ready-made Grafana dashboards and a `PrometheusRule` shipping the alert
set above ship inside the chart, both off by default: set
`dashboards.enabled` and `alerts.enabled` (each requires
`backend.metrics.enabled`); see the chart README "Dashboards and
alerts".

Saturation symptoms to expect, in order: `QUOTA_EXHAUSTED` 409s (quota gate
— by design), pod `Pending` on cpu/memory (cluster gate), image-pull
latency in `workspace_provisioning_seconds` (cold node), then session-listener CPU
saturation (scale `backend.replicas`; the default two replicas still enforce
single-writer-per-workspace via broker fencing).

## Postgres outage / failover timing — measured

Dependency-outage behaviour measured in the v0.3 drill
(`tests/integration/postgres_outage_test.go`; Postgres container
stop/start, production `-revoke-deadline` 30 s, fast renew cadence):

| Event | Measured | Budget |
|---|---|---|
| Short outage (10 s) — stream survival | WebSocket open + echoing during and after outage | survives while < revoke deadline |
| Long outage (45 s) — stream close | **30.1 s** after outage start (30.4 s after last counted renew; `renew_deadline`) | fail-closed at 30 s; must land 30–40 s |
| `GET /v1/workspaces` during outage | **503 `UNAVAILABLE` in ~1 ms** | ≤ 5 s, never a hang |
| Reconnect after 45 s outage | lease expired → new ticket + relaunch | same cookie only while lease row is active |

A database failover longer than the 30 s revoke deadline intentionally
drops all streams — budget it into DB maintenance windows.

## The 20-session load gate — measured

> Measured on the v0.1 topology (separate api, gateway and portal
> Deployments); component names below are v0.1's. The v0.2 soak harness is
> `tests/soak`.

A load run executed 20 concurrent sessions for 60 minutes
(2026-09-30): **14 Linux desktop** (test oracle template —
clock xterm for continuous screen change + typed-marker readback) + **6
browser** (pinned to a single profiled worker) + **2 churn** workers cycling
create/stop→start/delete. Users spread across the OIDC issuer's
tenant-a (peak 15 held) and tenant-b (peak 7 held); the seeded 10-slot
quota was raised to 20/tenant for the run.

### Measured results

| Metric | n | p50 | p95 | Contract budget |
|---|---|---|---|---|
| Create → Ready (incl. churn) | 86 | 9.6 s | 9.7 s | `bootDeadline` 5 m |
| Connect: launch → first frame | 20 | 1.0 s | 1.5 s | ticket TTL 60 s |
| Reconnect (takeover) → first frame | 114 | 0.6 s | 0.7 s | — |
| Input round-trip (typed marker → readback) | 1637 | 263 ms | 303 ms | — |
| Delete → teardown complete (CR+pod gone) | 20 | 11.3 s | 12.2 s | drain ≤ 45 s |
| Churn stop → start → Ready | 33 | 15.9 s | 16.2 s | — |
| Churn create → delete gone | 66 | 3.3 s | 3.4 s | — |
| Observed-state freshness (CR Ready → API view) | 1 probe | ~13 ms | — | ≤ 15 s |

**Errors: zero** — no session errors, no unexpected disconnects, no
reconnect errors, no input read-back timeouts, no delete failures across
the full window (~1600 input probes, 114 reconnects, ~66 churn cycles).
Connect modes: 20 first-connects via the portal UI button, 114 reconnects
via `POST /connections` takeover + the identical form-POST launch.

**Saturation:** max `connection_lease` active = 20, max
`workspace_activity.open_streams` = 20 (one live stream per lease — the
single-writer fence held). Gateway peak **60 mCPU / 18 MiB**; api 49 m /
25 MiB; operator 31 m / 25 MiB; portal 24 m / 10 MiB; Postgres 42 m /
88 MiB. Runtime pods (n=84 samples × 30 s): CPU p50 59 m, p95 274 m,
max 500 m (limit); memory p50 36 MiB, p95 245 MiB, max 250 MiB — a
session's steady footprint is far below its 500 mCPU / 1 GiB request.
Node peaks: the browser-pinned worker reached 26 % CPU / 17 % mem
(hosted 6 browser + 5 desktop pods); the other workers stayed at
21–22 % — no node approached the 85 %
pressure guard. Post-run: all 107 quota reservations `released`, zero
workspace pods/CRs left in either tenant namespace.

**Observed bottleneck: none at 20 sessions.** The uniform ~9.6 s
create→Ready (images pre-pulled on workers) is pod scheduling+start cost,
not contention — control plane stayed under ~200 mCPU aggregate and node
CPU under 26 %. The first ceilings to hit on the way up are (1) the
seeded tenant quota (10 slots — administratively gated, working as
designed), (2) browser-session density on the profiled node (all
browser pods share one profiled node), and (3) the single gateway replica's CPU on
the TLS+WS path at higher stream counts. The 50-session probe is
exploratory only — it finds the bottleneck, it is **not** a release gate
in the plan and was not run.

## Soak at scale on the dev cluster (E9)

Sizing math for the `soak-small` profile (250 mCPU / 512 MiB / 1 GiB
ephemeral, `requests = limits`, one `desktop` container per pod,
`dataPolicy: Ephemeral` so no PVC per session). On the e2e install the
workspace pods are pinned to `workload-type: infra` — the three infra
workers — so capacity is their allocatable minus what is already
requested, not the cluster total.

Measured node state (2026-10-04, idle soak namespaces):

| Node | CPU allocatable | CPU requested | CPU free | Mem free | Pod slots free |
|---|---|---|---|---|---|
| worker-01 | 16 | 7.58 (47 %) | 8.42 | ~43.5 GiB | 33 |
| worker-02 | 16 | 0.91 (6 %) | 15.09 | ~61.2 GiB | 89 |
| worker-03 | 16 | 8.85 (55 %) | 7.15 | ~40.4 GiB | 34 |
| **total** | **48** | **17.34** | **~30.7** | **~145 GiB** | **156** |

Per session the binding resource is **CPU requests**: floor(8.42/0.25) +
floor(15.09/0.25) + floor(7.15/0.25) = 33 + 60 + 28 = **121 pods is the
schedulable ceiling** on the three infra nodes as they stand. Memory
(512 MiB each) and pod count are not the limiter at this scale; ephemeral
storage is ~1 GiB per pod against ~91 GiB allocatable per node.

The advisor's operating rule for shared infra is a **≥ 20 % CPU-request
headroom per infra node** (keep live, ArgoCD and monitoring unstarved):
soakable requests per node = free − 0.20 × 16. At the measured state that
is floor(5.22/0.25) + floor(11.89/0.25) + floor(3.95/0.25) =
20 + 47 + 15 = **~82 sessions** — recompute against the live
`Allocated resources` at run time; it moves with whatever else lands on
the infra pool.

- **~82 sessions** fit inside the 20 % rule (the raw schedulable ceiling
  is ~121 — do not push to it).
- **200 sessions** do not fit: 50 CPU of requests needed against ~30.7
  free, ~21 soakable inside the rule. **Capacity finding:** running 200
  × 250 mCPU sessions needs ~50 CPU of spare requests on the workload
  pool → more or larger `workload-type: infra` nodes (or a smaller
  template request). The limiter is infra-node CPU *requests*, not
  usage — the measured steady-state draw is ~36 mCPU / ~240 MiB per soak
  pod (25-run average), so requests run out, not load.

The other two gates to open before a scale run:

- **Tenant quota** is the hard admission gate (Postgres `tenant_quota`,
  enforced before any pod exists): the seeded e2e quota of 30 running
  slots / 16 CPU / 32 GiB refuses the 31st soak workspace. An ~82-session
  run needs ≥ 82 slots / ≥ 21 CPU / ≥ 41 GiB in the tenant row (stage
  100 slots / 26 CPU / 52 GiB / 110 GiB for headroom, restore after).
- **Harness host** drives one Chromium tab per session; 25 tabs were
  unremarkable, 100+ wants a host with several free GiB and is worth a
  `ps`-level watch during the ramp-up.

Measured baselines: 25 × 60 min on the e2e install (2026-10-03): connect
p50 5.5 s / p95 5.9 s, reload reconnect p95 5.0 s (poll-quantised), zero
dropped/manual actions, infra node CPU max 51 % during the 25-pod
start-up burst, ~20 % steady.

### v0.3.0-rc.2 soak: 60 sessions × 60 min (2026-10-03)

The planned ~82-session run was cut to a **60-session cap** for two
reasons found on the night:

- **The scheduler does not honour the per-node headroom plan.** The
  template carries only `nodeSelector: workload-type=infra` — no spread
  or anti-affinity — so pods land where scoring puts them. With even
  spread the worst node (worker-03, 55 % baseline) crosses the 20 %
  request-headroom rule at ~45 sessions; the observed run spread
  10/30/8 because the freest node scored best. A per-wave guard in the
  harness stops workspace creation when any infra node's CPU requests
  plus the next wave's projected share would reach 80 %; effective N is
  recorded in the report.
- **The per-client-IP rate limits cap a same-IP ramp.** 20 OIDC lanes
  behind one source IP tripped `login-rate 30/min` (covers `/v1/login`,
  `/v1/auth/callback`, `GET /v1/session`) and `launch-rate 60/min` —
  152 × 429 in ~15 min, logins failed, connect p95 hit 65 s. The e2e
  run raised both to 600/min as a recorded deviation (restored after);
  production ramps from many IPs are unaffected. On rc.3 this is fixed by
  FX-R30 (per-session/per-state limiter keys — see "Sign-in rate limits
  and NAT"): the same 20-lane ramp at default limits produces zero 429s.

Measured on 60 × 60 min, 20 users (3 per user), soak-small on rc.2:
connect p50 6.1 s / p95 7.4 s (opens waved 10 per 15 s), reload
reconnect p50 0 / p95 5.0 s (half of 60 seamless), inputDispatch p50
67 ms / p95 89 ms, dropped 0, manual actions 0, per-lane 429s 0.
Infra usage peaked at 59 % CPU / ~15.5 GiB workspace memory; node
requests stayed ≤ 75 %. Two backend disruption drills plus a session
cert rotation mid-run: the drill session reconnected in ≤ 4.5 s, but
2/60 sessions reported `none` for ~85 s after the rollout and took a
~120 s end-to-end stream gap to recover — the reconnect tail after a
backend pod loss is ~2 min, the one metric over its 60 s gate (the
suppressed re-launch is fixed by FX-R31, PR #76, in rc.3). 13/60
sessions logged a false "open in another tab" view during the restarts
(watch item, non-zero on rc.2; same FX-R31 fix).

Capacity answer for the release: **~60 sessions** is the safe level on
the three-node infra pool under the 20 % headroom rule with today's
baselines; ~82 is schedulable only if placement could be pinned per
node; **200 sessions need ~50 CPU of spare requests → more or larger
workload nodes.**

### v0.3.0-rc.3 soak: 60 sessions × 60 min (2026-10-04)

Same shape as the rc.2 run: 60 soak-small workspaces over 20 OIDC lanes
behind one host IP, waves of 10 / 15 s, the projected-request node guard
(never fired — placement 15/30/11), default rate limits this time.

- **NAT / limiter (FX-R30 verified).** 20 lanes logged in, probed and
  launched from one IP: 0 × 429 on authenticated traffic (per-lane
  counters 2, both anonymous-phase hits during the OIDC ramp, none after
  lanes held cookies). An anonymous burst on `GET /v1/session` from the
  same IP was capped: 24 × 200 then 56 × 429 in ~5 s (28 per backend pod
  in the logs — matches the client count exactly).
- **Connected metrics.** connect p50 6.1 s / p95 6.3 s, mid-run reload
  reconnect p50 0 / p95 5.0 s (near-seamless), inputDispatch p50 66 ms /
  p95 83 ms, dropped 0, falseElsewhere 0 (rc.2: 13), manual actions 0,
  longest gap < 60 s. inputTimeouts 3 (bounded sends that hit the 5 s
  cap during the drill churn; no starvation).
- **Lease contract.** All 61 live leases kept the same lease id across
  every drill; stream_epoch bumped in place (1→2–5); zero launch tickets
  minted on reconnect — the `el.src` re-navigation re-claims the live
  lease and only a gone lease mints a ticket.
- **Drills.** Pod delete ~5.8 s and rollout ~5.8 s reconnect on the drill
  session (same lease), session-TLS rotate with zero disconnects, and a
  4th mTLS drill (operator client cert + internal CA, 6 live sessions):
  streams survived, operator kept reconciling, backend hot-reloaded the
  new server cert and client CA without a restart (`tlsreload` log
  lines), the pre-rotation client cert is refused after the CA rotation.
  Postscript (found in the rc.4 cleanup): the drill verified the
  backend's hot-reload and the old client cert being refused — it did
  **not** cover the operator's server-trust path. The broker client
  read its server-CA bundle once, so after the CA rotation it could not
  verify the backend's new internal server cert and workspace deletes
  wedged in the runtime-cleanup finalizer for ~4 h until an operator
  restart. FX-R33 (#83) closes it — the broker CA bundle hot-reloads;
  the rc.5 CA-rotation re-drill is the evidence (finalizer progress
  without a restart).
- **Fleet reconnect tail — the one failed gate.** After the all-pods
  rollout, 54/60 sessions had a disconnect span: p50 ~20 s, tail ~45 s
  (advisor budget p95 ≤ 10 s / p100 ≤ 30 s — FAIL). Single-pod delete was
  fine (p50 ~5 s, max ~10 s). Mechanism: the page's
  `RECONNECT_BACKOFF_MS` re-navigates the iframe without checking whether
  the previous navigation is still attaching, so at fleet scale the early
  steps abort nearly-landed claims and only a later, longer step lands.
  Fixed by FX-R32 (`fix/inframe-reconnect`): in-frame reconnect, no
  navigation thrash. The first run of this soak also surfaced a harness
  flaw — one wedged Playwright call stalled the serialized input loop,
  the 30 min session-idle mass-expired the fleet and the drop>1% abort
  fired correctly; the harness now bounds each input dispatch
  (`--input-timeout-ms`) and spreads the mid-run reload one per pass.
- **Abort rules.** dropped>1%, connect-p95>15 s, live-pod/ArgoCD/node
  guards all armed; no live pod restart, no node pressure, no Pending.

Capacity answer unchanged: **~60 sessions** remains the safe level. The
reconnect tail is a UX latency under full backend loss, not a capacity
limit — re-score on rc.4 (FX-R32) with the short N=60 × 30 min re-soak.

### v0.3.0-rc.4 soak: 60 sessions × 30 min (2026-10-04)

The FX-R32 re-score: same shape (60 soak-small, 20 lanes, waves 10/15 s,
node guard armed, default limits), half duration, drills at ~T+10 —
backend pod delete, backend **rollout**, session-TLS rotate.

- **In-frame reconnect verified at fleet scale.** PG `connection_lease`
  snapshots around each drill: all 60 leases kept their lease id,
  `stream_epoch` bumped in place, and `stream_owner_tab` unchanged for
  every session through both backend drills. Zero `launch.redeem` audit
  events and zero launch tickets minted in every drill window — the
  rc.3 relaunch path is gone. Caveat corrected in rc.5: `stream_owner_tab`
  is minted once per **outer** page load, so a frame re-navigation keeps
  the same id — the snapshots prove no relaunch/no outer reload, and
  cannot see navs (see the rc.5 navwatch below).
- **Drill spans.** Pod delete: 6 sessions on the killed pod, p100 ~5 s.
  Rollout: 38/60 sessions observed a span (22 reconnected inside one 5 s
  poll — invisible to the metric): p50 ~5 s / p95 ~15 s / p100 ~15.0 s.
  All span numbers are at the harness's 5 s poll resolution: a ~5 s
  span is one missed poll (≈0–5 s real), ~15 s is three (≈10–15 s
  real); a 1 s-poll rerun under rc.5 will re-score at true resolution.
  TLS rotate: zero disconnects again. The ~15 s tail is 7 sessions whose
  jittered in-frame retries (0.5–2 s, `reconnect_delay`) needed ~3 poll
  cycles to land a claim on the fresh pods.
- **Gates (advisor).** disconnect-span p95 14.96 s **fails** the ≤ 10 s
  bar (p100 15.02 s inside ≤ 30 s); inputResumeMs p100 23.3 s PASS
  (< 30 s); longest gap 15.0 s PASS (< 60 s); dropped 0; falseElsewhere
  0; manual actions 0; inputTimeouts 0. vs rc.3: p95 39.9 s → 15.0 s,
  p100 45 s → 15 s, relaunch thrash → none.
- **Everything else.** connect p50 5.9 s / p95 6.1 s, mid-run reload
  reconnect p50 0 / p95 5.0 s (near-seamless again), inputDispatch p50
  66 ms / p95 83 ms, lane 429s = 4 anonymous-phase hits on soak09/10/14
  during the OIDC ramp (same class as rc.3's 2; none on authenticated
  traffic).

Capacity answer unchanged: **~60 sessions** remains the safe level. The
residual is a ~15 s worst-case reconnect tail under full backend loss —
roughly three times better than rc.3 and no longer a correctness issue
(no relaunch, no lost claims), still over the 10 s p95 bar for the
v0.3.0 tag gate.

### v0.3.0-rc.5: internal-CA drill + 1 s-poll rollout rerun (2026-10-04)

- **Supply chain/upgrade.** 8/8 cosign OCI signatures + 3/3 release
  blobs at `refs/tags/v0.3.0-rc.5`; chart 0.3.0-rc.5 deployed (helm rev
  18); CRD guard held (305, never applied); schema stays 18.
- **Internal-CA rotation drill (FX-R33) — PASS.** With 6 live sessions
  `cmctl renew` on `tcdi-e2e-internal-ca` plus both leaves: operator logs
  show `tlsreload: reloaded CA bundle file=/etc/broker-ca/ca.crt` on both
  replicas (no restart), 6/6 sessions stayed connected, and **3 workspace
  deletes cleared their `runtime-cleanup` finalizer in 2.9 s without an
  operator restart** (rc.3/rc.4 wedged ~4 h here) with zero
  `broker unreachable` lines; old-CA material is refused at the
  handshake (HTTP 000) while the new cert reaches HTTP 403.
- **Targeted rerun N=60 × 15 min, rollout only, poll = 1 s.** Disconnect
  spans at true resolution: p50 **1.03 s** / p95 **15.01 s** /
  p100 **16.01 s** — bimodal, not quantized: ~60 % reconnect inside ~1 s
  (in-frame retry), a ~24-session cohort lands 11–16 s. p95 still
  **fails** the ≤ 10 s bar; p100 inside ≤ 30 s. inputResume p100
  27.7 s, longest gap ~16 s, dropped 0, falseElsewhere 0, inputTimeouts
  1, one lane 429 (OIDC ramp). All 60 claims landed on the same leases —
  zero tickets, zero `launch.redeem`.
- **Frame-navigation evidence (corrects the rc.4 claim).**
  `stream_owner_tab` is minted per outer-page load — a frame
  re-navigation keeps the same id, so the rc.4 "zero navigations" claim
  over-read the data. A 10-session `framenavigated` probe through the
  same rollout on rc.5: **5 sessions re-navigated the iframe once**, at
  12.7–16.5 s into their disconnect — the watch's fallback gate
  (5 s in-frame window + page-side poll lag + jitter) — and each nav's
  claim landed ~instantly. Sessions under ~2 s never navigated; one nav
  also fired late on an already-recovered session. So the tail is the
  single-shot fallback working as designed (one nav, one claim), not
  rc.3's repeated thrash — but "zero navigations" is not literally
  true at fleet scale on rc.4/rc.5.
