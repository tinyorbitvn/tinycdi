# ADR 0006 — Rate-limiter state placement

Status: Accepted

## Context

The client-facing rate limits run in `internal/ratelimit`: a per-key
token bucket on each backend replica for local ceilings, plus — for the
login family and launch — a shared Postgres window so the configured flag
is an *exact aggregate bound* at every replica count. A purely local
divide-by-replicas scheme has three documented gaps: a rolling surge
overshoots the bound while the extra pod serves, an out-of-band
`kubectl scale` or HPA leaves a rendered divisor stale, and a load
balancer that pins a client to one pod silently under-limits that key by
the replica count. This ADR records where the limiter state lives and
the budgets each flag sets.

## Decision

**option B — Postgres-backed shared fixed-minute windows** for the login
family and `/v1/launch`, with local token buckets as ceiling and floor:

- The shared state is the `rate_limit_window` table
  `(route, bucket_key, window_start, count)`
  (`internal/store/migrations/0001_baseline.sql`), written by one
  `INSERT … ON CONFLICT … RETURNING count` per check
  (`internal/store/rate_limit.go`) and enforced by `SharedLimiter`
  (`internal/ratelimit/shared.go`). The shared window bound is
  rate + burst per minute (`internal/backend/ratelimit.go:96,106`).
- **G1 — fail-open with a floor.** A store error falls back to the
  divided local limiter, never to unlimited and never to a hard refusal:
  every limited route's completion already needs Postgres, so the outage
  degrades to per-replica limiting.
- **G2 — split mode stays local-only.** A `-broker-url` session gateway
  owns no Postgres handle, so `/v1/launch` there keeps the divided local
  bucket rather than growing a broker RPC.
- **(i) — effective bound.** While Postgres is healthy the shared window
  is the exact aggregate bound and each pod's local bucket is the
  undivided rate+burst — a store-protection prefilter only (a
  locally-refused request never reaches the store, which bounds the
  upsert rate a key spray can cause). Only in degraded mode (breaker open
  / store error) does the divided rate/N + burst/N bucket decide — the
  fail-open floor.
- **(ii) — one upsert per check; leader-only cleanup.** The check is a
  single upsert returning the count (no second query); expired windows
  are deleted only by the replica holding the Postgres leader lock
  (`SweepRateLimitWindows`, `internal/store/rate_limit.go`).
- **RL-CEILING — the healthy-mode local ceiling is undivided.** Each
  shared-window route carries two local buckets: the undivided rate+burst
  ceiling (healthy) and the divided rate/N + burst/N floor (degraded);
  the breaker picks one per request. Neither bucket is reset at a
  transition — token state persists and refills lazily, so a breaker flap
  cannot storm resets. Right at a healthy→degraded switch a pod may have
  admitted up to rate+burst locally before the divided floor binds, and a
  key the floor never saw starts the outage with a fresh divided burst;
  conversely a ceiling bucket drained before the switch stays drained —
  it refills at the configured rate, not on recovery.
- **RL-DEFAULT — the default budgets** are the flag table below; the
  shared window makes them exact aggregates across replicas.
- **Latency bound + circuit breaker.** Every store check runs under a
  500 ms deadline (`storeCallTimeout`, `internal/ratelimit/shared.go:37`,
  always — healthy or probing), so a slow-but-alive Postgres adds at most
  ~500 ms to one request. A store error/timeout opens a 10 s cool-down
  (`storeCooldown`, `shared.go:45`): checks skip the store entirely (the
  divided local limiter decides), then exactly one request probes —
  single-flight, the rest keep their local verdict. Probe success closes
  the circuit; failure reopens it for another cool-down.
- **Observability.** `tinycdi_rate_limit_store_errors_total{route}`
  counts real store failures only — checks skipped while the breaker is
  open never reach the store; `tinycdi_rate_limit_store_degraded{route}`
  (gauge) mirrors the breaker. Fallback entry and exit each log one line
  (edge-triggered, never per request). Refusals return `429 RATE_LIMITED`
  with `Retry-After` and increment `tinycdi_rate_limited_total{route}`
  (`internal/observability/metrics.go`).

The multi-replica behaviour — shared-window bound including an uneven
spread, plus Postgres-outage fail-open and recovery — is pinned by
`tests/integration/rate_limit_pg_test.go`.

### Flags and budgets

| Flag | Default | Meaning |
|---|---|---|
| `-login-rate` | 60/min, burst 20 | per-client bound on `/v1/login`, `/v1/auth/callback` and `GET /v1/session` (`internal/backend/config.go:315-316`) |
| `-launch-rate` | 120/min, burst 40 | per-client bound on `/v1/launch` (`config.go:333-334`) |
| `-session-lookup-rate` | 300/min, burst 120 | per-client bound on requests carrying a session cookie that does not resolve: on the app listener (`internal/api/session_lookup.go`, only misses spend) and for cookies the session listener has never seen — a **local** per-replica bucket only, so a random-cookie spray cannot multiply Postgres reads (`internal/backend/config.go`, `internal/gateway/proxy.go`, `internal/api/session_lookup.go`) |
| `-rate-limit-replicas` | 1 | divisor of the degraded-mode floor — the shared window makes the flags exact aggregates while Postgres is healthy (`config.go:369`) |
| `-trusted-proxies` | empty | CIDRs of reverse proxies whose forwarding headers set the client key (`config.go:367`) |

While Postgres is healthy the login and launch budgets are aggregate
across replicas; a configured rate of `0` disables the limiter. The
callback additionally carries a per-IP ceiling at 10× the login budget
(`rateLimitRouteCallbackCeiling`, `internal/backend/ratelimit.go:50,98`).

Keying: the anonymous client key is the socket peer, or the right-most
`X-Forwarded-For` entry outside `-trusted-proxies` CIDRs — a non-IP token
is never trusted. Authenticated keys are `sha256` digests with a
`sess:`/`oidc:` namespace prefix so they can never collide with an IP key
(`ratelimit.KeyDigest`, `internal/ratelimit/ratelimit.go:143-148`).
Per-key local state lives in an LRU capped at `rateLimitMaxKeys =
100 000` (`internal/backend/wire.go:66`).

Everything else is deliberately unlimited at this layer: the
authenticated API carries quota/ownership bounds instead, the internal
mTLS surface trusts the broker/operator clients, and the per-session
input-activity reports to the broker are a fixed internal constant, not a
client limit.

## Alternatives considered

- **option A — keep divide-by-replicas, pin the divisor.** Each pod
  enforces `rate/N` locally; no store dependency, no code change, and the
  strongest availability property of the three. Rejected: it keeps all
  three documented inaccuracies — the rollout surge overshoot
  (~(N+1)/N×), a divisor that goes stale on out-of-band scaling or under
  an HPA, and the sticky-LB under-limit where a pinned client sees only
  `rate/N`.
- **option C — hybrid local bucket + periodic Postgres sync.** Replicas
  keep local buckets and periodically upsert consumption deltas, reading
  back a recomputed share. Rejected: between sync ticks it *is* option A
  (same surge, same staleness), and exactness needs liveness tracking,
  share redistribution and drift accounting — a small distributed-systems
  project inside the middleware. Its one advantage over B, correct bounds
  through a Postgres outage, buys nothing because the protected routes
  cannot complete during that outage anyway.

## Consequences

- `rate_limit_window` is part of the baseline schema; the limiter backend
  sits behind the existing `Limiter.Allow` call sites through
  `SharedLimiter`, and `-rate-limit-replicas` divides each budget into
  the degraded-mode floor only — healthy-mode bounds are the exact flag
  values at every replica count.
- The limiter↔DB coupling is the one new failure mode; it is mitigated by
  the fail-open floor and the breaker (one bounded probe per cool-down
  instead of a per-request stall).
- In split mode (`-broker-url`) `/v1/launch` keeps the divided local
  limiter per G2.
- Flag/env semantics, metrics labels and the keying rules are what the
  table above states — the change from a purely local scheme is where
  the counter lives, not what it counts.
