# ADR 0004 — Launch POST origin policy

Status: Accepted

## Context

`POST /v1/launch` redeems a one-use 60 s ticket: the portal submits a form
POST to the **workspace's own session host** (`<label>.<sessionDomain>`,
ADR 0005), the gateway redeems atomically through the broker, sets the
host-only `__Host-tcdi_session` cookie and 303-redirects to a clean URL
inside the portal's sandboxed iframe.

The launch POST crosses origins by design — the portal host and the session
hosts are different origins. Whether it is also *cross-site* depends on the
cookie mode (ADR 0005): `lax` deployments put portal and session domain on
the same registrable domain, `partitioned` covers cross-site. The CSRF and
session-fixation protection therefore cannot be "Origin equals the session
origin"; it is:

- the ticket itself — opaque, one-use, 60 s TTL, bound to the requesting
  user/tenant/workspace/runtimeGeneration;
- an explicit allowlist of portal origins the POST may come from;
- fetch metadata (`Sec-Fetch-Site`) as a hard browser-attribution gate.

## Decision

`Config.PortalOrigins` (flag `-portal-origin`, repeatable or CSV; env
`TCDI_PORTAL_ORIGINS`; default the origin of `-oidc-redirect-url`,
`internal/backend/config.go`) configures the allowlist. For
`POST /v1/launch` only:

- **Origin present** → accept iff it is an exact match
  (scheme+host+port, default ports normalized) of a configured portal
  origin **or** the request's own workspace origin
  (`launchOriginOK`, `internal/gateway/launch.go`).
  `Origin: null` never matches.
- **Sec-Fetch-Site present** → `same-origin`, `same-site` and `cross-site`
  are accepted **only together with** an allowlisted Origin; any other
  value is rejected (`403 bad_fetch_site`), and an absent/`null` Origin is
  rejected (`403 bad_origin`) — `internal/gateway/launch.go`. Fetch
  metadata is a forbidden header page JS cannot forge, so a site-labelled
  request with an allowlisted Origin is a real, attributable portal launch.
- **Neither header** → allowed: non-browser clients (curl, control/test
  tooling) send neither; the ticket requirement is unchanged.
- A failed validation never consumes the ticket.

Further current rules:

- A ticket carried in the **query string** is refused outright —
  `403 ticket_in_query` (`launch.go`): the bearer ticket rides the form
  body, never a URL (no logs, history or referrer copies).
- Every launch names the claiming browser tab: the request must carry a
  valid `tcdi_tab` (32 lowercase hex) or it is refused
  `400 owner_tab_required` **before** the ticket is redeemed
  (`launch.go`), so a bad request never burns the single-use
  ticket. The same id is what `ClaimStream` records as the lease's stream
  owner (ADR 0005's `stream_owner_tab`).
- WebSocket upgrades and every other session-host route keep the strict
  `Origin == workspace's own origin` rule — the portal-origin allowlist
  applies to `/v1/launch` only.
- **Session cookie.** `__Host-tcdi_session` is `Secure`, `HttpOnly`,
  `Path=/`, host-only; `SameSite` follows `-session-cookie-mode` — `lax`
  (default) or `partitioned` (`SameSite=None; Secure; Partitioned`,
  `config.go`). A `Strict` cookie would not be sent on the
  cross-site POST→303 top-level redirect nor on later cross-site
  top-level navigation; Lax still withholds it from subresource/fetch
  cross-site requests.
- **OIDC login binding.** `/v1/login` sets a `__Host-tcdi_login` cookie
  (Secure, HttpOnly, SameSite=Lax, Path=/) carrying AEAD-sealed login state
  (`internal/api/auth.go`); the callback requires a constant-time
  match against the sealed state. Because the state travels in the sealed
  cookie rather than process memory, a login started on one backend
  replica completes on another. A callback URL forwarded to a different
  browser cannot complete the login.
- **Portal `Referrer-Policy`.** The frontend sends
  `Referrer-Policy: strict-origin` (`build/frontend/main.go`), never
  `no-referrer`/`same-origin`: the browser derives the launch POST's
  `Origin` header from the page's effective referrer policy, and a
  referrer-suppressing policy makes it send `Origin: null` (or omit it) on
  a cross-site POST — which the fetch-metadata gate rejects as
  `bad_origin`. `strict-origin` keeps the privacy property: only the bare
  origin crosses, never the path or query. The gateway's own responses set
  no `Referrer-Policy`: its only Origin-bearing browser traffic is
  same-origin (the WebSocket upgrade and proxied desktop subresources
  carry the page origin regardless of referrer policy).
- **Session-domain validation.** `-session-domain` parses as
  `host[:port]` (`config.go`, `sessionhost.ParseDomain`), and the
  launch URL the API returns is `https://<label>.<sessionDomain>` — the
  SPA derives the session origin from `GET /v1/me`'s `sessionDomain` and
  posts the ticket only there.

## Consequences

- Browser launch works on both cookie topologies — same-site under `lax`,
  cross-site under `partitioned` — while a foreign site still cannot
  redeem: it cannot produce an allowlisted Origin (`Origin` is a forbidden
  header) and a site-labelled request without one is rejected.
- An empty portal-origin allowlist fails closed: browser launches 403
  rather than the policy silently widening.
- The ticket-in-query refusal keeps the bearer ticket out of browser
  history, logs and referrer headers.
