-- 018_lease_stream_owner.sql — per-tab stream ownership evidence on
-- connection leases.
--
-- stream_owner_tab records the opaque tab id the claiming portal tab sent
-- with its stream claim (written in the same row update that bumps
-- stream_epoch, so a claim's epoch and owner can never diverge). The tab id
-- is a 128-bit random value the portal mints per browser tab; it lets the
-- portal tell "my stream was re-claimed by my own tab" (a backend restart,
-- a client retry, a frame reload) from "another tab took the stream over"
-- without comparing stream epochs, which falsely fired "open in another
-- tab" for two same-tab claims inside one poll interval.
--
--   * Nullable, no backfill: a claim that arrives without an id (legacy or
--     hand-rolled clients) stores NULL, which is never trusted as a match —
--     the portal falls back to the epoch comparison for those streams.
--   * Expand-only: replicas predating the column simply never write or
--     read it; the value is not a credential and carries no user data.

ALTER TABLE connection_lease
    ADD COLUMN IF NOT EXISTS stream_owner_tab text;
