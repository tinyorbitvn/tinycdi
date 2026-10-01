-- 003_sessions.sql — server-side OIDC session store .
-- (002 is reserved for connections.)
--
-- The browser only ever holds the opaque session ID in a host-only cookie;
-- ID/access tokens are never persisted here. csrf_token is the synchronizer
-- token compared by RequireCSRF. Idle expiry is enforced by Get() sliding
-- last_seen_at; expires_at is the absolute cap.

CREATE TABLE sessions (
    id           text PRIMARY KEY,
    issuer       text NOT NULL,
    subject      text NOT NULL,
    tenant_id    text NOT NULL,
    groups       jsonb NOT NULL DEFAULT '[]',
    csrf_token   text NOT NULL,
    created_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    expires_at   timestamptz
);
CREATE INDEX sessions_expires_at ON sessions (expires_at)
    WHERE expires_at IS NOT NULL;
