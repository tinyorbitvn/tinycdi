-- 005_activity.sql — session activity records, durable stop intents and
-- per-generation revocations (design §8).
--
-- Invariants enforced here:
--   * workspace_activity is keyed by (workspace_id, runtime_generation): a
--     stale runtime generation can neither keep alive nor stop a newer
--     incarnation — its activity lands on a fenced row.
--   * Timestamps are server receipt times assigned by the broker from its
--     own clock; client-supplied times are never persisted.
--   * open_streams derives only from connected/disconnect events — the
--     operator drain check reads it; a crashed gateway may leak a stream,
--     which the finalizer's bounded drain window absorbs.
--   * stop_intent is the durable seam between the broker's expiry
--     planner/RequestStop and the lifecycle outbox: rows are drained once
--     (drained_at) into provisioning intents; (workspace, generation,
--     reason) is unique so repeated planner scans never duplicate intents.
--   * workspace_revocation (workspace_id, runtime_generation) blocks new
--     launch tickets and ticket redemption bound to that generation and
--     revokes its live leases — the operator teardown fencing edge.

CREATE TABLE IF NOT EXISTS workspace_activity (
    workspace_id       text NOT NULL REFERENCES workspaces (id),
    runtime_generation bigint NOT NULL,
    last_input_at      timestamptz,              -- last 'input' receipt (server clock)
    connected_at       timestamptz,              -- last 'connected' receipt
    disconnected_since timestamptz,              -- grace-window anchor; NULL while connected
    open_streams       integer NOT NULL DEFAULT 0 CHECK (open_streams >= 0),
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, runtime_generation)
);

CREATE TABLE IF NOT EXISTS stop_intent (
    id                 bigserial PRIMARY KEY,
    workspace_id       text NOT NULL REFERENCES workspaces (id),
    runtime_generation bigint NOT NULL,
    reason             text NOT NULL
                       CHECK (reason IN ('idle_timeout', 'disconnect_timeout', 'max_duration', 'requested')),
    deadline           timestamptz NOT NULL,     -- moment the budget was crossed
    created_at         timestamptz NOT NULL DEFAULT now(),
    drained_at         timestamptz,              -- set when the lifecycle pipeline consumed it
    UNIQUE (workspace_id, runtime_generation, reason)
);
CREATE INDEX IF NOT EXISTS stop_intent_undrained
    ON stop_intent (workspace_id) WHERE drained_at IS NULL;

CREATE TABLE IF NOT EXISTS workspace_revocation (
    workspace_id       text NOT NULL REFERENCES workspaces (id),
    runtime_generation bigint NOT NULL,
    revoked_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, runtime_generation)
);
