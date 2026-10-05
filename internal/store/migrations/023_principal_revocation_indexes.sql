-- 023_principal_revocation_indexes.sql — indexes backing
-- sign-out-everywhere (ADR 0007): the principal-scoped revocation resolves
-- a (tenant_id, principal_subject) pair's outstanding launch tickets and
-- active connection leases:
--
--   UPDATE launch_ticket    SET revoked_at = ...
--     WHERE tenant_id = $t AND principal_subject = $p
--       AND consumed_at IS NULL AND revoked_at IS NULL;
--   UPDATE connection_lease SET state = 'revoked', ...
--     WHERE tenant_id = $t AND principal_subject = $p AND state = 'active';
--
-- Both tables are append-mostly and never pruned, so without an index
-- every sign-out-everywhere scans them whole. Partial like the
-- portal_session_digest indexes (020): only rows a revoke can still touch
-- are indexed. Expand-only — replicas predating the index simply seq-scan
-- until they roll forward.

CREATE INDEX IF NOT EXISTS connection_lease_principal_active
    ON connection_lease (tenant_id, principal_subject)
    WHERE state = 'active';

CREATE INDEX IF NOT EXISTS launch_ticket_principal_outstanding
    ON launch_ticket (tenant_id, principal_subject)
    WHERE consumed_at IS NULL AND revoked_at IS NULL;
