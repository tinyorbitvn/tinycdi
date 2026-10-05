-- 020_portal_session_revocation.sql — indexes backing sign-out revocation
-- (threat-model S17, v0.4).
--
-- RevokePortalSession resolves a portal session's session-layer material by
-- the digest recorded on ticket issue and copied to the lease at redemption
-- (migration 018):
--
--   UPDATE launch_ticket    SET revoked_at = ...
--     WHERE portal_session_digest = $1 AND consumed_at IS NULL AND ...;
--   UPDATE connection_lease SET state = 'revoked', ...
--     WHERE portal_session_digest = $1 AND state = 'active';
--
-- Both tables are append-mostly and never pruned, so without an index every
-- sign-out scans them whole. Partial like the session_digest indexes (011):
-- the columns stay NULL on rows predating 018, which the revoke never
-- touches anyway. Expand-only — replicas predating the index simply
-- seq-scan until they roll forward.

CREATE INDEX IF NOT EXISTS connection_lease_portal_session
    ON connection_lease (portal_session_digest)
    WHERE portal_session_digest IS NOT NULL;

CREATE INDEX IF NOT EXISTS launch_ticket_portal_session
    ON launch_ticket (portal_session_digest)
    WHERE portal_session_digest IS NOT NULL;
