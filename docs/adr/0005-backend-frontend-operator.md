# ADR 0005 — backend/frontend/operator, per-workspace session hosts

Status: accepted (v0.2 architecture). Not independently security-reviewed;
a review is planned before v1.0.

Date: 2026-10-02

Supersedes: the transport section of [ADR 0003](0003-gateway-broker-internal-api.md)
for the gateway caller (the gateway↔broker calls become in-process); amends
[ADR 0004](0004-launch-origin-policy.md) (the launch form now targets the
workspace's own host inside a portal iframe). Both ADRs carry a pointer
back here; their wire contract and origin policy are unchanged.

## Context

v0.1 ships **four platform Deployments** plus the runtime pods:

| v0.1 component | Image | Serves |
|---|---|---|
| `portal` | `tinycdi-portal` (`build/portal`) | the SPA **and** a reverse proxy of `/v1/*` to the api (portal + API share one origin) |
| `api` | `tinycdi-api` (`cmd/api`) | public REST API on plain HTTP `:8080` (reached only through the portal proxy) + the broker's internal mTLS listener `:9443` |
| `gateway` | `tinycdi-gateway` (`cmd/gateway`) | the session origin: `/v1/launch`, the cookie-gated desktop proxy, websockify, `/v1/control/*`; calls the broker over mTLS |
| `operator` | `tinycdi-operator` (`cmd/operator`) | reconciles `Workspace`/`WorkspaceTemplate`; calls the broker over mTLS for teardown revoke/drain |

Pain points observed running v0.1:

- **Two visible URLs.** Opening a desktop navigated a new top-level tab to
  the session host. Users bookmarked and shared the session host; the
  portal had no way to frame, size, fullscreen or annotate the session.
- **Four deployments for one control plane.** The portal's `/v1` proxy is
  a second HTTP hop that exists only to keep the API same-origin with the
  SPA, and the gateway↔broker mTLS link (ADR 0003) needs its own client
  PKI and adds a network failure mode between components that are always
  deployed and upgraded together.
- **One session host for every workspace.** The gateway sets a single
  `__Host-tcdi_session` cookie with `Path=/` for the whole session host
  (`internal/gateway/launch.go`), so a second workspace opened in the same
  browser overwrites the first workspace's cookie. The gateway also
  proxies tenant-controlled HTML/JS from the runtime pod, so script from
  workspace A ran on the same origin as workspace B's session.
- **Session state was per-process.** Gateway sessions lived in three
  in-memory maps, pending OIDC logins lived in RAM, and TLS certificates
  loaded once at startup — so every component ran `replicas: 1` and a
  gateway restart killed every live session.
- **The portal could not grow.** The v0.2 UX (workspace, admin and data
  views, in-portal session, design system) is a frontend concern that
  should ship and scale independently of the API process.

Decisions taken in the v0.2 design review (2026-10-02):

1. **One visible URL.** Users only ever use the portal host. Sessions are
   shown **inside** the portal; each workspace keeps its own isolated
   session host behind the scenes. Runtime content is **never** served
   from the portal origin.
2. **Three components:** backend, frontend, operator.
3. **Sessions survive a replica restart.** Any backend replica can rebuild
   a session from the durable lease in Postgres; the default is two
   replicas with no sticky routing.

## Decision

### Components and listeners

| v0.2 component | Image | Command | Listeners |
|---|---|---|---|
| **backend** | `tinycdi-backend` | `cmd/backend` | `:8443` app (public API on the portal host) · `:8444` session (session gateway on `*.<sessionDomain>`) · `:9443` internal mTLS (operator broker client) · `:9090` metrics (optional) |
| **frontend** | `tinycdi-frontend` | `build/frontend` | `:8443` static SPA + browser security headers — **no `/v1` proxy** |
| **operator** | `tinycdi-operator` | `cmd/operator` | unchanged |
| runtimes | `tinycdi-linux-desktop`, `tinycdi-browser`, `kasmweb/*` via adapter | — | unchanged |

The `tinycdi-api`, `tinycdi-gateway` and `tinycdi-portal` images and
commands are removed. The chart deploys exactly these three control-plane
Deployments.

Each listener is a separate `http.Server` with its own `tls.Config`,
certificate and route table built at startup. The session listener's mux
contains only gateway routes and never serves `/v1/*` API routes; the app
listener serves only the public API and never the gateway routes; the
internal listener serves only the broker's internal routes. A request for
another listener's route answers 404 (or 401 on the cookie-gated session
catch-all) — a misconfigured edge cannot turn the session host into an API
host.

The **split/test mode** is kept: `--components` selects which listeners a
backend process runs, and with `session` alone the process is a standalone
session gateway that calls the broker over the unchanged ADR 0003 mTLS
contract. The chart deploys the combined mode; split mode exists for
end-to-end tests and as the documented way back to a separate gateway.
Rehydration (below) is an optional interface of the in-process broker; a
split gateway runs without it and a restart there drops streams, as in
v0.1.

### Hosts and routing

`sessionHost` is replaced by a **session domain**. Each workspace is
served on `<label>.<sessionDomain>` (e.g. `session.cdi.example.com`):

- `<label>` is a deterministic lower-case DNS-1123 label (≤ 63 chars)
  derived from the platform workspace ID `ws_<hex>` by replacing `_` with
  `-` — the same mapping `provisioning.WorkspaceCRName` uses, so the host
  label equals the Workspace CR name. `internal/sessionhost` owns the
  workspace ID ↔ label ↔ session-domain mapping.
- The session-domain setting accepts `host[:port]` so dev stacks and the
  e2e harness can run on non-default ports.
- The edge (Gateway API `HTTPRoute` or `Ingress`) gets **one** wildcard
  rule for `*.<sessionDomain>` and **one** wildcard certificate.
- The portal host keeps path routing: `/v1/*` goes to `backend:8443`,
  everything else to `frontend:8443`, so the SPA and API stay same-origin
  exactly as with the v0.1 proxy. Chart values `sessionHost`, `api.*`,
  `gateway.*` and `portal.*` fail render with a migration hint.

**Why a subdomain per workspace:** the browser then isolates cookies,
storage and service workers per workspace host; the `__Host-` cookie
prefix keeps working; and every KasmVNC version can be proxied from the
runtime's own pod. The cost is on the installer: wildcard DNS and a
wildcard certificate (usually DNS-01).

### Session routing and host binding

- The session cookie stays `__Host-tcdi_session` (host-only, `Secure`,
  `HttpOnly`, `Path=/`) and is set on **the workspace's own host**. Two
  workspaces in one browser profile no longer overwrite each other.
- `POST /v1/workspaces/{id}/connections` returns `launchUrl` on the
  workspace's own host. Launch is still a form POST checked against the
  ADR 0004 portal-origin allowlist; the 303 redirect now lands inside the
  portal iframe.
- The session listener rejects **any** request whose host is outside
  `<sessionDomain>`, and any request whose host label does not match the
  `workspaceUID` of the lease bound to the presented cookie — a cookie
  minted for workspace A replayed on workspace B's host gets 401/403 and
  proxies nothing. Odd Host headers (upper case, extra labels such as
  `a.ws-x.<domain>`, a trailing dot, a wrong port, labels longer than
  63 chars) are rejected; they are never remapped to another workspace.
- WebSocket upgrades keep the strict rule, now per workspace: `Origin`
  must equal the workspace host's own origin. Takeover inside one
  workspace follows the existing rules.

### Framing and sandbox

The SPA route `/workspaces/:id/session` embeds the session host in an
iframe with:

- `sandbox="allow-scripts allow-same-origin allow-forms allow-pointer-lock"`
  — **never** `allow-top-navigation*`, `allow-popups*` or `allow-modals`;
- `allow="clipboard-read <origin>; clipboard-write <origin>; fullscreen <origin>; keyboard-map <origin>"`,
  where `<origin>` is the workspace's own session origin. Each feature names
  it explicitly: the frame is navigated by a form POST and has no `src`
  attribute, so a bare feature name (default allowlist `'src'`) would
  delegate to the portal's own origin and nothing to the session.

Header contract:

| Origin | Header | Value |
|---|---|---|
| session | `Content-Security-Policy` | `frame-ancestors <portal-origin>`; `'none'` when no portal origin is configured (fail closed) — other directives unchanged |
| session | `Cross-Origin-Resource-Policy` | `same-origin` |
| session | `Origin-Agent-Cluster` | `?1` |
| portal | `frame-src` / `form-action` | `https://*.<sessionDomain>` / `'self' https://*.<sessionDomain>` |
| portal | `frame-ancestors` (+ `X-Frame-Options: DENY`) | `'none'` — the portal itself is never framed |
| portal | `Cross-Origin-Opener-Policy` | `same-origin` |

The gateway's `responsePolicy` keeps stripping upstream security headers
(including `X-Frame-Options`) and re-pins its own, so a hostile runtime
cannot widen `frame-ancestors`. Template clipboard policy is still
enforced server-side; the `allow=` delegation does not bypass it.

There is **no `postMessage` channel** between the portal and the session
frame. The portal learns connection state from the API — the passive
endpoint `GET /v1/workspaces/{id}/connection`, which never slides the
portal idle timer — and reloads the iframe on disconnect with bounded
backoff and a retry cap. "Open in new tab" remains the fallback for
browsers that block the embedding.

### Cookie modes

| Mode | Cookie attributes | Requirement |
|---|---|---|
| `lax` (default) | `SameSite=Lax` | portal and session domain on the **same registrable domain** (same eTLD+1), both `https` |
| `partitioned` | `SameSite=None; Secure; Partitioned` | cross-site deployments; requires CHIPS support and is covered by e2e in CI |

A cross-site iframe is a third-party context: a `Lax` cookie is not sent
on its navigations, subresources or WebSocket, so the desktop would load
"unauthorized". `partitioned` keys the cookie to the top-level site (the
portal), so it works inside the portal's iframe and is invisible under any
other top-level site. Browsers that block third-party cookies without
partition support cannot embed a cross-site session; the new-tab fallback
still works there.

The default mode requires **every** portal cookie to carry the `__Host-`
prefix: the JS-readable `tcdi_csrf` and `tcdi_session_origin` cookies are
removed, and the SPA reads `csrfToken` and `sessionDomain` from
`GET /v1/me` and keeps them in memory. Without that, a sibling host on the
same registrable domain could toss `Domain=` cookies at the portal.

### CSRF derivation

The portal CSRF token is **derived, not stored**:
`HMAC-SHA256(key = raw session ID, "tcdi-csrf-v2")`. The session row keeps
only a MAC of the token, so a store read can never yield a usable token —
yet `/v1/me` can still return the token to the SPA because it is
recomputable from the session ID. Cookie-authenticated mutations keep the
existing checks: synchronizer token compared in constant time plus the
exact-Origin allowlist; the session domain is never added to that
allowlist.

### Session directory and stream epochs

Restart safety needs server-side session state that any replica can
rebuild. The durable piece already exists: the `connection_lease` row in
Postgres. Migration `011_lease_session.sql` adds:

- `session_digest` — the SHA-256 digest of the session cookie value. The
  value itself is **never** stored.
- `stream_epoch` — a per-lease counter that fences interactive streams
  across replicas.

The broker grows a session directory (`internal/broker/sessions.go`:
`BindSession`, `LeaseBySession`, `ClaimStream`, `ConnectionState`) used by
the in-process gateway:

- **Rehydration.** A replica that receives a request with a valid cookie
  but has no in-memory session looks up the digest → lease → rebuilds the
  session (the upstream target resolves lazily). An expired or revoked
  lease is rejected. An unknown cookie costs one indexed lookup and a 401;
  no session object or goroutine is allocated.
- **One stream per lease, across replicas.** `ClaimStream` bumps
  `stream_epoch`; an older stream observing a newer epoch closes within
  one renew interval. The v0.1 single-stream rule was per-process;
  `stream_epoch` makes it hold across replicas.
- **Takeover and revoke across replicas** are unchanged in spirit: the old
  replica's 10 s renew sees the lease superseded or revoked and closes the
  socket; the existing 30 s bound stands.
- `gateway_id` on the lease is the identity of the **deployment**, shared
  by all replicas — not a per-pod identity.

### Restart behaviour

- Pending OIDC logins move out of process memory into an AEAD-sealed
  `__Host-tcdi_login` cookie; the key lives in a shared Secret and two
  keys are accepted at once for rotation. A login started on one replica
  completes on another.
- Every backend TLS listener hot-reloads its certificate when the files
  change.
- Chart defaults: `replicas: 2`, `maxUnavailable: 0`, a PDB with
  `minAvailable: 1`, **preferred** node anti-affinity (a `required` rule
  would wedge one replica on single-node clusters). No sticky routing is
  needed: a request carrying a valid cookie succeeds on the replica that
  did not redeem the ticket.
- On SIGTERM the backend stops accepting new connections and closes
  WebSockets in order; the session page reconnects with the existing
  cookie — no new ticket — while the lease lives. Deleting one backend pod
  mid-session resumes the stream within 15 s without user action, and a
  rolling restart of both replicas forces no re-launch.
- The portal session keeps its 30 min idle / 12 h absolute timeouts.
  Input activity on a user's lease extends **that user's** portal idle
  timer server-side (the client signal is never trusted), so a page left
  open on an active desktop does not idle out — while the passive
  connection-status poll never extends it.

### Blast radius of merging the gateway into the backend

The gateway is the most exposed code in the platform: it parses browser
input on a public host and relays responses and WebSocket frames from
tenant-influenced runtimes. In v0.1 a full gateway compromise exposed its
mTLS identity (and through it the runtime credentials of every lease that
identity held). In v0.2 the same compromise reaches the backend process,
which additionally holds the database DSN, the OIDC client secret, the
Kubernetes credentials and the internal listener's server key. **This is a
real increase in blast radius, accepted for v0.2** because the components
were already trust-equivalent for session data. Mitigations:

- separate listeners, TLS configs and route tables (above), with tests
  asserting no route bleeds across listeners;
- the session hosts never share cookies with the API — different hosts,
  host-only `__Host-` cookies;
- the internal mTLS listener is **operator-only**: it mounts only the
  workspace revoke/drain routes and refuses every other client identity,
  so no network-usable gateway identity exists to steal;
- the gateway is wired with the `BrokerClient` interface, its TLS
  material, the upstream CA and the control token only — not the database
  handle, OIDC configuration or Kubernetes client;
- pod hardening (unchanged, now load-bearing): distroless non-root image,
  read-only root filesystem, all capabilities dropped, `RuntimeDefault`
  seccomp; upstream responses treated as hostile;
- the backend pod carries the `workspaces.cdi.tinyorbit.vn/role=gateway`
  label so per-workspace NetworkPolicy admits only it to runtimes; ingress
  to `:8444` comes only from the edge and to `:9443` only from the
  operator;
- **reversible:** the `BrokerClient` boundary plus split mode
  (`--components=session` + mTLS broker client) make a split-back a wiring
  change. Deployments serving hostile tenants should revisit this first.

## Alternatives considered

- **Single host + path prefix** (`portal.example.com/session/<ws>/…`
  proxied to the runtime). Rejected: one `__Host-` `Path=/` cookie cannot
  express per-workspace scope, so workspaces would still overwrite each
  other; worse, runtime HTML/JS is tenant-influenced and the KasmVNC
  client needs `script-src 'unsafe-inline'` — on the portal origin that
  script gets the portal's DOM, storage and same-origin API access, and
  the portal CSP would have to drop to the runtime's needs.
- **Backend serves a pinned KasmVNC client.** Rejected: the TinyCDI image
  pins KasmVNC 1.5.0 (`docs/compatibility.md`) while `kasmweb/*` images
  carry 1.4.0 (`docs/kasm-images.md`), so one bundled client cannot match
  every runtime; proxying the runtime's own client works with any version.
- **Keep the gateway as a separate Deployment.** Rejected for v0.2: it
  keeps the mTLS client PKI and the broker network hop for components that
  are always co-deployed and co-upgraded. The `BrokerClient` interface and
  split mode keep this path open (see *Blast radius*).
- **Keep the frontend `/v1` reverse proxy.** Rejected: an extra hop and an
  extra place to get headers, body limits and streaming wrong; path
  routing at the edge gives the same single origin.
- **Merge the SPA into the backend.** Rejected: couples UI releases and
  scaling to the stateful control plane and puts static assets in the
  session gateway's restart domain.
- **`SameSite=None` without `Partitioned`.** Rejected: an unpartitioned
  third-party cookie is sent to the session host from any top-level site
  and is blocked by browsers phasing out third-party cookies.
- **Shared `Domain=<registrable-domain>` cookie.** Rejected: any sibling
  host could set or overwrite it; `__Host-` forbids it by design.

## Consequences

- One user-visible URL; sessions render inside the portal in a sandboxed
  iframe, with "open in new tab" as fallback.
- Three platform Deployments (`backend`, `frontend`, `operator`); the
  gateway mTLS client Secret and CA wiring disappear.
- Installers need wildcard DNS and a wildcard certificate for
  `*.<sessionDomain>`, and must pick hostnames that satisfy the cookie
  mode (same registrable domain for `lax`, or `partitioned` for
  cross-site).
- Parallel workspaces per browser now work; cookie replay across hosts is
  rejected by host binding.
- Backend pods are interchangeable and restart-safe for sessions; frontend
  restarts never drop streams.
- `SECURITY.md` sections written against v0.1 — the threat-model
  paragraph, the B.2 hardening rule that puts portal and session on
  different registrable domains, and the `tcdi_csrf` text — are stale;
  the security reviewer rewrites them at the G2 gate.
- `docs/runbooks/upgrade.md` gains the v0.1 → v0.2 migration (images,
  values, wildcard DNS/TLS, rollback).
