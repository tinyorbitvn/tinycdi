-- sessions.id_token: the OIDC ID token retained at login so logout can
-- send id_token_hint (RP-initiated logout — the provider then skips its
-- own confirmation page). Only the login-state AEAD seal of the token is
-- stored here, never the raw JWT (SEC-27 spirit: no usable credential at
-- rest). NULL for sessions that predate this migration or whose seal no
-- longer opens under the configured keys — logout then falls back to a
-- client_id-only end-session URL.

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS id_token TEXT;
