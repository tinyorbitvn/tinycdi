# Security review: TinyCDI 1.0.0

This is the internal security review of TinyCDI 1.0.0. The project's
maintainers ran it before the stable release. It is **not** an
independent audit. See [SECURITY.md](../../SECURITY.md) for how to
report a vulnerability.

- Reviewed commit: `351029a`
- Review date: 2026-10-14
- Threat model: [threat-model.md](threat-model.md), register rows TM-01 to TM-50
- Result: **0 P0, 0 P1, 9 P2, 32 P3**. All findings are open at the
  reviewed commit.

## 1. Scope

The whole product at the reviewed commit:

- **Backend:** `cmd/backend`, `internal/`. This covers:
  - the `/v1` API on the app listener, including the admin surfaces:
    audit, sessions and sign-out, session-policy `:apply`, overview,
    user directory, quotas and per-user limits;
  - OIDC sign-in and the portal session cookies (`lax` and
    `partitioned` modes);
  - CSRF and origin checks, and rate limits;
  - the session listener: launch tickets, the desktop proxy and the
    `/v1/transfers` leg;
  - the broker: tickets, leases, stream epochs and transfer claims;
  - the internal mTLS listener;
  - the Postgres store and the post-restore procedure.
- **Operator:** `cmd/operator`, `internal/operator`,
  `internal/provisioning`, `internal/runtime`. This covers the pod,
  Secret, Service and NetworkPolicy the operator builds from a
  `Workspace` and its `WorkspaceTemplate`, and the kasm adapter.
- **Runtime images and the in-pod helper:** `build/` (entrypoints,
  KasmVNC configuration, Dockerfiles), `cmd/tcdi-xferd`,
  `internal/xferd`, and `deploy/node-profiles`.
- **Portal:** `web/` and the frontend server `build/frontend`.
- **Helm chart:** `deploy/helm/tinycdi`. This covers the defaults and
  render guards: RBAC, NetworkPolicy, Pod Security labels, ClusterOnly
  egress, the `extraArgs`/`extraEnv` deny lists, the admission policy
  and the namespace quota.
- **CI and release supply chain:** `.github/workflows`,
  `.github/scripts`, `.github/tests`, and the quickstart pin
  verification in `hack/`.

Out of scope:

- the internals of third-party components: KasmVNC, Chromium and
  Firefox, Kubernetes, the network plugin, the OIDC IdP and Postgres;
- kernel vulnerabilities;
- an independent external audit.

## 2. Method

1. **Controls.** The threat model was the map. For every register row,
   the reviewers found the claimed control in the code, checked it was
   correct and wired in (a mounted route, a flag default, a chart
   default), and recorded its location as `file:line`.
2. **Bypasses.** On each boundary, the reviewers then looked for ways
   around the controls:
   - authentication and authorization: cross-tenant, cross-user, admin
     versus user, and direct object references on workspace, session,
     transfer, audit, retained-data and user identifiers;
   - ticket, claim and lease replay and races;
   - CSRF and origin handling;
   - upstream resolution in the gateway;
   - path traversal and symlinks in the transfer helper;
   - size and time bounds;
   - header and cookie handling;
   - injection: SQL, headers, logs, Kubernetes names and labels;
   - secrets in logs, audit rows, API responses and the environment;
   - what a hostile `Workspace` or template can make the operator do;
   - pod privilege and escape paths;
   - NetworkPolicy coverage;
   - token entropy, constant-time comparison, and OIDC validation (iss,
     aud, exp, nonce, alg);
   - CI permissions, action pinning, signing identity and environment
     gates.
3. **Evidence bar.** A finding needed three things: a location, a
   concrete attack (who, what input, what they gain), and a proof. The
   proof was a failing test run in an isolated copy of the reviewed
   commit, a reproduction, or a precise code trace. Anything that could
   not be substantiated was dropped.
4. **Dynamic testing.** All of it ran locally:
   - Go unit tests and envtest;
   - Postgres-backed tests on a disposable Postgres 18 container;
   - Helm renders;
   - the runtime images, built from the reviewed commit and run under
     Docker with the pod's restrictions: uid 1000, all capabilities
     dropped, `no-new-privileges`, read-only root.

   No live Kubernetes cluster was used.

**Severity scale:**

| Severity | Meaning |
|---|---|
| P0 | Unauthenticated or cross-tenant compromise, remote code execution, pod escape, or credential theft at scale |
| P1 | Authenticated privilege escalation, cross-user data access, an auth bypass that needs unusual conditions, or a supply-chain compromise path |
| P2 | A defense-in-depth gap with limited impact, or denial of service by an authenticated user |
| P3 | Hardening, or documentation that claims something the code does not do |

## 3. Verdicts by register row

Each row has one of three verdicts:

- **Verified:** the control exists as the register describes it, and no
  bypass was found.
- **Partial:** the control exists, but a finding weakens it.
- **Gap:** the control does not hold against the stated threat.

A finding listed against a Verified row is a documentation or hardening
item next to a sound control.

Totals: 39 Verified, 10 Partial, 1 Gap.

| Row | Boundary | Verdict | Control verified at | Findings |
|---|---|---|---|---|
| TM-01 | 1 | Verified | `build/frontend/main.go:155,168-174,188-199`; `internal/gateway/proxy.go:1249`; `internal/api/auth.go:123-132` | R1-10, R1-11 |
| TM-02 | 2 | Verified | `internal/api/middleware.go:153,199`; `internal/api/auth.go:1048`; `internal/backend/wire.go:912` | — |
| TM-03 | 2 | Verified | `internal/api/middleware.go:86-101`; `internal/store/sessions.go:221,264` | — |
| TM-04 | 2 | Verified | `internal/api/workspaces.go:370-401,582-600`; `internal/api/events.go:265-290`; `internal/broker/tickets.go:354-369` | R1-18, R1-19 |
| TM-05 | 2 | Verified | `build/frontend/main.go:254,288,323` | R1-13 |
| TM-06 | 2 | Partial | `internal/ratelimit/ratelimit.go:188-240`; `internal/api/middleware.go:307`; `internal/api/ratelimit_key.go:34` | R1-03, R1-15 |
| TM-07 | 2 | Verified | `internal/api/auth.go:708`; `internal/broker/sessions.go:113`; `internal/broker/leases.go:167` | R1-14 |
| TM-08 | 2 | Verified | `internal/api/revokeall.go:66`; `internal/broker/sessions.go:224` | — |
| TM-09 | 2 | Partial | `internal/provisioning/quota.go:189,433-496`; `internal/api/adminuserlimit.go:125-139` | R1-01, R1-20 |
| TM-10 | 2 | Verified | `internal/api/workspaces.go:32`; `internal/api/connections.go:161-174` | — |
| TM-11 | 2 | Verified | `internal/api/middleware.go:122`; `internal/api/principal.go:61`; `internal/backend/config.go:432` | R1-17 |
| TM-12 | 2 | Verified | `internal/api/adminaudit.go:52-59`; `internal/store/audit.go:99` | — |
| TM-13 | 2 | Verified | `internal/api/adminusers.go:49-110`; `internal/store/users.go:54` | R1-16 |
| TM-14 | 2 | Verified | `internal/api/adminsessions.go:62-74`; `internal/broker/sessions.go:224-275` | — |
| TM-15 | 2 | Verified | `internal/api/adminoverview.go:72-116`; `internal/api/statusview.go:649-680` | — |
| TM-16 | 2 | Verified | `internal/api/workspaces.go:679-695`; `internal/provisioning/service.go:644-700` | — |
| TM-17 | 2 | Verified | `internal/api/adminsessionpolicy.go:147-149`; `internal/api/adminsessionpolicyapply.go:75-205` | R1-21 |
| TM-18 | 2 | Verified | `internal/api/filetransfers.go:89-112,142-209`; `internal/store/filetransfer.go:15,40-44` | — |
| TM-19 | 3 | Verified | `internal/api/loginstate/sealer.go:122-173`; `internal/api/auth.go:450-478,531-576` | — |
| TM-20 | 3 | Verified | `internal/api/auth.go:369,384,802,828` | R1-12 |
| TM-21 | 4 | Verified | `internal/backend/postrestore.go:315-341,394-404`; `internal/store/sessions.go:141-251` | — |
| TM-22 | 4 | Verified | `internal/provisioning/k8sapplier.go:70-95,350-375`; `internal/operator/workspace_controller.go:893-905` | — |
| TM-23 | 5 | Verified | `internal/gateway/rehydrate.go:45,83,155`; `internal/broker/sessions.go:67`; `internal/broker/tickets.go:301` | R1-25 |
| TM-24 | 5 | Verified | `internal/gateway/authorization.go:282`; `internal/gateway/proxy.go:719`; `internal/broker/sessions.go:350`; `internal/gateway/launch.go:310` | — |
| TM-25 | 5 | Verified | `internal/sessionhost/sessionhost.go:181`; `internal/gateway/proxy.go:654`; `internal/gateway/launch.go:333` | — |
| TM-26 | 5 | Partial | `internal/gateway/proxy.go:378,456`; `internal/backend/wire.go:632` | R1-26 |
| TM-27 | 5 | Verified | `internal/gateway/authorization.go:730`; `internal/backend/wire.go:616` | — |
| TM-28 | 5 | Verified | `internal/gateway/authorization.go:586,654`; `internal/gateway/proxy.go:667` | — |
| TM-29 | 5 | Gap | `internal/gateway/proxy.go:338,396-402,1249` | R1-04 |
| TM-30 | 5 | Verified | `internal/gateway/transfer.go:42,237`; `internal/broker/transfers.go:86` | — |
| TM-31 | 5 | Verified | `internal/gateway/authorization.go:515,541`; `internal/gateway/transfer.go:166` | — |
| TM-32 | 6 | Verified | `internal/gateway/proxy.go:955,1010,1050,1078` | — |
| TM-33 | 6 | Verified | `internal/xferd/xferd.go:106-110`; `build/linux-base/kasmvnc.yaml:20-21,80-81`; `internal/runtime/linux/backend.go:1575-1592` | — |
| TM-34 | 6 | Verified | `internal/runtime/linux/backend.go:1605-1641`; `cmd/operator/main.go:118` | R1-28 |
| TM-35 | 6 | Verified | `internal/gateway/streamcap.go:48`; `internal/gateway/proxy.go:673`; `internal/backend/config.go:440` | — |
| TM-36 | 6 | Partial | `internal/runtime/linux/backend.go:1160`; `build/linux-base/entrypoint.sh:120-158`; `build/kasm-adapter/entrypoint.sh:151-216` | R1-06 |
| TM-37 | 6 | Verified | `internal/xferd/xferd.go:66,84,114-123,129-141,145-146,214-236`; `internal/runtime/linux/backend.go:796-812` | R1-27 |
| TM-38 | 7 | Partial | `internal/backend/listeners.go:90`; `internal/broker/httpapi/opclient/client.go:80,169`; `internal/tlsreload/blocklist.go:137,271` | R1-30, R1-31 |
| TM-39 | 7 | Verified | `internal/broker/activity.go:320,383-416` | — |
| TM-40 | 7 | Partial | `internal/broker/httpapi/server.go:148,182` | R1-33 |
| TM-41 | 8 | Verified | `internal/runtime/linux/backend.go:1069-1071,1269-1272`; `cmd/operator/main.go:242,624` | — |
| TM-42 | 8 | Partial | `deploy/helm/tinycdi/templates/_helpers.tpl:253,403-404,416-435,611` | R1-34 |
| TM-43 | 8 | Verified | `internal/operator/workspace_controller.go:430-512,1038,1098,1297`; `deploy/helm/tinycdi/templates/rbac.yaml:24-26,71-79` | — |
| TM-44 | 8 | Verified | `deploy/helm/tinycdi/templates/namespace-quota.yaml` | — |
| TM-45 | 8 | Verified | `deploy/helm/tinycdi/templates/admissionpolicy.yaml:11-65` | — |
| TM-46 | 8 | Verified | `internal/provisioning/template_family.go:172-199`; `internal/provisioning/k8sapplier.go:612-655` | — |
| TM-47 | 8 | Verified | `internal/runtime/linux/backend.go:1019-1065`; `internal/operator/workspace_controller.go:501,1138,1318` | R1-29 |
| TM-48 | X | Partial | `.github/scripts/check-kasm-catalog.sh:53-171`; `.github/scripts/scan-kasm-catalog.sh:51-77`; `deploy/helm/tinycdi/templates/_helpers.tpl:362-389` | R1-37 |
| TM-49 | X | Partial | `.github/workflows/runtime-images.yml:677-686,850`; `.github/scripts/cleanup-failed-digests.sh:59-309`; `.github/scripts/setup-repo-protection.sh:371-400` | R1-09, R1-38, R1-39, R1-40 |
| TM-50 | X | Partial | `internal/api/auditroutes.go:236-360`; `internal/api/auditstore.go:38-67` | R1-22, R1-23 |

Nine findings fit no single register row. They are listed against their
boundary in §4: R1-02, R1-05, R1-07, R1-08, R1-24, R1-32, R1-35, R1-36
and R1-41.

## 4. Findings

Fix guidance and the security test that each fix must add are tracked
with the fix work. This report gives the finding and where it is.

### P2

| ID | Sev | Row | Title | Where | Status |
|---|---|---|---|---|---|
| R1-01 | P2 | TM-09 | The quota settle pass can race a `:start` and release the reservation of a workspace that is running again, so it counts against neither the per-user limit nor the tenant quota | `internal/provisioning/recovery.go` | open |
| R1-02 | P2 | — (2) | A tenant admin can set an effectively unbounded quota for a tenant whose quota the platform configuration does not declare | `internal/api/adminquota.go` | open |
| R1-03 | P2 | TM-06 | Random session cookies cost a store read per request on the app listener, including requests the rate limiter then refuses; the session listener's unknown-cookie bound has no app-listener equivalent | `internal/api/ratelimit_key.go`, `internal/api/middleware.go` | open |
| R1-04 | P2 | TM-29 | An informational (1xx) response from the runtime removes the security headers the gateway pins on session-host responses | `internal/gateway/proxy.go` | open |
| R1-05 | P2 | — (5) | The session listener sets no request-body read deadline, so an unauthenticated client can hold connections open indefinitely | `internal/backend/listeners.go` | open |
| R1-06 | P2 | TM-36 | A program inside the session can re-enable the clipboard after start, because Xvnc's default parameter-override list stays in force | `build/linux-base/kasmvnc.yaml`, `build/kasm-adapter/entrypoint.sh` | open |
| R1-07 | P2 | — (6, 8) | Workspace pods have no process or thread bound, and the install guide does not require one, so one workspace can exhaust a shared node's thread table | `internal/runtime/linux/backend.go`, `docs/runbooks/install.md` | open |
| R1-08 | P2 | — (8) | The `WorkspaceTemplate` field `browserPolicy` is documented as restricting the in-session browser, but nothing applies it | `api/v1/workspacetemplate_types.go` | open |
| R1-09 | P2 | TM-49 | The runtime-train publish can sign images whose scan is weeks old and stamps `builtAt` with the publish time; release charts carry no `builtAt`, so the stale-image block never fires for them | `.github/scripts/write-runtime-manifest.sh`, `.github/workflows/runtime-images.yml` | open |

### P3

| ID | Sev | Row | Title | Where | Status |
|---|---|---|---|---|---|
| R1-10 | P3 | TM-01 | `/v1` responses on the portal host carry no `nosniff`, framing or `no-store` headers | `internal/backend/wire.go` | open |
| R1-11 | P3 | TM-01 | The API contract misstates the session cookie's name and SameSite mode, and the basis of the CSRF defence | `internal/api/openapi.yaml` | open |
| R1-12 | P3 | TM-20 | The API contract says the ID token never reaches the client; sign-out returns it as `id_token_hint` | `internal/api/auth.go`, `internal/api/openapi.yaml` | open |
| R1-13 | P3 | TM-05 | The register says branding serves fixed names only; it serves any regular file in the directory (containment holds) | `build/frontend/main.go` | open |
| R1-14 | P3 | TM-07 | A cross-site navigation to `GET /v1/login` replaces a signed-in user's portal session and ends their desktop stream | `internal/api/auth.go` | open |
| R1-15 | P3 | TM-06 | The install guide tells operators to trust the whole pod CIDR for `X-Forwarded-For` | `docs/runbooks/install.md` | open |
| R1-16 | P3 | TM-13 | The request log records the raw target issuer and subject for admin routes addressed by user | `internal/api/middleware.go` | open |
| R1-17 | P3 | TM-11 | Removal from a tenant-admin group takes effect only at the user's next sign-in, and the docs do not say so | `internal/api/middleware.go`, `SECURITY.md` | open |
| R1-18 | P3 | TM-04 | The connection route answers 403, not 404, for another user's workspace in the same tenant | `internal/broker/tickets.go` | open |
| R1-19 | P3 | TM-04 | NUL bytes in list filters and cursors answer 500 instead of 400 | `internal/api/workspaces.go` | open |
| R1-20 | P3 | TM-09 | The quota reservation can under-count what actually runs | `internal/provisioning/service.go` | open |
| R1-21 | P3 | TM-17 | The register says `:apply` runs a dry run first; the server does not require one | `internal/api/adminsessionpolicyapply.go` | open |
| R1-22 | P3 | TM-50 | Audit rows store the raw path value as the target id: unbounded, and invalid bytes fail the request | `internal/api/auditroutes.go` | open |
| R1-23 | P3 | TM-50 | Single-statement quota and user-limit writes commit outside the audit transaction | `internal/store/userlimit.go` | open |
| R1-24 | P3 | — (2, 8) | The retained-disk volume label carries the user-chosen workspace name | `internal/provisioning/retained_applier.go` | open |
| R1-25 | P3 | — (5) | A refused ticket redemption leaves the ticket unconsumed until it expires | `internal/broker/tickets.go` | open |
| R1-26 | P3 | TM-26 | Health probes answer on every Host of the session listener; the control-host class rests on the bearer token alone | `internal/backend/wire.go`, `internal/gateway/authorization.go` | open |
| R1-27 | P3 | TM-37 | The in-pod trust residual understates the risk: a replaced in-pod server can move files over the desktop leg with no grant, cap or transfer audit | `docs/security/threat-model.md`, `docs/releases/v1.0.0.md` | open |
| R1-28 | P3 | TM-34 | Default ClusterOnly egress admits every pod in other tenants' namespaces; only the destination's policy blocks it | `internal/runtime/linux/backend.go` | open |
| R1-29 | P3 | TM-47 | The browser seccomp and AppArmor pair lets a workspace user create user and network namespaces; the docs do not state the trade-off | `deploy/node-profiles/` | open |
| R1-30 | P3 | TM-38 | Rotating the internal client CA does not drop established connections, contrary to the runbook | `internal/backend/listeners.go` | open |
| R1-31 | P3 | TM-38 | The revocation runbook's issuer-name recipe can produce blocklist entries that never match | `internal/tlsreload/blocklist.go` | open |
| R1-32 | P3 | — (7) | The internal listener grants the gateway role to any client certificate from its CA whose name is not the operator's | `internal/broker/httpapi/server.go` | open |
| R1-33 | P3 | TM-40 | Handshake and role refusals on the internal listener are not counted | `internal/broker/httpapi/server.go` | open |
| R1-34 | P3 | TM-42 | The `extraArgs`/`extraEnv` deny lists are incomplete and asymmetric between flags and environment | `deploy/helm/tinycdi/templates/_helpers.tpl` | open |
| R1-35 | P3 | — (8) | The workspace NetworkPolicy is created after the pod and Service | `internal/runtime/linux/backend.go` | open |
| R1-36 | P3 | — (8) | The chart value `podSecurity.platformEnforce` has no effect | `deploy/helm/tinycdi/values.yaml` | open |
| R1-37 | P3 | TM-48 | The kasm engine-floor and image-scan gate is advisory; it blocks neither merge nor release | `.github/workflows/ci.yml` | open |
| R1-38 | P3 | TM-49 | The tests cited for TM-49 do not pin the signing chain (scan first, main only, only the gated job signs) | `.github/tests/` | open |
| R1-39 | P3 | TM-49 | The codified required-check set is narrower than the docs state | `.github/scripts/setup-repo-protection.sh` | open |
| R1-40 | P3 | TM-49 | The Helm chart is published under its version tag before it is signed | `.github/workflows/release.yml` | open |
| R1-41 | P3 | — (X) | Signed main-channel images build from the shared Actions cache | `.github/workflows/images.yml` | open |

## 5. Accepted risks

These are the known limitations in the
[1.0.0 release notes](../releases/v1.0.0.md#known-limitations) and
threat model [§7](threat-model.md#7-accepted-residuals). They were not
re-reported. Where the review found that the text understates the risk,
the finding is named.

| Accepted risk | Review note |
|---|---|
| In-pod trust (file transfer) | Understated: R1-27. The same in-pod position also reaches the clipboard settings (R1-06), which a configuration change can close. |
| File transfer scope | No change. |
| KasmVNC under connection saturation | No change. |
| mTLS revocation is server-side only | No change. Two related gaps in rotation and revocation: R1-30, R1-31. |
| Revocation records are not pruned | No change. The generation bound (TM-39) holds. |
| Some audit events are stdout-only | No change. Durable-audit gaps on two write paths: R1-22, R1-23. |
| Rate-limit budgets are per client address | No change. The proxy-trust guidance is too wide: R1-15. |
| DNS egress is open on every network profile | No change. |
| Node drain loses running desktops | No change. |
| The admin user directory lists signed-in users only | No change. |
| Failed-digest cleanup is check-then-act | No change. |
| Browser support (apt pin staleness) | No change. A related freshness gap in signing and `builtAt`: R1-09. |
| Single maintainer, no independent audit | No change. This review is internal. |

## 6. Residual risk

**Release gate.** No P0 or P1 finding is open at the reviewed commit.
Each of the nine P2 findings must be fixed, or accepted with an owner
and a reason, before the stable release. This report's status column
records the outcome. The P3 findings are hardening and documentation
corrections.

The open findings fall into five themes:

- **Availability under unauthenticated traffic** (R1-03, R1-05). Both
  listeners bound header parsing and request rates. Two gaps remain:
  the app listener has no bound on random-cookie store reads, and the
  session listener has no bound on how long a request body takes to
  arrive. A buffering reverse proxy in front of the session listener
  narrows R1-05.
- **Policies enforced inside the workspace** (R1-06, R1-08, R1-27,
  R1-29). The workspace user controls their runtime. Clipboard, browser
  and file-transfer restrictions bind an unmodified runtime only:
  - R1-06 closes the one-command clipboard override;
  - R1-08 needs `browserPolicy` implemented or removed;
  - R1-27 and R1-29 are about stating these limits plainly.

  The controls outside the pod hold: the broker claim, the gateway's
  byte cap, the path allowlist, the NetworkPolicy and the pod
  hardening.
- **Fair use of shared capacity** (R1-01, R1-02, R1-07, R1-20). The
  per-user limit and the tenant quota can be exceeded through a race or
  a tenant-admin write. Without a node PID bound, one workspace can
  affect every workspace on its node. The opt-in namespace quota (TM-44)
  and a kubelet `podPidsLimit` bound the worst case independently.
- **Hostile runtime responses** (R1-04). Every pinned session-host
  header can be removed through a 1xx response. The impact stays within
  the workspace's own origin, in its owner's browser.
- **Supply-chain freshness and test coverage** (R1-09, R1-37 to R1-41).
  Signing, digest pinning, least-privilege permissions and SHA-pinned
  actions hold. The gaps are freshness (scan age and `builtAt`),
  advisory gates, and tests that do not pin the signing order.

**What held.** The review found no cross-tenant or cross-user access
path. Every id-bearing route scopes by tenant and owner in SQL. Every
state-changing route carries CSRF and audit. Session cookies and
tickets are stored only as digests. OIDC validation covers iss, aud,
exp, nonce and alg, and uses PKCE S256. Launch Origin checks and host
binding hold. The desktop-path allowlist resists encoding tricks. The
transfer helper's jail and token check hold. Workspace pods run
non-root, with no privilege escalation, all capabilities dropped and no
service-account token. They use RuntimeDefault seccomp and AppArmor
unless a template names a checked `localhost/` profile, and run with
`hostUsers: false` in the chart default.
NetworkPolicy ingress to a workspace is limited to the gateway. The
operator alone records the template snapshot. CI actions are SHA-pinned
with least-privilege tokens, and no untrusted input reaches a `run:`
step.

**Limits of this review.**

- **No live Kubernetes cluster was used.** These were reviewed from
  manifests and code: NetworkPolicy behaviour on specific network
  plugins, RBAC at runtime, node-wide PID exhaustion (R1-07), and
  kubelet handling of `localhost/` AppArmor profiles.
- **No real browser was used.** These were not exercised: partitioned
  cookies on a truly cross-site topology, how browsers treat relayed
  1xx headers, and the KasmVNC client's in-frame message handling. The
  last was judged unreachable, because of the portal's
  `frame-ancestors 'none'` and `Cross-Origin-Opener-Policy`.
- **GitHub settings were not inspected live.** This covers environments,
  the tag ruleset, branch protection and admin bypass. The approval
  controls in TM-49 assume `setup-repo-protection.sh` has been applied
  to the repository.
- **Kasm coverage was partial.** Only one cached `kasmweb/*` image was
  exercised, and the full catalog image scan was not run.
- **Some parts got less depth.** The split-deployment broker client was
  reviewed in less depth than the merged deployment. KasmVNC's
  handlers behind the allowlisted desktop paths are third-party code
  and were not audited.
