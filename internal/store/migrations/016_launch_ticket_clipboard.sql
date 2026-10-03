-- launch_ticket.clipboard_policy: the workspace template's clipboard
-- policy recorded at issue, so the gateway's post-redemption redirect can
-- re-assert the KasmVNC client's clipboard flags (its embed mode disables
-- every direction by default). NULL for tickets minted before the column
-- or when the policy cannot be resolved — the redirect then appends the
-- least-privilege pair (both directions off).

ALTER TABLE launch_ticket ADD COLUMN IF NOT EXISTS clipboard_policy TEXT;
