-- 013_sessions_principal_index.sql — index for the desktop-input idle touch.
--
-- Every recorded input event slides last_seen_at for the lease principal's
-- sessions (store.TouchPrincipal), matching on (issuer, subject). Without an
-- index that UPDATE scans the whole sessions table; with it the lookup is a
-- point read. The statement is idempotent so the file can be replayed safely.
CREATE INDEX IF NOT EXISTS sessions_issuer_subject ON sessions (issuer, subject);
