-- 002_connections.sql — session-broker store: launch tickets and connection
-- leases (design §6).
--
-- Invariants enforced here:
--   * launch_ticket stores ONLY the SHA-256 hash of the opaque ticket —
--     plaintext tickets never reach the DB.
--   * consumed_at is set on the first redemption attempt, even when the
--     ticket is already expired (strictly single-use), and revoked_at
--     permanently denies the ticket — a consumed or revoked ticket can never
--     redeem again.
--   * At most one 'active' connection_lease per workspace, enforced by the
--     partial unique index — that is the atomic claim the broker relies on.
--     A takeover transaction supersedes the old row before inserting the new
--     lease so the old socket is fenced before the new one works.
--   * Leases bind (workspace_id, runtime_generation, runtime_uid) of the
--     incarnation observed at redemption, plus a monotonically increasing
--     fencing_version, so a recreated runtime or a superseded lease
--     auto-invalidates old access.

CREATE TABLE IF NOT EXISTS launch_ticket (
    ticket_hash        bytea PRIMARY KEY,              -- sha256(opaque ticket)
    workspace_id       text NOT NULL REFERENCES workspaces (id),
    tenant_id          text NOT NULL,
    principal_subject  text NOT NULL,                  -- iss|sub of the requester
    runtime_generation bigint NOT NULL,
    runtime_uid        text NOT NULL,
    audience           text NOT NULL,                  -- gateway audience binding
    takeover           boolean NOT NULL DEFAULT false, -- may supersede a live lease
    request_id         text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,           -- issued + 60s
    consumed_at        timestamptz,                    -- first redemption attempt
    revoked_at         timestamptz
);
CREATE INDEX IF NOT EXISTS launch_ticket_workspace
    ON launch_ticket (workspace_id);

CREATE TABLE IF NOT EXISTS connection_lease (
    id                 text PRIMARY KEY,
    workspace_id       text NOT NULL REFERENCES workspaces (id),
    tenant_id          text NOT NULL,
    principal_subject  text NOT NULL,
    runtime_generation bigint NOT NULL,
    runtime_uid        text NOT NULL,
    fencing_version    bigint NOT NULL CHECK (fencing_version > 0),
    gateway_id         text NOT NULL,
    state              text NOT NULL DEFAULT 'active'
                       CHECK (state IN ('active', 'superseded', 'revoked', 'expired')),
    created_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,           -- sliding TTL; gateway renews ~every 10s
    last_renewed_at    timestamptz,
    closed_at          timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS connection_lease_one_active
    ON connection_lease (workspace_id) WHERE state = 'active';
CREATE INDEX IF NOT EXISTS connection_lease_workspace
    ON connection_lease (workspace_id);
