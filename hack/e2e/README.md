# hack/e2e — release-candidate API smoke

`smoke.sh` is an **env-driven, API-level end-to-end smoke** for a live
TinyCDI deployment — the check a release candidate must pass before it
ships. It needs nothing but `curl`, `jq` and reachability to the portal
and a session host: no cluster access, no kubeconfig, no browser. The
browser-level smoke (Playwright) lives in `hack/quickstart/smoke/`; this
one covers the HTTP contract the release notes' "release-candidate
results" sections describe, so an rc can be re-verified from the repo
alone.

Tooling choice: bash + curl + jq, the same toolchain as the rest of
`hack/` — no build step and no new dependencies. curl supplies the exact
TLS, redirect and cookie-jar control the scripted OIDC code flow needs.

## Usage

```sh
# everything below is configuration; there are no positional arguments
TCDI_E2E_PORTAL_URL=https://portal.example.test \
TCDI_E2E_SESSION_HOST=ws-00000000.session.example.test \
TCDI_E2E_IDP_ISSUER=https://idp.example.test/realms/tinycdi \
TCDI_E2E_USER=e2e-user TCDI_E2E_PASSWORD='...' \
TCDI_E2E_TEMPLATE=browser \
  hack/e2e/smoke.sh
```

On the kind quickstart (`hack/quickstart/up.sh`) the values are:

```sh
TCDI_E2E_PORTAL_URL=https://portal.tcdi.localtest.me \
TCDI_E2E_SESSION_HOST=ws-00000000.session.tcdi.localtest.me \
TCDI_E2E_IDP_ISSUER=https://keycloak.tcdi.localtest.me/realms/tinycdi \
TCDI_E2E_USER=demo TCDI_E2E_PASSWORD=tcdi-demo-dev-only \
TCDI_E2E_TEMPLATE=browser \
TCDI_E2E_CA_FILE="$HOME/.local/state/tcdi-quickstart/tcdi-quickstart/tls/local-ca.crt" \
  hack/e2e/smoke.sh
```

`--anon-only` runs just the anonymous surface (portal host, session host,
security headers — no login, so `TCDI_E2E_IDP_ISSUER` / user credentials
are not required). `--sign-out-everywhere` adds the
`POST /v1/me/sessions:revoke-all` check: it signs in a second session and
asserts that session answers 401 after the first revokes everything.

## Environment

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `TCDI_E2E_PORTAL_URL` | yes | — | Portal base URL (`https://...`). |
| `TCDI_E2E_SESSION_HOST` | yes | — | A well-formed workspace session host `ws-<suffix>.<session-domain>` — `ws-00000000.<session-domain>` works; no workspace needs to exist. Also defines the session domain for the foreign-host check. |
| `TCDI_E2E_IDP_ISSUER` | unless `--anon-only` | — | Expected OIDC issuer URL. `/v1/login` must redirect to this host. |
| `TCDI_E2E_USER` | unless `--anon-only` | — | Test user login. |
| `TCDI_E2E_PASSWORD` | unless `--anon-only` | — | Test user password. Never printed. |
| `TCDI_E2E_TEMPLATE` | no | `browser` | Template **name** in `GET /v1/templates` to instantiate. |
| `TCDI_E2E_CA_FILE` | no | system roots | CA bundle for TLS verification (the quickstart CA lives under `…/tcdi-quickstart/tls/local-ca.crt`). |
| `TCDI_E2E_RESOLVE` | no | — | Comma-separated `curl --resolve` entries (`host:port:addr`) when hosts lack public DNS. |
| `TCDI_E2E_HTTP_TIMEOUT` | no | `15` | Per-request timeout, seconds. |
| `TCDI_E2E_READY_TIMEOUT` | no | `600` | Workspace `Ready` deadline, seconds. |
| `TCDI_E2E_DELETE_TIMEOUT` | no | `300` | Workspace delete/`Stopped` deadline, seconds. |
| `TCDI_E2E_MAX_WORKSPACES` | no | `4` | Cap on concurrently `Running` workspaces the quota check may hold (including the lifecycle one); the check `SKIP`s when the limit is above this probe budget. |
| `TCDI_E2E_NAME_PREFIX` | no | `e2e` | Workspace display-name prefix (`<prefix>-<epoch>-<n>`). |

## What it checks

Anonymous (`--anon-only` stops here):

- `portal-root` — `GET /` answers 200 (the SPA is served publicly).
- `security-headers` — the portal pins CSP (`default-src 'self'`),
  `X-Frame-Options: DENY`, `Cross-Origin-Opener-Policy: same-origin`,
  `Referrer-Policy: strict-origin`, `X-Content-Type-Options: nosniff`,
  HSTS.
- `healthz` / `readyz` — 200.
- `session-probe` — `GET /v1/session` answers `{"authenticated": false}`.
- `me-unauthenticated` / `templates-unauthenticated` — `/v1/me` and
  `/v1/templates` answer 401 without a session.
- `metrics-not-public` — `/v1/metrics` does not exist on the portal (404).
- `session-host-no-cookie` — a well-formed `ws-<suffix>.<session-domain>`
  host is the cookie-gated desktop proxy: no workspace cookie → 401. The
  session host must be named `ws-<suffix>.<session-domain>` — any other
  name is refused `421` by design.
- `session-host-foreign-421` — a non-workspace label under the session
  domain is misdirected traffic: 421 `bad_host`
  (`internal/gateway/proxy.go`). The probe uses the fixed name
  `e2e-foreign.<session-domain>` — on deployments without wildcard DNS it
  needs its own `TCDI_E2E_RESOLVE` entry just like the portal and session
  hosts (a DNS miss fails loudly as `-> 000`).
- `session-metrics-not-public` — `/metrics` does not exist on the session
  listener either (404).

Authenticated:

- `login` — the scripted OIDC authorization-code + PKCE flow: `/v1/login`
  redirects to the configured issuer host, the IdP credential form POST
  returns to `/v1/auth/callback`, and a session cookie authenticates
  `/v1/me`. Works with any IdP that serves an HTML username/password form
  (the quickstart Keycloak realm is the reference); an already-live SSO
  session is followed through the redirect chain instead.
- `me-authenticated` — `/v1/me` carries `tenant`, `sessionDomain`, and
  the `csrfToken` every mutation echoes in `X-CSRF-Token`.
- `templates` — the catalog lists `TCDI_E2E_TEMPLATE` and yields its
  `tpl_` ref.
- `workspace-create` / `workspace-ready` — `POST /v1/workspaces`
  (`desiredState: Running`) reaches `Ready` inside `READY_TIMEOUT`.
- `workspace-stop` / `workspace-start` — a stop → `Stopped` → start →
  `Ready` round trip.
- `quota-running-limit` — with running slots held up to the tenant /
  per-principal limit, the next `Running` create intent is refused
  `409 QUOTA_EXHAUSTED` (a create reserves the running slot up front;
  `details.reason: UserLimitReached` when a per-principal limit binds).
  `SKIP`s when the configured limit exceeds `TCDI_E2E_MAX_WORKSPACES`.
- `workspace-delete` — every workspace the smoke created deletes and
  disappears (404) inside `DELETE_TIMEOUT`.
- `logout` — `POST /v1/logout` ends the session (`/v1/me` → 401); the
  provider `endSessionUrl` is visited when the API returns one.
- `revoke-all` (`--sign-out-everywhere` only) — two live sessions;
  `POST /v1/me/sessions:revoke-all` on one leaves **both** answering 401.

Output is one `PASS`/`FAIL`/`SKIP <check>[: reason]` line per check; the
first `FAIL` exits non-zero. On any failure the exit trap best-effort
deletes the workspaces the run created — nothing is left running.

The smoke never prints credentials, cookies, CSRF tokens or launch
material: failures carry only the HTTP status, the API error `code` and,
where relevant, `details.reason`.
