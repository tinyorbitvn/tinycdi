# ADR 0006 — rate-limiter state placement (per-replica vs Postgres-backed)

Status: **accepted — implemented** (v0.4; see Decision below)

Date: 2026-10-05

## Decision

The advisor picked **option B** (2026-10-05), with these guardrails:

- **G1 — fail-open with a floor.** A store error falls back to the
  divided local limiter, never to unlimited and never to a hard refusal:
  every limited route's completion already needs Postgres, so the outage
  degrades to option-A behaviour.
- **G2 — split mode stays local-only.** A `-broker-url` session gateway
  owns no Postgres handle, so `/v1/launch` there keeps the divided local
  bucket rather than growing a broker RPC.
- **i — effective bound = min(shared window, local ceiling).** The
  divided local bucket stays on BOTH as the fail-open fallback AND as a
  per-replica ceiling while Postgres is healthy: a locally-refused
  request never reaches the store, which also bounds the upsert rate a
  key spray can cause.
- **ii — one upsert per check; leader-only cleanup.** The check is a
  single `INSERT ... ON CONFLICT` returning the count (no second query);
  expired windows are deleted only by the replica holding the Postgres
  leader lock (the existing singleton machinery).
- **iii — observability.** `tinycdi_rate_limit_store_errors_total{route}`
  counts real store failures only — checks skipped while the circuit
  breaker is open never reach the store, so a sustained outage shows
  ~one increment per 10 s cool-down per limiter, not one per request;
  `tinycdi_rate_limit_store_degraded{route}` (gauge) mirrors the
  breaker. Fallback ENTRY and EXIT each log one line (edge-triggered,
  never per request).
- **iii-bis — latency bound + circuit breaker.** Every store check runs
  under a 500 ms deadline (`storeCallTimeout`, always — healthy or
  probing), so a slow-but-alive Postgres can add at most ~500 ms to one
  request. A store error/timeout opens a 10 s cool-down: checks skip
  the store entirely (the divided local limiter decides), then exactly
  one request probes — single-flight, the rest keep their local
  verdict. Probe success closes the circuit; failure reopens it for
  another cool-down.
- **iv — migration.** `rate_limit_window` (route, bucket_key,
  window_start, count) + an index on `window_start`; expand-only, no
  backfill, rolling-upgrade safe.
- `-rate-limit-replicas` keeps its value and rendering
  (`backend.replicas`) but its meaning changed: it divides each budget
  into the per-replica LOCAL ceiling, not the aggregate bound — the
  shared window makes the flags exact aggregates at every replica count.

Implemented: `internal/store/rate_limit.go` (one-statement window
check + sweep), `internal/ratelimit/shared.go` (`SharedLimiter` —
min(Postgres window, divided local), fail-open, 500 ms per-call
deadline + 10 s single-flight-probe circuit breaker, edge-triggered
logs), wiring in `internal/backend/wire.go`, migration
`021_rate_limit_window`. The B6 gate lives in
`tests/integration/rate_limit_pg_test.go` (two-replica shared-window
abuse + Postgres outage fail-open/recovery).

## Context

The v0.3 line enforces its client-facing rate limits in-process
(`internal/ratelimit`): a per-key token bucket on each backend replica,
no shared counter. RL-1 (#88) then made the `-login-rate`/`-launch-rate`
flags *aggregate* bounds by dividing each budget by `-rate-limit-replicas`
(the chart renders `backend.replicas`). That closed the N× multiplier but
left two documented gaps — a rolling surge overshoots to ~(N+1)/N× while
the extra pod serves, and an out-of-band `kubectl scale` or an HPA leaves
the rendered divisor stale until the next `helm upgrade` (docs advise
pinning the flag to `maxReplicas`). A third gap is visible in the same
model: division assumes an *even spread* — under an ingress that pins a
client to one pod (cookie/IP-hash affinity), that key's whole budget is
the single pod's `rate/N` share, so stickiness silently **under**-limits
by N.

v0.4 has to decide where limiter state lives before the build tasks.
This note inventories what is limited today and prices three options:
(A) keep divide-by-replicas and pin the divisor, (B) Postgres-backed
windows for the login family and launch (the advisor's lean), (C) a
hybrid local bucket with periodic Postgres sync.

## Inventory — every limited route today (main @ 93178cc)

Mechanism: `ratelimit.Limiter` — continuous-refill token bucket, burst
cap, per-key state in an LRU capped at `rateLimitMaxKeys = 100 000`
(`internal/backend/wire.go:57`). A refused request returns 429
(`RATE_LIMITED`) with `Retry-After` = time to one token and increments
`tinycdi_rate_limited_total{route}` (route = bounded mux template, E8).
A configured rate of `0` disables the limiter. The client-IP key is the
socket peer, or the right-most X-Forwarded-For entry outside
`-trusted-proxies` CIDRs (S18) — a non-IP token is never trusted.
Authenticated keys are `sha256` digests with a `sess:`/`oidc:` namespace
prefix so they can never collide with an IP key (`ratelimit.KeyDigest`,
FX-R30); validity is always proven *before* a digest key is minted, and
anything unverifiable falls back to the client-IP key.

| Route | Listener | Key when authenticated | Key otherwise | Configured budget | Extra ceiling |
|---|---|---|---|---|---|
| `GET /v1/login` | app | — deliberately IP-keyed (FX-R30 review: cookie-keying would buy every forged value a store read) | client IP | `-login-rate` 30/min, burst 10 | — |
| `GET /v1/auth/callback` | app | `oidc:`+sha256(state) — `?state` must equal the sealed login-cookie state (SEC-03) | client IP | same shared login bucket: 30/min, burst 10 | per-IP ceiling 10×: 300/min, burst 100 |
| `GET /v1/session` (probe) | app | `sess:`+sha256(session ID) via `Peek` — read-only, no idle slide | client IP | same shared login bucket: 30/min, burst 10 | — |
| `POST /v1/launch` | session | `sess:`+sha256(cookie) iff the cookie maps to a session **live on this replica** (in-memory check only — the limiter never spends a directory read) | client IP | `-launch-rate` 60/min, burst 20 | — |

All four budgets are divided by `-rate-limit-replicas`
(`ratelimit.PerReplica`, integer round-down, `max(1, ·)` clamps): at the
chart default `backend.replicas=2` each pod enforces 15/min + burst 5
(login family), 30/min + burst 10 (launch), 150/min + burst 50 (callback
ceiling). A configured rate below the replica count clamps to 1/min per
pod — the one upward drift (~N/min aggregate). The launch limiter is
built in both merged and split (`-broker-url`) mode — a split session
gateway has no Postgres handle — while the login family only exists
where the app listener is wired; this matters under option B.

Everything else is deliberately unlimited at this layer: the
authenticated API carries quota/ownership bounds instead, the internal
mTLS surface trusts the broker/operator clients, and the one other
throttle — per-session input-activity reports to the broker
(`internal/gateway/activity.go`) — is a fixed internal constant, not a
client limit.

Postgres coupling today: the probe's session-digest key and the
callback's session write already hit Postgres per request; ticket
redemption on `/v1/launch` is a `launch_ticket` row update; only
`/v1/login` (sealed-cookie mint + IdP redirect) completes without the
store.

## Option A — keep divide-by-replicas, pin the divisor

Status quo plus the already-documented HPA advice
(`-rate-limit-replicas=maxReplicas` via `backend.extraArgs`), optionally
surfaced as a chart value. No code change beyond docs/chart sugar.

- **Rolling surge.** With `maxUnavailable=0` and surge 1 the transient
  aggregate is ≤ (N+1)/N× — 1.5× at the default N=2, 1.1× at N=10.
  Bounded and short, but a real overshoot on every rollout.
- **HPA scale out/in.** No static setting is exact: pinned to
  `maxReplicas`, a partial scale-out under-limits by `maxReplicas/n`
  (too strict); unpinned, the rendered `backend.replicas` goes stale and
  scale-out over-limits until the next `helm upgrade`. Pinning is the
  safer failure direction (users see tighter limits, not a lifted cap).
- **Replica crash.** N−1 pods enforce `rate/N` each → aggregate (N−1)/N×
  — a brief, safe-direction under-limit until replacement.
- **Postgres outage.** None — the limiter has no store. The strongest
  availability property of the three options.
- **Clock skew.** Each pod's clock refills only its own buckets; a skewed
  pod shifts that pod's share rate, never the others'. Bounded by ±one
  replica's share — minor.
- **Many-IP abuse.** Per-IP budget × attacker IP count, inherent to any
  IP-keyed scheme. Memory is capped at 100k keys/pod; an LRU eviction
  hands the evicted key a fresh full bucket — a bounded amnesty that
  slightly *helps* evicted legitimate users and cannot unlimit the
  attacker.
- **Sticky LB.** As above — a pinned key sees `rate/N`, under-limit by N.
- **Test plan (B6).** Multi-replica abuse: one key driven at ~3× the
  bound across pods → aggregate ≈ bound on even spread; measure surge
  overshoot and uneven-spread under-limit; document both as accepted.
- **Migration.** None.

## Option B — Postgres-backed windows for login + launch

One shared counter per key, checked by every replica. Fixed per-minute
window as the primary sketch (cheapest exact primitive); a sliding
variant noted below.

**Schema sketch** (migration 019, expand-only):

```sql
CREATE TABLE rate_limit_window (
    route        text        NOT NULL,   -- 'login' | 'callback_ceiling' | 'launch'
    bucket_key   text        NOT NULL,   -- IP string or sess:/oidc: digest, as today
    window_start timestamptz NOT NULL,   -- date_trunc('minute', now()) at insert
    count        integer     NOT NULL,
    PRIMARY KEY (route, bucket_key, window_start)
);
CREATE INDEX rate_limit_window_expiry ON rate_limit_window (window_start);
```

**One statement per check** — the upsert is the whole read-modify-write,
serialised by Postgres across replicas:

```sql
INSERT INTO rate_limit_window (route, bucket_key, window_start, count)
VALUES ($1, $2, date_trunc('minute', now()), 1)
ON CONFLICT (route, bucket_key, window_start)
DO UPDATE SET count = rate_limit_window.count + 1
RETURNING count;
-- allow iff count <= limit; Retry-After = window_start + 1min - now()
```

Semantics mapping: today's token bucket `rate R/min + burst B` ≈ a fixed
window with `limit = R + B` per minute (login 40, launch 80, callback
ceiling 400 at defaults). Fixed window's boundary effect allows ≤2×limit
inside any <2 min span — same order as today's surge overshoot and as
the token bucket's own `burst + rate` draw-down, so no new abuse
magnitude. If the advisor wants the edge tighter, the standard estimator
`count = cur + prev × (1 − elapsed/60s)` costs one extra row read and
halves the boundary overshoot — an implementation choice, not a schema
change.

**Index/TTL cleanup.** Rows die by one periodic statement —
`DELETE FROM rate_limit_window WHERE window_start < now() - interval '15 minutes'`
— on an existing background tick (e.g. the recovery tick), idempotent
across replicas. The `window_start` index makes it a range delete.
Retained size ≈ active keys × 15 windows — thousands of rows, not
millions, at any realistic key count.

**Cost per request at expected QPS.** One indexed single-row upsert:
~1–3 ms in-cluster, ~100 B of WAL. Measured demand: the 60-session soak
peaked the limited routes at ≈1 RPS aggregate (20 OIDC lanes, opens
waved 10 per 15 s) and ran far below that steady-state; a 500-user
Monday sign-in rush plus probes and launches is ≈20 RPS — call it
50 RPS with headroom. 50 single-row
upserts/s is <2% of a small Postgres's write capacity and adds ~1–3 ms
to a login whose real cost is an IdP round trip, and to a launch whose
redeem is already a Postgres write — inside noise. Refusals still cost
the write (the count must increment to be known over), so the DB sees
the *full* attack rate: ~1k RPS of upserts remains tolerable; beyond
that a local pre-gate is needed (below).

**Fail-open vs fail-closed per route** — the deciding observation: every
limited route's *completion* already requires Postgres, so a limiter
that opens on store error admits requests that fail downstream anyway.

| Route | Store dependency of the real work | Recommended on check error |
|---|---|---|
| `GET /v1/login` | none for the start (sealed cookie + redirect); the *callback* needs the session write | fail-open — worst case is unbounded login starts that mint cookies and die at the callback; keep the local floor below |
| `GET /v1/auth/callback` | session insert | fail-open — the handler 503s on the same outage regardless |
| `GET /v1/session` | `Peek` on the session store | fail-open — probe just reports `authenticated:false` anyway |
| `POST /v1/launch` | `launch_ticket` redeem | fail-open — redemption fails downstream on the same outage |

Recommended shape: **fail-open with a floor** — on check error, fall
back to the existing divided in-memory limiter rather than to unlimited.
The code path already exists; an outage then degrades to option-A
behaviour instead of to a lifted cap or a hard 503. Fail-closed is
available per route (a Postgres blip becomes instant 429/503 on the
entry surface) but buys nothing — every route it would block fails
identically downstream — while turning a failover-second into a visible
login outage.

- **Rolling surge.** Exact — one counter; N+1 pods draw from the same
  window. Both RL-1 gaps and the sticky-LB under-limit disappear.
- **HPA scale out/in.** Exact at every N, no divisor, no pin, no stale
  render. This is option B's main correctness win.
- **Replica crash.** No per-replica state; an in-flight request retried
  by a client may double-count — bounded ±1 per crash, same class as any
  non-idempotent write.
- **Postgres failover/outage.** Per the table above: fail-open with the
  local floor. Failover blips cost ~seconds of degraded-to-A limiting —
  no additional exposure class.
- **Clock skew.** None by construction — window boundaries come from the
  DB server's `now()`; app clocks are out of the check. (Today's skew
  exposure is already minor, so this is a simplification, not a fix.)
- **Many-IP abuse.** The key space moves to Postgres: a spray inserts a
  row per (key, window) — ~100 B each, bounded in retention by the TTL
  delete but not in *insert rate*. Keep a cheap in-memory per-IP ceiling
  in front (the callback's existing 10× pattern generalised) if the
  advisor wants the spray absorbed before the store; at these limits the
  simpler reading is that a spray large enough to hurt the table is
  already a DoS the edge should answer.
- **Split mode.** A `-broker-url` session gateway owns no DB handle, so
  a Postgres-backed `/v1/launch` there needs either a broker-mediated
  check (new internal-RPC surface) or stays local-only. Recommendation:
  local-only in split mode — it is the dev/test topology; the merged
  chart topology gets the shared counter. Decide explicitly in the build
  task.
- **Test plan (B6).** The option-A suite plus: two+ replicas, one key at
  3× the bound spread across pods → aggregate ≤ bound ±1; `kubectl
  scale` 2→4 mid-flood → bound holds immediately; Postgres kill/restart
  mid-flood → chosen fail-open/floor behaviour verified per route;
  migration up/down on a live v0.3.x replica set; split-mode launch
  documented/tested as local-only if taken.
- **Migration needs.** 019 `CREATE TABLE` + index — expand-only,
  rolling-upgrade safe: v0.3.x pods never read the table and keep their
  local limiters; a request lands on exactly one pod, so mixed-version
  rollouts apply one enforcement or the other, never both. Rollback
  leaves an orphan table (harmless; droppable later). No backfill.
  `-rate-limit-replicas` stays for the degraded-mode floor or is
  deprecated — build-task decision; flag semantics otherwise revert to
  the true aggregate bound.

## Option C — hybrid local token bucket + periodic Postgres sync

Each replica keeps its token bucket and periodically upserts consumption
deltas to Postgres, reading back a recomputed share — either a lease on
`rate/N_effective` or a proportional slice of the remaining window.

- **Correctness.** Between sync ticks this *is* option A — same (N+1)/N
  surge overshoot, same stale share on scale events, same sticky-LB
  skew — converging toward the bound only as fast as the sync interval
  (say 10 s) *after* membership settles. Exactness needs liveness
  tracking (which replicas are alive → heartbeats + TTL), share
  redistribution, and drift accounting — a small distributed-systems
  project inside the middleware.
- **Rolling surge / HPA.** Transient overshoot like A, then convergence;
  better than A only once the sync has run and membership agrees on N.
- **Replica crash.** The dead replica's synced share goes stale until
  its TTL expires — survivors under-limit (safe direction) for up to
  the TTL.
- **Postgres outage.** Degrades gracefully to pure A — the one failure
  mode C handles best.
- **Clock skew.** Local buckets use local clocks; sync windows use DB
  time — minor boundary noise.
- **Many-IP abuse.** Local LRU still bounds memory; the sync upserts
  carry only keys active this tick.
- **Cost.** A batched upsert per replica per tick covering that pod's
  active keys — more writes than B at high key counts (every active key,
  every tick, whether or not it drew), less per request. Latency
  unchanged (~0 per check).
- **Test plan.** A's suite plus convergence assertions (post-scale bound
  reached within ~2× the sync interval), stale-share expiry, and the
  same Postgres-drill cases.
- **Migration.** Same 019-style table plus replica-heartbeat state.

Verdict: C pays distributed-limiter complexity for an exactness B
delivers trivially at this traffic level, and its differentiator —
correct bounds through a DB outage — is worth little because the
protected routes cannot complete during that outage anyway.

## Comparison

| Axis | A — divide-by-replicas | B — Postgres window | C — local + sync |
|---|---|---|---|
| Bound accuracy | ≈flag; (N+1)/N surge, stale on scale, /N under sticky LB | exact at every N | ≈A between syncs, converges |
| Surge overshoot | ≤(N+1)/N transient | none | like A until sync |
| HPA | needs maxReplicas pin (under-limit) or goes stale (over) | nothing to configure | needs membership to converge |
| Replica crash | (N−1)/N under-limit | ±1 double-count | stale share until TTL |
| Postgres outage | unaffected | fail-open + local floor (routes fail downstream anyway) | degrades to A |
| Clock skew | per-pod share drift | none (DB clock) | minor |
| Key-space abuse | LRU-bounded memory | row/insert-rate growth; TTL + optional local ceiling | bounded memory; sync carries active keys |
| Cost/request | ~0, all in-process | ~1–3 ms, one upsert | ~0 per check + per-tick upserts |
| New failure mode | none | limiter↔DB coupling (mitigated: fail-open floor) | sync/membership machinery |
| Migration | none | 019 expand-only | 019 expand-only + heartbeat state |
| Code delta | none | limiter backend swap + sweep + split-mode decision | largest of the three |

## Recommendation

**Option B**, with two guardrails: (1) fail-open on store error into the
existing divided local limiter — an outage degrades to today's behaviour
rather than to a lifted cap or a hard refusal; (2) `/v1/launch` under
split mode stays local-only rather than growing a broker RPC.

Rationale in numbers: the entire limited surface tops out near ~20–50
RPS in the largest plausible legitimate burst (soak measured <1 RPS);
each check is a ~1–3 ms indexed upsert — under ~2% of Postgres write
capacity and inside the latency noise of routes whose real work already
requires the store. For that price the bound becomes *exact*: no surge
overshoot, no HPA divisor to pin, no sticky-LB under-limit — the flag
means what it says at every replica count. Every limited route's
completion already needs Postgres, so the new coupling cannot turn a
survivable outage into a user-visible one. Fixed-window edge ≤2×(R+B)
matches today's transient overshoot class; the sliding-window estimator
is an implementation-level upgrade if the advisor wants it tighter.

Option A is the honest runner-up — zero migration, zero new failure
modes — but it keeps all three documented inaccuracies, two of which
(stale divisor, sticky under-limit) are silent. Option C is rejected on
complexity grounds: its only real advantage over B is exact limiting
through a Postgres outage, a window in which none of the protected
routes can complete anyway.

## Consequences

- If B is chosen: migration 019, a limiter-backend abstraction so the
  Postgres check and the local floor sit behind the existing
  `Limiter.Allow` call sites, a window-sweep tick, a `-rate-limit-replicas`
  decision (floor divisor vs deprecation), and the split-mode launch
  carve-out. The B6 multi-replica abuse test becomes the correctness
  gate.
- Post-review hardening (PR #109, MINOR finding): the store check
  carries a 500 ms per-call deadline and a 10 s open-circuit cool-down
  with a single-flight probe — a persistently slow Postgres costs one
  bounded probe per cool-down instead of a per-request stall.
- If A is kept: pin guidance stays documentation-only; the three
  inaccuracies are accepted as documented bounds and B6 only needs to
  *measure* them, not gate on exactness.
- Flag/env semantics, metrics labels (`tinycdi_rate_limited_total{route}`)
  and the FX-R30 keying rules are unchanged under every option — the
  change is where the counter lives, not what it counts.
