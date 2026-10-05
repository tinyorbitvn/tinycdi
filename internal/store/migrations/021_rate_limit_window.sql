-- 021_rate_limit_window.sql — shared fixed-window counters for the
-- per-client rate limits (ADR 0006, option B).
--
-- Model:
--   * One row per (route, bucket_key, window_start): the counter a fixed
--     per-minute window carries for one limiter family and one client
--     key. window_start is always date_trunc('minute', now()) evaluated
--     on the server, so application-clock skew never shifts a boundary.
--   * route is the limiter family, not the mux pattern: 'login' is the
--     shared login bucket (/v1/login, /v1/auth/callback, GET /v1/session),
--     'callback_ceiling' the per-IP spray ceiling on the callback, and
--     'launch' the session gateway's POST /v1/launch budget.
--   * bucket_key is the same key the in-memory limiter used (FX-R30):
--     the client address string, or a sess:/oidc: SHA-256 digest — never
--     a raw session ID, cookie or OIDC state.
--   * count is bigint so a single-key flood can never wrap the counter
--     inside a window.
--
-- A check is ONE statement — INSERT ... ON CONFLICT increments and
-- returns the new count — serialised by Postgres across replicas. Rows
-- for expired windows are deleted by the leader replica's periodic sweep
-- (DELETE ... WHERE window_start < now() - interval '15 minutes'), which
-- the window_start index turns into a range scan.
--
-- Expand-only and rolling-upgrade safe: replicas predating the table
-- never read or write it and keep enforcing their in-process buckets;
-- a request lands on exactly one pod, so a mixed-version rollout applies
-- one enforcement or the other, never both. Rollback leaves an orphan
-- table — harmless, droppable later.

CREATE TABLE rate_limit_window (
    route        text        NOT NULL,
    bucket_key   text        NOT NULL,
    window_start timestamptz NOT NULL,
    count        bigint      NOT NULL,
    PRIMARY KEY (route, bucket_key, window_start)
);

CREATE INDEX rate_limit_window_expiry ON rate_limit_window (window_start);
