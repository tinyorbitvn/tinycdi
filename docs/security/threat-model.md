# Threat model — v1.0 security review preparation

Status: **working document for the independent security review planned before
v1.0** (the review is deferred by product-owner decision; the architecture
ADRs already note they were merged without independent review — see
`docs/adr/0005-backend-frontend-operator.md`). This document inventories what
exists **today on `main`**, with code and test references, so the reviewer can
spend time on judgement rather than discovery. The normative policy for
contributors and operators stays in [`../../SECURITY.md`](../../SECURITY.md);
this file describes, it does not prescribe.

Every claim below was verified against the tree when written. Where a design
decision or review question comes from the project's post-MVP review it is
cited as **A6-S\<n\>** (the numbered "questions for the v1.0 security
reviewer" list), **D\<n\>** (design decisions) or **E\<n\>** (v0.3 decisions).

## 1. System in one paragraph

TinyCDI is a Kubernetes-native browser/desktop-as-a-service platform. Three
components ship as images: **frontend** (static SPA + browser security
headers; does not proxy `/v1`), **backend** (public API, session gateway, and
in-process broker on four listeners) and **operator** (reconciles
`Workspace`/`WorkspaceTemplate` CRDs, creates runtime pods, Secrets, Services
and per-workspace NetworkPolicies in managed namespaces). Users authenticate
through an external OIDC IdP, get a launch ticket, and their browser is
reverse-proxied to a per-workspace KasmVNC server running inside a pod that is
treated as **tenant-controlled** (the user has a shell in it —
`SECURITY.md:93-96`). Containers share the host kernel; the product does not
claim isolation between mutually hostile tenants.

Backend listeners (`internal/backend/config.go`, `listeners.go`):

| Listener | Flag / port | Serves |
|---|---|---|
| app | `-listen` `:8443` | public API `/v1/*` for browsers (OIDC login, workspace CRUD, ticket issue) |
| session | `-session-listen` `:8444` | per-workspace hosts `<label>.<sessionDomain>`: launch POST + desktop proxy + in-cluster control surface |
| internal | `-internal-listen` `:9443` | mTLS broker API used by the operator (ADR 0003) |
| metrics | `:9090` | scrape-only metrics |

## 2. Assets

- **User accounts and sessions** — OIDC identity, server-side portal session
  (`__Host-tcdi_session`), retained `id_token` used as `id_token_hint` at
  logout.
- **Workspace sessions** — the interactive KasmVNC stream, clipboard content,
  and anything the user does inside the desktop (which has Internet egress).
- **Workspace data** — persistent home volumes and retained disks.
- **Credentials and keys** — OIDC client secret, AEAD login-state sealing keys,
  session cookie values, launch tickets, per-workspace KasmVNC credentials,
  internal mTLS certificates and CA bundles, Postgres DSN, session-listener
  control bearer token.
- **Control plane** — Postgres (sessions, leases, tickets, quotas, principal
  directory), the `Workspace`/`WorkspaceTemplate` CRDs and the managed
  namespaces the operator writes to.
- **Supply chain** — published images and Helm chart, their signatures, SBOMs
  and provenance attestations; the CI workflows that produce them.
- **Availability** — backend replicas, Postgres, the runtime pods and the
  quotas that bound them.

## 3. Actors

- **End user (semi-trusted)** — authenticated through OIDC and gated by
  `oidc.requiredGroups`. Fully controls their workspace pod at runtime
  (shell access); may be curious or abusive but is not assumed to be a
  kernel-level attacker on shared nodes.
- **Tenant admin** — a user whose verified `groups` claim includes
  `tenant-admin`; gets a curated tenant-scoped view (S9).
- **Operator / platform admin** — deploys the chart, owns Postgres, the OIDC
  client, sealing keys, mTLS material and node pools. Trusted.
- **Unauthenticated Internet client** — can reach the portal host, the app
  listener's login endpoints and every `*.<sessionDomain>` name.
- **External OIDC IdP** — trusted for identity assertions; its discovery
  document and JWKS are consumed by the backend.
- **Kubernetes API** — trusted; the operator and backend hold scoped RBAC.

## 4. Trust boundaries and data flows

```
            OIDC IdP
               ^  (3) auth-code + PKCE, discovery/JWKS
               |
 browser --(1)--> edge (Ingress / Gateway API: portal host + *.<sessionDomain>)
   |  |                 |                         |
   |  |--(2) SPA-------> frontend :8443           |
   |  |--(2) /v1/* ----> backend app :8443        |
   |  |                       | (4) pgx, TLS-verified
   |  |                       v
   |  |                    Postgres
   |  |
   |  `--(5) launch POST + desktop HTTP/WS --> backend session :8444
   |                                             | (6) per-lease pinned CA + injected creds
   |                                             v
   |                                       workspace pod (KasmVNC)  [tenant-controlled]
   |
 operator --(7) mTLS, client cert --> backend internal :9443
 operator --(8) k8s API ----------> creates pod/Secret/Service/NetworkPolicy
 kasm-adapter init image --(9)--> shared emptyDir (read-only into desktop container)
```

### Boundary 1 — browser ↔ edge

Flows: everything above crosses one TLS edge. The chart enforces Ingress TLS
and an HTTPRoute `sectionName`; `*.<sessionDomain>` rides one wildcard DNS +
wildcard cert rule (`deploy/helm/tinycdi/templates/httproute.yaml`,
`ingress.yaml`). HSTS is sent by both portal and session listener.

Controls that exist today:

- All portal cookies carry the `__Host-` prefix and are host-only
  (`internal/api/auth.go` — `SessionCookieName`/`LoginCookieName` doc block);
  the session cookie is `Secure`, `HttpOnly`, `SameSite=Lax` (or
  `SameSite=None; Secure; Partitioned` under the optional `partitioned`
  cookie mode — `internal/gateway/proxy.go:81` `ParseCookieMode`).
- The two JS-readable cookies from earlier designs are gone: the SPA reads
  the CSRF token and session domain from `GET /v1/me`
  (`internal/api/me.go:41-42,77-78`), never from `document.cookie`.
- NetworkPolicy restricts app/session ingress to the edge
  (`deploy/helm/tinycdi/templates/networkpolicy.yaml`, `edgeIngressRule`).

Open questions: A6-S5 (Host-header parsing as a boundary — see boundary 5),
cookie-mode `partitioned` has less e2e coverage than the default `lax` mode.

### Boundary 2 — edge ↔ frontend :8443 / backend app :8443

Flows: `GET /*` → frontend static SPA; `/v1/*` → backend API (path-routed by
the edge; the frontend never proxies it — `build/frontend/main.go` header,
`TestNoAPIProxy`).

Controls:

- **Frontend security headers** — CSP (incl. `frame-src https://*.<sessionDomain>`,
  `form-action 'self' https://*.<sessionDomain>`, `frame-ancestors 'none'`),
  COOP `same-origin`, and the rest of the portal policy are pinned by
  `securityHeaders`/`frontendCSP` in `build/frontend/main.go`
  (tests: `build/frontend/main_test.go` — `TestSecurityHeaders`,
  `TestFrontendCSP`, `TestFrontendCSP_WildcardSessionDomain`,
  `TestFrontendHeaders_COOP`). The iframe sandbox/allow attributes are emitted
  by the SPA session page (`web/src/`).
- **OIDC login** — authorization-code + PKCE S256, issuer/audience verified by
  go-oidc discovery (`internal/api/auth.go:28-40`); the in-flight login rides
  in an **AEAD-sealed** `__Host-tcdi_login` cookie (AES-256-GCM,
  `internal/api/loginstate/sealer.go`), so a login can start on one replica
  and finish on another; `keys[0]` seals, every key opens for rotation
  (`-login-key-file`, `internal/backend/wire.go:562-569`). State+nonce+PKCE
  verifier are bound to the initiating browser (SEC-03).
- **Portal session** — server-side, opaque cookie value; rows keyed by
  SHA-256 digest of the session ID so a DB/backup read never yields a usable
  cookie (`internal/store/sessions.go:23,63,102`). Sliding 30 min idle,
  12 h absolute cap (`auth.go:133-137` config defaults); the backend extends
  idle on server-measured input activity. The retained `id_token` is sealed
  at rest with the same keys under a purpose-bound AAD
  (`internal/api/idtoken.go`); on read failure logout degrades to a
  `client_id`-only end-session URL.
- **CSRF** — synchronizer token derived from the session ID
  (`csrfTokenFor`, `internal/api/middleware.go:141-167` `RequireCSRF`);
  verified with a constant-time compare. Defence in depth:
  `RequireTrustedOrigin` (middleware.go:169-227) requires a single `Origin`
  byte-equal to the allowlist, or `Sec-Fetch-Site: same-origin` when Origin
  is absent (tests: `middleware_test.go` CSRF/Origin cases).
- **Rate limits** — shared Postgres fixed-minute windows (ADR 0006) over
  per-replica token buckets in `internal/ratelimit`, applied to
  `/v1/login`, `/v1/auth/callback` and the `GET /v1/session` probe
  (`internal/api/middleware.go:252-316`, wiring `internal/backend/wire.go`,
  store `internal/store/rate_limit.go`, migration 021). Keys: client IP,
  or a digest of the *validated* session ID / OIDC login state when
  present so NAT-shared users keep separate buckets
  (`internal/api/ratelimit_key.go`, FX-R30); the callback is additionally
  capped by a 10× per-IP ceiling so validated-state spray stays bounded.
  The effective bound is `min(shared window, divided local bucket)` —
  `PerReplica` still divides the configured budget into each pod's local
  ceiling and fail-open fallback for a store outage (RL-1). Client IPs
  derive from the socket peer, or the right-most untrusted X-Forwarded-For
  entry when the peer sits inside `-trusted-proxies` CIDRs
  (`ratelimit.go:174`, `ParseTrustedProxies`).
- **Passive auth** — `GET /v1/connections/.../status` authenticates via
  `RequireAuthPassive`, which never extends the idle clock
  (`internal/api/middleware.go:83-86`, `internal/api/connection_status.go:78`) —
  A6-S6's second authenticated path exists to be reviewed.
- **Tenant scoping** — the principal is built only from verified claims
  (`internal/api/principal.go`); tenant-admin surface is scoped and events are
  curated, not raw (`internal/api/events.go`, `statusview.go`;
  tests `tenant_scope_test.go`, `events_test.go`). `tenant_id` must come from
  an admin-controlled IdP mapper (SECURITY.md operator checklist).
- **Quotas** — per-user and per-tenant limits (`internal/api/quota.go`,
  `adminquota.go`; `internal/backend/tenantquota.go`).
- **Branding** — the frontend serves an operator-supplied directory at
  `/branding/` with traversal and symlink containment checks
  (`build/frontend/main.go:271-353`; tests `TestBrandingDir_NoTraversal`,
  `TestBrandingDir_NoListing`, `TestBrandingDir_OtherNamesStay404`) — A6-S10.

### Boundary 3 — backend app listener ↔ OIDC IdP

Flows: discovery + JWKS fetch, auth-code token exchange (confidential client;
PKCE means a secretless public client also works), RP-initiated logout.

Controls: exact issuer/audience match via go-oidc; `endSessionURL` is built
only from discovery + static configuration so request input cannot produce an
open redirect (`internal/api/auth.go:655-677`);
`PostLogoutRedirect` is validated as an absolute http(s) URL at startup
(auth.go:341). Client secret comes from `oidc.existingSecret` (chart).
Tests: `auth_test.go`, `logout_test.go`, `oidctest/` in-memory IdP.

Open question: A6-S2 — replay of the sealed login cookie within its TTL
(`PendingTTL`, 10 min default) is inherent to state leaving the server; the
cookie is `HttpOnly` and single-purpose but not single-use.

### Boundary 4 — backend ↔ Postgres

Flows: sessions (digest-keyed), connection leases, launch tickets, quotas,
principal directory, intent log.

Controls: the DSN's `sslmode` must verify the server certificate —
`checkDatabaseTLS` refuses `disable/allow/prefer/require` unless
`-dev-insecure-db` is passed (`internal/backend/config.go:414-424,493-514`);
the chart defaults `database.tls.mode: verify-full` and feeds
`PGSSLMODE`/`PGSSLROOTCERT`. Session rows store only SHA-256 digests and
sealed `id_token` blobs (SEC-27, above). Ticket and cookie values are stored
hashed (`ticketHash`, `sessionDigest`). Migration startup takes a lock so
concurrent replicas can't race schema creation
(`internal/backend/migrate_lock_test.go`). NetworkPolicy: DB egress allowed
only to configured `database.allowedPeers` (chart; empty list fails render —
SECURITY.md checklist).

### Boundary 5 — browser ↔ session listener :8444 (launch + desktop proxy)

Flows: one-time `POST` launch (ticket redemption) on
`<label>.<sessionDomain>`, then the cookie-gated reverse proxy for the
KasmVNC HTTP/WebSocket stream; `/healthz` + `/v1/control/*` on in-cluster
control hosts only.

Controls:

- **Host classes** — `Gateway.ServeHTTP` routes by host class first:
  workspace hosts matched by `sessionhost.Domain.Match` (label
  `ws-[a-z0-9]{8,60}` ↔ `ws_<suffix>`), control hosts from
  `-session-control-hosts`; anything else gets `421 bad_host` and API paths
  simply do not exist on this listener
  (`internal/gateway/proxy.go:268-330`, `internal/sessionhost/sessionhost.go`;
  tests `hostbinding_test.go`, `proxy_test.go`) — the A6-S8 listener isolation.
- **Launch ticket** — 60 s TTL, single-use, redeemed atomically (even an
  expired ticket is consumed), stored as SHA-256, bound to
  (workspaceUID, tenant, principal subject, gateway audience) and records the
  clipboard policy + portal session binding (`internal/broker/tickets.go:53-60,257-383`).
  The ticket travels in the POST **body** only — `?ticket=` is rejected with
  403 (`internal/gateway/launch.go:293-296`). The launch origin policy
  (ADR 0004): when `Sec-Fetch-Site` is present, an absent/`null` Origin can
  never redeem and a present Origin must be in the portal allowlist
  (`launchOriginOK`); with no fetch metadata a present Origin is still
  checked, and a request carrying neither header is treated as a non-browser
  client and admitted (the ticket is still required)
  (`launch.go:263-282`). Rate limiting runs **before** redemption so a
  limited attempt never consumes the ticket (`launch.go:235-308`;
  tests `launch_test.go`, `launch_origin_test.go`, `launch_ratelimit_test.go`).
- **Per-workspace cookie + host binding** — `__Host-tcdi_session` is set
  host-only on the workspace host; every proxied request's cookie digest must
  resolve to a lease whose `workspaceUID` matches the request host label —
  a mismatch is denied and audited (`proxy.go:472-510`, A6-S5).
- **Leases and fencing** — one interactive stream per lease; a new upgrade
  fences the old socket via `admitUpgrade`, and the Postgres `stream_epoch`
  fences streams admitted by a dead replica (A6-S4;
  `internal/gateway/authorization.go:1-104`). Renew every ~10 s; a lease that
  stops renewing fails closed within 30 s (revoke/expiry/supersede all kill;
  tests `leases_test.go`, `wsclose_test.go`, `owner_tab_test.go` for the
  owner-tab takeover flow).
- **Rehydration** — a cookie unknown to RAM is resolved by SHA-256 digest to
  the lease and the session rebuilt on any replica; expired/revoked leases
  reject (`internal/gateway/rehydrate.go:42-146`; tests `rehydrate_test.go`,
  `tests/integration/session_cookie_liveness_test.go`).
- **WebSocket origin** — upgrade requires `Origin` equal to the workspace
  host's own origin (`originOK`, `internal/gateway/authorization.go:440-446`;
  upgrade detection itself is strict RFC 7230 — `isUpgrade`);
  framing tests `framing_test.go`, `framereload_test.go`.
- **Response policy pinned by the gateway** — `sessionCSP` sized to the pinned
  KasmVNC 1.5.0 client plus `Cross-Origin-Resource-Policy: same-origin`,
  `Origin-Agent-Cluster: ?1` and `frame-ancestors <portal-origin>` are set on
  every response; headers from the upstream pod are deleted so a hostile
  runtime cannot weaken them (`proxy.go:276-290,966`, `sessionCSP`).
- **Forwarded-header hygiene** — client-supplied `X-Forwarded-For` never
  reaches the runtime, and rate-limit keys only honor XFF from configured
  trusted proxies (`internal/gateway/proxy.go:820-830`;
  tests `forwarded_test.go` — FX-R26) — most of A6-S18's surface.
- **Upstream TLS pin** — the runtime's self-signed cert is verified against a
  per-lease pinned CA pool with a pinned server name — never
  `InsecureSkipVerify` (`proxy.go:704-769`); per-workspace credentials are
  injected from the workspace Secret (`internal/broker/credentials.go`).
- **Upstream path allowlist** — only the paths the stock KasmVNC client
  needs are proxied; everything else, including any upstream
  management/API surface, is denied at the gateway. `cleanedProxyPath`
  rejects traversal and parser-differential tricks (dot segments, encoded
  dots/slashes/backslashes, double encodings) before the allowlist runs
  (`internal/gateway/authorization.go:444-521`, `upstreamAllowlist`,
  `cleanedProxyPath` — SEC-20).
- **Control surface** — `/v1/control/*` answers only on control hosts and
  requires the bearer token from `-control-token-file`
  (`proxy.go:342-468`, `controlAuth`); on workspace or foreign hosts the
  paths do not exist.
- **Timeouts** — app/internal/metrics listeners bound every request phase;
  the session listener bounds only header read since connections are hijacked
  for WebSocket (SEC-23, `internal/backend/listeners.go:15-62`).

Open questions: A6-S4's residual window (two streams for at most one renew
cycle when a replica dies mid-claim); A6-S18's remainder — whether KasmVNC's
own brute-force protection still means anything behind an authenticating
proxy, and what else the client can influence in the forwarded chain.

### Boundary 6 — session listener ↔ workspace pod / KasmVNC

Flows: reverse-proxied HTTP + WebSocket to the pod's KasmVNC server;
egress from the pod to the Internet (by profile) and DNS.

Controls:

- **Per-workspace NetworkPolicy** `ws-<uid>-boundary`: default-deny, ingress
  only from the gateway, DNS egress, template/profile-scoped egress
  (`internal/runtime/linux/backend.go:1144-1194` `buildNetPol`; test
  `backend_netpol_test.go`); chart-level `default-deny` baselines for the
  release and managed namespaces (`templates/networkpolicy.yaml`).
- **Pod hardening** — `RunAsNonRoot`, `AllowPrivilegeEscalation=false`,
  `Drop: ALL` capabilities, seccomp `RuntimeDefault` (or a `localhost/`
  profile via the template annotation, validated),
  `securityContext.appArmorProfile` RuntimeDefault or `localhost/`
  (`internal/runtime/linux/backend.go:78-90,790-840,894-899`). Setting
  `runtime.appArmor.requireRuntimeDefault=false` drops the fail-closed
  guarantee on nodes without AppArmor (`AppArmorNotRequired`) — A6-S16.
- **Credentials** — per-workspace KasmVNC credentials live in a per-workspace
  Secret mounted read-only (`secretMountDir`); the broker reads them at
  target resolution (`credentials.go`). The pod's user can read them — by
  design, since the pod is tenant-controlled; they only authenticate to that
  workspace's own VNC server.
- **Kasm adapter** — an init image writes adapter scripts into a shared
  emptyDir mounted read-only into the desktop container
  (`build/kasm-adapter/main.go`); it neutralizes the baked-in `VNC_PW`
  defaults, makes the rootfs read-only outside the writable mounts, shadows
  Chromium wrappers with the sandbox-preserving `browser-shim.sh`, and drops
  managed Chromium policy over each engine's policy dir
  (`internal/runtime/linux/backend.go` kasm block ~L910-960;
  `tests/integration/kasm_adapter_test.go`,
  `deploy/helm/kasm_adapter_test.go`, CI `kasm-contract` job).
- **Node isolation (posture)** — chart defaults workspaces onto a dedicated
  tainted node pool; `allowSharedNodes` is opt-in and warns; node-profile
  DaemonSet delivers AppArmor/seccomp profiles (`deploy/node-profiles/`,
  `templates/nodeprofiles.yaml`; test `chart_hardening_test.go`).

Open question: hostile-pod escape/kernel CVEs are explicitly out of the
product's isolation claim (`SECURITY.md`); container sandboxing
(`runtimeClassName` passthrough exists; Kata/gVisor) is post-v1.0 scope.

### Boundary 7 — operator ↔ internal mTLS listener :9443

Flows: the operator calls the broker internal API (ADR 0003): target
resolution inputs, workspace revoke/drain, activity reporting surface.

Controls:

- `ServerTLSConfig`: `VerifyClientCertIfGiven`, TLS 1.3 minimum
  (`internal/broker/httpapi/server.go:455-468`); every route asserts a peer
  certificate and maps the client identity to a role — `requireGateway` /
  `requireOperator` (server.go:106-140,331-348).
- **Hot reload everywhere** — the internal listener's cert and client CA
  bundle reload on file change via `tlsreload.Reloader`/`CAPool`
  (`internal/backend/wire.go:421-429`, `listeners.go:87`
  `hotReloadClientCAs`; test `internal/backend/mtls_reload_test.go`). On the
  client side `opclient` reloads both its certificate and the broker CA:
  `GetClientCertificate` re-reads per handshake and `VerifyConnection` runs
  the full chain + hostname check against the newest CA pool — the
  `InsecureSkipVerify` field is set only as that handoff
  (`internal/broker/httpapi/opclient/client.go:95-160`; tests
  `ca_reload_test.go` — FX-R33, `verify_internal_test.go`). A6-S15's
  load-once gap is therefore closed on `main`.
- Public listener certs (app :8443, session :8444) hot-reload the same way
  (`wire.go:728-736`; `build/frontend/main_test.go:TestHotReload` for the
  frontend server).
- Audit logging redacts object IDs (`server.go:170` `redactID`).

### Boundary 8 — operator ↔ Kubernetes API (incl. kasm-adapter delivery)

Flows: CRUD on `Workspace`/`WorkspaceTemplate`, runtime pods, per-workspace
Secrets/Services/NetworkPolicies in managed namespaces; the kasm-adapter init
image delivers scripts into the pod.

Controls: scoped RBAC in `deploy/helm/tinycdi/templates/rbac.yaml` +
`config/rbac/`; CRD/CEL validation requires digest-pinned image refs
(`api/v1alpha1/validation.go` `DigestPattern`) and forbids
`spec.linux.command` with `adapter=kasm` (the adapter entrypoint wins —
`buildPod` kasm block); `hostUsers` is a typed template field with an
operator-wide default knob, nil leaves the pod field unset
(`internal/runtime/linux/backend.go:295-298,1097-1103`). Image-digest
verification of the running template snapshot:
`internal/operator/snapshot_verify_test.go`.

## 5. Cross-cutting controls

### Secrets handling

- Sealing keys are files (`-login-key-file`, repeatable for rotation); OIDC
  client secret and DB DSN arrive via Secret env/mounts (`oidc.existingSecret`,
  `database` chart values); the session-listener control token is a file.
- Nothing persists usable session material: session IDs → SHA-256, tickets →
  SHA-256, `id_token` → AEAD-sealed blob (SEC-27).
- Logs redact IDs (`redactID`); the post-redemption redirect URL is static
  and carries no ticket or session material (`launch.go` `DesktopPath`
  contract); tests assert cookie/CSRF/id_token values never leak into API
  responses (`internal/api/auth_test.go`, `workspaces_handlers_test.go`).

### Supply chain (CI) — summary; full inventory in `test-inventory.md`

- Build jobs push **by digest only**; cosign keyless signs and attaches the
  SPDX SBOM attestation on the digest **before** any tag is promoted
  (`.github/workflows/images.yml`, `release.yml`; `docs/security/provenance.md`).
- trivy gate per image (fixable CRITICAL/HIGH fails); `.trivyignore` is an
  expiring-exceptions file enforced by `check-trivyignore.sh`; the scanner
  runs with an empty `--config` (SUPR-8).
- govulncheck, `npm audit` (prod deps, high+), `dependency-review-action` on
  PRs; actionlint + yamllint + zizmor on workflows; `.github/tests/*`
  regression tests guard the release/workflow scripts.
- Runtime freshness: `runtime-freshness.yml` (daily) opens pin-bump PRs when
  bookworm-security publishes a newer Chromium/Firefox-ESR and fails while a
  pin is stale; `runtime-images.yml` is the always-on runtime train
  (check → PR → build/scan/sign/publish) — **no human gate on publish**,
  intentional for daily runs (A6-S12: review each job's permissions, the
  keyless signing identity, and whether consumers can distinguish a train
  image from a release image).
- Required checks and enforce-admins are codified in
  `.github/scripts/setup-repo-protection.sh`.

### Failure posture

- Broker freshness budget: issuing/renewing stops when observed runtime
  state is >15 s stale (`ErrFreshness`, `internal/broker/tickets.go`).
- Postgres outage: sessions close after ~30 s without renewal (leases fail
  closed; `tests/integration/postgres_outage_test.go`,
  `backend_restart_test.go`).
- Leader election off by default for the operator (single replica assumption
  — noted in the post-MVP architecture review).

## 6. Known gaps and open items for the v1.0 review

The post-MVP review (A6) phrased each item as a *question for the reviewer*,
not a confirmed bug. Status below is against `main` at the time of writing.

| # | Item | State today |
|---|---|---|
| S1 | CSRF token derived from session ID | Implemented (`RequireCSRF`, `csrfTokenFor`); model change vs v0.1 — verify derivation is unforgeable cross-session |
| S2 | AEAD login cookie, two-key rotation | Implemented (`loginstate`, `-login-key-file`); review in-TTL replay (Boundary 3 above) |
| S3 | Cookie digest on lease; session restore | Implemented (`rehydrate.go`, store digests); new auth path for the session listener — review digest lookup + singleflight |
| S4 | `stream_epoch` cross-replica fencing | Implemented (`authorization.go`); residual ≤1-renew-cycle two-stream window stands |
| S5 | Per-workspace host binding / domain match | Implemented (`sessionhost`, `ServeHTTP` host classes, `host_mismatch` deny); Host-header parsing is the boundary |
| S6 | Connection-status endpoint without idle renewal | Implemented (`RequireAuthPassive`); second auth path to review |
| S7 | iframe sandbox, CSP, COOP/CORP, `lax` same-site | Implemented (frontend headers + `sessionCSP` + `responsePolicy`); tenant-controlled content sits same-site with the portal by design |
| S8 | Listener isolation | Implemented (host classes; API paths absent on :8444, catch-all only on workspace hosts) |
| S9 | Tenant scope, curated events, principal directory | Implemented (`principal.go`, `events.go`, `directory.go`); new tenant-admin surface |
| S10 | Branding directory handler | Implemented (`build/frontend/main.go`); serves operator-supplied files — traversal containment tested |
| S11 | KASM-2 risk acceptance | The risk acceptance lapsed with v0.2; the kasm adapter + catalog scan ship now. Reviewer should confirm the catalog gate (`check-kasm-catalog.sh`, `kasm-contract` job) actually covers the documented minimum engine floor |
| S12 | Runtime image release train | Live (`runtime-images.yml`); intentionally no human gate — review job permissions, keyless identity, train-vs-release image distinguishability |
| S13 | G0–G5 merged without independent review | Standing: the whole v0.1→v0.3 delta has had no external security review — this document exists to scope it |
| S14 | App-layer rate limit for `/v1/login`, `/v1/launch` | **Implemented since** (`internal/ratelimit` + Postgres windows, ADR 0006 — FX-R30 keying, shared bound with per-replica ceiling and fail-open fallback); reviewer verifies coverage, ceilings, bypass resistance and the outage degradation path |
| S15 | Operator mTLS client cert / listener client-CA hot reload | **Implemented since** (`opclient` reload loop + `hotReloadClientCAs`; FX-R33 test); reviewer confirms rotation edge cases |
| S16 | `runtime.appArmor.requireRuntimeDefault` opt-out | Implemented (`AppArmorNotRequired`); review docs/default/preflight detection on AppArmor-less nodes |
| S17 | Sign-out vs live desktop streams | **Still open**: `LogoutHandler` destroys the portal session but does not revoke connection leases or the workspace-host session cookie (`internal/api/auth.go:621-650`); a live stream survives sign-out until lease expiry — needs a product decision, then review |
| S18 | Client address chain gateway → KasmVNC | Partially closed: client XFF never reaches the runtime and only trusted proxies shift rate-limit keys (`forwarded_test.go`); open: whether pod-side brute-force protection is meaningful behind the authenticating gateway |

Additional items found while writing this document (not from A6):

- **Cookie-mode `partitioned`** has materially less e2e coverage than the
  default `lax` mode.
- **Portal idle-extension depends on lease activity** — verify a stolen
  portal cookie alone cannot extend itself, and that idle extension only
  credits input activity measured server-side.
- **Metrics listener** is scrape-only but has no auth; confirm chart
  NetworkPolicy + docs keep it off the edge path.
- **Operator leader election off by default** — two simultaneous operators
  would double-drive reconciliation; a deploy-time footgun rather than a
  code bug.
- **`kasmweb/*` third-party images** — the adapter neutralizes their startup
  but they carry KasmVNC 1.4.0 (vs the pinned 1.5.0 in TinyCDI-built images);
  the catalog gate enforces a floor — reviewer should confirm the floor and
  the exception file's expiry policy together.
- **Postgres backup/restore of digest-keyed sessions** — restore semantics
  vs live leases is undocumented for a DR scenario.
- **Audit trail completeness** — `session.host_mismatch`, denies and revokes
  are audited; confirm coverage of admin operations (quota writes, tenant
  actions) and log integrity expectations.

## 7. What the reviewer should read first

1. `SECURITY.md` — normative hardening rules + operator checklist.
2. `docs/adr/0003`, `0004`, `0005` — internal API contract, launch origin
   policy, component/session-host split (0005 self-declares as not
   independently reviewed).
3. `docs/security/test-inventory.md` — the automated evidence that exists.
4. `docs/security/provenance.md` + `vulnerability-policy.md` — supply-chain
   posture and scan gates.
5. This file's §6 table — the explicit question list.
