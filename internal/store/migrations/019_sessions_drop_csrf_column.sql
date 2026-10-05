-- sessions.csrf_token: the contract half of the expand/contract started
-- by 017_sessions_drop_csrf.sql. 017 dropped NOT NULL so v0.2 replicas —
-- which name the column in every session SELECT/INSERT — could survive a
-- rolling upgrade into v0.3. v0.3 removed every code reference to the
-- column; a v0.3.x replica never names it, so dropping it now is
-- rolling-upgrade safe from v0.3.x (old binary + new schema never
-- touches the gone column).
--
-- It is NOT safe from v0.2.x replicas: they still SELECT/INSERT
-- csrf_token and fail the moment the column disappears. Upgrades must
-- therefore pass through v0.3.x — never jump v0.2.x -> v0.4 directly,
-- and do not roll back to v0.2 after this migration without restoring
-- the pre-upgrade dump (docs/runbooks/upgrade.md). The drop is
-- irreversible: there is no down migration and no rewrite of the rows.

ALTER TABLE sessions DROP COLUMN IF EXISTS csrf_token;
