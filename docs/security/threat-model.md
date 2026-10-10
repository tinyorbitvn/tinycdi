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
cookie-mode `partitioned` cross-site behaviour (a deployment topology the
kind e2e cannot reproduce).

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
  cookie (`internal/store/sessions.go:23,63,102`). Sliding 30 min idle
  (`-session-idle`/`TCDI_SESSION_IDLE` → `store.NewSessionStore`), 12 h
  absolute cap (`AuthConfig.AbsoluteTimeout`); the backend extends
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
  `/v1/login`, `/v1/auth/callback`, the `GET /v1/session` probe and
  `POST /v1/me/sessions:revoke-all`
  (`internal/api/middleware.go:252-316`, wiring `internal/backend/wire.go`,
  store `internal/store/rate_limit.go`, migration 021). Keys: client IP,
  or a digest of the *validated* session ID / OIDC login state when
  present so NAT-shared users keep separate buckets
  (`internal/api/ratelimit_key.go`, FX-R30); the callback is additionally
  capped by a 10× per-IP ceiling so validated-state spray stays bounded.
  While the store is healthy the window is the exact aggregate bound and
  each pod's local bucket is the undivided rate+burst — a
  store-protection prefilter (RL-CEILING amendment); the `PerReplica`
  divided bucket is the fail-open floor, engaged only while the circuit
  breaker is open (RL-1). Degraded-mode
  cost is bounded by a circuit breaker: every store call carries a 500 ms
  deadline, an error opens the circuit for a 10 s cool-down during which
  checks skip the store entirely, and one single-flight probe re-tests it
  (`internal/ratelimit/shared.go`; tests `shared_test.go`,
  `tests/integration/rate_limit_pg_test.go`). The v0.4.0 flag defaults
  restore the v0.3.x effective two-replica budgets as window bounds
  (login 80/min, launch 160/min, callback ceiling 800;
  `TestRateLimitDefaults_V040Budgets`). Client IPs derive from the socket
  peer, or the right-most untrusted X-Forwarded-For entry when the peer
  sits inside `-trusted-proxies` CIDRs (`ratelimit.go:174`,
  `ParseTrustedProxies`).
- **Passive auth** — since FIX-IDLE every cookie-authenticated `GET` mounts
  `RequireAuthPassive`, which never extends the idle clock
  (`internal/api/middleware.go`, and the `safe`/`RequireAuthPassive` mounts
  in `workspaces.go`, `data.go`, `me.go`, `quota.go`, `adminquota.go`,
  `adminuserlimit.go`): the portal's interval polls can no longer hold a
  visible-but-unattended session open. Only mutations and server-measured
  desktop input slide the window. `GET /v1/session` is anonymous (Peek) and
  `GET /v1/connections/.../status` was already passive — A6-S6's second
  authenticated path exists to be reviewed.
- **Tenant scoping** — the principal is built only from verified claims
  (`internal/api/principal.go`); tenant-admin surface is scoped and events are
  curated, not raw (`internal/api/events.go`, `statusview.go`;
  tests `tenant_scope_test.go`, `events_test.go`). `tenant_id` must come from
  an admin-controlled IdP mapper (SECURITY.md operator checklist).
- **Quotas** — per-tenant limits (`internal/api/quota.go`,
  `adminquota.go`; `internal/backend/tenantquota.go`) plus per-principal
  running-workspace limits (#121): a tenant default and per-owner
  overrides (migration 022) are written through the tenant-admin
  `/v1/admin/tenants/{tenant}/user-limits` API
  (`internal/api/adminuserlimit.go`, `RequireAuth` + audit + `RequireCSRF`)
  and enforced inside the reservation transaction under the
  `tenant_quota` row lock (`checkUserLimit`,
  `internal/provisioning/quota.go:433-492`) — the live count comes from
  `quota_reservation` `held` rows, disk-only holds and retained disks
  never count, a missing workspace row fails closed, and refusal is 409
  `QUOTA_EXHAUSTED` with `details.reason` `UserLimitReached`
  (`internal/api/errors.go`). No rows means unlimited, so a fresh upgrade
  changes nothing until an admin sets a limit.
- **Branding** — the frontend serves an operator-supplied directory at
  `/branding/` with traversal and symlink containment checks
  (`build/frontend/main.go:271-353`; tests `TestBrandingDir_NoTraversal`,
  `TestBrandingDir_NoListing`, `TestBrandingDir_OtherNamesStay404`) — A6-S10.
- **Audit trail** — every mutating API route and every `/v1/admin/` read
  emits exactly one dedicated domain event via `audited()`
  (`internal/api/auditroutes.go`): the actor is pseudonymised
  (`observability.ActorRef`), the outcome derives from the response
  status, `role=tenant-admin` marks elevated use of shared routes, and a
  handler panic still emits `failure`/`panic`; the openapi↔table pairing
  is pinned by `TestAuditCoverage_SpecMatchesTable` (#120). The sink is
  `GuardedSink` wrapping JSONL stdout — a write failure is counted on
  `tinycdi_audit_write_errors_total{event}` plus a rate-limited warning
  and never propagated into the request (`internal/observability/audit.go`;
  wiring `internal/backend/wire.go`). Durability and tamper-evidence of
  the stream are the operator's log pipeline (`docs/runbooks/observability.md`
  "Log integrity").

### Boundary 3 — backend app listener ↔ OIDC IdP

Flows: discovery + JWKS fetch, auth-code token exchange (confidential client;
PKCE means a secretless public client also works), RP-initiated logout.

Controls: exact issuer/audience match via go-oidc; `endSessionURL` is built
only from discovery + static configuration so request input cannot produce an
open redirect (`internal/api/auth.go:770-787`);
`PostLogoutRedirect` is validated as an absolute https URL at startup
(auth.go) — http is accepted only for loopback dev hosts. Client secret
comes from `oidc.existingSecret` (chart).
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
hashed (`ticketHash`, `sessionDigest`). Session rows also carry
`platform_meta.session_epoch` at insert and every read compares it to the
current value in the same statement (`internal/store/sessions.go`
`currentEpochSQL`), so rotating the epoch — the `post-restore` kill-step —
makes every row a restored dump brought back dead on its next read,
including sessions deleted after the dump (S23). Migration startup takes
a lock so concurrent replicas can't race schema creation
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
  owner-tab takeover flow). Since #123, every lease read that feeds
  renew/attach/claim — and the `LeaseBySession` cookie→lease resolve —
  re-validates the bound `portal_session_digest` against a live,
  current-epoch session row in the same indexed read and revokes the lease
  on the spot when it fails (`liveLease`, `internal/broker/leases.go:75-190`;
  `tinycdi_lease_session_missing_total{reason}`) — the second barrier behind
  the sign-out revoke, and what kills a lease a Postgres restore resurrected
  (S17, S23).
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
- **Forwarded-header hygiene** — the whole client-supplied forwarding
  family (`X-Forwarded-*`, `X-Original-*`, `X-Rewrite-Url`, `Forwarded`,
  `X-Real-Ip`) and the vendor client-address headers (`Via`,
  `X-Client-Ip`, `True-Client-Ip`, `CF-Connecting-Ip`,
  `X-Cluster-Client-Ip`, `X-Envoy-*`) are stripped from one table and the
  address headers rebuilt from the
  verified chain only (`rewriteForwarded`, `internal/gateway/proxy.go`);
  rate-limit keys only honor XFF from configured trusted proxies
  (tests `forwarded_test.go`, `connheaders_test.go` — FX-R26) — most of
  A6-S18's surface.
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
cycle when a replica dies mid-claim).

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
(`internal/runtime/linux/backend.go:295-298,1097-1103`). The template
snapshot that drives pod building is operator-authoritative: it is
recorded to `status.templateSnapshot` — writable only through the
`workspaces/status` subresource, granted to the operator service account
alone — from the live template at first admit, and only mirrored to the
`workspaces.cdi.tinyorbit.vn/template-snapshot` annotation for readers,
so Workspace writers can no longer steer convergence by editing
annotation bytes (S30). Image-digest verification of the recorded spec:
`internal/operator/snapshot_verify_test.go`. Intent delivery is fenced:
the operator converges a Workspace only to the newest recorded intent
(`spec.intentRevision` vs the `applied-intent` annotation — a spec at or
behind the record is ignored, so a replayed stale intent can never flip
`desiredState` back; `internal/operator/workspace_controller.go`). Since
#125 a dropped intent that evidences stream drift — a strictly-behind
revision, or an equal revision with diverged fields — stamps the
`workspaces.cdi.tinyorbit.vn/intent-behind` marker annotation
(`intentFenceDrifted`, `internal/provisioning/k8sapplier.go`), which the
operator reconciles into an `IntentBehind` condition plus one
edge-triggered Warning event until the stream realigns (S24).

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
- The train's deployment manifest is signed too (v1.0 fix — it was the
  one unsigned artifact): `runtime-images.json` ships a
  `cosign sign-blob` Sigstore bundle on every `runtime-*` release;
  consumers verify it before pinning digests (`.github/README.md`).
- Scan jobs validate artifact-supplied image refs against the strict
  `ghcr.io/tinyorbitvn/tinycdi-<img>@sha256:<64hex>` form before writing
  them to `$GITHUB_ENV` (SUPF-10; v1.0 fix — previously unvalidated).
- Build jobs push by digest before the scan gate; a gate-failed digest
  stays pullable-by-digest but is never tagged or signed, so it carries
  no release trust — accepted residual, documented in `provenance.md`
  "Digest-addressable does not mean released".
- The train publish job emits `attest-build-provenance` only under the
  dedicated `TRAIN_ATTESTATIONS_ENABLED` repo variable (off by default,
  SEC-I12), and the release ships dedicated `sbom-chart` /
  `sbom-binaries` SPDX SBOMs for the packaged chart and static binaries.
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
S1–S18 are the A6 questions; S19 onward continue the numbering for the v0.5
items that landed since.

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
| S14 | App-layer rate limit for `/v1/login`, `/v1/launch` | Implemented (v0.4: `internal/ratelimit` + Postgres fixed-minute windows, ADR 0006 — FX-R30 keying, exact shared-window bound with the undivided local bucket as a healthy-mode store-protection prefilter and the divided local bucket as the degraded-mode floor (v0.5 RL-CEILING amendment), fail-open on store error). Since then: a circuit breaker bounds degraded-mode cost (500 ms per-call deadline, 10 s cool-down skipping the store, single-flight probe — `internal/ratelimit/shared.go`, `shared_test.go`), the v0.4.0 defaults restore the v0.3.x effective budgets as window bounds (`TestRateLimitDefaults_V040Budgets`), and `/v1/me/sessions:revoke-all` joined the session-keyed limiter. Reviewer verifies coverage, ceilings, bypass resistance and the outage degradation path. Residual: IP-keyed budgets still multiply across the prefixes a client holds — IPv6 keys by /64 and mapped spellings unify with the native v4 key (v1.0), so rotation inside one prefix or across family spellings no longer helps, but a client holding several /64s or several egress addresses still gets a budget per prefix (per-prefix limiting as designed; authenticated routes key by session). A selected XFF entry that is not a plain IP — a garbage token, hostname or zoned IPv6 literal — can never become a key: it collapses to the socket peer for both `ClientKey` and `ClientAddr`, so a client whose own bytes reach the selected slot shares one peer bucket instead of minting fresh keys, and the forwarded-header rebuild never carries non-address bytes (v1.0, `TestClientKey_NonIPSelectedEntryFallsBackToPeer`). The right-most-untrusted XFF derivation additionally assumes every proxy hop in front is in `-trusted-proxies` — a pass-through trusted hop would hand the key to client bytes; the invariant and the dual-stack listing requirement are documented in the chart README |
| S15 | Operator mTLS client cert / listener client-CA hot reload | **Implemented since** (`opclient` reload loop + `hotReloadClientCAs`; FX-R33 test); reviewer confirms rotation edge cases |
| S16 | `runtime.appArmor.requireRuntimeDefault` opt-out | Implemented (`AppArmorNotRequired`); review docs/default/preflight detection on AppArmor-less nodes |
| S17 | Sign-out vs live desktop streams | Implemented (#107 + #123; `LogoutHandler` → `Broker.RevokePortalSession`, `internal/api/auth.go:685`, `internal/broker/sessions.go:109`): sign-out revokes the session's digest-bound leases and outstanding tickets in one store tx — every replica's renew loop closes the bound stream within one renew cycle, and a replayed workspace cookie resolves to a revoked lease (401) on any replica; ticket-redemption re-checks the session row (`internal/broker/tickets.go`). Since the SIGNOUT-FIX pass both revoke sweeps pre-lock their covered rows in a deterministic key order (`lockByKeysInOrderTx`, `sessions.go:418` — tickets by `ticket_hash`, sessions by `id`, leases by `id`), so overlapping sweeps serialize on the first contested row instead of crossing waits. Defence-in-depth (#123, DR): every lease read that feeds renew/attach/claim or the cookie→lease resolve (`liveLease`/`LeaseBySession`, `internal/broker/leases.go`) re-validates the bound portal session row — epoch + absolute expiry, same semantics as the redeem-time check — in the same indexed read, and revokes the lease in one statement when it fails (`tinycdi_lease_session_missing_total{reason=absent\|invalid}` + one log line). A lease resurrected as `active` by a Postgres restore therefore dies at its next renew or attach even when the sign-out revoke itself was lost in the dump window; leases with a NULL `portal_session_digest` (pre-018 rows) are exempt — nothing to verify against, and revoking them on sight would mass-kill sessions mid-rolling-upgrade — they keep the TTL lifecycle and are covered by the DR runbook's post-restore lease sweep. Since the FIX-LOGOUT pass the session-row delete is the ordering's point of no return: a store failure on it answers retryable `503 UNAVAILABLE` (+ `Retry-After`) and tears nothing down — no cookie expiry, no lease/ticket revoke — so a response never claims a sign-out that did not commit, and the still-valid session keeps its live leases until a retry completes the destroy (revoking first would leave a live session with dead streams, a half-revoked state the refusal would be lying about). Tests `internal/broker/lease_session_test.go`, `signout_test.go` (api, broker, gateway), `TestLogout_StoreDeleteFailureReturns503` |
| S18 | Client address chain gateway → KasmVNC | Closed (ADR 0008): the whole client-supplied forwarding family (`X-Forwarded-*`, `X-Original-*`, `X-Rewrite-Url`, `Forwarded`, `X-Real-Ip`) and vendor client-address headers (`Via`, `X-Client-Ip`, `True-Client-Ip`, `CF-Connecting-Ip`, `X-Cluster-Client-Ip`, `X-Envoy-*`) are stripped and only trusted proxies shift the rebuilt forwarded keys (`forwarded_test.go`, `connheaders_test.go`); the pod-side 5/10 blacklist is correctly keyed on the derived client address and stays as defence-in-depth — credential guesses are unreachable by construction since `Authorization` is broker-injected on every proxied request (`TestProxy_StripsClientAuth`). The injection now runs in the proxy's `Rewrite` hook — after the stdlib hop-by-hop strip — and client `Connection` token lists are canonicalised to upgrade/keep-alive/close, because before this fix a client could name the injected headers in `Connection:` and have them deleted on the wire: unauthenticated requests reached the runtime's auth layer and the forwarded chain arrived degraded |
| S19 | Sign-out-everywhere vs the principal's other sessions | Implemented (ADR 0007; `RevokeAllSessionsHandler` → `Broker.RevokePrincipalSessions`, `internal/api/revokeall.go`, `internal/broker/sessions.go`): `POST /v1/me/sessions:revoke-all` destroys every portal session of the principal **in the caller's tenant** — caller's own session included — and revokes its active leases and outstanding tickets in one store transaction (lock order tickets → sessions → leases, identical to `RedeemTicket`/`RevokePortalSession` and deterministic within each kind via `lockByKeysInOrderTx`; both redeem interleavings, a multi-ticket inversion and a `-race` three-way run are pinned on real row locks). Streams die within one renew cycle on every replica; every revoked session's cookie replays to 401. Failure rolls back whole (500, caller stays signed in — never a partial revoke reported as success). No IdP back-channel logout; the provider session ends only via the same RP-initiated `endSessionUrl` as logout. Tests `revokeall_test.go` (api, broker, gateway) |
| S20 | Per-principal running-workspace limits | Implemented (#121, migration 022): `tenant_user_limit_default` + `user_session_limit` tables written through tenant-admin routes `/v1/admin/tenants/{tenant}/user-limits[/default]` (`internal/api/adminuserlimit.go` — auth + audit + CSRF, set/clear audited under `admin.user_limit.*`); admission enforces the effective limit inside the reservation transaction under the `tenant_quota` row lock (`checkUserLimit`, `internal/provisioning/quota.go:433-492`), counting only `held` running slots — disk-only holds and retained disks are exempt, a missing workspace row fails closed, refusal maps to 409 `UserLimitReached`. No rows means unlimited, so upgrades change nothing until an admin acts. Tests `adminuserlimit_test.go`, `provisioning/userlimit_test.go`; reviewer checks the owner→principal identity mapping (`issuer\|sub`) and cross-tenant authz |
| S21 | Audit coverage of admin and mutating routes | Implemented (#120): `audited()` emits exactly one domain event per covered route — every non-GET API route plus the `/v1/admin/` reads — with a pseudonymous actor, status-derived outcome, `role=tenant-admin` marker and a deferred emit that survives handler panic (`internal/api/auditroutes.go`; `TestAuditCoverage_SpecMatchesTable` fails when a mutating/admin route is added to the spec without a table entry, and `TestAuditCoverage_MountedMuxEmitsEventPerRoute` replays the mounted mux with a capturing sink — driving one request through every mutating/admin spec route must emit exactly one domain event, so a plainly-mounted (or wrapped-and-discarded) route fails the build (v1.0)). The gateway's control surface audits `session.list`/`session.revoke` including bearer denials (`proxy.go`, `TestControlAudit_OperatorSurface`). `GuardedSink` makes a failing sink loud-but-never-blocking: `tinycdi_audit_write_errors_total{event}` + rate-limited warn (`internal/observability/audit.go`, `TestGuardedSink_ReportsFailureNeverPropagates`). What the platform does NOT provide: tamper-evident/durable storage — the stream is stdout JSONL and inherits the operator's log pipeline (runbook "Log integrity") |
| S22 | Deploy-time guards for hazardous value combinations | Implemented (#119): `tinycdi.validate` now fails the render on `operator.leaderElect=false` with `operator.replicas>1` — unelected replicas would double-drive reconciliation and the binary cannot observe the Deployment's replica count (`templates/_helpers.tpl`, `cmd/operator` note; `TestOperatorLeaderElectionGuard`). Metrics-port isolation is pinned for every `edgeIngress` mode: only `allow-metrics-scrape` opens :9090, to exactly `networkPolicy.prometheusPeers` (`TestMetricsListenerIsolation`, `TestEdgePolicyNeverOpensInternalPort`). Residual: single-replica installs (`replicas: 1`) may still run unelected, and guards only constrain chart-rendered manifests — hand-rolled manifests bypass them |
| S23 | Postgres-only restore onto a live cluster (DR) | Implemented (#122 runbook + #124 tool): `docs/runbooks/disaster-recovery.md` §"Postgres-only restore onto a live cluster" enumerates what an older dump resurrects — `sessions` rows including post-dump sign-outs (killed by `platform_meta.session_epoch` rotation — `SessionStore.Get` compares epoch in the same read), `'active'` `connection_lease` rows including post-dump revokes (a non-NULL `portal_session_digest` dies at next renew/attach via the S17 check, but the tool revokes every restored lease unconditionally anyway — NULL-digest rows have no barrier), unconsumed `launch_ticket` rows re-arming (denied; redeem also re-checks the session), and `workspaces` rows trailing live CRs (aligned to `spec.desiredState`/`runtimeGeneration`/`intentRevision` so the applied-intent fence is not bypassed). `backend post-restore` is the one-shot implementation: dry-run by default; `-apply` requires `-i-have-scaled-down`, takes the leader advisory lock on a dedicated connection and holds it for the whole run (a serving replica or concurrent run means refusal with zero writes), refuses while backend connections remain in `pg_stat_activity` unless `-i-know-backends-are-running`, and exits non-zero printing per-workspace SQL when Kubernetes read access for CR alignment is absent (`internal/backend/postrestore.go`; tests `postrestore_test.go`, `TestSessionEpochRotation`). The `hack/quickstart/restore-drill.sh` kind drill is manual, not a per-PR gate |
| S24 | Intent-stream drift vs the applied-intent fence | Implemented (#125): after a DB restore the platform's `workspaces.intent_revision` can trail the fence the CR already recorded, and every new intent would be dropped as stale — silently. The applier now detects the two unreachable-by-replay shapes (strictly-behind revision; equal revision with diverged `desiredState`/`runtimeGeneration`) and stamps `workspaces.cdi.tinyorbit.vn/intent-behind` `{rowRevision, crRevision}` (`intentFenceDrifted`, `internal/provisioning/k8sapplier.go`); the operator raises `ConditionIntentBehind` with the revision params plus one edge-triggered Warning event, and clears it once the stream applies forward again (`internal/operator/workspace_controller.go`, `api/v1alpha1/validation.go`; events `create`/`patch` RBAC added). The fence itself is unchanged — a stale intent is still never applied; drift is only surfaced. Surfaced to users via the conditions/events projection (`statusview.go`, `events.go`; `TestIntentBehindDrift`, `TestK8sApplierIntentDrift`, `TestProjectConditions_IntentBehind`) |
| S25 | Dead/misleading auth config knobs | Fixed: `AuthConfig` carried `IdleTimeout` — defaulted at startup but never consumed (the session idle window is owned by the session store: `-session-idle`/`TCDI_SESSION_IDLE` → `store.NewSessionStore`) — and `AllowedTenants` — enforced in `tenantAllowed` but with no flag/env/chart path able to populate it, so the gate could never engage; `-required-groups` (`oidc.requiredGroups`) remains the supported login gate. Both knobs were deleted rather than wired: wiring `IdleTimeout` would have created a second source of truth for the store's window, and `AllowedTenants` had no reachable configuration. Guard: `TestAuthConfig_NoDeadSessionKnobs` fails if either field returns |
| S26 | Post-logout redirect accepted plaintext `http` | Fixed: `AuthConfig.PostLogoutRedirect` and the discovered `end_session_endpoint` now require an absolute https URL — `http` is accepted only for loopback hosts (`localhost`, 127.0.0.0/8, `::1`; strict `netip` parse — mapped/octal spellings do not count), matching the dev plain-http-on-loopback IdP convention (`oidctest`); the chart schema already required `^https://` (`values.schema.json`). Tests: `TestNewAuthenticator_RejectsBadPostLogoutRedirect`, `TestNewAuthenticator_AllowsLoopbackPostLogoutRedirect`, `TestLogout_UntrustedDiscoveredEndpointIs204` |
| S27 | create-connection echoed JSON decoder detail | Fixed: `ConnectionHandler.Create` decodes via the shared `decodeJSON` helper — which also rejects trailing data after the first document, a check the previous inline decode lacked — answers 400 `INVALID_REQUEST` with the generic "invalid request body", and logs the decode detail server-side with the request id (SEC-I7). `decodeJSON` now returns the error so callers can log it without echoing it. Tests: `TestCreateConnection_InvalidBodyGenericMessage`, `TestCreateConnection_TrailingJSONRejected` |
| S28 | Workspace revoke accepted an unbounded `runtimeGeneration` | Fixed: `POST /internal/v1/broker/workspaces/{uid}/revoke` wrote its `runtimeGeneration` straight into `workspace_revocation` — a far-future value (e.g. 2^62) fenced every generation the workspace would ever mint, permanently (the table has no delete path). `RevokeWorkspaceLeases` now refuses any value beyond the recorded generation plus one (max of `workspaces.runtime_generation` — the mint counter — and the informer binding's observed generation) with `ErrGenerationUnbounded` → 400 `INVALID_REQUEST`, a warn log carrying request id, workspace, requested and recorded generation; calls within the bound stay idempotent and an unknown workspace keeps its no-op shape. Tests: `TestRevokeWorkspaceLeases_GenerationBounded`, `TestRevokeWorkspace_GenerationUnbounded` |
| S29 | Auth-failed requests on the internal mTLS listener were never logged | Fixed: `identify` is the outer middleware on :9443, so no-cert/empty-CN/SPIFFE-mismatch rejections bypassed `logRequests` and left no record at the trust boundary. `identify` now counts every refusal on `tinycdi_internal_auth_failures_total{reason}` (bounded classes `no_cert`/`no_cn`/`spiffe_mismatch`) and emits a warn line rate-limited per reason class (1/s sustained, burst 10) carrying request id, method, peer and the SEC-41-redacted path — raw paths embed lease ids (bearer material); certificate/SAN material is never logged. Tests: `TestIdentify_RejectionsLogged`, `TestIdentify_RejectedPathRedacted`, `TestIdentify_RejectionsRateLimited`, `TestIdentify_RejectionsCounted` |

| S30 | Forged `template-snapshot` annotation drove operator-built pods | Fixed: the snapshot that drives pod building moved to `status.templateSnapshot` — writable only through the `workspaces/status` subresource, which RBAC grants to the operator service account alone (pinned for chart + kustomize by `TestWorkspaceStatusWritableOnlyByOperator`/`TestKustomizeWorkspaceStatusWritableOnlyByManager`). The operator records the snapshot itself from the live template named by `spec.templateRef` and mirrors it to the annotation for readers; a workspace with no status record and no resolvable live template holds `Degraded`/`TemplateInvalid` plus one edge event instead of trusting annotation bytes, and the live-object provenance check is gone by design (the status record *is* the provenance — catalog rotation after admit still runs from it). Upgrade never reads the annotation: a running workspace establishes its record from its operator-owned pod — stamped pods name their revision via `…/template-name`/`…/template-revision` pod annotations a workspace writer cannot set, and pre-stamp (pre-upgrade) pods get one candidate — the live revision `spec.templateRef` resolves to — adopted only when the normalized pod rebuild matches (identity stamps are backfilled onto the pod) — with the recorded content always re-read from the LIVE revision object; an unprovable identity (mismatched, pruned, republished, webhook-altered or forged-spec builds) holds `Degraded`/`TemplateRevisionGone` with the pod left running until a stop/start re-snapshots, and workspaces without a running pod snapshot the live template at next start. The broker's expiry planner likewise ignores the annotation for held workspaces — caps come from the stricter of the resolved template lifecycle and chart defaults (`TestSnapshot_UpgradeAdoption`, `snapshot_adopt_test.go`). Residual: the annotation stays readable/writable by Workspace writers (the backend SA) — it is a mirror for consumers and is never trusted; a ValidatingAdmissionPolicy tightening who may write it is a separate hardening item (S36) |
| S31 | Unauthenticated cookie spray reached Postgres via the session-listener rehydrate path | Fixed: a request carrying a cookie the replica has never seen was the only pre-auth path that spent store reads — a `session_digest` lookup plus an `EXISTS` probe on miss — and unique cookie values mint a fresh singleflight entry each, so the dedup could never help a spray. The unknown-cookie path now sits behind a LOCAL per-client token bucket (`SessionLookupLimiter`, keyed by `ratelimit.ClientKey` — IPv6 folds to /64, non-IP claims collapse to the socket peer) consulted in `lookupSession` before any store read; refusal answers 429 + `Retry-After`, counts `tinycdi_rate_limited_total{route="session_lookup"}` and audits `session.lookup` denied. The bound is deliberately local-only — a shared window would spend the reads it exists to prevent — sized by `-session-lookup-rate` (300/min, burst 120, per replica; 0 disables). Valid-but-uncached cookies draw tokens too (a backend rollout makes every session uncached), so the burst absorbs a 100-deep reconnect storm from one client address. Cookies a live local session owns, cookie-less requests and rehydrated sessions never consult it. Tests `lookup_limit_test.go`, `lookup_limit_config_test.go` |
| S32 | Launch abandoned between ticket redeem and 303 delivery pinned the workspace | Fixed: session registration + `renewLoop` start before the Set-Cookie/303 is written, so a client disconnecting in that window left a session no browser holds renewing the lease — and pinning the workspace (`IssueTicket` → `CONNECTION_IN_USE`) — until takeover, control revoke, idle-stop or the bound portal session's absolute expiry. Sessions now mint unattached: the first request admitted on their own host marks them attached, and the renew loop (the session's existing expiry check) reaps a session still unattached at `-unattached-session-ttl` (90 s). The reap's decision runs under the session lock together with the lease revoke, so an attach that already won can never be revoked — an attach either lands before the decision (the reap aborts, the session lives) or after it (the request loses to a session already committed to die). The revoke drops the pin promptly; it is skipped when the lease's stream epoch is nonzero (a sibling replica already claimed a stream on it: the lease is in use elsewhere and only the local copy dies); a failed revoke still kills the session and the lease lapses inside one lease TTL. Tests `unattached_test.go`, `unattached_race_test.go` — redeem-then-abandon, attached-survives, cross-replica used-lease, revoke-failure, attach/reap serialization under -race |
| S33 | Session listener pinned no `Cross-Origin-Opener-Policy` | Fixed: `Cross-Origin-Opener-Policy: same-origin` is pinned on every session-origin response alongside the existing CSP/Permissions-Policy/CORP/OAC/nosniff/HSTS set, matching the frontend's pin — a document that opens a session-host URL can no longer keep it inside its browsing-context group. Upstream-supplied COOP joined the `responsePolicy` strip list so a tenant-controlled runtime cannot weaken it. Framing is unaffected by construction: embeddability stays governed by CSP `frame-ancestors` (unchanged). Tests `coop_test.go` (`TestResponsePolicy_PinsCOOP`, `TestResponsePolicy_StripsUpstreamCOOP`) |
| S36 | `template-snapshot` mirror annotation writable by any Workspace writer | Hardened (opt-in): chart `admissionPolicy.enabled` (default OFF) and the `config/admissionpolicy/` kustomize sample render a `ValidatingAdmissionPolicy` + binding — `failurePolicy: Fail`, `validationActions: [Deny]`, `admissionregistration.k8s.io/v1` (Kubernetes >= 1.30, the chart's `kubeVersion` floor) — matching only `workspaces` CREATE/UPDATE in the managed namespaces. The CEL exempts the operator ServiceAccount (`system:serviceaccount:<release-ns>:operator` from release values, plus optional `extraAllowedUsernames` for restore tooling); for everyone else a CREATE carrying the annotation is denied and an UPDATE is denied whenever the filtered annotation differs between `oldObject` and `object` (add/change/remove). Nothing trusted reads the annotation (S30), so this is defence-in-depth for consumers that still read the mirror and for the pre-reconcile window. Tests: `chart_admissionpolicy_test.go` (default renders nothing, enabled renders policy+binding with the operator identity), `snapshot_annotation_vap_envtest_test.go` (a real apiserver denies the writer SA and admits the operator) |

*Review note — S17:* the ticket-lock serialization claim (a redeem's
ticket-row `FOR UPDATE` vs the revoke's `UPDATE` under READ COMMITTED) is
the load-bearing ordering argument — it is driven deterministically by
`TestRevokePortalSession_RedeemCommitThenRevoke` and
`TestRevokePortalSession_RevokeCommitThenRedeem`, which pin both
interleavings on real row locks. Since the SIGNOUT-FIX pass the sweeps
also pre-lock in deterministic key order (`lockByKeysInOrderTx`), and
`TestRevokePrincipalSessions_MultiTicketInversion` runs the principal-
scoped sweep against a multi-ticket redeem under `-race` to pin the
no-deadlock claim.

Additional items found while writing this document (not from A6):

- **Cookie-mode `partitioned`** — e2e coverage added on kind
  (`hack/quickstart` + `values-partitioned.yaml`, `partitioned.spec.ts`,
  `ci.yml` job `partitioned`): real Set-Cookie attributes, in-frame
  reconnect across a backend rollout, revoked-lease cookie rejection.
  Same-site topology only; a cross-site deployment is not exercised.
- **Portal idle-extension depends on lease activity** — Implemented
  (FIX-IDLE): portal reads are all passive server-side, so no GET a client
  can shape extends the idle window; extension only ever credits
  (a) mutations — including the explicit activity beat
  `POST /v1/session:touch` — passive auth + CSRF with the slide applied
  explicitly only after the token check, so a cookie-only request never
  earns a slide; login-family rate limit keyed on the session digest; the
  SPA sends it on pointer/key/navigation events throttled to 1/min, never
  from timer polls — and (b) RFB input measured broker-side. Forging a
  touch requires the session cookie + CSRF token — the same bar as any
  mutation, so the beat grants nothing a caller could not already do. That input touch
  is now scoped to the bound portal session digest — the session the
  stream's lease was minted under — rather than every session of the
  principal (SR-1-F3; `TouchSessionDigest`,
  `internal/store/sessions.go`), with the principal-wide path kept only
  as the NULL-digest fallback for pre-binding leases. Lease
  redeem/renew/rehydrate honour the portal idle window (SR-1-F2): the
  liveness re-checks in `loadLease` and `RedeemTicket` consult
  `last_seen_at` under `WithSessionIdle`, so an idled-out session's lease
  is revoked at the next renew (reason `invalid`) and a stale cookie
  cannot rehydrate it. Renew deliberately does NOT slide the window —
  otherwise a connected-but-idle stream would pin the session open
  forever — so an abandoned desktop tab dies with its portal session
  inside one renew cycle.
- **Metrics listener** is scrape-only but has no auth — Implemented:
  served on the dedicated ClusterIP `backend-metrics` Service; the only
  rule opening the metrics port is `allow-metrics-scrape` admitting
  exactly `networkPolicy.prometheusPeers`, and no edge rule (ipBlock, any
  or cilium `edgeIngress`) carries it (`TestMetricsListenerIsolation`,
  `TestEdgePolicyNeverOpensInternalPort`); the observability runbook tells
  operators to keep it off the edge path.
- **Operator leader election off by default** — Implemented: election is
  on by default since v0.3.0 and `operator.leaderElect=false` with
  `operator.replicas>1` now fails the chart render (`tinycdi.validate`;
  `TestOperatorLeaderElectionGuard`); the binary cannot observe the
  replica count, so the guard is chart-side only (cmd/operator note).
  Single-replica installs (`replicas: 1`) may still run unelected.
- **`kasmweb/*` third-party images** — the adapter neutralizes their startup
  but they carry KasmVNC 1.4.0 (vs the pinned 1.5.0 in TinyCDI-built images);
  the catalog gate enforces a floor — reviewer should confirm the floor and
  the exception file's expiry policy together.
- **Postgres backup/restore of digest-keyed sessions** — covered by S23:
  the restore semantics are documented and the resurrection risks have a
  tool + tests; what remains manual is running the procedure and the
  drill.
- **Audit trail completeness** — coverage of admin operations and
  mutating routes is implemented (#120, S21); what remains for review is
  integrity/durability of the stdout JSONL stream, which is delegated to
  the operator's log pipeline (runbook "Log integrity"), and the fact
  that platform-level admin actions that never pass through TinyCDI —
  `WorkspaceTemplate` CRD writes, Helm-value changes — are only in the
  Kubernetes API audit log.

### What the v1.0 reviewer should re-verify

Load-bearing claims the tests pin today, worth re-checking on the tree
under review:

- **Upstream `Authorization` overwrite** — the session proxy must
  *replace* (not merely strip) a client-supplied `Authorization` header
  on every proxied path, since the pod-side credential check is only
  unreachable while broker injection is unconditional
  (`TestProxy_StripsClientAuth`; S18, ADR 0008). Injection lives in the
  `Rewrite` hook — after the stdlib hop-by-hop strip — precisely so a
  client `Connection:` token list cannot remove it
  (`connheaders_test.go`).
- **Per-workspace NetworkPolicy assumes an enforcing CNI** —
  `ws-<uid>-boundary` policies bind only where the cluster CNI implements
  NetworkPolicy (`docs/compatibility.md` lists Cilium; the kind e2e runs
  on one that enforces). A non-enforcing CNI silently reverts workspace
  ingress/egress to open — confirm the supported-deployments matrix keeps
  that assumption explicit.
- **Lease renew/attach session check coverage** — `liveLease`
  re-validates the bound portal session on every renew/attach/claim and
  on the `LeaseBySession` cookie resolve (S17). Re-verify that every new
  lease-read path funnels through it and that the NULL-digest exemption
  still matches the rolling-upgrade story.
- **Deterministic lock order** — the no-deadlock argument for the
  session-layer revokes is `lockByKeysInOrderTx` (tickets → sessions →
  leases, ascending key order within each; `sessions.go:418`), pinned by
  the two redeem/revoke interleaving tests and the `-race`
  multi-ticket inversion run. Re-check when new session-layer mutators
  land.

## 7. What the reviewer should read first

1. `SECURITY.md` — normative hardening rules + operator checklist.
2. `docs/adr/0003`, `0004`, `0005` — internal API contract, launch origin
   policy, component/session-host split (0005 self-declares as not
   independently reviewed).
3. `docs/security/test-inventory.md` — the automated evidence that exists.
4. `docs/security/provenance.md` + `vulnerability-policy.md` — supply-chain
   posture and scan gates.
5. This file's §6 table — the explicit question list.
