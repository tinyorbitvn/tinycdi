# ADR 0004 — Launch POST origin policy (portal-origin allowlist)

Status: accepted (policy fixed in design review)

Date: 2026-09-30

## Context

`POST /v1/launch` (design §6.3) redeems a one-use 60 s ticket: the portal
page submits a form POST to the **session origin**, the gateway redeems
atomically through the broker, sets the host-only `__Host-tcdi_session`
cookie and 303-redirects to a clean URL.

End-to-end testing showed the original gate wrong: the handler required
`Origin == <session public origin>` and rejected `Sec-Fetch-Site:
cross-site` outright. But the launch POST is **cross-site by design** —
design §6 requires portal and session on different registrable domains, so
every real browser launch arrives with `Origin: https://<portal>` and
`Sec-Fetch-Site: cross-site`. The old rule could only ever pass for
non-browser clients and made browser launch impossible (403 `bad_origin`).

The actual CSRF / session-fixation protection is:

- the ticket itself — opaque, one-use, 60 s TTL, bound to the requesting
  user/tenant/workspace/runtimeGeneration;
- an explicit allowlist of portal origins the POST may come from;
- fetch metadata (`Sec-Fetch-Site`) as a hard browser-attribution gate.

## Decision

`Config.PortalOrigins` (flag `-portal-origin`, repeatable or CSV; env
fallback `TCDI_GW_PORTAL_ORIGINS`) configures the allowlist. For
`POST /v1/launch` only:

- **Origin present** → accept iff it is an exact match
  (scheme+host+port, default ports normalized) of a configured portal
  origin **or** the gateway's own public origin on the request authority
  (the old triple-match leg). `Origin: null` never matches.
- **Sec-Fetch-Site present** → `same-origin`, `same-site` and `cross-site`
  are accepted **only together with** an allowlisted Origin; any other
  value is rejected (`bad_fetch_site`), and an absent/`null` Origin is
  rejected (`bad_origin`). Fetch metadata is a forbidden header page JS
  cannot forge, so a cross-site label plus allowlisted Origin is a real,
  attributable portal launch.
- **Neither header** → allowed unchanged: non-browser clients (curl,
  control/test tooling) send neither; the ticket requirement is
  unchanged. This is the documented pre-metadata browser gap, closed on
  modern browsers by the fetch-metadata gate.
- A failed validation never consumes the ticket (existing rule).

WebSocket upgrades and every other desktop route keep the strict
`Origin == public origin` rule — the allowlist applies to `/v1/launch`
only.

## Configuration

- Helm: `deploy/helm/tinycdi` renders
  `-portal-origin=https://<portalHost>` plus `gateway.extraPortalOrigins`;
  the chart already fails render when portal and session hosts collide.
- Kind dev stack (removed before publication): the dev gateway manifest passed
  `-portal-origin=https://portal.tcdi.localtest.me`; the dev tooling rendered the
  derived portal host port (both hosts sat under `localtest.me`, so dev
  launches were same-site, not cross-site — both were accepted).

## Consequences

- Browser launch works on separate portal/session domains (fixed).
- A foreign site still cannot redeem: it cannot produce an allowlisted
  Origin (`Origin` is a forbidden header) and cross-site POSTs without
  fetch metadata are legacy-only.
- Operators must configure the portal origin; an empty allowlist fails
  closed (browser launches 403) rather than open.

## Amendments (2026-10-01 security fixes)

- **SEC-26:** the API's `-session-origin` must now be a bare `https://`
  origin — no path, query, fragment, or userinfo — and is normalized
  (lowercase host, default port dropped) before it is used to build
  `launchUrl`. At login the API additionally publishes that origin to the
  SPA in a JS-readable `tcdi_session_origin` cookie (Secure, SameSite=Lax,
  Path=/, session lifetime). `launchSession` in the portal refuses to
  submit the ticket form when `launchUrl`'s origin differs from the
  configured origin, or when the cookie is absent — the bearer-equivalent
  ticket can never be POSTed to a foreign origin.
- **SEC-03:** OIDC logins are browser-bound: `/v1/login` sets a
  `__Host-tcdi_login` cookie (Secure, HttpOnly, SameSite=Lax, Path=/,
  Max-Age=600) whose SHA-256 is stored with the pending state; the
  callback requires a constant-time match and expires the cookie. A
  callback URL forwarded to a different browser can no longer complete the
  login.
- **Session cookie `SameSite`:** `__Host-tcdi_session` is `SameSite=Lax` (Secure,
  HttpOnly, Path=/, host-only — `__Host-` rules unchanged). The launch
  POST is cross-site by design and a `Strict` cookie is not sent on the
  POST→303 top-level redirect nor on later cross-site top-level
  navigation, so the desktop loaded "unauthorized" on every real launch.
  Lax still withholds the cookie from subresource/fetch cross-site
  requests; launch validation (Origin/Sec-Fetch-Site/Host) is unchanged.
- **SEC-I5 (operational):** pending OIDC logins live in an in-memory map
  and gateway session/lease state is likewise per-replica. A multi-replica
  deployment MUST route a given browser consistently — sticky routing
  (session affinity) on the portal/API and session ingress — or run a
  single replica; a callback that lands on a different replica than the
  one that served `/v1/login` cannot complete the login.
- **Portal `Referrer-Policy`:** the portal MUST send
  `Referrer-Policy: strict-origin` (or `strict-origin-when-cross-origin`),
  never `no-referrer`/`same-origin`. The browser derives the launch form
  POST's `Origin` header from the page's effective referrer policy: a
  referrer-suppressing policy makes it send `Origin: null` (or omit it)
  on the cross-site POST, which the fetch-metadata gate above rejects as
  `bad_origin` — reproduced end-to-end in headless Chromium, every launch
  403s (security review 20261001, `e2e-referrer-variants.log`).
  `strict-origin` keeps the privacy property — only the bare origin
  crosses, never the path or query. Verified by `TestSecurityHeaders`
  (`build/portal`) and by `web/tests-portal/portal-csp.spec.ts`, where
  the mocked session origin enforces this same gate against the real
  portal binary (positive flow plus a `no-referrer` negative control).
  The gateway's own responses set no `Referrer-Policy`; its only
  Origin-bearing browser traffic is same-origin (the WebSocket upgrade
  and proxied desktop subresources carry the page origin regardless of
  referrer policy), so the browser default suffices — but if a policy is
  ever added there it must not suppress the referrer for the same reason.
