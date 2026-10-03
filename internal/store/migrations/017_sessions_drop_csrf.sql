-- sessions.csrf_token: the v0.1 synchronizer-token column. Since v0.2 the
-- token is derived from the session ID (P1) and served by GET /v1/me, so
-- the column carried only the MAC of the empty token and is dropped in
-- v0.3 (E14). Rows are untouched — no usable credential was ever at rest
-- here (the column held a session-ID-keyed HMAC, never a raw token).

ALTER TABLE sessions DROP COLUMN IF EXISTS csrf_token;
