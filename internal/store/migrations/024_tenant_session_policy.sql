-- 024_tenant_session_policy.sql — per-tenant session data-transfer
-- ceilings (ADR 0010, T10).
--
-- Model:
--   * tenant_session_policy holds the tenant administrator's ceiling over
--     every WorkspaceTemplate's own transfer policy: at most one row per
--     tenant, keyed by tenant_id. A NULL column means FollowTemplate —
--     that channel is not narrowed — and the write path deletes the row
--     when every ceiling column is NULL, so "no row" stays the single
--     representation of the default. A fresh upgrade has no rows
--     anywhere, so nothing changes until a tenant administrator acts.
--   * launch_ticket.session_policy records the resolved vector (the meet
--     of the template policy and the ceiling) at ticket issue, as JSON —
--     the audit record of which policy applied. clipboard_policy keeps
--     its existing meaning for the gateway redirect; NULL for tickets
--     minted before the column or when the policy cannot be resolved.
--
-- Expand-only and rolling-upgrade safe: replicas predating the table
-- never read or write it, and a NULL launch_ticket.session_policy reads
-- exactly like a pre-column ticket. Rollback leaves an orphan table and
-- column — harmless, droppable later.

CREATE TABLE IF NOT EXISTS tenant_session_policy (
    tenant_id      text PRIMARY KEY,
    clipboard      text CHECK (clipboard IN
                       ('Disabled','Send','Receive','Bidirectional')),
    file_transfer  text CHECK (file_transfer IN
                       ('Disabled','Upload','Download','Bidirectional')),
    file_max_bytes bigint CHECK (file_max_bytes IS NULL OR file_max_bytes > 0),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE launch_ticket ADD COLUMN IF NOT EXISTS session_policy text;
