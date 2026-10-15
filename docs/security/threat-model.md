# Threat model

This document describes the TinyCDI 1.0.0 architecture as built: its
assets, actors, trust boundaries and the controls at each boundary, with
references to the code that implements a control and the test that pins
it. It is not an independent audit, and it does not set policy:
[`SECURITY.md`](../../SECURITY.md) is the normative policy for
contributors and operators. Threats carry ids `TM-NN`. Each id is one row
of the register in [§6](#6-threat-register), and every row names its
control and the test, gate script or CI job that pins it. §7 lists the
residual risks the project accepts, and §8 is the guide for a reviewer.

## 1. System in one paragraph

TinyCDI is a Kubernetes-native browser/desktop-as-a-service platform. Three
components ship as images: **frontend** (static SPA and browser security
headers; it does not proxy `/v1`), **backend** (public API, session
gateway and in-process broker on four listeners) and **operator**
(reconciles the `Workspace` and `WorkspaceTemplate` CRDs and creates
runtime pods, Secrets, Services and per-workspace NetworkPolicies in
managed namespaces). Users authenticate through an external OIDC IdP, get
a launch ticket, and their browser is reverse-proxied to a per-workspace
pod. Inside the pod, the transfer helper `tcdi-xferd` owns the only
exposed port and proxies the desktop to a KasmVNC server that listens on
loopback. The pod is **tenant-controlled**: the user has a shell in it
(see [`SECURITY.md`](../../SECURITY.md)). Containers share the host
kernel; the product does not claim isolation between mutually hostile
tenants.

Listeners (`internal/backend/config.go`, `listeners.go`;
`build/frontend/main.go`; `cmd/tcdi-xferd/main.go`):

| Component | Listener | Flag / port | Serves |
|---|---|---|---|
| frontend | HTTPS | `-listen` `:8443` | static SPA, `/branding/` |
| backend | app | `-listen` `:8443` | public API `/v1/*` (OIDC login, workspace CRUD, admin API, ticket and transfer-grant mint) |
| backend | session | `-session-listen` `:8444` | per-workspace hosts `<label>.<sessionDomain>`: launch `POST`, desktop proxy, transfer leg; in-cluster control surface on control hosts |
| backend | internal | `-internal-listen` `:9443` | mTLS broker API used by the operator ([ADR 0003](../adr/0003-gateway-broker-internal-api.md)) |
| backend | metrics | `:9090` | scrape-only metrics |
| workspace pod | `tcdi-xferd` | `:8443` | `/xfer/` file transfer; proxies everything else to KasmVNC at `https://127.0.0.1:8444` |

The gateway reaches a pod through its Service `ws-<uid>` on port `8443`
(`internal/runtime/linux/backend.go`, `streamingPort`).

## 2. Assets

- **User accounts and sessions**: OIDC identity, the server-side portal
  session (`__Host-tcdi_session`), and the retained `id_token` used as
  `id_token_hint` at logout.
- **Workspace sessions**: the interactive KasmVNC stream, clipboard
  content, files moved through the transfer leg, and anything the user
  does inside the desktop (which can have Internet egress).
- **Workspace data**: persistent home volumes and retained disks.
- **Credentials and keys**: OIDC client secret, AEAD login-state sealing
  keys, session cookie values, launch tickets, transfer grants,
  per-workspace KasmVNC credentials and `xfer-token`, internal mTLS
  certificates, CA bundles and the serial blocklist, Postgres DSN,
  session-listener control bearer token.
- **Control plane**: Postgres (sessions, leases, tickets, transfer grants,
  quotas, principal directory, audit store), the `Workspace` and
  `WorkspaceTemplate` CRDs, and the managed namespaces the operator writes
  to.
- **Audit record**: the `audit_event` table and the stdout audit stream.
- **Supply chain**: published images and Helm chart, their signatures,
  SBOMs and provenance attestations; the CI workflows that produce them.
- **Availability**: backend replicas, Postgres, the runtime pods and the
  quotas that bound them.

## 3. Actors

- **End user (semi-trusted)**: authenticated through OIDC and gated by
  `oidc.requiredGroups`. Fully controls their workspace pod at runtime
  (shell access); may be curious or abusive but is not assumed to be a
  kernel-level attacker on shared nodes.
- **Tenant admin**: a user whose verified `groups` claim includes one of
  the configured admin groups (`-tenant-admin-groups` /
  `oidc.tenantAdminGroups`, default `tenant-admin`). Gets a tenant-scoped
  admin surface: overview, users, live sessions and sign-out, audit log,
  quotas and per-user limits, and the session-policy ceiling. A
  misconfigured group list is a privilege grant (TM-11).
- **Operator / platform admin**: deploys the chart, owns Postgres, the
  OIDC client, sealing keys, mTLS material and node pools. Trusted.
- **Unauthenticated Internet client**: can reach the portal host, the app
  listener's login endpoints and every `*.<sessionDomain>` name.
- **External OIDC IdP**: trusted for identity assertions; the backend
  consumes its discovery document and JWKS.
- **Kubernetes API**: trusted; the operator and backend hold scoped RBAC.

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
   |  `--(5) launch POST, desktop HTTP/WS, transfer leg --> backend session :8444
   |                                       | (6) Service ws-<uid> :8443, pinned CA,
   |                                       v     injected credentials
   |                       workspace pod [tenant-controlled]
   |                         tcdi-xferd :8443 --> KasmVNC 127.0.0.1:8444
   |
 operator --(7) mTLS, client cert --> backend internal :9443
 operator --(8) k8s API ----------> pod / Secret / Service / NetworkPolicy
 kasm-adapter init image ---------> shared emptyDir (read-only into the desktop container)
```

### Boundary 1 — browser ↔ edge

Flows: everything above crosses one TLS edge. The chart enforces Ingress
TLS and an HTTPRoute `sectionName`; `*.<sessionDomain>` uses one wildcard
DNS name and one wildcard certificate
(`deploy/helm/tinycdi/templates/httproute.yaml`, `ingress.yaml`). Both
the portal and the session listener send HSTS.

Controls:

- All portal cookies carry the `__Host-` prefix and are host-only
  (`internal/api/auth.go`, the `SessionCookieName` / `LoginCookieName`
  block). The workspace session cookie is `Secure`, `HttpOnly`,
  `SameSite=Lax`, or `SameSite=None; Secure; Partitioned` under the
  optional `partitioned` cookie mode (`ParseCookieMode`,
  `internal/gateway/proxy.go`).
- The SPA reads the CSRF token and the session domain from `GET /v1/me`
  (`internal/api/me.go`), never from `document.cookie`; no cookie is
  JS-readable.
- NetworkPolicy restricts app and session ingress to the edge
  (`deploy/helm/tinycdi/templates/networkpolicy.yaml`, `edgeIngressRule`).

Register: TM-01.

### Boundary 2 — edge ↔ frontend and backend app listener :8443

Flows: `GET /*` goes to the frontend's static SPA; `/v1/*` goes to the
backend API. The edge routes by path; the frontend never proxies `/v1`
(`build/frontend/main.go`, `TestNoAPIProxy`).

Controls:

- **Frontend security headers**: CSP (including
  `frame-src https://*.<sessionDomain>`,
  `connect-src 'self' https://*.<sessionDomain>`,
  `form-action 'self' https://*.<sessionDomain>` and
  `frame-ancestors 'none'`), COOP `same-origin` and the rest of the portal
  policy are set by `securityHeaders` and `frontendCSP`
  (`build/frontend/main.go`). The SPA session page sets the iframe
  `sandbox` and `allow` attributes (`web/src/`).
- **API response headers**: `APIBrowserHeaders` (`internal/api/headers.go`)
  is the outermost app-listener middleware (`wrapApp`,
  `internal/backend/wire.go`). Every `/v1` answer carries
  `X-Content-Type-Options: nosniff`,
  `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'`,
  `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer` and, unless the
  handler sets its own, `Cache-Control: no-store, no-transform`.
- **OIDC login**: authorization code with PKCE S256; go-oidc discovery
  verifies issuer and audience (`internal/api/auth.go`). The in-flight
  login rides in an AEAD-sealed `__Host-tcdi_login` cookie (AES-256-GCM,
  `internal/api/loginstate/sealer.go`), so a login can start on one
  replica and finish on another. `keys[0]` seals and every listed key
  opens, for rotation (`-login-key-file`). State, nonce and PKCE verifier
  are bound to the initiating browser.
- **Portal session**: server-side with an opaque cookie value. Rows are
  keyed by the SHA-256 digest of the session ID, so a database or backup
  read never yields a usable cookie (`internal/store/sessions.go`). The
  idle window is 30 minutes (`-session-idle`) and the absolute cap 12 h
  (`AuthConfig.AbsoluteTimeout`). The retained `id_token` is sealed at
  rest with the same keys under a purpose-bound AAD
  (`internal/api/idtoken.go`); if it cannot be opened, logout degrades to
  a `client_id`-only end-session URL. The retained `id_token` reaches the
  browser at sign-out: `POST /v1/logout` and
  `POST /v1/me/sessions:revoke-all` return it as `id_token_hint` in
  `endSessionUrl`, so it lands in browser history and the provider's
  access logs. At login, an existing session cookie is deleted unless it
  is a live session of the same principal, which is kept and
  re-issued (`CallbackHandler`, `internal/api/auth.go`): a cross-site navigation
  to `/v1/login` cannot end the user's live desktop, and a planted or
  foreign session is still destroyed.
- **CSRF**: a synchronizer token derived from the session ID
  (`csrfTokenFor`, `internal/api/auth.go`), checked in constant time by
  `RequireCSRF` (`internal/api/middleware.go`). Defense in depth:
  `RequireTrustedOrigin` requires a single `Origin` byte-equal to the
  allowlist, or `Sec-Fetch-Site: same-origin` when `Origin` is absent.
- **Rate limits**: shared Postgres fixed-minute windows over per-replica
  token buckets ([ADR 0006](../adr/0006-rate-limit-state.md);
  `internal/ratelimit`, `internal/store/rate_limit.go`), applied to
  `/v1/login`, `/v1/auth/callback`, the `GET /v1/session` probe,
  `POST /v1/session:touch` and `POST /v1/me/sessions:revoke-all`. Keys
  are the client IP, or a digest of the validated session ID or OIDC
  login state when present, so users behind one NAT keep separate buckets
  (`internal/api/ratelimit_key.go`); the callback also has a 10× per-IP
  ceiling. Session-store reads for cookie values are bounded per client
  on the app listener too: `lookupSession`
  (`internal/api/session_lookup.go`) spends a token from a local bucket
  (`-session-lookup-rate`, burst 120) before the read and returns it
  only when the read finds a live session — a store error keeps it
  spent. A cookie the replica confirmed live in the last 5 minutes (a
  bounded cache of SHA-256 digests) skips the budget but is still read,
  so a forged-cookie spray from a shared client key cannot lock signed-in
  users out, and logout, revocation and expiry still end the session;
  when the budget is spent, authenticated routes answer `429` to other
  cookies and the probe answers signed out, without a store read. Session reads never
  delete; the leader prunes dead rows (`PruneDeadSessions`). While the
  store is healthy the shared window is the exact
  bound and each pod's local bucket is a store-protection prefilter; the
  divided per-replica bucket is the floor only while the circuit breaker
  is open. Every store call has a 500 ms deadline; an error opens the
  circuit for a 10 s cool-down that skips the store, and one
  single-flight probe re-tests it (`internal/ratelimit/shared.go`). The
  default window bounds are login 80/min, launch 160/min and callback
  ceiling 800/min. Client IPs come from the socket peer, or from the
  right-most untrusted `X-Forwarded-For` entry when the peer is inside
  `-trusted-proxies` (`ClientKey`, `ParseTrustedProxies`,
  `internal/ratelimit/ratelimit.go`); IPv6 folds to its /64.
- **Passive authentication**: every cookie-authenticated `GET` mounts
  `RequireAuthPassive` (`internal/api/middleware.go`), which never
  extends the idle clock, so interval polls cannot hold an unattended
  session open. The idle window slides only on mutations — including the
  explicit activity beat `POST /v1/session:touch`, applied after the
  CSRF check — and on desktop input measured by the broker, which credits
  only the portal session the stream's lease was minted under
  (`TouchSessionDigest`, `internal/store/sessions.go`).
- **Tenant scoping**: the principal is built only from verified claims
  (`internal/api/principal.go`). The tenant-admin role resolves once per
  request in `RequireAuth` against the configured admin groups (exact
  match; an explicitly empty list is a startup error), and every admin
  gate consults `Principal.IsTenantAdmin`; no handler names a group
  literal. A foreign workspace ID answers 404, never 403. Events shown to
  users are curated, not raw (`internal/api/events.go`,
  `statusview.go`). `tenant_id` must come from an admin-controlled IdP
  mapper (`SECURITY.md` operator checklist).
- **Quotas**: per-tenant limits (`internal/api/quota.go`,
  `adminquota.go`; `internal/backend/tenantquota.go`) and per-principal
  running-workspace limits written through the tenant-admin
  `/v1/admin/tenants/{tenant}/user-limits` API (`adminuserlimit.go`) and
  enforced inside the reservation transaction under the `tenant_quota`
  row lock (`checkUserLimit`, `internal/provisioning/quota.go`). A tenant
  admin can write the tenant quota (`PUT /v1/admin/tenants/{tenant}/quota`)
  only up to the operator-declared ceiling
  (`managedNamespaces[].quotaCeiling`, `-tenant-quota-ceilings`); with no
  ceiling declared the write is refused (`QUOTA_ABOVE_CEILING`), and a
  tenant with a declared `quota` is read-only (`QUOTA_MANAGED_BY_CONFIG`).
  The live count comes from `held` reservations; disk-only holds and retained disks
  never count; a missing workspace row fails closed; refusal is 409
  `QUOTA_EXHAUSTED` with reason `UserLimitReached`. No rows means
  unlimited.
- **Branding**: the frontend serves an operator-supplied directory at
  `/branding/` with traversal and symlink containment (`branding`,
  `serveBranding`, `brandingFile` in `build/frontend/main.go`).
- **Tenant-admin surfaces**: every `/v1/admin/tenants/{tenant}/…` route
  goes through `tenantAdminPrincipal` (`internal/api/adminquota.go`),
  which requires the tenant-admin role for the path tenant; every query
  is scoped by tenant in SQL; every mutation is CSRF-guarded and every
  route is audited. The audit viewer, user directory, live sessions and
  sign-out, overview and session-policy `:apply` each have a register
  row (TM-12 to TM-17).
- **File-transfer grants**: `FileTransferHandler.Create`
  (`internal/api/filetransfers.go`) mints a single-use grant that the
  session listener's transfer leg (Boundary 5) spends (TM-18).
- **Audit trail**: every mutating API route and every `/v1/admin/` read
  emits exactly one domain event through `audited()`
  (`internal/api/auditroutes.go`). The actor is pseudonymous
  (`observability.ActorRef`), the outcome derives from the response
  status, `role=tenant-admin` marks elevated use of shared routes, and a
  handler panic still emits a failure. Every event emitted through
  `audited()`, and the broker's transfer claim and completion, is
  persisted to `audit_event` in the audited transaction; if persisting
  fails, the request fails (`DurableAuditSink`,
  `internal/api/auditstore.go`). See TM-50,
  [ADR 0010](../adr/0010-audit-store-and-retention.md) and
  [audit.md](../runbooks/audit.md).

Register: TM-02 to TM-18.

### Boundary 3 — backend ↔ OIDC IdP

Flows: discovery and JWKS fetch, authorization-code token exchange
(confidential client; with PKCE a secretless public client also works),
RP-initiated logout.

Controls: exact issuer and audience match through go-oidc.
`endSessionURL` (`internal/api/auth.go`) is built only from discovery and
static configuration, so request input cannot produce an open redirect.
`PostLogoutRedirect` and the discovered `end_session_endpoint` must be
absolute `https` URLs; `http` is accepted only for loopback hosts. The
client secret comes from `oidc.existingSecret`. Tests: `auth_test.go`,
`logout_test.go` and the in-memory IdP in `oidctest/`.

Register: TM-19, TM-20.

### Boundary 4 — backend ↔ Postgres

Flows: sessions (digest-keyed), connection leases, launch tickets,
transfer grants, quotas, principal directory, intent log, audit store.

Controls: the DSN's `sslmode` must verify the server certificate:
`checkDatabaseTLS` (`internal/backend/config.go`) refuses
`disable`, `allow`, `prefer` and `require` unless `-dev-insecure-db` is
passed, and the chart defaults `database.tls.mode: verify-full`. Session
rows store only SHA-256 digests and sealed `id_token` blobs; ticket and
cookie values are stored hashed. Session rows carry
`platform_meta.session_epoch` at insert and every read compares it with
the current value in the same statement (`internal/store/sessions.go`),
so rotating the epoch (the `backend post-restore` step) makes every row a
restored dump brings back dead on its next read. Schema creation takes a
lock, so concurrent replicas cannot race it
(`internal/backend/migrate_lock_test.go`). NetworkPolicy allows database
egress only to `database.allowedPeers`; an empty list fails the render.

Register: TM-21, TM-22.

### Boundary 5 — browser ↔ session listener :8444 (launch, desktop proxy, transfer leg)

Flows: a one-time launch `POST` (ticket redemption) on
`<label>.<sessionDomain>`; the cookie-gated reverse proxy for the KasmVNC
HTTP and WebSocket stream; the transfer leg
`/v1/transfers/{id}/content`; `/healthz` and `/v1/control/*` on
in-cluster control hosts only.

Controls:

- **Host classes**: `Gateway.ServeHTTP` (`internal/gateway/proxy.go`)
  routes by host class first. Workspace hosts are matched by
  `sessionhost.Domain.Match` (label `ws-[a-z0-9]{8,60}`), control hosts
  come from `-session-control-hosts`, and anything else gets
  `421 bad_host`. API paths do not exist on this listener. The host class
  is a routing check on the client-chosen `Host` header, not a network
  boundary: a client that reaches `:8444` directly can name a control
  host. `/healthz` and `/readyz` answer on control hosts only
  (`controlHostHealth`, `TestSessionListener_HealthOnlyOnControlHosts`).
- **Launch ticket**: 60 s TTL, single use, consumed by every redemption
  attempt that passes the gateway-audience check, whatever its outcome (an
  expired, revoked or refused attempt commits the consume;
  `TestRedeemTicket_EveryRefusalConsumes`), stored as SHA-256, and bound to the
  workspace UID, tenant, principal, gateway audience and portal session
  (`IssueTicket`, `RedeemTicket`, `internal/broker/tickets.go`). The ticket
  travels in the `POST` body only; `?ticket=` is refused with 403. A
  launch without a valid tab id (`tcdi_tab`) is refused with
  `400 owner_tab_required` before the ticket is redeemed
  (`internal/gateway/launch.go`). The launch origin policy
  ([ADR 0004](../adr/0004-launch-origin-policy.md), `launchOriginOK`):
  when `Sec-Fetch-Site` is present, an absent or `null` Origin never
  redeems and a present Origin must be in the portal allowlist; without
  fetch metadata a present Origin is still checked, and a request with
  neither header is treated as a non-browser client and admitted (the
  ticket is still required). Rate limiting runs before redemption, so a
  limited attempt never consumes the ticket.
- **Per-workspace cookie and host binding**: `__Host-tcdi_session` is set
  host-only on the workspace host; every proxied request's cookie digest
  must resolve to a lease whose workspace UID matches the request host
  label, and a mismatch is denied and audited.
- **Leases and fencing**: one interactive stream per lease. A new upgrade
  fences the old socket (`admitUpgrade`,
  `internal/gateway/authorization.go`), and the Postgres `stream_epoch`
  fences streams admitted by a dead replica. Every stream claim carries
  the owning browser tab's id (`ValidStreamOwnerTab`,
  `Broker.ClaimStream`, `internal/broker/sessions.go`). Leases renew
  about every 10 s; a lease that stops renewing fails closed within
  30 s, and revoke, expiry and supersede all kill the stream. Every lease
  read that feeds renew, attach, claim or the cookie-to-lease resolve
  (`liveLease`, `LeaseBySession`) re-validates the bound portal session
  (epoch, absolute expiry and idle window) in the same indexed read and
  revokes the lease on the spot when it fails.
- **Rehydration**: a cookie unknown to the replica's memory is resolved
  by SHA-256 digest to its lease, and the session is rebuilt on any
  replica; expired or revoked leases are refused
  (`internal/gateway/rehydrate.go`). Unknown cookies are bounded by a
  local per-client token bucket before any store read
  (`SessionLookupLimiter`, TM-27).
- **WebSocket origin**: an upgrade requires `Origin` equal to the
  workspace host's own origin (`originOK`); upgrade detection is strict
  RFC 7230 (`isUpgrade`).
- **Response policy**: `sessionCSP`, `Cross-Origin-Resource-Policy:
  same-origin`, `Cross-Origin-Opener-Policy: same-origin`,
  `Origin-Agent-Cluster: ?1` and `frame-ancestors <portal-origin>` are set
  on every response, and the upstream's copies are deleted, so a hostile
  runtime cannot weaken them (`responsePolicy`,
  `internal/gateway/proxy.go`). The upstream's `Content-Disposition` is
  dropped as well, so a desktop path is never offered as a download. The
  proxy's response writer drops every
  upstream informational response other than `101` and re-applies the
  full set when the final status is written, so a `1xx` from the runtime
  can neither reach the browser nor clear the set (`hijackCapture`,
  `pinSessionHeaders`; `TestResponsePolicy_Upstream1xxCannotStripPinnedHeaders`).
- **Upstream path allowlist**: only the paths the stock KasmVNC client
  needs are proxied (`upstreamPathOK`, `internal/gateway/authorization.go`);
  everything else, including any upstream management API and `/xfer/`,
  is refused. `cleanedProxyPath` rejects traversal and parser-differential
  tricks (dot segments, encoded dots, slashes and backslashes, double
  encodings) before the allowlist runs.
- **Transfer leg**: `Gateway.serveTransfer`
  (`internal/gateway/transfer.go`) is the only path that builds
  `/xfer/files/<name>` (`helperURL`). It checks Origin, the session
  cookie, host and live lease, then claims the grant through
  `Broker.ClaimTransfer` before any byte reaches the pod (TM-30, TM-31).
- **Control surface**: `/v1/control/*` answers only on control hosts, and
  the bearer from `-control-token-file` is its only gate (`controlAuth`,
  `internal/gateway/proxy.go`). The backend refuses to start with a token
  shorter than 32 bytes (`checkControlToken`, `TestControlToken_MinLength`).
  Each client key may fail the bearer check 10 times, refilled at 10 per
  minute; after that every control request from that client answers `429`,
  the right token included (`TestControlAuth_FailuresLimitedPerClient`).
- **Timeouts**: the app, internal and metrics listeners bound every
  request phase; the session listener bounds the header read (10 s)
  and the whole request read (`sessionReadTimeout`, 30 s), so a client
  that stalls a declared body is cut at the bound
  (`TestSessionListener_StalledBodyBounded`). Headers are capped at
  64 KiB. net/http clears the deadline when a WebSocket upgrade is
  hijacked, and the transfer leg extends its own deadline after the
  broker claim, so admitted streams and granted transfers outlive the
  bound (`TestSessionListener_AdmittedStreamOutlivesReadBound`,
  `TestSessionListener_TransferUploadOutlivesReadBound`;
  `internal/backend/listeners.go`).

Register: TM-23 to TM-31.

### Boundary 6 — session listener ↔ workspace pod (`tcdi-xferd` :8443 → KasmVNC 127.0.0.1:8444)

Flows: the gateway reverse-proxies HTTP and WebSocket to Service
`ws-<uid>` on `:8443`, where `tcdi-xferd` serves `/xfer/` itself and
proxies everything else to KasmVNC on `https://127.0.0.1:8444`
(`cmd/tcdi-xferd/main.go`, `build/linux-base/kasmvnc.yaml`). The pod has
egress to the Internet (by network profile) and to cluster DNS.

Controls:

- **Per-workspace NetworkPolicy** `ws-<uid>-boundary` (`buildNetPol`,
  `internal/runtime/linux/backend.go`): default deny, ingress on TCP
  `8443` from gateway pods only, DNS egress, and profile-scoped egress.
  The chart renders `default-deny` baselines for the release and managed
  namespaces (`templates/networkpolicy.yaml`). `ClusterOnly` egress is
  restricted by default to pods in the workspace's own tenant's
  namespaces (its `workspaces.cdi.tinyorbit.vn/tenant` label value), and
  the policy is created before the pod so a runtime never runs without
  it (`Ensure`); the
  `runtime.networkProfiles.clusterOnly.*` values narrow it further or,
  with `unrestricted: true`, open it to any pod (TM-34). Opt-in
  `managedNamespaces[].namespaceQuota` bounds a namespace's summed
  resources (TM-44).
- **Upstream TLS pin and credentials**: the runtime's self-signed
  certificate is verified against a per-lease pinned CA pool with a
  pinned server name, never `InsecureSkipVerify`
  (`TestUpstreamTLS_RequiresPinnedCA`). The gateway injects the
  workspace's own KasmVNC credential from the workspace Secret
  (`internal/broker/credentials.go`) and overwrites any client
  `Authorization` header. The injection runs in the proxy's `Rewrite`
  hook, after the hop-by-hop strip, and client `Connection` token lists
  are reduced to `upgrade`, `keep-alive` and `close`, so a client cannot
  name an injected header for removal.
- **Forwarded-header hygiene**: the client-supplied forwarding family
  (`X-Forwarded-*`, `X-Original-*`, `X-Rewrite-Url`, `Forwarded`,
  `X-Real-Ip`) and the vendor client-address headers (`Via`,
  `X-Client-Ip`, `True-Client-Ip`, `CF-Connecting-Ip`,
  `X-Cluster-Client-Ip`, `X-Envoy-*`) are stripped from one table, and the
  address headers are rebuilt from the verified chain only
  (`rewriteForwarded`, `internal/gateway/proxy.go`).
- **Pod hardening**: `RunAsNonRoot`, `AllowPrivilegeEscalation=false`,
  all capabilities dropped, seccomp `RuntimeDefault` (or a validated
  `localhost/` profile, TM-47) and AppArmor `RuntimeDefault` or
  `localhost/` (`buildPod`, `internal/runtime/linux/backend.go`).
  The browser seccomp profile `localhost/profiles/chromium-userns.json`
  lets the workspace user create user, PID and network namespaces, which
  the Chromium sandbox needs; it widens the kernel surface reachable from
  the pod, so the operator admits it only when the pod runs with
  `hostUsers: false` (`requireOwnUserNamespace`, TM-47), and the chart
  fails the render otherwise.
  `runtime.appArmor.requireRuntimeDefault=false` drops the fail-closed
  guarantee on nodes without AppArmor (`AppArmorNotRequired`, TM-41).
  The pod spec has no process limit: the process and thread count is
  bounded by the kubelet's `podPidsLimit` on the workspace nodes, which
  the install guide requires and the preflight `pids` check reads.
  Without it, a workspace bounded only by its memory limit can exhaust
  the node's `kernel.threads-max`.
- **Kasm adapter**: an init image writes adapter scripts into a shared
  `emptyDir` mounted read-only into the desktop container
  (`build/kasm-adapter/main.go`). The adapter neutralizes the image's
  baked-in `VNC_PW` defaults, makes the root filesystem read-only outside
  the writable mounts, shadows Chromium wrappers with the
  sandbox-preserving `browser-shim.sh`, and drops managed Chromium policy
  into each engine's policy directory (`tests/integration/kasm_adapter_test.go`,
  `deploy/helm/kasm_adapter_test.go`, CI `kasm-contract` job).
- **Clipboard policy (server side)**: the template `clipboardPolicy`,
  narrowed by the tenant ceiling, reaches the pod as
  `TCDI_CLIPBOARD_POLICY`. Both session entrypoints render KasmVNC's
  `data_loss_prevention.clipboard` block from it into
  `$HOME/.vnc/kasmvnc.yaml`, which merges after `/etc` and wins per key.
  Direction semantics are defined once in `internal/api/openapi.yaml`:
  `Send` maps to `client_to_server` only, `Receive` to `server_to_client`
  only (the primary selection follows it), `Bidirectional` to both; unset
  or unknown renders `Disabled`. Client-side overrides and DLP logging
  stay off, and `runtime_configuration` turns Xvnc's parameter-override
  list on with no entries
  (`allow_override_standard_vnc_server_settings: true`,
  `allow_override_list: []`, rendered as `-AllowOverride ""`). An X
  client in the session therefore cannot flip `SendCutText`,
  `AcceptCutText`, `SendPrimary` or `SetPrimary` with `vncconfig`
  (`.github/tests/clipboard-policy.test.sh`,
  `TestLinuxRuntimeClipboardParamsNotOverridable`,
  `TestKasmAdapterChromium`). The gateway's `clipboard_up/down/seamless`
  client flags and the portal iframe `allow` attribute are
  least-privilege hints on top; enforcement is the server's
  `SendCutText` / `AcceptCutText` (TM-36). The policy binds an
  unmodified runtime: a process the workspace user starts can still
  stop Xvnc and run a server of its own, the same in-pod trust limit as
  the transfer helper (§7 item 1).
- **Tenant session-policy ceiling**
  ([ADR 0009](../adr/0009-session-data-transfer-policy.md)): a tenant
  admin can cap the clipboard and file-transfer policy below every
  template's own policy through
  `PUT /v1/admin/tenants/{tenant}/session-policy` (tenant-admin only,
  CSRF and `If-Match`, audited). One resolver computes the meet; a
  ceiling never widens. Launch tickets record the resolved vector, the
  transfer claim re-resolves it, and the workspace's
  `spec.sessionPolicy` drives the pod's environment. A running workspace
  keeps its start-time server-side policy until its next start or until
  an admin restarts it with `:apply` (TM-17); until then the
  server-side residual is bounded by the template's own policy.
- **Node isolation (posture)**: the chart places workspaces on a
  dedicated tainted node pool by default; `allowSharedNodes` is opt-in
  and warns; the node-profile DaemonSet delivers AppArmor and seccomp
  profiles (`deploy/node-profiles/`, `templates/nodeprofiles.yaml`).
- **Connection bound**: the gateway caps live upstream connections per
  workspace per replica (`-session-stream-cap`, TM-35).

Hostile-pod escape and kernel CVEs are outside the product's isolation
claim (`SECURITY.md`); `runtimeClassName` passes through for operators
who run a sandboxed runtime.

Register: TM-32 to TM-37.

#### KasmVNC credential brute force

KasmVNC enforces a per-address credential blacklist (`blacklist_threshold:
5` failures, `blacklist_timeout: 10` seconds; `build/linux-base/kasmvnc.yaml`
and the kasm adapter's generated config in `build/kasm-adapter/entrypoint.sh`).
No client can feed that check a guess:

- KasmVNC listens on `127.0.0.1:8444` (TLS) inside the pod. `tcdi-xferd`
  owns the pod's only exposed port, `:8443`, and reverse-proxies the desktop
  to it. A desktop request without Basic credentials is answered by
  `tcdi-xferd` with its own `401` and never forwarded, so anonymous probes
  never reach KasmVNC.
- The pod NetworkPolicy admits ingress on TCP `8443` from gateway pods only.
  Xvnc also binds `:8444/udp` (KasmVNC has no switch to turn its UDP
  transport off); no ingress rule admits UDP, and the kasm adapter contract
  test asserts no other wildcard-bound UDP listener exists in the
  kasm adapter image.
- The gateway overwrites `Authorization` on every proxied request with the
  workspace's own credential (`TestProxy_StripsClientAuth`), so a client
  cannot present a guess through the gateway either.
- The blacklist stays on as defense in depth. It keys on the forwarded
  client address, which the gateway rebuilds and `tcdi-xferd` passes
  through unchanged.

Residual: a process inside the pod can reach `127.0.0.1:8444` directly, but
it already runs as the desktop user, who can read the credential from the
workspace Secret mount, so it gains nothing.

#### In-pod transfer helper

This is the trust model of
[ADR 0009 §5](../adr/0009-session-data-transfer-policy.md#5-file-transfer).
Register row TM-37.

- The workspace Secret is mounted with mode `0440` and `fsGroup` 1000.
  The desktop and `tcdi-xferd` both run as uid 1000 in one container
  (`internal/runtime/linux/backend.go`;
  `build/linux-base/Dockerfile:106`). So any process the workspace user
  starts can read `xfer-token`, the KasmVNC password and the TLS key.
- Such a process can kill `tcdi-xferd` (same uid) and bind `:8443` (an
  unprivileged port) with the same certificate, so the gateway cannot
  tell the difference. It holds the port only until the entrypoint's
  supervision loop sees the helper gone and exits the container: about
  14 s, a 2 s poll then a 12 s grace (`build/linux-base/entrypoint.sh`,
  `build/kasm-adapter/entrypoint.sh`). It can do this again after
  each restart.
- The token therefore proves only "the gateway, or a process in this
  pod". The broker's `ClaimTransfer` is the authoritative check, and the
  gateway calls it before sending any byte to the pod
  (`internal/gateway/transfer.go`;
  `internal/broker/transfers.go`).
- A replaced helper can change or withhold the bytes of that workspace's
  own transfers. It cannot exceed the grant's direction or size: the
  gateway counts and caps the bytes and forces the download's content
  type (`transfer.go`). The audited byte count is the
  gateway's own count. It sees no portal cookie or session credential:
  the transfer leg sends only the token (`transfer.go`).
- The same replaced server also answers the desktop leg, which the
  gateway proxies on the KasmVNC client paths `/app/`, `/assets/`,
  `/core/` and `/websockify` (`upstreamPathOK`,
  `internal/gateway/authorization.go`). There, bytes move in either
  direction with no `ClaimTransfer`, no byte cap and no transfer audit
  row. The gateway drops the runtime's `Content-Disposition` on that leg
  (`responsePolicy`), which removes the one-click download but not the
  channel. The file-transfer and clipboard policies therefore bind an
  unmodified runtime only
  (`TestDesktopLeg_ReplacedHelperRelaysBytesWithoutDisposition`).

### Boundary 7 — operator ↔ internal mTLS listener :9443

Flows: the operator calls the broker's internal API
([ADR 0003](../adr/0003-gateway-broker-internal-api.md)): workspace
revoke and drain, activity reporting, target resolution inputs.

Controls:

- `ServerTLSConfig` (`internal/broker/httpapi/server.go`) uses
  `VerifyClientCertIfGiven` with TLS 1.3 minimum. `identify` asserts a
  peer certificate on every route and admits only the operator CN and the
  CNs listed in `-internal-gateway-cns` (empty by default);
  `requireGateway` and `requireOperator` keep each role on its routes.
  Every refusal — a refused handshake (through the server's `ErrorLog`), a
  missing or role-less identity, a role mismatch — is counted on
  `tinycdi_internal_auth_failures_total` and logged rate-limited (TM-40).
- **Hot reload**: the listener's certificate and client CA bundle reload
  on file change (`tlsreload.Reloader`, `hotReloadClientCAs` in
  `internal/backend/listeners.go`). A client-CA swap also closes every
  established connection whose certificate chain no longer verifies
  against the new bundle (`clientConns`, `installClientConnTracking`).
  On the client side, `opclient`
  re-reads its certificate per handshake and its `VerifyConnection` runs
  the full chain and hostname check against the newest CA pool
  (`internal/broker/httpapi/opclient/client.go`). The public listeners
  (app `:8443`, session `:8444`) and the frontend hot-reload the same way.
- **Serial revocation**: `-internal-revoked-list` names a blocklist of
  (issuer, serial) pairs; issuers compare canonically (any RFC 2253
  spelling of the DN), and an entry naming an issuer outside the
  client-CA bundle logs a warning at load; a listed client certificate fails the handshake,
  and live connections it holds are severed on the same reload pass
  ([ADR 0008](../adr/0008-mtls-cert-revocation.md), `internal/tlsreload`).
- Request logs redact object IDs (`redactID`).

Register: TM-38 to TM-40.

### Boundary 8 — operator ↔ Kubernetes API (including the kasm adapter)

Flows: CRUD on `Workspace` and `WorkspaceTemplate`, runtime pods, and
per-workspace Secrets, Services and NetworkPolicies in managed
namespaces; the kasm-adapter init image delivers scripts into the pod.

Controls: scoped RBAC (`deploy/helm/tinycdi/templates/rbac.yaml`,
`config/rbac/`). CRD and CEL validation require digest-pinned image
references (`DigestPattern`, `api/v1/validation.go`) and forbid
`spec.linux.command` with `adapter=kasm`. `hostUsers` is a typed
template field with an operator-wide default; nil leaves the pod field
unset. The template snapshot that drives pod building is recorded by the
operator to `status.templateSnapshot`, which only the operator service
account can write (the `workspaces/status` subresource); the
`workspaces.cdi.tinyorbit.vn/template-snapshot` annotation is a mirror
for readers and is never trusted (TM-43). Intent delivery is fenced: the
operator converges a Workspace only to the newest recorded intent
(`spec.intentRevision` against the `applied-intent` annotation), so a
replayed stale intent can never flip `desiredState` back
(`internal/operator/workspace_controller.go`); drift between the intent
stream and the fence is surfaced, never applied (TM-22).

Register: TM-41 to TM-47.

## 5. Cross-cutting controls

### Secrets handling

- Sealing keys are files (`-login-key-file`, repeatable for rotation); the
  OIDC client secret and the database DSN arrive through Secret env or
  mounts (`oidc.existingSecret`, `database` values); the session-listener
  control token is a file.
- Nothing persists usable session material: session IDs and tickets are
  stored as SHA-256, and the `id_token` as an AEAD-sealed blob.
- The per-workspace Secret holds the KasmVNC credential, the TLS key and
  a separate `xfer-token` (`ensureSecret`,
  `internal/runtime/linux/backend.go`); the token is generated once and
  never rotated while the Secret exists.
- Logs redact IDs (`redactID`); the post-redemption redirect URL is
  static and carries no ticket or session material (`launch.go`,
  `DesktopPath`); tests assert that cookie, CSRF and `id_token` values
  never leak into API responses (`internal/api/auth_test.go`,
  `workspaces_handlers_test.go`).

### Supply chain

Full inventory: [`test-inventory.md`](test-inventory.md).

- Build jobs push **by digest only**; cosign keyless signing and the SPDX
  SBOM attestation are attached to the digest **before** any tag is
  promoted (`.github/workflows/images.yml`, `release.yml`;
  [`provenance.md`](provenance.md)).
- A trivy gate runs per image (fixable CRITICAL and HIGH fail);
  `.trivyignore` is an expiring-exceptions file enforced by
  `check-trivyignore.sh`; the scanner runs with an empty `--config`.
- govulncheck, `npm audit` (production dependencies, high and above) and
  `dependency-review-action` on pull requests; actionlint, yamllint and
  zizmor on workflows; `.github/tests/*` guard the release and workflow
  scripts.
- Runtime freshness: `runtime-freshness.yml` (daily) opens pin-bump pull
  requests when Debian security publishes a newer Chromium or Firefox ESR,
  and fails while a pin is stale. `runtime-images.yml` is the runtime
  image train (check, pull request, build, scan, sign, publish); its
  publish job waits on the `runtime-train` environment approval, while
  build and scan are ungated. The train manifest `runtime-images.json`
  ships with a `cosign sign-blob` Sigstore bundle on every `runtime-*`
  release (`.github/README.md`).
- Scan jobs validate artifact-supplied image references against the
  strict `ghcr.io/tinyorbitvn/tinycdi-<img>@sha256:<64hex>` form before
  writing them to `$GITHUB_ENV` (`validate-image-refs.sh`).
- A digest that fails its gates is never tagged or signed, so it carries
  no release trust; the failed run's cleanup job deletes the digests that
  run pushed (`cleanup-failed-digests.sh`; see
  [`provenance.md`](provenance.md)).
- The train publish job emits `attest-build-provenance` only under the
  `TRAIN_ATTESTATIONS_ENABLED` repository variable (off by default); the
  release ships `sbom-chart` and `sbom-binaries` SPDX SBOMs for the
  packaged chart and static binaries.
- Required checks and enforce-admins are codified in
  `.github/scripts/setup-repo-protection.sh`.
- Kasm catalog: `check-kasm-catalog.sh` runs in the required
  workflow-policy job and requires every `kasmweb/*` reference to be a
  digest-pinned catalog entry whose declared engine floor meets its
  class budget. The measured engine floor and the trivy gate
  (`scan-kasm-catalog.sh`) report in the CI `kasm-contract` job and gate
  every release in the release.yml `kasm-catalog` job, which the publish
  job needs.

Register: TM-48, TM-49.

### Audit

- Every route wrapped by `audited()` (`internal/api/auditroutes.go`) and
  the broker's transfer claim and completion (`internal/broker/transfers.go`)
  are persisted to `audit_event` in the audited transaction. If
  persisting fails, the request fails (`DurableAuditSink`,
  `internal/api/auditstore.go`; `TestAudited_PersistFailureFailsClosed`).
  Rows are kept 90 days by default (`-audit-retention-days`). See
  [ADR 0010](../adr/0010-audit-store-and-retention.md) and
  [audit.md](../runbooks/audit.md).
- Five kinds of record stay stdout-only:
  1. `session.touch`, the portal's activity beat
     (`notPersisted`, `internal/api/auditstore.go`);
  2. the per-request `http.request` record (`internal/api/middleware.go`);
  3. the gateway-local launch and control-surface events
     (`Gateway.audit`, `internal/gateway/launch.go`);
  4. the sign-out `session.revoke` record (`LogoutHandler`,
     `internal/api/auth.go`);
  5. the startup `admin.quota.config_apply` record
     (`writeQuotaApplyAudit`, `internal/backend/tenantquota.go`).
- The stdout stream goes through `GuardedSink`
  (`internal/observability/audit.go`): a write failure is counted on
  `tinycdi_audit_write_errors_total{event}` with a rate-limited warning
  and never fails the request. Delivery, retention and tamper evidence of
  the log stream are the operator's
  ([observability.md](../runbooks/observability.md#log-integrity--what-tinycdi-guarantees-vs-what-you-must-provide)).
- Platform-level actions that never pass through TinyCDI
  (`WorkspaceTemplate` writes, Helm value changes) are recorded only in
  the Kubernetes API audit log.

Register: TM-50.

### Failure posture

- Broker freshness budget: issuing and renewing stop when the observed
  runtime state is more than 15 s stale (`ErrFreshness`,
  `internal/broker/tickets.go`). Staleness is judged by watch health
  (delivered events, an uncached liveness probe, watch failures), not by
  informer resync replays, which keep firing while the watch is down
  (`internal/broker/bindings.go`, TM-16).
- Postgres outage: sessions close about 30 s after renewal stops, because
  leases fail closed (`tests/integration/postgres_outage_test.go`,
  `backend_restart_test.go`).
- Audit persistence fails closed: an audited mutation whose
  `audit_event` insert fails is rolled back and answered as an error.
- The operator runs leader election by default; the chart refuses
  `operator.leaderElect=false` with `operator.replicas>1` (TM-42).
- The metrics listener has no authentication; it is served on the
  dedicated ClusterIP `backend-metrics` Service, and the only rule that
  opens it admits exactly `networkPolicy.prometheusPeers` (TM-42).

## 6. Threat register

Columns: the boundary (§4, or X for cross-cutting), the threat from the
attacker's side, the control as the code implements it, what pins the
control, and the residual risk (accepted residuals are also in §7).

| ID | Boundary | Threat | Control | Pinned by | Residual |
|---|---|---|---|---|---|
| TM-01 | 1 | A hostile page frames the portal or a session host, or a tenant-controlled desktop uses the shared site to reach portal state. | `securityHeaders` and `frontendCSP` (`build/frontend/main.go`) pin CSP with `frame-ancestors 'none'` and COOP `same-origin` on the portal; `responsePolicy` and `sessionCSP` (`internal/gateway/proxy.go`) pin CSP, CORP, `Origin-Agent-Cluster` and `frame-ancestors <portal-origin>` on session hosts and delete the upstream's copies; portal cookies are `__Host-`, host-only, `SameSite=Lax`; the workspace session cookie is Lax, or `None; Partitioned` under `ParseCookieMode`. Every app-listener response carries `X-Content-Type-Options: nosniff`, a CSP with `frame-ancestors 'none'` and `X-Frame-Options: DENY` from the outermost `wrapApp` middleware (`internal/backend/wire.go`); the OpenAPI contract states the `__Host-tcdi_session` cookie as `SameSite=Lax`, with CSRF blocked by the synchronizer token plus the Origin gate. | `TestSecurityHeaders`, `TestFrontendCSP`, `TestFrontendCSP_WildcardSessionDomain`, `TestFrontendHeaders_COOP`, `TestHeaders_SessionResponses`, `TestHeaders_UpstreamCannotOverride`, `TestProxy_SecurityHeadersEverywhere`, `TestFraming_PortalOriginsOnEveryResponse`, `TestCookieMode_LaxDefault`, `TestCookieMode_PartitionedCrossSite`, `TestLoginCookieAttributes`, `TestAPIBrowserHeaders_SetOnEveryResponse`, `TestWrapApp_PortalBrowserHeaders`, `TestOpenAPI_SessionCookieMatchesAuthDefaults` | Session hosts are same-site with the portal by design; the `partitioned` cookie mode is exercised on a same-site topology only. |
| TM-02 | 2 | A cross-site page makes a signed-in browser send a state-changing API request. | `RequireCSRF` (`internal/api/middleware.go`) checks the synchronizer token from `csrfTokenFor` (`internal/api/auth.go`) in constant time; `RequireTrustedOrigin` requires one `Origin` byte-equal to the allowlist, or `Sec-Fetch-Site: same-origin` when `Origin` is absent. | `TestCSRFPostWithoutTokenRejected`, `TestCSRFWrongTokenRejected`, `TestCSRFValidTokenAccepted`, `TestCSRFUnauthenticatedPostRejected`, `TestCSRFChainForgedOriginRejected`, `TestOriginAllowlist`, `TestOriginMultipleHeadersRejected` | — |
| TM-03 | 2 | A page left open keeps an unattended portal session alive by polling, or one session's activity extends another. | Every cookie-authenticated `GET` mounts `RequireAuthPassive` (`internal/api/middleware.go`), which never slides the idle window; only mutations (including `POST /v1/session:touch`, after the CSRF check) and broker-measured input slide it, and input credits only the lease's own portal session (`TouchSessionDigest`, `internal/store/sessions.go`). | `TestPolledReads_DoNotSlideIdleWindow`, `TestMeRead_DoesNotSlideIdleWindow`, `TestConnectionEndpoint_DoesNotSlideIdle`, `TestSessionProbe_DoesNotSlideIdleTimer`, `TestMutation_StillSlidesIdleWindow`, `TestSessionTouch_SlidesIdleWindow`, `TestInputActivity_SlidesOnlyBoundSession`, `TestInputActivity_OtherPrincipalUntouched`, `TestTouchSessionDigest_ScopesToBoundSession`, `TestSessionIdleAndAbsoluteExpiry` | — |
| TM-04 | 2 | A user reads or changes another tenant's or another user's workspaces, events or data. | The principal comes only from verified claims (`internal/api/principal.go`); handlers scope by tenant and owner, and a foreign ID answers 404; tenant-admin views are tenant-scoped and events are curated (`internal/api/events.go`, `statusview.go`). Connection issuance answers 404 for another owner's workspace in the same tenant; a list filter or cursor key with NUL or invalid UTF-8 answers 400. | `TestScopeTenant_ForbiddenForUser`, `TestScopeTenant_CrossTenantNeverLeaks`, `TestScopeMine_RestrictsAdmin`, `TestWorkspaceOwnership`, `TestOwnerDerivedFromPrincipalNotBody`, `TestUnknownTenantForbidden`, `TestTenantMembershipRequired`, `TestEvents_Curated`, `TestEvents_NotOwner`, `TestGetRetainedData_Scope`, `TestAttachRetained_ForeignRecord`, `TestCreateConnection_ForeignOwnerIs404`, `TestListWorkspaces_BadPhaseIs400`, `TestAdminUsers_ForgedCursorIs400`, `TestParsePage_CursorTextMustBePostgresSafe` | `tenant_id` must come from an admin-controlled IdP mapper; a user-editable claim would let a user pick a tenant. |
| TM-05 | 2 | A request escapes the operator-supplied branding directory to read other frontend files. | `branding`, `serveBranding` and `brandingFile` (`build/frontend/main.go`) serve fixed names only, contain traversal and symlinks, and never list the directory. Only the documented branding file names are served. | `TestBrandingDir_NoTraversal`, `TestBrandingDir_NoListing`, `TestBrandingDir_OtherNamesStay404`, `TestBrandingDir_ServesFiles`, `TestBrandingDir_OnlyAllowlistedNames` | — |
| TM-06 | 2 | An attacker floods login, callback, launch or session endpoints, or rotates keys to escape the budget. | Shared Postgres windows over local buckets (`internal/ratelimit/shared.go`, ADR 0006), keyed by session or login-state digest when present, else by `ClientKey` (`internal/ratelimit/ratelimit.go`: right-most untrusted `X-Forwarded-For` behind `-trusted-proxies`, IPv6 folded to /64, mapped forms unified, a non-IP entry collapses to the peer); a circuit breaker with a 500 ms deadline and a 10 s cool-down bounds the cost of a store outage. Unknown-cookie store lookups are bounded per client on the app listener by a local limiter, and a session miss never writes. | `TestRateLimitDefaults_Budgets`, `TestShared_WindowBound`, `TestShared_DegradedModeDivides`, `TestShared_FailOpen`, `TestShared_BrownOutStoreDeadline`, `TestShared_CircuitProbeRestoresShared`, `TestShared_ProbeIsSingleFlight`, `TestClientKey_RightmostUntrusted`, `TestClientKey_IgnoresXFFFromUntrustedPeer`, `TestClientKey_IPv6FoldedTo64`, `TestClientKey_MappedFormsUnified`, `TestClientKey_NonIPSelectedEntryFallsBackToPeer`, `TestCallback_ValidatedStateCeiling`, `TestLaunch_RateLimitedDoesNotConsumeTicket`, `TestAppListener_UnknownCookieSprayBounded`, `TestAppListener_ValidSessionsBehindOneClientNotThrottled`, `TestAppListener_UnknownCookieSprayRefused`, `TestAppListener_StoreErrorsKeepLookupSpent`, `TestAppListener_LiveSessionSurvivesSharedKeySpray`, `TestAppListener_DeletedCachedSessionRefusedAndEvicted`, `TestLimiter_RefundReturnsOneToken`, `TestSessionStore_MissNeverWrites`, `TestPruneDeadSessions_RemovesOnlyDeadRows`, `TestNotesWarnUnrestrictedEdgeOpen`, `security-doc-claims.test.sh` | Budgets are per client address or /64 prefix, so a client holding many gets many; the derivation assumes every proxy hop in front is listed in `-trusted-proxies`; the limiter fails open on a store error. |
| TM-07 | 2 | A signed-out user's desktop stream, ticket or copied cookie keeps working. | `LogoutHandler` (`internal/api/auth.go`) calls `Broker.RevokePortalSession` (`internal/broker/sessions.go`), which revokes the session's leases and outstanding tickets in one transaction, locking rows in a fixed key order (`lockByKeysInOrderTx`); a failed session-row delete answers 503 and revokes nothing; `liveLease` (`internal/broker/leases.go`) re-validates the bound portal session on every renew, attach, claim and cookie resolve and revokes the lease when it fails; `RedeemTicket` re-checks the session row. A login that completes while the browser holds a live session of the same principal re-issues that session instead of minting a second one, so sign-out revokes every lease and ticket bound to it. | `TestLogout_RevokesSessionBoundMaterial`, `TestLogout_StoreDeleteFailureReturns503`, `TestRevokePortalSession_RevokesBoundLeases`, `TestRevokePortalSession_RevokesOutstandingTickets`, `TestRevokePortalSession_RedeemCommitThenRevoke`, `TestRevokePortalSession_RevokeCommitThenRedeem`, `TestRedeemTicket_DeadPortalSessionDenied`, `TestLeaseSession_RenewRevokesWhenSessionDeleted`, `TestLeaseSession_AttachDeniedWhenSessionDeleted`, `TestLeaseSession_StaleEpochRevokes`, `TestLeaseSession_IdleDeadSessionRevokesOnRenew`, `TestSignout_RevokeEndsStreamOnOtherReplica`, `TestSignout_CookieCannotRehydrateAfterRevoke`, `TestLogin_SamePrincipalKeepsLiveSession` | A live stream on another replica ends at that replica's next renew, within one renew cycle. |
| TM-08 | 2 | A user who lost a device cannot end sessions on other devices, or a partial sign-out reports success. | `RevokeAllSessionsHandler` (`internal/api/revokeall.go`) calls `Broker.RevokePrincipalSessions`, which destroys every portal session of the principal in the caller's tenant and revokes its leases and tickets in one transaction (lock order tickets, sessions, leases); a failure rolls back whole and answers 500 ([ADR 0007](../adr/0007-sign-out-everywhere.md)). | `TestRevokeAll_RevokesEverySessionOfPrincipal`, `TestRevokeAll_UnauthenticatedAndCSRF`, `TestRevokeAll_StoreFailureIs500AndKeepsSession`, `TestRevokePrincipalSessions_RevokesAllInTenant`, `TestRevokePrincipalSessions_TenantScoped`, `TestRevokePrincipalSessions_RedeemCommitThenRevoke`, `TestRevokePrincipalSessions_RevokeCommitThenRedeem`, `TestRevokePrincipalSessions_MultiTicketInversion`, `TestSignoutAll_RevokeEndsEverySessionStream` | No IdP back-channel logout; the IdP session ends only through the RP-initiated end-session URL. |
| TM-09 | 2 | A user starts more workspaces than the tenant allows them, for example by racing launches. | `checkUserLimit` (`internal/provisioning/quota.go`) enforces the per-principal limit inside the reservation transaction under the `tenant_quota` row lock, counting `held` reservations only; a missing workspace row fails closed; limits are written through tenant-admin routes in `internal/api/adminuserlimit.go` (CSRF, `If-Match`, audited). Settle re-checks the workspace under its row lock and releases a reservation only while the workspace is deleted or wanted Stopped in the generation it observed; reservations carry exact template bytes, and a start that moves revision re-reserves the new vector; retained PVC labels carry the workspace CR name. | `TestUserLimit_DefaultEnforced`, `TestUserLimit_OverrideBeatsDefault`, `TestUserLimit_ConcurrentLaunches`, `TestUserLimit_MissingWorkspaceRowFailsClosed`, `TestUserLimit_NoRowsUnlimited`, `TestUserLimit_PutRequiresIfMatch`, `TestAdminUserLimits_Audit`, `TestUserLimitReachedMapping`, `TestSettleQuota_StartRaceKeepsReservation`, `TestSettleQuota_StoppedWorkspaceStillReleases`, `TestCreateWorkspace_ReservesExactTemplateBytes`, `TestStart_RevisionMoveReservesNewVector`, `TestRetainedAttach_LabelIsCRName` | — |
| TM-10 | 2 | A malformed request body makes the API echo parser internals to the client. | `decodeJSON` (`internal/api/workspaces.go`) rejects trailing data and returns the error to the caller, which answers 400 `INVALID_REQUEST` with a generic message and logs the detail with the request ID (`ConnectionHandler.Create`, `internal/api/connections.go`). | `TestCreateConnection_InvalidBodyGenericMessage`, `TestCreateConnection_TrailingJSONRejected`, `TestCreateDecoderErrorNotEchoed` | — |
| TM-11 | 2 | A user gains tenant-admin rights through a group name the deployment did not intend, or a handler checks the wrong group. | `-tenant-admin-groups` / `oidc.tenantAdminGroups` lists the verified `groups` values that grant the role (exact match; an empty list is a startup error); `RequireAuth` resolves it once into `Principal.IsTenantAdmin` (`internal/api/principal.go`), the only check admin gates use. | `TestTenantAdminGroups_AuthConfigValidation`, `TestNoRawTenantAdminMembershipCheck`, `TestMe_CustomAdminGroups`, `TestScopeTenant_CustomAdminGroups`, `TestAdminSessions_CustomAdminGroups`, `TestAdminAudit_CustomAdminGroups`, `TestParseFlags_TenantAdminGroups`, `TestHardeningTenantAdminGroups` | A misconfigured list is a privilege grant: every member of a listed group gets tenant-wide read, stop/start, retained-disk attach/purge and admin rights. The role follows the `groups` stored with the portal session at sign-in, so a removal from an admin group at the IdP takes effect at the user's next sign-in (up to the 12 h absolute timeout); the immediate remedy is another tenant admin's `POST /v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all`. |
| TM-12 | 2 | A tenant admin reads another tenant's audit log, or reads it without leaving a trace. | `AdminAuditHandler.List` (`internal/api/adminaudit.go`) is tenant-admin only through `tenantAdminPrincipal`; `DB.ListAudit` (`internal/store/audit.go`) scopes by tenant in SQL with a keyset cursor; the read is itself audited. | `TestAdminAudit_Authz`, `TestAdminAudit_ListPagesAndFilters`, `TestAdminAudit_RejectsBadInput`, `TestAudit_InsertListFilterCursor`, `TestAdminAudit_CustomAdminGroups` | — |
| TM-13 | 2 | A tenant admin lists users of another tenant, or uses directory data as an authorization input. | `AdminUsersHandler.List` and `AdminUsersHandler.Get` (`internal/api/adminusers.go`) are tenant-admin only and read `principal_directory` scoped by tenant in SQL; the data is display-only (names and emails of the caller's own tenant) and the detail route is addressed by an owner reference; detail reads are audited with a pseudonymous target. The per-request log records the route pattern, never the raw path. | `TestAdminUsers_ListIncludesUsersWithoutWorkspacesOrLimits`, `TestAdminUsers_DetailNotFoundAndBadRef`, `TestAdminUsers_CursorCarriesSortKey`, `TestAdminUsers_CustomAdminGroups`, `TestAudit_RequestLogNeverContainsRawOwnerRef` | The directory lists only users who have signed in to the tenant. |
| TM-14 | 2 | A tenant admin signs out a user of another tenant, a forged request revokes sessions, or the session list leaks lease ids that act as bearer material. | `MountAdminSessionRoutes` (`internal/api/adminsessions.go`) wraps `RevokeAll` in `audited(…, authn.RequireCSRF(…))`; `tenantAdminPrincipal` gates list and revoke; the revoke reuses the principal revoker transaction (sessions, tickets, leases); the list exposes only `leaseRef`, a SHA-256 prefix, never the lease ID, and the cursor carries no lease ID. | `TestAdminSessions_RevokeAllTargetsTheNamedUser`, `TestAdminSessions_RevokeAllRefusals`, `TestAdminSessions_ListHidesLeaseIDs`, `TestAdminSessions_PagingCursorHasNoLeaseID`, `TestAudit_AdminSessionRevoke`, `TestLiveLeases_ListFiltersAndPages` | — |
| TM-15 | 2 | A user or another tenant's admin reads a tenant's overview to learn its activity or identities. | `AdminOverviewHandler.Get` (`internal/api/adminoverview.go`) is tenant-admin only and returns counts only, from a cache kept per tenant. | `TestAdminOverview_NonAdminRefused`, `TestAdminOverview_CacheIsPerTenant`, `TestAdminOverview_AggregatesAndCaches`, `TestAdminOverview_CustomAdminGroups` | — |
| TM-16 | 2 | A user restarts or recovers a workspace they do not own, or a recovery leaves the old runtime's sessions alive. | `WorkspaceHandler.Start` (`internal/api/workspaces.go`) on a `Failed` workspace is owner-scoped (the database read comes before the CR state) and acts only on a fresh, settled projection; `RestartWorkspace` (`internal/provisioning/service.go`) bumps `runtimeGeneration` and revokes the old generation's leases and tickets in one transaction, fenced on the observed generation under the workspace row lock; unrecoverable reasons answer 409 `INVALID_STATE`. Independently of those revocations, the broker's binding fence (`boundCurrent`, `internal/broker/leases.go`) refuses renewal and target resolution for a superseded incarnation, and the binding source fails closed (`ErrFreshness`) once its Workspace watch has had no delivered event or uncached liveness probe for 15 s, or has reported failures for over 15 s; informer resync replays never count as liveness (`K8sBindingSource`, `internal/broker/bindings.go`). | `TestStart_FailedRecoverableRestartsWithObservedGeneration`, `TestStart_FailedUnrecoverableIs409WithReason`, `TestStart_ForeignFailedWorkspaceIs404BeforeStatus`, `TestStart_StaleOrUnsettledStatusIsAPlainStart`, `TestRestartWorkspace_BumpsGenerationRevokesOldAndAppendsStart`, `TestRestartWorkspace_GenerationFenced`, `TestRestartWorkspace_FencesOldGenerationEndToEnd`, `TestRecovery_DeletedPodGoesRuntimeLostWithoutRebuild`, `TestK8sBindingSource_ResyncDoesNotMaskDeadWatch`, `TestK8sBindingSource_WatchFailureStreak`, `TestK8sBindingSource_IdleHealthyWatchStaysFresh` | A lost pod is never rebuilt inside its generation; the owner or an admin must press Start. A Workspace watch that client-go retries without reporting (a 429 at watch start) while uncached lists still succeed is not detected. |
| TM-17 | 2 | A forged or replayed `:apply` restarts a tenant's workspaces, kills live sessions, or reaches another tenant. | `AdminSessionPolicyHandler.Apply` (`internal/api/adminsessionpolicyapply.go`) is mounted behind `RequireCSRF` (`internal/api/adminsessionpolicy.go`); `If-Match` pins the ceiling version; the portal runs a dry run first, and the API requires an explicit `dryRun` flag but does not require an earlier dry run; live sessions are skipped unless `includeActive`; at most `maxPolicyApply` (500) rows per call; restarts go through the generation-fenced `RestartWorkspace`. | `TestApplySessionPolicy_AuthzAndTenantIsolation`, `TestApplySessionPolicy_Preconditions`, `TestApplySessionPolicy_DryRunListsLooserOnly`, `TestApplySessionPolicy_ConfirmSkipsLiveUnlessIncluded`, `TestApplySessionPolicy_MovedGeneration`, `TestApplySessionPolicy_TruncatesAtCap` | A running workspace that is not restarted keeps its start-time server-side policy, bounded by its template's own policy. A confirmed call with a current `If-Match` restarts at once, with no earlier dry run; that is within the tenant admin's privilege. |
| TM-18 | 2 | A user mints a transfer grant for a workspace they do not own, with a path-traversing name, or reuses a grant. | `FileTransferHandler.Create` (`internal/api/filetransfers.go`) allows the owner only and requires `Ready`; `validTransferName` accepts single-file names only; rows are single use, bound to workspace, principal, generation and runtime UID, expire after 2 minutes and are written in the audited request's transaction. | `TestMintFileTransfer_RejectsBadNames`, `TestMintFileTransfer_PolicyOwnerAndReadiness`, `TestMintFileTransfer_AuditsMintAndRefusal`, `TestFileTransfer_InsertAndSweep` | — |
| TM-19 | 3 | An attacker replays a captured login cookie or swaps login state between browsers. | The in-flight login rides in an AEAD-sealed `__Host-tcdi_login` cookie (`internal/api/loginstate/sealer.go`) carrying state, nonce and the PKCE verifier; the authorization code is single use at the IdP and bound to the sealed verifier; the cookie is cleared after use. | `TestSealOpen_RoundTrip`, `TestOpen_Tampered`, `TestOpen_Expired`, `TestOpen_PreviousKeyStillOpens`, `TestCallbackRejectsStateReplay`, `TestCallbackRejectsWrongLoginCookie`, `TestLogin_CookieClearedAfterUse`, `TestLogin_CallbackOnOtherReplica`, `TestLoginSendsPKCES256` | The sealed cookie can be replayed within its TTL (10 minutes), but a replay gains nothing without the matching authorization code. |
| TM-20 | 3 | A configured or discovered logout redirect sends users to an attacker or over plaintext. | `endSessionURL` (`internal/api/auth.go`) is built from discovery and static configuration only; `PostLogoutRedirect` and the discovered `end_session_endpoint` must be absolute `https` URLs, with `http` only for loopback hosts. The OpenAPI contract states that the retained ID token is sent as `id_token_hint` in the end-session URL. | `TestNewAuthenticator_RejectsBadPostLogoutRedirect`, `TestNewAuthenticator_AllowsLoopbackPostLogoutRedirect`, `TestLogout_UntrustedDiscoveredEndpointIs204`, `TestLogout_NoOpenRedirect`, `TestOpenAPI_LogoutDocumentsIDTokenHint` | — |
| TM-21 | 4 | Restoring an older Postgres dump onto a live cluster resurrects signed-out sessions, revoked leases or spent tickets. | `platform_meta.session_epoch` is compared on every session read, so rotating it kills every restored session; `backend post-restore` (`internal/backend/postrestore.go`) rotates the epoch, revokes restored leases and tickets and aligns workspace rows to their CRs; it is a dry run by default, `-apply` takes the leader advisory lock for the whole run and refuses while backends are connected. See [disaster-recovery.md](../runbooks/disaster-recovery.md#postgres-only-restore-onto-a-live-cluster). | `TestPostRestoreDryRunMakesNoWrites`, `TestPostRestoreApplyRotatesRevokesDenies`, `TestPostRestoreRefusesWhileLeaderHeld`, `TestPostRestoreRefusesWithBackendConnections`, `TestPostRestoreAlignsDivergedRow`, `TestSessionEpochRotation` | Running the procedure, and the restore drill (`hack/quickstart/restore-drill.sh`), are manual. |
| TM-22 | 4 | After a database restore the intent stream trails the CR's applied-intent fence and every new intent is dropped silently. | `intentFenceDrifted` (`internal/provisioning/k8sapplier.go`) stamps the `intent-behind` annotation on drift; the operator raises the `IntentBehind` condition and one Warning event until the stream realigns (`internal/operator/workspace_controller.go`); a stale intent is never applied. | `TestK8sApplierIntentDrift`, `TestIntentBehindDrift`, `TestProjectConditions_IntentBehind`, `TestEvents_IntentParams` | — |
| TM-23 | 5 | An attacker with database or backup read access, or a cookie unknown to the replica, takes over a desktop session. | Session and workspace cookies are stored only as SHA-256 digests; `rehydrate.go` resolves an unknown cookie by digest to a live lease, rebuilds the session on any replica and refuses expired or revoked leases; concurrent lookups are single-flight. Every redemption refused after the audience check still commits the consume. | `TestSessionKeyIsDigest`, `TestRehydrate_SecondGatewayServesCookie`, `TestRehydrate_UnknownCookieAllocatesNothing`, `TestRehydrate_DeadLeaseRejected`, `TestRehydrate_ConcurrentRequestsSingleLookup`, `TestRehydrate_TerminalErrorsAre401`, `TestRehydrate_NilDirectorySingleProcess`, `TestRedeemTicket_EveryRefusalConsumes` | — |
| TM-24 | 5 | A second tab, browser or a dead replica's stream keeps driving a desktop next to the current one. | One interactive stream per lease: a new upgrade fences the old socket (`admitUpgrade`, `internal/gateway/authorization.go`); the Postgres `stream_epoch` fences streams admitted by another replica; every launch and stream claim carries a valid owner tab id, else `400 owner_tab_required` (`ValidStreamOwnerTab`, `Broker.ClaimStream`, `internal/broker/sessions.go`; `internal/gateway/launch.go`). | `TestUpgrade_SequentialFencesPrevious`, `TestStreamFence_CrossGateway`, `TestClaimStream_StaleFence`, `TestClaimStream_ZeroesPreviousStream`, `TestConnection_StreamEpochAdvancesOnNewStream`, `TestLaunch_RequiresOwnerTab`, `TestStreamClaim_RequiresOwnerTab`, `TestClaimStream_RequiresValidOwnerTab`, `TestStreamClaim_SameTabReconnectKeepsOwner`, `TestSingleActiveLeaseAcrossGateways` | When a replica dies mid-claim, two streams can coexist for at most one renew cycle. |
| TM-25 | 5 | A cookie or ticket for one workspace is used on another workspace's host, or a crafted Host header reaches a different handler. | `sessionhost.Domain.Match` (`internal/sessionhost/sessionhost.go`) maps a label to exactly one workspace; every proxied request's cookie must resolve to a lease for the host's workspace, else it is denied and audited (`host_mismatch`); launch on a foreign host is refused without consuming the ticket. | `TestHostBinding_LaunchOnOtherHost`, `TestHostBinding_CookieOnOtherHost`, `TestHostBinding_UnknownHost`, `TestHostBinding_TwoWorkspacesStayConnected`, `TestHostBinding_WebSocketOrigin`, `TestLaunch_BadHostRejected_NoConsume`, `TestProxy_BadHostRejected` | — |
| TM-26 | 5 | A browser on the session listener reaches the public API, or the control surface from a workspace host. | `Gateway.ServeHTTP` (`internal/gateway/proxy.go`) routes by host class first: workspace hosts get launch, proxy and transfer only; control hosts get `/healthz` and `/v1/control/*` behind `controlAuth`; any other host gets `421`; API paths do not exist on `:8444`. On the session listener, `/healthz` and `/readyz` answer only on control hosts; the control token must be at least 32 bytes, and failed control attempts are limited per client. | `TestRouteIsolation_SessionListener`, `TestRouteIsolation_AppListener`, `TestRouteIsolation_InternalListener`, `TestHostBinding_ControlOnlyOnControlHosts`, `TestControlEndpoints_RequireBearer`, `TestControlEndpoints_NoTokenFailsClosed`, `TestControlEndpoints_WrongBearerRejected`, `TestSessionListener_HealthOnlyOnControlHosts`, `TestControlToken_MinLength`, `TestControlAuth_FailuresLimitedPerClient`, `TestLimiter_PeekDoesNotSpend` | — |
| TM-27 | 5 | An unauthenticated client sprays random cookies at session hosts to load Postgres. | `lookupSession` (`internal/gateway/authorization.go`) consults a local per-client token bucket (`SessionLookupLimiter`, keyed by `ratelimit.ClientKey`, `-session-lookup-rate`) before any store read for a cookie the replica does not know; refusal is 429 with `Retry-After` and is counted and audited. | `TestRehydrate_UnknownCookieSprayBounded`, `TestRehydrate_LimiterSkipsKnownSession`, `TestRehydrate_LimiterSkipsRehydratedSession`, `TestRehydrate_ReconnectStormOneClient`, `TestRehydrate_InjectedLookupLimiter` | The bound is per replica by design; a shared window would spend the reads it exists to prevent. |
| TM-28 | 5 | A client redeems a ticket and drops the connection before the redirect, pinning the workspace with a session no browser holds. | Sessions mint unattached; the first request on their own host attaches them, and the renew loop reaps a session still unattached at `-unattached-session-ttl` (90 s) under the session lock, revoking its lease (`internal/gateway/authorization.go`). | `TestLaunch_UnclaimedSessionReaped`, `TestLaunch_AttachedSessionSurvivesTTL`, `TestLaunch_GhostReapSkipsUsedLease`, `TestLaunch_ReapOnRevokeFailureStillFrees`, `TestLaunch_AttachDuringReapLoses`, `TestLaunch_AttachReapRaceConsistency` | — |
| TM-29 | 5 | A page that opens a session-host URL keeps it in its browsing-context group, or a runtime weakens the session origin's headers. | `responsePolicy` (`internal/gateway/proxy.go`) pins `Cross-Origin-Opener-Policy: same-origin` with the rest of the session header set and strips the upstream's COOP. `responsePolicy` writes the pinned header set onto every proxied response, and upstream 1xx responses other than 101 are not relayed. | `TestResponsePolicy_PinsCOOP`, `TestResponsePolicy_StripsUpstreamCOOP`, `TestProxy_HostileUpstreamHeadersDropped`, `TestResponsePolicy_Upstream1xxCannotStripPinnedHeaders`, `TestProxy_WebSocketUpgradeSurvives1xxGuard` | — |
| TM-30 | 5 | A user or a cross-site page moves a file through the transfer leg without a valid grant, beyond the policy, or beyond the grant's size. | `Gateway.serveTransfer` (`internal/gateway/transfer.go`) checks Origin, the session cookie, host and live lease, then calls `Broker.ClaimTransfer` (`internal/broker/transfers.go`) before any byte reaches the pod; the claim is single use, re-resolves the policy, allows one live transfer per workspace and is audited in its transaction; the gateway caps the bytes at the grant's size, forces `Content-Type: application/octet-stream` and a `Content-Disposition` built from the grant, and sends only `X-Tcdi-Xfer-Token` upstream. | `TestTransfer_UploadRoundTrip`, `TestTransfer_DownloadRoundTrip`, `TestTransfer_UploadOverCapRefused`, `TestTransfer_DownloadOverCapAborts`, `TestTransfer_Refusals`, `TestTransfer_BrokerRefusalsMapped`, `TestClaimTransfer_HappyPathIsSingleUse`, `TestClaimTransfer_PolicyTightenedAfterMint`, `TestClaimTransfer_RejectsMismatches`, `TestClaimFileTransfer_OnePerWorkspaceConcurrent`, `TestCompleteTransfer_BoundToLeaseWorkspaceAndSurvivesRevoke`, `TestTransferClaim_OK_AndErrorMapping` | — |
| TM-31 | 5 | A client reaches `tcdi-xferd`'s `/xfer/` through the desktop proxy and skips the claim. | `serveProxy` refuses any path not on `upstreamPathOK`'s allowlist (`internal/gateway/proxy.go`, `internal/gateway/authorization.go`); only the transfer leg builds `/xfer/files/<name>` (`helperURL`, `internal/gateway/transfer.go`). | `TestTransfer_DesktopProxyStillRefusesXfer`, `TestProxy_ManagementPathsDenied`, `TestProxy_DotSegmentsRejected` | — |
| TM-32 | 6 | A client spoofs its address toward the runtime, or strips the injected credential, through forwarding or `Connection` headers. | `rewriteForwarded` (`internal/gateway/proxy.go`) strips the client forwarding and vendor address families and rebuilds the address headers from the trusted chain only; the credential is injected in the `Rewrite` hook after the hop-by-hop strip, and `Connection` tokens are reduced to `upgrade`, `keep-alive` and `close`. | `TestProxy_ClientXFFNeverReachesRuntime`, `TestProxy_XFFFromTrustedChainOnly`, `TestClientAddr_NonIPSelectedEntryFallsBackToPeer`, `TestProxy_ClientForwardedFamilyStripped`, `TestProxy_VendorClientAddressHeadersStripped`, `TestProxy_ConnectionTokensCannotStripInjectedHeaders`, `TestProxy_UpgradeConnectionTokensCannotStripInjected` | — |
| TM-33 | 6 | An attacker guesses the workspace's KasmVNC password through the gateway or the pod network ([KasmVNC credential brute force](#kasmvnc-credential-brute-force)). | `tcdi-xferd` answers a desktop request without Basic credentials with its own 401 (`internal/xferd/xferd.go`); the gateway overwrites `Authorization`; KasmVNC listens on `127.0.0.1:8444` only (`build/linux-base/kasmvnc.yaml`; for the adapter, `build/kasm-adapter/entrypoint.sh`); the pod NetworkPolicy admits TCP 8443 from gateway pods only (`buildNetPol`, `internal/runtime/linux/backend.go`); the blacklist is 5 failures per 10 s (`build/linux-base/kasmvnc.yaml`). | `TestProxy_StripsClientAuth`, `TestXferd_ProxiesDesktopAndStripsToken`, `TestIngressPeerScopedToGatewayNamespace`, `.github/tests/xfer-entrypoint.test.sh` | A process inside the pod can reach `127.0.0.1:8444` directly, but it can already read the credential. |
| TM-34 | 6 | A compromised workspace reaches services elsewhere in the cluster. | `buildNetPol` (`internal/runtime/linux/backend.go`) restricts `ClusterOnly` egress by default to pods in namespaces labelled `workspaces.cdi.tinyorbit.vn/tenant`; `--runtime-clusteronly-egress` (`parseClusterOnlyEgress`, `cmd/operator/main.go`) narrows it further, or opens it with `unrestricted`, and a ports-only value keeps a pod-only peer so external IPs stay denied. Without `--runtime-clusteronly-egress`, the peer is limited to namespaces carrying the workspace's own tenant label value; the workspace NetworkPolicy is created before its pod. | `TestClusterOnlyEgressDefaultRestricted`, `TestClusterOnlyEgress_DefaultRestricted`, `TestClusterOnlyEgressUnrestrictedOptInOnPolicy`, `TestClusterOnlyEgressRestrictionOnPolicy`, `TestClusterOnlyEgressPortsOnlyStaysPodOnly`, `TestParseClusterOnlyEgress`, `TestClusterOnlyEgress_DefaultRendersNoFlagAndNoWarning`, `TestClusterOnlyEgress_UnrestrictedRendersFlag`, `TestClusterOnlyEgressDefaultSameTenantOnly`, `TestEnsure_NetPolPrecedesPod` | DNS egress to the cluster DNS pods is open on every network profile, `Isolated` included; with `unrestricted`, confinement rests on destination-side policies. With `unrestricted`, the backend's edge ingress (`networkPolicy.edgeIngressCIDRs`, default `0.0.0.0/0`) admits workspace pods too, so `backend.trustedProxies` must never cover a range where workspace pods run; the chart NOTES warn on that combination. |
| TM-35 | 6 | A reconnect storm crashes a workspace runtime by exhausting KasmVNC's file-descriptor bound. | The gateway caps live upstream connections per workspace per replica at `-session-stream-cap` (default 32, minimum 1); over the cap it answers 429 with `Retry-After: 1`, counted and audited, before upgrade admission; the image lowers `RLIMIT_NOFILE` below `FD_SETSIZE`, so overload becomes `EMFILE` refusals. | `TestStreamCap_ExactBound`, `TestStreamCap_MixedHTTPAndWebsocket`, `TestStreamCap_SlotRelease`, `TestStreamCap_ConcurrentStormNoLeak`, `TestStreamCap_PerWorkspace`, `TestStreamCap_Metrics`, `TestSessionStreamCapBelowOneRejected` | The cap is per replica (N replicas admit N × cap); a direct burst far above the cap can still crash Xvnc and restart the container, but that path is not reachable through the gateway. |
| TM-36 | 6 | A user copies data out of, or into, a desktop whose template or tenant ceiling forbids it. | `buildPod` (`internal/runtime/linux/backend.go`) carries the resolved policy as `TCDI_CLIPBOARD_POLICY`; both entrypoints render KasmVNC's DLP clipboard block from it, unset or unknown failing closed to `Disabled`, with client overrides and DLP logging off. Xvnc runs with an `-AllowOverride` list that names no clipboard parameter, so `vncconfig -set` cannot re-enable the clipboard at runtime. | `TestClipboardPolicyDirectionSemantics`, `TestPodCarriesClipboardPolicy`, `TestTicket_ClipboardPolicyRecordedOnRedeem`, `TestLaunch_RedirectCarriesRecordedClipboardPolicy`, `.github/tests/clipboard-policy.test.sh`, `TestLinuxRuntimeClipboardParamsNotOverridable`, `TestKasmAdapterChromium`, `clipboard-policy.test.sh` | A running workspace keeps its start-time policy until it restarts (TM-17). The policy binds an unmodified runtime: the workspace user can replace Xvnc itself (§7 item 1). |
| TM-37 | 6 | A process in the pod, or a request reaching `:8443`, reads or writes files outside the transfer folder, transfers against the policy, or learns the transfer token ([In-pod transfer helper](#in-pod-transfer-helper)). | `internal/xferd/xferd.go` compares the token in constant time and an empty token refuses everything (`:145-146`); it strips the token before proxying to the desktop (`:66`); names are checked by `ValidName` (`:84`) and confined by an `os.Root` jail (`:129`); an unknown policy fails closed (`:114`); the size cap is enforced on upload (`:214`); the token is a separate per-workspace Secret key, `xfer-token` (`internal/runtime/linux/backend.go`); both entrypoints start and supervise the helper (`build/linux-base/entrypoint.sh`). | `TestXferd_TokenPolicyAndCap`, `TestXferd_JailRefusesEscapes`, `TestXferd_DownloadAndUnknownPolicyFailsClosed`, `TestXferd_ProxiesDesktopAndStripsToken`, `TestXferd_ProxiesWebSocketUpgrade`, `TestEnsureSecret_CarriesXferTokenAndNeverRotatesIt`, `.github/tests/xfer-entrypoint.test.sh` | Any process the workspace user starts can read `xfer-token` and replace the helper on `:8443`. On the transfer leg it can then mishandle only that workspace's own transfers, within the claimed direction and size. On the desktop leg (`/app/`, `/assets/`, `/core/`, `/websockify`) it can move data in either direction with no grant, byte cap or transfer audit: the file-transfer and clipboard policies bind an unmodified runtime only. |
| TM-38 | 7 | An attacker with a leaked or removed operator certificate, or a rotated-out CA, keeps calling the internal API. | The internal listener's certificate and client CA bundle hot-reload (`hotReloadClientCAs`, `internal/backend/listeners.go`); `opclient` re-reads its certificate and verifies against the newest CA pool per handshake; `-internal-revoked-list` (`internal/tlsreload`) refuses a listed (issuer, serial) at the handshake and severs its live connections on the same reload pass. A client-CA swap closes live internal connections whose chain no longer verifies; blocklist issuers are compared canonically, and an issuer absent from the client-CA bundle is reported at load. | `TestInternalListener_AcceptsNewCAAfterRotation`, `TestInternalListener_DropsRemovedCA`, `TestOpClient_ReloadsServerCA`, `TestOpClient_DropsRemovedServerCA`, `TestOpClient_RejectsServerHostnameMismatch`, `TestRevokedSerialHandshakeAborts`, `TestRevokedLiveConnSevered`, `TestRevokedListReloadBlocks`, `TestRevokedResumedHandshakeRejected`, `TestNewBlocklistFailsClosed`, `TestBlocklistReloadKeepsLastGood`, `TestInternalListener_RemovedCASeversLiveConn`, `TestBlocklistIssuerCanonicalMatch` | Revocation is server-side only: the operator does not check the server certificate's serial; a listed serial takes effect within the Secret sync plus one 30 s poll; a suspect CA is rotated, not revoked. The internal client CA must be a dedicated issuer that signs nothing else (the chart's `selfSigned` bootstrap, or an `Issuer` in the release namespace): any certificate it signs with the operator CN is the operator. |
| TM-39 | 7 | A caller holding an operator certificate fences every future generation of a workspace with a far-future `runtimeGeneration`. | `RevokeWorkspaceLeases` (`internal/broker/activity.go`) refuses a generation beyond the recorded generation plus one with `ErrGenerationUnbounded` (400 `INVALID_REQUEST`). | `TestRevokeWorkspaceLeases_GenerationBounded`, `TestRevokeWorkspace_GenerationUnbounded` | Revocation records (`workspace_revocation`) are never pruned; the table grows by one row per revoked generation. |
| TM-40 | 7 | Probes against the internal mTLS listener leave no record at the trust boundary. | `identify` (`internal/broker/httpapi/server.go`) counts every refusal on `tinycdi_internal_auth_failures_total{reason}` and logs a rate-limited warning with request ID, method, peer and redacted path; certificate material is never logged. TLS handshake refusals and role mismatches are counted (`reason="handshake"`, `reason="role_mismatch"`) with a rate-limited structured warning. | `TestIdentify_RejectionsLogged`, `TestIdentify_RejectedPathRedacted`, `TestIdentify_RejectionsRateLimited`, `TestIdentify_RejectionsCounted`, `TestNoClientCert_401`, `TestSPIFFESAN_MismatchRejected`, `TestRoleMismatchCounted`, `TestIdentify_HandshakeRejectionsCounted` | — |
| TM-41 | 8 | A workspace runs without AppArmor confinement on nodes that lack it. | By default the pod requests AppArmor `RuntimeDefault` and fails to start where it is unavailable; `runtime.appArmor.requireRuntimeDefault=false` sets `AppArmorNotRequired` (`cmd/operator/main.go`), which omits only the `RuntimeDefault` request and keeps every other hardening field. The chart has no unreferenced `podSecurity` value. | `TestBuildPod_AppArmorDefaultUnchanged`, `TestBuildPod_AppArmorNotRequired_RuntimeDefaultOmitted`, `TestBuildPod_AppArmorNotRequired_LocalhostKept`, `TestBuildPod_AppArmorNotRequired_OtherHardeningUnchanged`, `TestAppArmorProfileRejected`, `TestTemplateRejectedAppArmor`, `TestPodSecurityValuesAllReferenced` | With the opt-out set, workspaces on nodes without AppArmor run unconfined by it. |
| TM-42 | 8 | A chart value combination silently disables a guard: unelected operator replicas, an exposed metrics port, or an `extraEnv` entry overriding a security flag. | `tinycdi.validate` fails the render on `operator.leaderElect=false` with `operator.replicas>1`; only `allow-metrics-scrape` opens `:9090`, to `networkPolicy.prometheusPeers`; `tinycdi.denyExtraEnv` fails the render on the env equivalents of security-relevant flags and on chart-managed names unless `dev.enabled` (`deploy/helm/tinycdi/templates/_helpers.tpl`). Every flag the chart renders is refused in `extraArgs`, every denied flag's env form is refused in `extraEnv` and the reverse, and the leader-election guard reads `operator.extraArgs`. | `TestOperatorLeaderElectionGuard`, `TestMetricsListenerIsolation`, `TestEdgePolicyNeverOpensInternalPort`, `TestExtraEnvDevSurfaceNamesDenied`, `TestExtraEnvDeniedNamesSuggestValue`, `TestExtraEnvManagedDuplicateDenied`, `TestExtraEnvMalformedEntryFails`, `TestExtraEnvDevGateEscape`, `TestExtraEnvBenignPasses`, `TestExtraArgsCannotOverrideRenderedFlags` | Guards constrain chart-rendered manifests only; hand-written manifests bypass them, and a single-replica install may run unelected. |
| TM-43 | 8 | A Workspace writer forges the template snapshot to steer the pod the operator builds. | The operator records the snapshot itself, from the live template, to `status.templateSnapshot`, which RBAC lets only the operator write; `recordedSnapshot` and `verifySnapshotRecord` (`internal/operator/workspace_controller.go`) ignore the annotation and re-check the record; a workspace restored without status adopts only from its own stamped pod (`adoptSnapshotFromStampedPod`) and otherwise holds `Degraded`. | `TestWorkspaceStatusWritableOnlyByOperator`, `TestKustomizeWorkspaceStatusWritableOnlyByManager`, `TestStatusSnapshotAuthoritativeOverForgedAnnotation`, `TestStatusSnapshotRecordedAtFirstAdmit`, `TestRecordedSnapshot_IgnoresAnnotation`, `TestAdoptSnapshot_AnnotationNeverConsulted`, `TestAdoptSnapshot_StampedPodAdoptsLiveRevision`, `TestAdoptSnapshot_UnstampedPodHolds`, `TestAdoptSnapshot_ForgedNetworkProfileLeavesNetPol`, `TestForgedSnapshotBadHashRejected`, `TestBuildPod_AnnotationNoLongerHonoured` | The mirror annotation stays writable by Workspace writers unless the admission policy (TM-45) is enabled; nothing trusted reads it. |
| TM-44 | 8 | A pod spec that reaches the operator through an admission bypass requests unbounded cluster resources. | Opt-in `managedNamespaces[].namespaceQuota` renders a `ResourceQuota` and/or `LimitRange` into the listed namespaces, so apiserver admission bounds the blast radius independently of the API layer. | `TestNamespaceQuotaDefaultRendersNothing`, `TestNamespaceQuotaRendersPerEntry`, `TestNamespaceQuotaRendersSubObjectsIndependently`, `TestNamespaceQuotaSchema` | Off by default: without it, only the API-layer tenant quota bounds workspaces, and that quota is set by the platform configuration (`quota`) or bounded by its `quotaCeiling`. |
| TM-45 | 8 | A Workspace writer edits the template-snapshot mirror annotation that consumers read. | Opt-in `admissionPolicy.enabled` and the `config/admissionpolicy/` sample render a `ValidatingAdmissionPolicy` (`failurePolicy: Fail`, `Deny`) that exempts the operator service account and denies any other create carrying, or update changing, the annotation. | `TestAdmissionPolicyOffByDefault`, `TestAdmissionPolicyObjects`, `TestAdmissionPolicyExpressions`, `TestAdmissionPolicyBinding`, `TestSnapshotAnnotationVAP_Enforced` | Off by default; the annotation is never trusted either way. |
| TM-46 | 8 | A successor template revision moves a workspace's egress, clipboard policy, adapter, `hostUsers`, placement or security profiles at its next start without notice. | `revisionIncompatible` (`internal/provisioning/template_family.go`) treats each of those fields as an exact-match dimension from `templateEntry` (`internal/provisioning/k8sapplier.go`); any change records `TemplateUpdateSkipped` with a reason naming the field and keeps the recorded revision. `WorkspaceTemplate` carries no browser URL policy, so no unenforced restriction is advertised. | `TestUpdate_SkippedOnSecurityFieldDrift`, `TestUpdate_SkippedReasonMatrix`, `TestEvents_TemplateSkipReasons`, `TestStart_PolicyFromRecordedSnapshotOnly`, `TestWorkspaceTemplate_BrowserPolicyFieldRemoved` | — |
| TM-47 | 8 | A template author points the pod's seccomp profile outside the kubelet seccomp root or to `unconfined`. | `resolveSeccompProfile` and `validSeccompProfileName` (`internal/runtime/linux/backend.go`) accept only `runtime/default` or a clean relative `localhost/<path>` and reject the template otherwise; the operator re-checks at admission (`ValidateSeccompAnnotation`) and never records an out-of-policy snapshot. The operator refuses a template that names the `chromium-userns` seccomp profile unless the pod runs with `hostUsers: false`. | `TestSeccompProfileAccepted`, `TestSeccompProfileRejected`, `TestEnsureRejectsBadSeccompAnnotation`, `TestTemplateInvalidBadSeccompAnnotation`, `TestRecordedSnapshotBadSeccompAnnotationResnapshots`, `TestEnsureUsernsProfileRequiresHostUsersFalse`, `TestSeededUsernsProfileRequiresHostUsersFalse`, `security-doc-claims.test.sh` | With the userns browser profile `localhost/profiles/chromium-userns.json` the workspace user can create user, PID and network namespaces and reach the kernel code behind them; the profile is admitted only with an effective `hostUsers: false`, so they sit inside the pod's own user namespace. Accepted: the Chromium sandbox needs them. The check matches the shipped profile by name; a custom `Localhost` profile that allows namespace creation is not detected and needs `hostUsers: false` from the operator. |
| TM-48 | X | A third-party `kasmweb/*` image below the supported engine floor, or not digest-pinned, enters the catalog. | `check-kasm-catalog.sh` requires every `kasmweb/*` reference to be a digest-pinned catalog entry; `scan-kasm-catalog.sh` and the CI `kasm-contract` job scan the catalog and check the adapter contract; the chart refuses a kasm template without a digest. Releases run the measured engine floor and trivy catalog scan before publish; merges are gated by the declared floor. | `kasm-contract` CI job, `.github/tests/kasm-catalog.test.sh`, `TestKasmAdapterEnabledRequiresDigest`, `TestKasmAdapterTagWithoutDigestRefused`, `TestKasmBrowserTemplateNeedsFreshEngine`, `TestValidateSnapshotSpecKasmAdapter`, `release-topology.test.sh`, `security-doc-claims.test.sh`, `failed-digest-cleanup.test.sh` | Catalog images carry their own KasmVNC build; the floor and the exception file's expiry are the controls. |
| TM-49 | X | A compromised or careless change publishes an unsigned, unscanned or stale runtime image that consumers then trust. | `runtime-images.yml` builds, scans and signs by digest, and its publish job waits on the `runtime-train` environment approval; the train manifest ships with a Sigstore bundle; a gate-failed run's cleanup job deletes the digests that run pushed and refuses tagged versions (`cleanup-failed-digests.sh`); `runtime-freshness.yml` fails while a browser pin is stale; required checks are codified in `setup-repo-protection.sh`. The train and release record the image build time as `builtAt` and refuse to sign when any scan leg finished more than 24 hours earlier (`check-scan-age.sh`); per-job publish gating is pinned by `publish-gating.test.sh`; a chart whose signing fails is deleted from the registry (`push-sign-chart.sh`); `:main` images build without the shared Actions cache. | `.github/tests/failed-digest-cleanup.test.sh`, `.github/tests/supply-chain-hardening.test.sh`, `.github/tests/write-runtime-manifest.test.sh`, `.github/tests/check-runtime-image-age.test.sh`, `.github/tests/bump-browser-pin.test.sh`, `.github/tests/setup-repo-protection.test.sh`, `write-runtime-manifest.test.sh`, `check-scan-age.test.sh`, `publish-inputs.test.sh`, `release-topology.test.sh`, `publish-gating.test.sh`, `setup-repo-protection.test.sh`, `push-sign-chart.test.sh`, `security-doc-claims.test.sh` | One account can author, merge and approve a publish; failed-digest cleanup is check-then-act; browser pins need repinning when Debian security supersedes them. |
| TM-50 | X | An admin or user action leaves no durable record, or an attacker makes the audit write fail to hide an action. | `audited()` (`internal/api/auditroutes.go`) emits one event per covered route; `DurableAuditSink` (`internal/api/auditstore.go`) persists every action except `session.touch` in the audited transaction and fails the request when persisting fails; the broker's transfer claim and completion are persisted the same way. `audited()` stores a bounded, valid-UTF-8 target, and quota and user-limit writes run inside the audited transaction. | `TestAuditCoverage_SpecMatchesTable`, `TestAuditCoverage_MountedMuxEmitsEventPerRoute`, `TestAudited_PersistFailureFailsClosed`, `TestAudited_NoTxMutationPersistFailureFailsClosed`, `TestAudited_TouchStaysStdoutOnly`, `TestAudit_AmendAndPrune`, `TestControlAudit_OperatorSurface`, `TestGuardedSink_ReportsFailureNeverPropagates`, `TestAudited_TargetSanitizedAndBounded`, `TestAudit_InsertSanitizedTarget`, `TestAdminUserLimit_AuditFailureRollsBackWrite` | Five kinds of record are stdout-only (§5 Audit); delivery and tamper evidence of the log stream are the operator's. |
| TM-51 | 2 | A tenant admin lifts their own tenant's quota without bound. | A tenant-admin quota write is refused unless the platform configuration declares `managedNamespaces[].quotaCeiling` for the tenant (rendered as `-tenant-quota-ceilings`), and a write above the ceiling is refused; a tenant with a declared `quota` stays read-only (`internal/api/adminquota.go`). | `TestAdminQuota_PutAboveOperatorCeilingRefused`, `TestAdminQuota_UndeclaredTenantCreateRefused`, `TestAdminQuota_PutAtCeilingAccepted` | The ceiling bounds the API-layer quota only; the opt-in `namespaceQuota` (TM-44) is the independent bound. |
| TM-52 | 5 | An unauthenticated client holds session-listener connections and goroutines by never finishing a request body. | The session listener bounds every request's read phase and caps request headers (`internal/backend/listeners.go`); an admitted WebSocket upgrade and a claimed transfer lift the bound for their own connection only. | `TestSessionListener_StalledBodyBounded`, `TestSessionListener_AdmittedStreamOutlivesReadBound`, `TestSessionListener_TransferUploadOutlivesReadBound` | Each connection can still be held for the bound; the per-client launch limiter and a buffering edge proxy narrow it further. |
| TM-53 | 8 | One workspace exhausts a shared node's process and thread table, failing forks for the kubelet and other tenants. | The install guide requires a kubelet `podPidsLimit` on workspace nodes; `hack/preflight/preflight.sh` reads each workspace node's kubelet configuration and warns when the limit is unset or `-1`; the kind quickstart sets it. | `preflight.test.sh`, `security-doc-claims.test.sh` | Kubernetes has no pod-spec PID limit, and the product cannot set kubelet configuration; a node without `podPidsLimit` bounds a pod's threads only by its memory limit (accepted, §7). |
| TM-54 | 7 | A workload holding a client certificate from the internal issuer calls the gateway routes on :9443. | The gateway role is granted only to CNs listed in `-internal-gateway-cns` (empty by default; the in-process gateway needs none); every other non-operator CN gets 403, counted as `role_mismatch` (`internal/broker/httpapi/server.go`). | `TestUnknownCN_Refused`, `TestRoleMismatchCounted` | `certManager.issuerRef` must be a dedicated issuer: anyone it signs for can present the operator CN. |

## 7. Accepted residuals

Each item is also a known limitation in the
[1.0.0 release notes](../releases/v1.0.0.md#known-limitations).

1. **In-pod trust (file transfer).** A process the workspace user starts
   can read `xfer-token` and replace `tcdi-xferd`; the broker claim and
   the gateway's byte cap are the controls on the transfer leg. The
   desktop leg carries data either way with no grant, so the file-transfer
   and clipboard policies bind an unmodified runtime only. See
   [In-pod transfer helper](#in-pod-transfer-helper) and TM-37.
2. **KasmVNC under connection saturation.** The stream cap is per
   replica, and a direct burst far above it can still crash Xvnc. See
   TM-35.
3. **mTLS revocation is server-side only.** The operator does not check
   the server certificate's serial. See TM-38.
4. **Revocation records are not pruned.** `workspace_revocation` grows by
   one row per revoked generation. See TM-39.
5. **Some audit events are stdout-only.** Five kinds of record never
   reach `audit_event`. See TM-50 and §5 Audit.
6. **Rate-limit budgets are per client address.** A client holding many
   addresses or /64 prefixes gets one budget per address or prefix. See
   TM-06.
7. **DNS egress is open on every network profile.** See TM-34.
8. **Failed-digest cleanup is check-then-act.** A version could gain a
   tag or a referrer between the check and the deletion. See TM-49.
9. **Browser support.** Only the apt-pin staleness is a security
   residual: pins need repinning when Debian security supersedes them.
   See TM-49.
10. **Single maintainer, no independent audit.** The same account can
    author, merge and approve a publish, and no independent security
    audit has been done. There is no register row: this is a property of
    the project, not of a control.
11. **Process count needs a kubelet bound.** The product cannot set or
    enforce the kubelet's `podPidsLimit`; it requires it at install and
    checks it in preflight. See TM-53.

## 8. Review guide

Read in this order:

1. [`SECURITY.md`](../../SECURITY.md): the normative hardening rules and
   the operator checklist.
2. [ADR 0003](../adr/0003-gateway-broker-internal-api.md),
   [ADR 0004](../adr/0004-launch-origin-policy.md) and
   [ADR 0005](../adr/0005-backend-frontend-operator.md): the internal API
   contract, the launch origin policy and the component and session-host
   split. Then [ADR 0009](../adr/0009-session-data-transfer-policy.md)
   §5 (file transfer) and
   [ADR 0010](../adr/0010-audit-store-and-retention.md) (audit store).
3. The gateway: `internal/gateway/proxy.go` (`ServeHTTP`, `serveProxy`,
   `responsePolicy`, `rewriteForwarded`), `authorization.go` (sessions,
   fencing, `upstreamPathOK`, `cleanedProxyPath`), `launch.go` and
   `transfer.go`.
4. The broker: `internal/broker/leases.go` (`liveLease`), `sessions.go`
   (revokes, `lockByKeysInOrderTx`, `ClaimStream`), `tickets.go` and
   `transfers.go` (`ClaimTransfer`).
5. The pod side: `internal/xferd/xferd.go`,
   `internal/runtime/linux/backend.go` (`buildPod`, `buildNetPol`,
   `ensureSecret`) and the two entrypoints in `build/`.
6. [`test-inventory.md`](test-inventory.md),
   [`provenance.md`](provenance.md) and
   [`vulnerability-policy.md`](vulnerability-policy.md): the automated
   evidence and the supply-chain posture.
7. §6 of this document, row by row.

Questions a reviewer should answer on the tree under review:

- **Is the upstream `Authorization` always overwritten?** The session
  proxy must replace, not merely strip, a client-supplied `Authorization`
  on every proxied path, because KasmVNC's credential check is unreachable
  only while injection is unconditional (`TestProxy_StripsClientAuth`;
  TM-33). Injection lives in the `Rewrite` hook, after the hop-by-hop
  strip, so a client `Connection` token list cannot remove it
  (`connheaders_test.go`).
- **Does the cluster CNI enforce NetworkPolicy?** `ws-<uid>-boundary`
  policies bind only where the CNI implements NetworkPolicy
  ([`compatibility.md`](../compatibility.md) lists the tested CNI).
  A non-enforcing CNI silently opens workspace ingress and egress, which
  removes TM-33 and TM-34. Confirm the supported-deployments matrix keeps
  that assumption explicit.
- **Does every lease read re-check the portal session?** `liveLease`
  re-validates the bound portal session on every renew, attach and claim
  and on the `LeaseBySession` cookie resolve (TM-07). Check that every
  new lease-read path goes through it.
- **Is the lock order still deterministic?** The no-deadlock argument for
  the session-layer revokes is `lockByKeysInOrderTx` (tickets, sessions,
  leases, ascending key order within each kind), pinned by
  `TestRevokePortalSession_RedeemCommitThenRevoke`,
  `TestRevokePortalSession_RevokeCommitThenRedeem` and
  `TestRevokePrincipalSessions_MultiTicketInversion` under `-race`.
  Re-check it whenever a session-layer mutator is added.
- **Can any path reach `/xfer/` without `ClaimTransfer`?** Only
  `serveTransfer` builds a `/xfer/` URL, after the claim; `serveProxy`
  refuses it (TM-30, TM-31).
- **Can a pod-side process obtain anything beyond its own workspace?**
  The workspace Secret, the transfer token and the KasmVNC credential are
  per workspace; the pod NetworkPolicy admits only gateway pods; the
  transfer leg sends the pod nothing but the token
  ([In-pod transfer helper](#in-pod-transfer-helper), TM-37).
