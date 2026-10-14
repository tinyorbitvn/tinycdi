# ADR 0005 — Backend, frontend, operator and per-workspace session hosts

Status: Accepted

## Context

The platform serves one user-visible URL — the portal host — while each
workspace's session runs on its own isolated host. Users see sessions
inside the portal; runtime content is never served from the portal origin.
The design constraints behind that split:

- **One visible URL.** A session on a separate top-level host would be a
  second URL users bookmark and share, and the portal could not frame, size
  or annotate the session. Sessions render inside the portal instead.
- **Per-workspace session hosts.** A single session host would force one
  `__Host-tcdi_session` cookie (`Path=/`) shared by every workspace: a
  second workspace in the same browser would overwrite the first's cookie,
  and tenant-controlled runtime HTML/JS from workspace A would share an
  origin with workspace B's session. A subdomain per workspace gives each
  workspace its own cookie/storage/service-worker scope.
- **Shared session state.** Backend replicas are interchangeable: any
  replica rebuilds a session from the durable lease in Postgres, so a
  restart never kills live sessions and no sticky routing is needed.
- **Three components.** The gateway↔broker link (ADR 0003) runs in process;
  the frontend is a static SPA server that ships and scales independently
  of the stateful control plane.

## Decision

### Components and listeners

| Component | Image | Command | Listeners |
|---|---|---|---|
| **backend** | `tinycdi-backend` | `cmd/backend` | `:8443` app (public API on the portal host, `-listen`, `internal/backend/config.go:273`) · `:8444` session (session gateway on `*.<sessionDomain>`, `-session-listen`, `config.go:319`) · `:9443` internal mTLS (operator broker client, `-internal-listen`, `config.go:343`) · `:9090` metrics (**only** when `backend.metrics.enabled` — `-metrics-listen` defaults to empty, `config.go:354`; the chart renders it on its own ClusterIP Service, `deploy/helm/tinycdi/templates/backend.yaml:11,214`) |
| **frontend** | `tinycdi-frontend` | `build/frontend` | `:8443` static SPA + browser security headers — **no `/v1` proxy** |
| **operator** | `tinycdi-operator` | `cmd/operator` | reconciles `Workspace`/`WorkspaceTemplate`; calls the broker over mTLS for teardown revoke/drain |
| runtimes | `tinycdi-linux-base` (shared base), `tinycdi-linux-desktop`, `tinycdi-browser`, `kasmweb/*` via adapter | — | per-workspace pod endpoints (`docs/images.md`) |

The broker lives **in the backend process** (`broker.LocalGateway`,
`internal/broker/gatewayshim.go`); Postgres is the only state store.

Each listener is a separate `http.Server` with its own `tls.Config`,
certificate and route table built at startup. The session listener's mux
contains only gateway routes and never serves `/v1/*` API routes; the app
listener serves only the public API and never the gateway routes; the
internal listener serves only the broker's internal routes. A request for
another listener's route answers 404 (or 401 on the cookie-gated session
catch-all) — a misconfigured edge cannot turn the session host into an API
host.

A **split/test mode** exists: `-broker-url` makes the process a standalone
session gateway that calls the broker over the ADR 0003 mTLS contract
(`config.go:373`). The chart deploys the combined mode and refuses
`broker-url` in `backend.extraArgs`; split mode exists for end-to-end
tests.

### Hosts and routing

Every workspace is served on `<label>.<sessionDomain>` (e.g.
`session.cdi.example.com`):

- `<label>` is a deterministic lower-case DNS-1123 label (≤ 63 chars)
  derived from the platform workspace ID `ws_<hex>` by replacing `_` with
  `-` — the same mapping `provisioning.WorkspaceCRName` uses, so the host
  label equals the Workspace CR name. `internal/sessionhost` owns the
  workspace ID ↔ label ↔ session-domain mapping.
- The session-domain setting accepts `host[:port]` so dev stacks and the
  e2e harness can run on non-default ports.
- The edge (Gateway API `HTTPRoute` or `Ingress`) gets **one** wildcard
  rule for `*.<sessionDomain>` (`templates/httproute.yaml`) and **one**
  wildcard certificate.
- `portalHost` must differ from `sessionDomain` and must not sit inside it
  — the `*.<sessionDomain>` wildcard route would otherwise capture portal
  traffic (the chart fails the render otherwise, `templates/_helpers.tpl`).
- The portal host keeps path routing: `/v1/*` goes to `backend:8443`,
  everything else to `frontend:8443`, so the SPA and API stay same-origin.

**Why a subdomain per workspace:** the browser then isolates cookies,
storage and service workers per workspace host; the `__Host-` cookie
prefix keeps working; and every KasmVNC version can be proxied from the
runtime's own pod. The cost is on the installer: wildcard DNS and a
wildcard certificate (usually DNS-01).

### Session routing and host binding

- The session cookie is `__Host-tcdi_session` (host-only, `Secure`,
  `HttpOnly`, `Path=/`), set on **the workspace's own host**. Two
  workspaces in one browser profile never overwrite each other.
- `POST /v1/workspaces/{id}/connections` returns `launchUrl` on the
  workspace's own host. Launch is a form POST checked against the ADR 0004
  portal-origin allowlist; the 303 redirect lands inside the portal iframe.
- The session listener rejects **any** request whose host is outside
  `<sessionDomain>`, and any request whose host label does not match the
  `workspaceUID` of the lease bound to the presented cookie — a cookie
  minted for workspace A replayed on workspace B's host gets 401/403 and
  proxies nothing. Odd Host headers (upper case, extra labels such as
  `a.ws-x.<domain>`, a trailing dot, a wrong port, labels longer than
  63 chars) are rejected; they are never remapped to another workspace.
- WebSocket upgrades keep the strict rule, per workspace: `Origin` must
  equal the workspace host's own origin. Takeover inside one workspace
  follows the lease rules (ADR 0003).

### Framing and sandbox

The SPA route for a session embeds the session host in an iframe with:

- `sandbox="allow-scripts allow-same-origin allow-forms allow-pointer-lock"`
  — **never** `allow-top-navigation*`, `allow-popups*` or `allow-modals`
  (`SESSION_FRAME_SANDBOX`, `web/src/session/launch.ts`);
- `allow="clipboard-read <origin>; clipboard-write <origin>; fullscreen <origin>; keyboard-map <origin>"`,
  where `<origin>` is the workspace's own session origin. Each feature
  names it explicitly: the frame is navigated by a form POST and has no
  `src` attribute, so a bare feature name (default allowlist `'src'`) would
  delegate to the portal's own origin and nothing to the session.

Header contract:

| Origin | Header | Value |
|---|---|---|
| session | `Content-Security-Policy` | `frame-ancestors <portal-origin>`; `'none'` when no portal origin is configured (fail closed) — other directives unchanged |
| session | `Cross-Origin-Resource-Policy` | `same-origin` |
| session | `Origin-Agent-Cluster` | `?1` |
| portal | `frame-src` / `form-action` | `https://*.<sessionDomain>` / `'self' https://*.<sessionDomain>` |
| portal | `connect-src` | `'self' https://*.<sessionDomain>` (file-transfer content requests, ADR 0009 §5) |
| portal | `frame-ancestors` (+ `X-Frame-Options: DENY`) | `'none'` — the portal itself is never framed |
| portal | `Cross-Origin-Opener-Policy` | `same-origin` |

The gateway's `responsePolicy` strips upstream security headers (including
`X-Frame-Options`) and re-pins its own, so a hostile runtime cannot widen
`frame-ancestors`. Template clipboard policy is enforced server-side; the
`allow=` delegation does not bypass it.

There is **no `postMessage` channel** between the portal and the session
frame. The portal learns connection state from the API — the passive
endpoint `GET /v1/workspaces/{id}/connection`, which never slides the
portal idle timer — and reloads the iframe on disconnect with bounded
backoff and a retry cap. "Open in new tab" is the fallback for browsers
that block the embedding.

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
prefix: the SPA reads `csrfToken` and `sessionDomain` from `GET /v1/me`
(`internal/api/me.go:79-80`) and keeps them in memory, so no JS-readable
portal cookie exists a sibling host on the same registrable domain could
exploit with `Domain=` cookies.

### CSRF derivation

The portal CSRF token is **derived, not stored**:
`base64url(HMAC-SHA256(key = raw session ID, "tcdi-csrf-v2"))`
(`csrfTokenFor`, `internal/api/auth.go:1043-1050`). The session row keeps
only a MAC of the token, so a store read can never yield a usable token —
yet `/v1/me` can still return the token to the SPA because it is
recomputable from the session ID. Cookie-authenticated mutations keep the
existing checks: synchronizer token compared in constant time plus the
exact-Origin allowlist; the session domain is never added to that
allowlist.

### Session directory and stream epochs

Restart safety needs server-side session state that any replica can
rebuild. The durable piece is the `connection_lease` row in Postgres
(`internal/store/migrations/0001_baseline.sql`), which carries:

- `session_digest` — the SHA-256 digest of the session cookie value. The
  value itself is **never** stored.
- `stream_epoch` — a per-lease counter that fences interactive streams
  across replicas.
- `stream_owner_tab` — the claiming browser tab's id (32 lowercase hex);
  `stream_epoch = 0` exactly when it is `NULL` (the baseline's CHECK
  constraints `connection_lease_stream_owner_check` /
  `connection_lease_stream_owner_tab_check`).

The broker's session directory (`internal/broker/sessions.go`:
`BindSession`, `LeaseBySession`, `ClaimStream`, `ConnectionState`) is used
by the in-process gateway:

- **Rehydration.** A replica that receives a request with a valid cookie
  but has no in-memory session looks up the digest → lease → rebuilds the
  session (the upstream target resolves lazily). An expired or revoked
  lease is rejected. An unknown cookie costs one indexed lookup and a 401;
  no session object or goroutine is allocated.
- **One stream per lease, across replicas.** Every claim must carry a
  well-formed `tcdi_tab` (`ErrOwnerTabInvalid` otherwise); `ClaimStream`
  bumps `stream_epoch` and records the claiming tab as `stream_owner_tab`
  in the same row update, fencing every earlier stream of the lease — an
  older stream observing a newer epoch closes within one renew interval
  (`internal/broker/sessions.go:342-390`).
- **Takeover and revoke across replicas**: the old replica's 10 s renew
  sees the lease superseded or revoked and closes the socket; the 30 s
  bound (ADR 0003) stands.
- `gateway_id` on the lease is the identity of the **deployment**, shared
  by all replicas — not a per-pod identity.

### Readiness and cold connect

A workspace is Ready only once a ready EndpointSlice endpoint backs the
current pod — `endpointReady`
(`internal/runtime/linux/backend.go:595-600`): the kubelet's readiness
probe has already proved the stream port answers inside the pod, and the
EndpointSlice check proves the Service route the gateway dials has caught
up. Behind it the gateway retries a refused upstream dial within a 15 s
budget (`UpstreamDialBudget`, `internal/gateway/dial.go:11`). On a cold
first connect the portal re-mints a launch ticket with 1 s, 2 s, 4 s
backoff within a 60 s total budget (`COLD_BACKOFF_MS`, `COLD_BUDGET_MS`,
`web/src/session/coldConnect.ts:13-16`).

### Restart behaviour

- Pending OIDC logins live in an AEAD-sealed `__Host-tcdi_login` cookie;
  the sealing keys live in a shared Secret and several keys are accepted
  at once for rotation (`-login-key-file`). A login started on one replica
  completes on another.
- Every backend TLS listener hot-reloads its certificate when the files
  change (`internal/tlsreload`).
- Chart defaults: `replicas: 2`, `maxUnavailable: 0`, a PDB with
  `minAvailable: 1`, **preferred** node anti-affinity (a `required` rule
  would wedge one replica on single-node clusters). No sticky routing is
  needed: a request carrying a valid cookie succeeds on the replica that
  did not redeem the ticket.
- On SIGTERM the backend stops accepting new connections and closes
  WebSockets in order; the session page reconnects with the existing
  cookie — no new ticket — while the lease lives.
- The portal session keeps its 30 min idle / 12 h absolute timeouts
  (`-session-idle`, `config.go:294`; `Authenticator.AbsoluteTimeout`,
  `internal/api/auth.go:139`). Input activity on a user's lease extends
  **that user's** portal idle timer server-side (the client signal is
  never trusted), so a page left open on an active desktop does not idle
  out — while the passive connection-status poll never extends it.

### Blast radius of the merged gateway

The gateway is the most exposed code in the platform: it parses browser
input on a public host and relays responses and WebSocket frames from
tenant-influenced runtimes. Running it inside the backend means a full
gateway compromise reaches a process that additionally holds the database
DSN, the OIDC client secret, the Kubernetes credentials and the internal
listener's server key. **This is a real increase in blast radius, accepted**
because the components are trust-equivalent for session data. Mitigations:

- separate listeners, TLS configs and route tables (above), with tests
  asserting no route bleeds across listeners;
- the session hosts never share cookies with the API — different hosts,
  host-only `__Host-` cookies;
- the internal mTLS listener splits its two client populations: the
  workspace revoke/drain routes admit only the operator CN and the lease
  routes refuse it (ADR 0003). In the chart topology the gateway calls the
  broker in process, so no network-usable gateway identity exists to
  steal;
- the gateway code is wired with the narrow `BrokerClient` interface
  (`internal/gateway/proxy.go:37`), its TLS material, the upstream CA and
  the control token — its own dependencies stay narrow even though the
  hosting process holds more;
- pod hardening: distroless non-root image (`build/backend/Dockerfile`),
  read-only root filesystem, all capabilities dropped
  (`backend.securityContext` hardened defaults, `values.yaml:405-406`);
  upstream responses treated as hostile;
- the backend pod carries the `workspaces.cdi.tinyorbit.vn/role=gateway`
  label so per-workspace NetworkPolicy admits only it to runtimes, and only
  from the platform namespace (`internal/runtime/linux/backend.go:332-335`);
  ingress to `:8444` comes only from the edge and to `:9443` only from the
  operator.

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
- **Keep the gateway as a separate Deployment.** Rejected: it keeps the
  mTLS client PKI and the broker network hop for components that are
  always co-deployed and co-upgraded. Split mode (`-broker-url`) keeps the
  code path exercised for tests.
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
- Three platform Deployments (`backend`, `frontend`, `operator`); no
  gateway mTLS client Secret or CA wiring exists in the chart.
- Installers need wildcard DNS and a wildcard certificate for
  `*.<sessionDomain>`, and must pick hostnames that satisfy the cookie
  mode (same registrable domain for `lax`, or `partitioned` for
  cross-site).
- Parallel workspaces per browser work; cookie replay across hosts is
  rejected by host binding.
- Backend pods are interchangeable and restart-safe for sessions; frontend
  restarts never drop streams.
