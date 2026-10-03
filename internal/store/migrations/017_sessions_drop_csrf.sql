-- sessions.csrf_token: the v0.1 synchronizer-token column. Since v0.2 the
-- token is derived from the session ID (P1) and served by GET /v1/me, so
-- the column carried only the MAC of the empty token. v0.3 removes every
-- code reference to it but KEEPS the column (expand/contract): a
-- still-running v0.2 replica names it in every session SELECT/INSERT
-- until it terminates in a rolling upgrade. Dropping NOT NULL lets the
-- v0.3 insert — which no longer writes the column — coexist with old
-- replicas that still do. The column itself drops in v0.4.

ALTER TABLE sessions ALTER COLUMN csrf_token DROP NOT NULL;
