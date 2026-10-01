-- 012_principal_directory.sql — display identity for the portal.
--
--   * sessions gain display_name/email so GET /v1/me can show the caller's
--     name without re-reading the ID token. Both come from the verified ID
--     token at login and carry no authorization meaning.
--   * principal_directory maps an owner reference (issuer|sub, the same
--     value workspaces.owner_subject stores) to the display name last seen
--     at login, scoped by tenant. It lets tenant views (quota per user,
--     workspace/data owners) show names instead of opaque subjects. Rows
--     are upserted on every successful login and never used for authz.
--
-- The statements are idempotent so the file can be replayed safely.
ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS display_name text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS email        text NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS principal_directory (
    tenant_id     text NOT NULL,
    owner_ref     text NOT NULL,
    subject       text NOT NULL,
    display_name  text NOT NULL DEFAULT '',
    email         text NOT NULL DEFAULT '',
    last_login_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, owner_ref)
);
