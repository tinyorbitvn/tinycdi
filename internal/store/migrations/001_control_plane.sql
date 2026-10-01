-- 001_control_plane.sql — control-plane store for quota reservations,
-- transactional outbox intents and idempotent request results.
--
-- Invariants enforced here:
--   * tenant_quota is the serialization point for Reserve(); callers take
--     SELECT ... FOR UPDATE inside their transaction so concurrent creates
--     for one tenant serialize on a single row.
--   * quota_reservation.state 'held' rows are the only rows counted toward
--     usage; 'released' rows keep an audit trail and the release proof.
--   * workspaces.intent_revision increments inside the same transaction as
--     the outbox_intent insert, so (workspace_id, revision) is gapless and
--     unique per workspace intent stream.
--   * outbox_intent rows are delivered at-least-once: dispatched_at IS NULL
--     means pending; consumers drop revision <= lastAppliedIntentRevision.
--   * idempotency maps (tenant_id, key) -> request hash + stored result so
--     a replayed request returns the stored result and a conflicting body
--     for the same key is rejected.

CREATE TABLE tenant_quota (
    tenant_id        text PRIMARY KEY,
    max_running_slots bigint NOT NULL CHECK (max_running_slots >= 0),
    max_cpu_millis    bigint NOT NULL CHECK (max_cpu_millis >= 0),
    max_memory_bytes  bigint NOT NULL CHECK (max_memory_bytes >= 0),
    max_disk_bytes    bigint NOT NULL CHECK (max_disk_bytes >= 0),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE workspaces (
    id              text PRIMARY KEY,              -- workspace UID
    tenant_id       text NOT NULL,
    owner_subject   text NOT NULL DEFAULT '',
    request_id      text NOT NULL UNIQUE,          -- deterministic create request ID
    intent_revision bigint NOT NULL DEFAULT 0 CHECK (intent_revision >= 0),
    state           text NOT NULL DEFAULT 'active'
                    CHECK (state IN ('active', 'deleted')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz
);

CREATE TABLE quota_reservation (
    workspace_id   text PRIMARY KEY REFERENCES workspaces (id),
    tenant_id      text NOT NULL,
    running_slots  bigint NOT NULL CHECK (running_slots >= 0),
    cpu_millis     bigint NOT NULL CHECK (cpu_millis >= 0),
    memory_bytes   bigint NOT NULL CHECK (memory_bytes >= 0),
    disk_bytes     bigint NOT NULL CHECK (disk_bytes >= 0),
    state          text NOT NULL DEFAULT 'held'
                   CHECK (state IN ('held', 'released')),
    release_proof  text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    released_at    timestamptz
);
CREATE INDEX quota_reservation_held_tenant
    ON quota_reservation (tenant_id) WHERE state = 'held';

CREATE TABLE outbox_intent (
    seq           bigserial PRIMARY KEY,
    workspace_id  text NOT NULL REFERENCES workspaces (id),
    tenant_id     text NOT NULL,
    revision      bigint NOT NULL CHECK (revision > 0),
    kind          text NOT NULL
                  CHECK (kind IN ('create', 'start', 'stop', 'delete')),
    request_id    text NOT NULL,
    payload       jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    dispatched_at timestamptz,
    UNIQUE (workspace_id, revision)
);
CREATE INDEX outbox_intent_pending
    ON outbox_intent (workspace_id, revision) WHERE dispatched_at IS NULL;

CREATE TABLE idempotency (
    tenant_id    text NOT NULL,
    key          text NOT NULL,
    op           text NOT NULL DEFAULT 'create',
    request_hash bytea NOT NULL,
    request_id   text NOT NULL,
    result       jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key)
);
