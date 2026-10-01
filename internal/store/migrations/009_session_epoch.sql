-- 009_session_epoch.sql — session epoch binding (defect F5).
--
-- Every session record binds to the epoch in platform_meta.session_epoch:
-- SessionStore.Get only accepts rows whose epoch equals the current value.
-- The epoch is minted once at install and rotated by the restore
-- procedure, so a session captured in a backup — including one revoked
-- after the backup was taken — can never be resurrected by loading the
-- dump into a fresh release: the rotated epoch rejects it and the user
-- re-authenticates.
--
-- Sessions written before this column existed carry epoch '' and are
-- rejected on first read (a one-time re-login at this upgrade); sessions
-- written after it survive normal pod restarts because the epoch persists
-- in the same database.

CREATE TABLE IF NOT EXISTS platform_meta (
    key   text PRIMARY KEY,
    value text NOT NULL
);
INSERT INTO platform_meta (key, value)
VALUES ('session_epoch', gen_random_uuid()::text)
ON CONFLICT (key) DO NOTHING;

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS epoch text NOT NULL DEFAULT '';
