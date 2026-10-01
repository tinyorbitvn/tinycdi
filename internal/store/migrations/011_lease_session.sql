-- 011_lease_session.sql — session-cookie digests and stream epochs on
-- connection leases (design §3.6 restart-safe sessions).
--
-- Invariants enforced here:
--   * session_digest stores ONLY the SHA-256 of the gateway session cookie
--     value — the cookie itself never reaches the DB, so any backend
--     replica can rebuild a session from cookie → digest → live lease.
--   * The partial unique index guarantees a digest resolves to at most one
--     ACTIVE lease at a time; dead leases keep their digest so a replayed
--     cookie still fails closed with "revoked" instead of "unknown".
--   * stream_epoch is a per-lease monotonically increasing stream fence:
--     every interactive stream claims the next epoch, so when the same
--     cookie reaches two replicas only the newest stream survives.

ALTER TABLE connection_lease
    ADD COLUMN IF NOT EXISTS session_digest bytea,
    ADD COLUMN IF NOT EXISTS stream_epoch   bigint NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX IF NOT EXISTS connection_lease_session_digest
    ON connection_lease (session_digest)
    WHERE session_digest IS NOT NULL AND state = 'active';
