-- 022_user_session_limits.sql — per-principal running-workspace limits
-- within a tenant.
--
-- Model:
--   * tenant_user_limit_default holds the tenant-wide fallback: at most one
--     row per tenant; absent means UNLIMITED — a fresh upgrade has no rows
--     anywhere, so nothing changes until a tenant administrator sets one.
--   * user_session_limit holds a per-principal override keyed by
--     workspaces.owner_subject ("issuer|sub"). A row wins over the tenant
--     default; deleting it restores the default.
--
-- Enforcement: Reserve()/reacquireCompute() read the effective limit and
-- the owner's held running slots inside the same transaction, under the
-- tenant_quota row lock every reservation already serializes on — the
-- check is race-free because concurrent admissions for one tenant always
-- queue behind that single lock. The count is derived live from
-- quota_reservation (state 'held', running_slots > 0), so the existing
-- release paths decrement it: Release for Ephemeral/deleted workspaces,
-- convertToDiskOnly for a stopped Retain one. Disk-only holds and
-- retained disks never count — the limit is on concurrent running
-- sessions, not storage.
--
-- Expand-only and rolling-upgrade safe: replicas predating the tables
-- never read or write them. Rollback leaves two orphan tables — harmless,
-- droppable later.

CREATE TABLE tenant_user_limit_default (
    tenant_id   text PRIMARY KEY,
    max_running bigint NOT NULL CHECK (max_running >= 0),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE user_session_limit (
    tenant_id     text NOT NULL,
    owner_subject text NOT NULL,
    max_running   bigint NOT NULL CHECK (max_running >= 0),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, owner_subject)
);
