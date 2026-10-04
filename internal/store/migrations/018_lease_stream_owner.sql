-- 018_lease_stream_owner.sql — per-tab stream ownership evidence on
-- connection leases, plus the portal-session binding that scopes it.
--
-- stream_owner_tab records the opaque tab id the claiming portal tab sent
-- with its stream claim (written in the same row update that bumps
-- stream_epoch, so a claim's epoch and owner can never diverge). The tab id
-- is a 128-bit random value the portal mints per browser page instance; it
-- lets the portal tell "my stream was re-claimed by my own tab" (a backend
-- restart, a client retry, a frame reload) from "another tab took the
-- stream over" without comparing stream epochs, which falsely fired "open
-- in another tab" for two same-tab claims inside one poll interval.
--
--   * stream_owner_epoch repeats the stream_epoch the id was recorded at:
--     a replica predating these columns bumps stream_epoch without naming
--     them, so an owner whose epoch no longer matches is stale evidence —
--     read as absent, never as a match.
--   * portal_session_digest is SHA-256 of the issuing portal session's id,
--     copied from the launch ticket at redemption: the owner id is only
--     ever reported to the portal session that minted the lease — never to
--     a second session of the same user. The session id itself never
--     reaches the database (same convention as session_digest).
--   * Nullable, no backfill: a claim that arrives without an id (legacy or
--     hand-rolled clients) stores NULL, which is never trusted as a match —
--     the portal falls back to the epoch comparison for those streams.
--   * Expand-only: replicas predating the columns simply never write or
--     read them; the value is not a credential and carries no user data.

ALTER TABLE connection_lease
    ADD COLUMN IF NOT EXISTS stream_owner_tab       text,
    ADD COLUMN IF NOT EXISTS stream_owner_epoch     bigint,
    ADD COLUMN IF NOT EXISTS portal_session_digest  bytea;

ALTER TABLE launch_ticket
    ADD COLUMN IF NOT EXISTS portal_session_digest  bytea;
