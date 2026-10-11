# TinyCDI v1.0 — internal security review report

Date: 2026-10-11 · Status: complete — internal review; **not an
independent audit** (see Sign-off).

## 1. Scope and freeze

- **Reviewed tree:** the product at tag **v0.5.0** (commit `7e2e5f8`),
  Helm chart **0.5.0**, and the deployment wrapper as shipped.
- **Fix release:** **v0.5.1** (tag `acb5642`) carries the review fixes;
  it is the tree this report's statuses describe. See
  [`../releases/v0.5.1.md`](../releases/v0.5.1.md).
- **In scope:** the eight trust boundaries of
  [`threat-model.md`](threat-model.md) (§4) and its item table
  (S1–S38, §6): OIDC login/session lifecycle, the session listener and
  gateway proxy, the internal mTLS control plane, tenant data and
  operations, and the supply chain (workflows, image train, release).
- **Out of scope:** the Windows runtime (schema-only in v0.5), KasmVNC
  internals beyond the proxy boundary, and the live identity provider.

## 2. Method

- **Independent review passes.** Five read-only passes ran in parallel,
  one per boundary group: (1) auth and session, (2) session listener and
  gateway, (3) control plane, (4) data/tenancy/operations,
  (5) supply chain and CI. Each pass worked on the frozen tree and could
  write local proof-of-concept tests (run against the unit harness and,
  where needed, a real Postgres); PoCs that pin *weak* behaviour stayed
  review artifacts and were never merged. An earlier attempt to run the
  review as one hosted session was dropped in favour of these
  per-boundary passes.
- **Adversarial verification.** A second pass reproduced or refuted
  every finding on the frozen tree: **33 findings filed — 33 confirmed,
  0 refuted, 0 downgraded.** The table lists the adjusted severities;
  none differed from the claims. One sub-claim
  (SR-1-F5(c), a pass-through trusted proxy) is deployment-conditional —
  the mechanism is confirmed in code; whether a given edge honours the
  invariant is a deployment fact. Two findings were confirmed on the
  frozen tree and verified as already fixed on main at verification
  time (SR-4-F1, SR-4-F2 — see the table).
- **Live-fire drills.** There was no separate drill phase: the
  release-candidate end-to-end gates doubled as the drills, run against
  a disposable install — the forged-annotation drill (G4), the
  injected-headers drill (G5), the session-lookup / ghost-session / COOP
  drill (G6), the rate-limit-keys and audit drill (G7), the internal-API
  drill (G8), the rollback drill (G10), and the rc.2 held-cap
  verification (N1/N2).

### Severity rubric

- **P0**: remote, unauthenticated or cross-tenant compromise
  (desktop/stream takeover, session theft, data of another tenant).
- **P1**: auth/authz bypass with realistic preconditions, in-cluster
  privilege escalation, secret disclosure.
- **P2**: defence-in-depth gap or limit bypass without a direct
  compromise.
- **P3**: hardening or docs.

**Exit criterion:** 0 open P0/P1; every P2 fixed or accepted with an
owner and a target version; P3 listed.

## 3. Findings

Status legend: **Fixed** — corrected and pinned by a regression test in
the listed PR; **Accepted** — risk accepted for v1.0 with an owner and
target version (§6); **Open** — unresolved, listed per the rubric
(governance items are in §7). Findings fixed on v0.5.0-era main before
the v0.5.1 tag are marked accordingly.

| ID | Sev | Title | Status | Fix PR | Pinning test(s) |
|---|---|---|---|---|---|
| SR-1-F1 | P2 | Portal idle timeout defeated by the SPA's visible-tab polls | Fixed | #133 | `TestPolledReads_DoNotSlideIdleWindow`, `TestMeRead_DoesNotSlideIdleWindow`, `TestSessionTouch_SlidesIdleWindow`; `tests/integration/session_touch_test.go` |
| SR-1-F2 | P2 | Lease-layer session liveness ignores the idle window | Fixed | #133 | `TestLeaseSession_IdleDeadSessionRevokesOnRenew`, `TestLeaseSession_RedeemRejectsIdleDeadSession` |
| SR-1-F3 | P2 | `TouchPrincipal` slides every session of the principal | Fixed | #133 | `TestInputActivity_SlidesOnlyBoundSession`, `TestTouchSessionDigest_ScopesToBoundSession` |
| SR-1-F4 | P2 | Logout reports success while a failed session delete leaves the session live | Fixed | #131 | `TestLogout_StoreDeleteFailureReturns503`, `TestRevokeAll_StoreFailureThenRetrySignsOut` |
| SR-1-F5 | P2 | Per-address rate-limit budgets multiply with the client's address pool; XFF depends on an unverified edge invariant | Fixed | #132 | `TestClientKey_IPv6FoldedTo64`, `TestClientKey_MappedFormsUnified`, `TestShared_IPv6WindowAndLocalShareKey`, `TestProxy_IPv6ForwardsExactAddrKeyFolds` — residual in §6 |
| SR-1-F6 | P3 | Dead / misleading auth config surface | Fixed | #134 | `TestAuthConfig_NoDeadSessionKnobs`, `TestNewAuthenticator_RejectsBadPostLogoutRedirect`, `TestNewAuthenticator_AllowsLoopbackPostLogoutRedirect` |
| SR-1-F7 | P3 | `connections.Create` echoes the JSON decoder error | Fixed | #134 | `TestCreateConnection_InvalidBodyGenericMessage`, `TestCreateConnection_TrailingJSONRejected` |
| SR-2-F1 | P2 | Client `Connection:` tokens strip gateway-injected headers | Fixed | #142 | `TestProxy_ConnectionTokensCannotStripInjectedHeaders`, `TestProxy_UpgradeConnectionTokensCannotStripInjected`; e2e injected-headers drill (G5) |
| SR-2-F2 | P3 | Other `X-Forwarded-*` / `X-Original-*` headers reach the runtime verbatim | Fixed | #142 | `TestProxy_ClientForwardedFamilyStripped`, `TestProxy_VendorClientAddressHeadersStripped` |
| SR-2-F3 | P3 | No rate limit on the session-listener proxy path | Fixed | #147 | `TestRehydrate_UnknownCookieSprayBounded`, `TestRehydrate_ReconnectStormOneClient`; e2e session-lookup drill (G6) |
| SR-2-F4 | P3 | Session upstream transport has no `ResponseHeaderTimeout` | Fixed | #142 | `TestProxy_UpstreamResponseHeaderTimeout` |
| SR-2-F5 | P3 | Ghost session pins the workspace between redeem and 303 delivery | Fixed | #147 | `TestLaunch_UnclaimedSessionReaped`, `TestLaunch_AttachDuringReapLoses`; e2e ghost-session leg (G6) |
| SR-2-F6 | n/a | Duplicate record — same defect as SR-4-F1 (and SR-4-F2) | Fixed | #133 | as SR-4-F1; recorded on the frozen tree, already fixed on main |
| SR-2-F7 | P3 | Session listener sets no `Cross-Origin-Opener-Policy` | Fixed | #147 | `TestResponsePolicy_PinsCOOP`, `TestResponsePolicy_StripsUpstreamCOOP`; e2e COOP leg (G6) |
| SR-2-F8 | P3 | `ClusterOnly` egress reaches any pod in any namespace on any port | Accepted | #148 (opt-in mitigation) | `TestClusterOnlyEgressDefaultUnrestricted`, `TestClusterOnlyEgressRestrictionOnPolicy`, `TestParseClusterOnlyEgress`; residual in §6 |
| SR-3-F1 | P1 | Forged `template-snapshot` annotation yields operator-built pods from an arbitrary spec | Fixed | #146 (+#149 optional VAP) | `TestStatusSnapshotAuthoritativeOverForgedAnnotation`, `TestForgedSnapshotStructurallyValidRejected`, `TestAdoptSnapshot_*`, `TestWorkspaceStatusWritableOnlyByOperator`; e2e forged-annotation drill (G4) |
| SR-3-F2 | P3 | `extraEnv` defeats the chart's dev-surface render guards | Accepted | docs only | owner `maintainers`, target v1.1 — §6 |
| SR-3-F3 | P3 | Workspace revoke accepts an unbounded `runtimeGeneration` → permanent fencing | Fixed | #143 | `TestRevokeWorkspaceLeases_GenerationBounded`, `TestRevokeWorkspace_GenerationUnbounded`; e2e internal-API drill (G8) |
| SR-3-F4 | P3 | Auth-failed requests on :9443 are never logged | Fixed | #143 | `TestIdentify_RejectionsLogged`, `TestIdentify_RejectedPathRedacted`, `TestIdentify_RejectionsRateLimited`, `TestIdentify_RejectionsCounted` |
| SR-3-F5 | P3 | No per-certificate revocation on the internal mTLS hop | Accepted | docs only | owner `maintainers`, target v1.1 — §6 |
| SR-3-F6 | P3 | Start-time template guard ignores security-relevant fields | Fixed | #150 | `TestUpdate_SkippedOnSecurityFieldDrift`, `TestUpdate_SkippedReasonMatrix`, `TestEvents_TemplateSkipReasons` |
| SR-4-F1 | P2 | Bound leases ignore the portal session's idle window (duplicate of SR-2-F6; same defect as SR-1-F2) | Fixed | #133 — fixed on v0.5.0-era main before v0.5.1 | `TestLeaseSession_IdleDeadSessionRevokesOnRenew`, `TestLeaseSession_RedeemRejectsIdleDeadSession` |
| SR-4-F2 | P3 | Desktop input extends the principal's other sessions (duplicate of SR-1-F3, claimed P3 vs SR-1-F3's P2) | Fixed | #133 — fixed on v0.5.0-era main before v0.5.1 | `TestInputActivity_SlidesOnlyBoundSession`, `TestTouchSessionDigest_ScopesToBoundSession` |
| SR-4-F3 | P3 | Non-IP `X-Forwarded-For` entries become verbatim rate-limit keys | Fixed | #145 | `TestClientKey_NonIPSelectedEntryFallsBackToPeer`, `TestClientAddr_NonIPSelectedEntryFallsBackToPeer`; e2e junk-XFF leg (G7) |
| SR-4-F4 | P3 | Audit coverage pin is spec↔table only; an unwrapped mutating route escapes detection | Fixed | #145 | `TestAuditCoverage_MountedMuxEmitsEventPerRoute` |
| SR-4-F5 | P3 | Migrations 020/023 build indexes with non-concurrent `CREATE INDEX` inside the migration transaction | Fixed | #162 | `TestSweepTerminalConnectionRows_*`, `TestPruneTerminal_BatchBoundary`, `TestRetention*` (chart) |
| SR-5-F1 | P2 | `runtime-images.json` is the only unsigned artifact in the release chain | Fixed | #135 | `.github/tests/supply-chain-hardening.test.sh` (sign-blob step + asset assertions) |
| SR-5-F2 | P2 | Single-actor merge→signed-publish path | Accepted | #157 (approval-gate mitigation) | `.github/tests/supply-chain-hardening.test.sh` (publish job under `environment: runtime-train`), `.github/tests/setup-repo-protection.test.sh`; residual + trigger in §7 |
| SR-5-F3 | P3 | Gate-failed build digests stay pullable-by-digest | Open | — | n/a — listed (§6) |
| SR-5-F4 | P3 | Scan jobs write unvalidated artifact refs into `GITHUB_ENV` | Fixed | #135 | `.github/tests/supply-chain-hardening.test.sh` (ref validated before the env write) |
| SR-5-F5 | P3 | Repository Actions policy does not enforce SHA pinning | Fixed | #157 | `.github/tests/setup-repo-protection.test.sh` (`sha_pinning_required` PUT preserving live values) |
| SR-5-F6 | P3 | Train images carry no SLSA build-provenance attestation | Fixed | #135 | `.github/tests/supply-chain-hardening.test.sh` (var-gated `attest-build-provenance` legs) |
| SR-5-F7 | P3 | No dedicated SBOM for the packaged chart or the release binaries | Fixed | #135 | `.github/tests/supply-chain-hardening.test.sh`, `publish-inputs.test.sh` (`sbom-chart`/`sbom-binaries` assets) |

## 4. Exit-criterion check

| Severity | Found | Disposition |
|---|---|---|
| P0 | 0 | — |
| P1 | 1 | fixed — SR-3-F1 in #146 (verified by the forged-annotation drill, G4) |
| P2 | 9 | 8 fixed (#131, #132, #133, #135, #142); 1 accepted — SR-5-F2, owner `maintainers`, target = a second maintainer joining, mitigated by the `runtime-train` publish approval environment in #157 (§7) |
| P3 | 22 | 17 fixed; 3 accepted with owner `maintainers` and target v1.1 (SR-2-F8, SR-3-F2, SR-3-F5 — §6); 1 fixed post-review in v1.1 (SR-4-F5 — retention sweep); 1 listed open (SR-5-F3) |
| duplicate | 1 | SR-2-F6 (== SR-4-F1) — fixed in #133 |

**Result:** no open P0/P1. Every P2 is fixed or accepted: SR-5-F2's
single-actor merge→publish residual is accepted for a
single-maintainer project (§7), and from this release every train
publish waits on a recorded `runtime-train` approval — the trigger
that resolves the residual is a second maintainer joining.

## 5. Held controls

Controls the review attacked and could not break, with the tests that
pin them.

**Auth and session (boundary group 1)**

- OIDC code flow end to end — state+nonce+PKCE sealed into the
  `__Host-` login cookie (AES-256-GCM, purpose-bound AAD), constant-time
  state compare, sealed verifier at exchange, nonce from claims; in-TTL
  cookie replay gains nothing since the code is single-use at the IdP
  and bound to the sealed verifier. `auth_test.go`
  (`TestLogin_MissingCookie`, `TestCallbackRejectsUnknownState`,
  `TestCallbackRejectsStateReplay`, `TestLoginSendsPKCES256` and
  siblings), `loginstate/sealer_test.go`.
- Two-key login-state rotation; issuer/audience/algorithm verified via
  discovery. `sealer_test.go`, `auth_test.go`.
- `id_token` sealed at rest (purpose-bound AAD); open failure degrades
  logout, never fails it. `TestLogout_NoIDTokenFallsBackToClientID`.
- Post-logout URL safety — end-session URL built only from discovery +
  static config; no request input reaches it; `?error=` on callback not
  reflected. `TestLogout_NoOpenRedirect`,
  `TestNewAuthenticator_RejectsBadPostLogoutRedirect`,
  `TestLogout_UntrustedDiscoveredEndpointIs204`.
- CSRF derivation (S1) — token = HMAC of the raw session ID, never
  stored, constant-time compare; unforgeable cross-session; every
  mutating route mounted `RequireAuth → audited → RequireCSRF`, backed
  by the single-Origin allowlist (`RequireTrustedOrigin`) with
  Sec-Fetch-Site fallback. `TestCSRF*` (middleware_test.go),
  `TestCSRFChainForgedOriginRejected`, `TestOriginAllowlist`,
  `TestOriginMultipleHeadersRejected`, `TestAuditCoverage_SpecMatchesTable`.
- Session cookie attributes — `__Host-`, host-only, Secure, HttpOnly,
  SameSite=Lax, no flag weakens Secure in production.
  `TestLoginCookieAttributes`, `TestOIDCLoginFlowSucceeds`.
- Session store hardening — SHA-256-digest row keys, epoch bound at
  insert, idle + absolute expiry, lazy delete, fixation prevented on
  login. `TestSessionKeyIsDigest`, `TestSessionIdleAndAbsoluteExpiry`,
  `TestSessionFixationPrevented`, `TestSessionEpochRotation`.
- Fail-closed auth paths — store errors 503, unknown/expired 401.
  `TestRequireAuth_SessionStoreErrors`.
- Sign-out vs streams (S17) — delete-then-revoke ordering, deterministic
  lock order, ticket-lock serialization vs in-flight redeem, per-read
  re-validation of the bound session with on-the-spot revoke.
  `signout_test.go`, `lease_session_test.go`
  (`TestLeaseSession_RenewRevokesWhenSessionDeleted`,
  `TestLeaseSession_StaleEpochRevokes`,
  `TestLeaseSession_ForeignGatewayStillRevokes`,
  `TestLeaseSession_NullDigestLeaseExempt`),
  `TestLogout_RevokeSurvivesClientDisconnect`.
- Sign-out-everywhere (S19) — tenant+principal scoped, caller included,
  one transaction, whole-tx rollback on failure, both redeem
  interleavings and a `-race` multi-ticket inversion pinned on real row
  locks. `revokeall_test.go` (api, broker, gateway),
  `TestRevokePrincipalSessions_MultiTicketInversion`.
- Rate-limit keying — validated session/state digests only; forged
  values fall back to the client-IP bucket; per-IP ceiling on the
  callback; shared fixed-minute window is the exact aggregate bound;
  degraded-mode divided floor plus circuit breaker.
  `ratelimit_key_test.go`, `shared_test.go`,
  `tests/integration/rate_limit_pg_test.go`.
- Error paths and logs — stable error model, `?error=` not reflected,
  state/code never logged, actors pseudonymized, no headers/body/tokens
  in audit or logs. `TestAuditLogNeverContainsSecrets`,
  `TestLogsNeverContainRawSubject`, `TestRequiredGroupsDeniesNonMember`.
- Frontend boundary — no `/v1` proxy, pinned CSP/COOP/nosniff/HSTS,
  branding directory traversal and symlink containment.
  `build/frontend/main_test.go` (`TestSecurityHeaders`,
  `TestNoAPIProxy`, `TestBrandingDir_*`).
- Cross-cutting — tenant/owner always derived from the verified
  principal (never request input); request-detached work bounded
  (`WithoutCancel` + timeout). `tenant_scope_test.go`,
  `workspaces_handlers_test.go`.

**Session listener and gateway (boundary group 2)**

- Host classes and domain match — exactly one `ws-<id>` label,
  case-sensitive suffix, port rules, trailing dot and multi-label
  rejected; foreign host 421. `FuzzDomainMatch`, `FuzzHostClassify`,
  `hostbinding_test.go`.
- Listener isolation — `/v1/launch` only on workspace hosts, control
  paths only on control hosts, metrics nowhere, everything else 421.
  `hostbinding_test.go`, `proxy_test.go`,
  `TestEdgePolicyNeverOpensInternalPort`.
- Control surface — bearer via constant-time compare, disabled when
  unset (never fail-open), denials audited.
  `TestControlAudit_OperatorSurface`.
- Launch ticket lifecycle — SHA-256 stored, 60 s TTL, `FOR UPDATE`
  single-use consume, audience/tenant binding, host binding, origin
  policy (ADR 0004), limiter before redemption, POST only, `?ticket=`
  refused, 4 KiB body cap. `tickets_test.go`, `launch_test.go`,
  `launch_origin_test.go`, `launch_ratelimit_test.go`,
  `FuzzLaunchRedeem`, `hostbinding_test.go`.
- Cookie↔lease binding (S3) — digest bound at launch, foreign/dup
  denied, `LeaseBySession` through `liveLease`, singleflight per digest,
  panic-safe waiter release. `rehydrate_test.go`,
  `tests/integration/session_cookie_liveness_test.go`.
- Lease↔host binding — a cookie resolving to another workspace's lease
  is denied and audited on RAM hit and rehydrate.
  `hostbinding_test.go`.
- Stream fencing (S4) — per-replica admission plus cross-replica
  `stream_epoch` claim/fence; residual ≤1 renew-cycle two-stream window
  documented. `wsclose_test.go`, `owner_tab_test.go`, `signout_test.go`.
- WebSocket upgrade checks — strict RFC 7230 detection, Origin must
  equal the request host exactly, draining replicas refuse.
  `framing_test.go`.
- Upstream path allowlist — dot segments, encoded dots/slashes,
  separators and double-encodings rejected before the KasmVNC allowlist
  (SEC-20). `proxy_test.go`.
- Authorization overwrite resistance — a client credential can neither
  reach nor replace the broker-injected one in any spelling tried
  (plain, duplicate, mixed-case, `Proxy-Authorization`, trailer, H2
  pseudo-headers); since #142 the injection runs after the stdlib
  hop-by-hop strip. `TestProxy_StripsClientAuth`,
  `connheaders_test.go`.
- Forwarded chain (S18) — client `X-Forwarded-For`/`Forwarded`/
  `X-Real-Ip` stripped and rebuilt from the verified chain only; the
  whole forwarding family and vendor client-address headers stripped
  since #142. `forwarded_test.go`, `connheaders_test.go`.
- Upstream TLS pin — per-lease pinned CA pool and ServerName, TLS ≥1.2,
  never `InsecureSkipVerify`; upstream URL never client-derived (SSRF
  guard). `TestUpstreamTLS_RequiresPinnedCA`.
- Response policy (S7) — per-host `connect-src` CSP, `frame-ancestors`
  from configured portal origins or `'none'`, Permissions-Policy, CORP,
  OAC, nosniff, HSTS, COOP since #147; upstream copies of the policy
  headers plus `Set-Cookie`/`Refresh`/`Link` stripped. `proxy_test.go`,
  `hostbinding_test.go`, `coop_test.go`.
- Service-worker and fetch-metadata guards; iframe sandbox +
  Permissions-Policy delegation (portal half). `proxy_test.go`,
  `framing_test.go`, `build/frontend/main_test.go`.
- Lease fail-closed loop — renew every ~10 s; terminal errors kill;
  transient failures die inside `RevokeDeadline`; kill never touches a
  successor's slot. `postgres_outage_test.go`,
  `backend_restart_test.go`.
- Listener hardening — TLS ≥1.2 hot-reload, header-read/max-header/idle
  timeouts, zero read/write deadlines on hijacked streams.
  `listeners_test.go`.
- KasmVNC credentials — read per-resolve from the per-workspace Secret
  in the tenant's managed namespace, injected server-side only, never
  logged or serialized. `credentials_envtest_test.go`.

**Control plane (boundary group 3)**

- Internal listener :9443 — every route requires a verified peer cert
  (none → 401 before routing, TLS 1.3 minimum); role split between
  operator and gateway identities on every route; SPIFFE URI SAN must
  equal the CN when present. `server_test.go`
  (`TestNoClientCert_401`, `TestWrongClientCA_Rejected`,
  `TestOperatorEndpoints_GatewayForbidden`, `TestSPIFFESAN_*`).
- Lease↔gateway binding — renew/target/activity deny foreign gateway
  IDs. `leases_test.go`, `targets_internal_test.go`.
- Request hygiene — body cap, `DisallowUnknownFields`, identifier
  bounds, lease IDs hashed in logs (SEC-41). `TestNoSecretsInLogs`,
  `TestNoLeaseIDsInLogs`.
- :9443 not exposed — edge policy admits it only from operator pods in
  the release namespace. `TestEdgePolicyNeverOpensInternalPort`,
  `TestMetricsListenerIsolation`.
- mTLS rotation (S15) — listener cert hot-reload, client-CA pool re-read
  per handshake, a bad bundle never replaces a good pool, `opclient`
  re-verifies chain+hostname+EKU per handshake. `capool_test.go`,
  `mtls_reload_test.go`, `ca_reload_test.go`, `verify_internal_test.go`.
- RBAC least privilege — no ClusterRoleBindings; operator ClusterRole
  bound per managed namespace only; PSA `restricted` on managed
  namespaces; backend role has `workspaces` CRUD but no `pods create`
  and no `workspaces/status`; frontend SA automounts no token.
  `TestNoClusterRoleBindings`, `TestRoleBindingsOnlyInAllowedNamespaces`,
  `TestOperatorClusterRoleBoundPerManagedNamespace`,
  `TestOperatorWatchNamespacesExcludesRelease`,
  `TestHardeningManagedEnforcePrivilegedGated`.
- CRD/CEL surface — `Workspace.spec` carries no image/env/command/
  placement fields; `WorkspaceTemplate` fully immutable, digest-pinned
  image required, adapter rules enforced; server-keyed CR writes with
  request-id + workspace-uid replay checks. `snapshot_verify_test.go`,
  `k8sapplier_test.go`.
- Workspace-pod confinement — non-root uid 1000, drop ALL, no privilege
  escalation, `RuntimeDefault` seccomp with validated `localhost/`
  names (S38), AppArmor allowlist (S16), read-only rootfs on kasm,
  bounded emptyDirs, `automountServiceAccountToken=false`,
  per-workspace boundary NetworkPolicy reconciled against drift.
  `backend_test.go`, `apparmor_setting_test.go`,
  `seccomp_profile_test.go`, `backend_netpol_test.go`.
- kasm adapter (S11) — init image digest-pinned and operator-configured
  only; adapter delivered via read-only emptyDir; wrapper paths
  shadowed; managed policy mounted; credential neutralization; catalog
  gate enforces digest-pinned refs and an engine-freshness floor, and
  the chart `browserAllowlist` is bounded by the catalog.
  `backend_kasm_test.go`, `kasm_adapter_test.go`,
  `check-kasm-catalog.sh`, `scan-kasm-catalog.sh` (kasm-contract job).
- Deploy-time guards (S22, minus the SR-3-F2 residual) — `tinycdi.validate`
  fails the render on hazardous combinations: unelected multi-replica,
  `privileged` managed enforce, non-verifying DB TLS, hostPath extra
  volumes, weakening securityContext overrides, dev flags, adapter
  without digest, Browser+kasm off the allowlist, InternetOnly without
  cluster CIDRs, session-domain collisions, drain budgets. No admission
  webhooks exist (`webhookPort: -1`). `chart_guards_test.go`,
  `chart_hardening_test.go` (`TestHardeningDevGateBypasses`),
  `chart_edgepolicy_test.go`.
- Intent-drift surfacing (S24) — applied-intent fence plus the
  `intent-behind` marker, `ConditionIntentBehind`, one edge event,
  cleared on realign. `TestIntentBehindDrift`,
  `TestK8sApplierIntentDrift`, `TestProjectConditions_IntentBehind`.
- Finalizer/teardown — ordered steps, persisted checkpoint, bounded
  drain, fail-closed broker seam, revocation rows that fence covered
  generations. `finalizer_test.go`, `activity_e2e_test.go`.

**Data, tenancy and operations (boundary group 4)**

- Tenant identity from verified claims only; `tenant_id` +
  `owner_subject` predicates on every workspace read/write — a foreign
  ID is a 404, never a 403; `?scope=tenant` requires the verified
  tenant-admin group. `tenant_scope_test.go`, `auth_test.go`.
- Curated events — operator conditions projected through an allowlist;
  intents bounded to curated kinds; `IntentHistory` tenant-predicated.
  `statusview_test.go`, `events_test.go`.
- Principal directory — display-only, tenant-scoped, bounded, never
  authorizes. `tests/integration/principal_directory_test.go`.
- Admin surface — path tenant must equal the verified principal's
  tenant plus tenant-admin group; If-Match optimistic concurrency;
  denied admin attempts audited. `adminquota_test.go`,
  `adminuserlimit_test.go`.
- Per-principal limits — enforced inside the reservation transaction
  under the tenant-quota row lock; attach enforces the disk's owner;
  missing workspace row fails closed. `userlimit_test.go`
  (`TestUserLimit_ConcurrentLaunches`,
  `TestUserLimit_MissingWorkspaceRowFailsClosed`,
  `TestUserLimit_ReleasePending`), `retained_*_test.go`.
- Purge confirmation — caller-bound HMAC nonce, epoch-fenced,
  single-transition; replay owner-checked (SEC-21).
  `retained_*_test.go`, `tests/integration/sec_retainedref_test.go`.
- Idempotency keys — actor-scoped + tenant-keyed, op/body-hash mismatch
  conflicts. `workspaces_handlers_test.go`.
- Rate-limit correctness — shared window is atomic and exact, denied
  requests counted, uneven splits bounded, fail-open on outage,
  breaker-bounded recovery. `shared_test.go`,
  `tests/integration/rate_limit_pg_test.go`.
- Branding containment (S10) — `..` rejected, `EvalSymlinks` on root
  and target, regular files only, no listing. `main_test.go`
  (`TestBrandingDir_*`).
- Audit — exactly one dedicated event per mutating/admin route
  including denied, panic and error paths; actor/target unforgeable;
  sensitive keys redacted; sink failure never blocks.
  `auditroutes_test.go` (`TestAuditCoverage_SpecMatchesTable`,
  `TestAuditCoverage_MountedMuxEmitsEventPerRoute`), `audit_test.go`.
- Post-restore (S23) — dry-run default, advisory-lock exclusivity,
  refusal while backends serve, epoch rotation kills restored sessions,
  lease/ticket revoke, CR alignment, zero-writes failure modes.
  `postrestore_test.go`.
- Deterministic lock order — tickets → sessions → leases, ascending
  within each, `-race` runs prove no deadlock. `revokeall_test.go`.
- Secrets handling — DSN excluded from errors, non-verifying sslmode
  refused without the dev flag, OIDC secret and control token
  env/file-only, per-workspace creds read by binding UID, lease IDs
  log-redacted. `config_test.go`, `credentials_envtest_test.go`.
- SQL — every variable bound; the only concatenations are placeholder
  indices and validated constants across the store. `sessions_test.go`,
  store suite.

**Supply chain and CI (boundary group 5)**

- No privileged-trigger surface — zero `pull_request_target`,
  `workflow_run`, `issue_comment`, `workflow_call`; the PR workflow's
  token is read-only with no secrets or OIDC and the workflow file comes
  from the base branch.
- No untrusted expressions in `run:` blocks — event data bound via
  `env:`, version strings regex-anchored.
  `workflow-expressions.test.sh`.
- Per-job least-privilege `GITHUB_TOKEN` — top-level `contents: read`;
  signing jobs alone hold `id-token: write`.
- Every action pinned by full SHA; BuildKit image digest-pinned;
  all downloaded tools sha256-verified (helm, kind, trivy, syft,
  cosign, actionlint, PyPI wheels via `--require-hashes`, the KasmVNC
  deb). `script-checkout.test.sh`, `release-topology.test.sh`.
- Push-by-digest + sign-before-tag — SBOM attested on the digest before
  any tag promotion (SEC-04/SUPR-13). `release-topology.test.sh`.
- Exact keyless cosign identities — certificate-identity and issuer
  pinned in docs and verified live against published releases; chart
  signed in OCI. `provenance.md`, `stamp-image-digests.sh`.
- Protected release path — `v*` tag ruleset (admin-only bypass),
  protected `release` environment, required strict checks +
  enforce-admins on `main`, refuse-overwrite on existing tags/releases.
  `setup-repo-protection.test.sh`, `repo-protection.test.sh`.
- trivy gate — CRITICAL/HIGH with `--ignore-unfixed` and an empty
  `--config`; `.trivyignore` entries need reason + approver + ≤30-day
  expiry, enforced in CI and inside the scan jobs.
  `check-trivyignore.sh`, `trivyignore.test.sh`.
- SBOMs — syft-generated per image from the pushed digest, attested,
  shipped as release assets; chart and binary SBOMs added in #135.
  `supply-chain-hardening.test.sh`, `publish-inputs.test.sh`.
- Image hardening — every `FROM`/base `ARG` digest-pinned, browser
  engines and CVE'd libraries apt-pinned with a freshness gate, setuid
  strip, non-root final stages. `runtime-freshness.yml`,
  `check-browser-freshness.sh`.
- Artifact handoff — exact-name downloads only, folder/file allowlist,
  strict ref format, publish consumes nothing unvalidated.
  `publish-inputs.test.sh`.
- Meta-guards — `.github/tests/*.test.sh` run in the required
  `workflow-policy` job; actionlint + yamllint + zizmor required;
  `shell: bash` pipefail workflow-wide. `pipefail.test.sh`.
- Repo hygiene — secret scanning + push protection, private
  vulnerability reporting, dependabot, no repo secrets configured.

## 6. Residual risks and accepted items

Accepted items carry an owner and a target version per the exit
criterion; open items are listed.

- **SR-3-F2 — `extraEnv` vs the chart's render guards — ACCEPTED**,
  owner `maintainers`, target **v1.1**. `denyExtraArgs` refuses the
  dangerous flags unless `dev.enabled`, but `<component>.extraEnv` is
  rendered verbatim (after the managed env block, so a duplicate name
  wins) and can carry the same flags' environment equivalents —
  `TCDI_DEV_INSECURE_DB`, `TCDI_DEV_ALLOW_NO_BROKER`,
  `TCDI_METRICS_LISTEN`, `TCDI_KASM_ADAPTER_IMAGE`, `PGSSLMODE`,
  `TCDI_OPERATOR_CN`, `TCDI_WATCH_NAMESPACES`, `TCDI_REQUIRED_GROUPS`.
  Admin-tier precondition (values control); an env-name deny-list is
  the planned fix.
- **SR-3-F5 — per-certificate revocation on the internal mTLS hop —
  ACCEPTED**, owner `maintainers`, target **v1.1**. There is no
  CRL/OCSP/serial blocklist: removing trust in one client certificate
  means rotating the client CA bundle, which drops every client.
  Short-lived certs via cert-manager remain the mitigation — rotation
  is the revocation mechanism.
- **SR-2-F8 — `ClusterOnly` egress posture — ACCEPTED** as documented
  posture. A `ClusterOnly` workspace may reach any pod that does not
  restrict its own ingress; confinement rests on destination-side
  policies. The opt-in narrowing
  (`runtime.networkProfiles.clusterOnly.egress*`, #148) confines egress
  to selector-matched namespaces/pods and optional ports; default
  unchanged.
- **S14 — per-prefix rate budgets — ACCEPTED** (by design). IPv6 keys
  fold to /64 and mapped spellings unify with the native v4 key (#132),
  but a client holding several /64s or several egress addresses still
  gets a budget per prefix — per-prefix limiting as designed;
  authenticated routes key by session. The forwarded-key derivation
  additionally assumes every hop in front is in `-trusted-proxies`: a
  pass-through trusted hop would hand the key to client bytes
  (deployment-conditional — the chart README documents the invariant).
- **KasmVNC upstream bound — measured.** A burst of 100 simultaneous
  reconnects to one desktop saw 65 succeed and 35 answered `502` **by
  the runtime** (upstream connection resets) — not by the gateway and
  not by the limiter. This is one measurement, not a precise cap; see
  `docs/runbooks/capacity.md`.
- **One-time upgrade hold, v0.5.0 → v0.5.1.** A workspace whose pod
  predates `status.templateSnapshot` and whose recorded revision is
  unprovable holds `Degraded`/`TemplateRevisionGone` (pod untouched)
  until a stop/start re-snapshots; held workspaces keep `Ready` while
  the pod serves and stay under the stricter of template/default caps
  (#152, verified by the rc.2 held-cap legs N1/N2 and the rollback
  drill G10). See `docs/releases/v0.5.1.md`.
- **Watch item.** On one rc.2 rollout a desktop stream did not re-land
  within a 30 s observation window while its lease stayed live
  (re-landed in ~1 s on a controlled re-run — a marginal retry-window
  edge, not reproduced). Flagged for v1.0: watch for reports of
  mid-rollout stream loss.
- **SR-4-F5 — non-concurrent `CREATE INDEX` inside the migration
  transaction — FIXED** (v1.1). Migrations 020/023 take a SHARE
  lock on append-mostly `connection_lease`/`launch_ticket`
  tables; on a long-lived deployment the build can stall writes while
  replicas roll. Closed by the retention sweep: the leader replica now
  deletes terminal rows older than `-terminal-retention-days` (default
  30, `0` disables) in bounded hourly batches
  (`internal/store/retention.go`, `internal/backend/retention.go`), so
  table size — and the cost of any future index build — no longer grows
  with deployment age. The authoring rule for the next large-table index
  (documented `CREATE INDEX CONCURRENTLY IF NOT EXISTS` runbook step
  before the upgrade) is codified in `internal/store/migrations/README.md`
  and `docs/development.md`.
- **SR-5-F3 — gate-failed digests remain pullable-by-digest — OPEN**
  (listed, P3). Build jobs push by digest before the scan gate; an
  unsigned, never-promoted digest stays addressable. Consumers pinning
  an arbitrary digest (including via a forged manifest) could deploy
  unscanned content — mitigated in practice by #135's signed manifest
  and per-digest cosign verification documented as the consumption
  contract.
- **Audit durability.** The audit stream is stdout JSONL behind
  `GuardedSink` — emission and shape are guaranteed; delivery,
  retention and tamper evidence are the operator's log pipeline
  (threat-model S21; unchanged from v0.5.0).
- **DNS egress.** DNS resolution is allowed for every network profile
  including `Isolated` — a unidirectional exfil channel by design;
  noted for the `Isolated` wording in operator-facing docs.

## 7. Governance items — decisions of record

These are repository-organization decisions, not code changes. Each was
decided on 2026-10-11, decided by the maintainer.

- **SR-5-F2 — single-actor merge→publish path — ACCEPTED residual**
  (decided 2026-10-11 by the maintainer). `main` requires zero
  approving reviews and the `release` environment's only reviewer is
  the same account that pushes the tag, with self-review allowed — so
  one account can take a commit from merge to a signed, attested,
  tenant-executed image with no second actor. For a single-maintainer
  project that residual is accepted, owner `maintainers`, with this
  mitigation in force: the runtime train's `publish` job runs under the
  `runtime-train` environment with a required reviewer (#157), so
  every train publish carries a durable approval record — approve only
  once the publish job itself is waiting (pending deployment), never
  at run start. **Trigger — when a second maintainer joins:** `main`
  gets `required_approving_review_count` 1 + `require_last_push_approval`,
  and `prevent_self_review` goes `true` on the `release` and
  `runtime-train` environments.
- **Release-environment self-review setting — decided** (2026-10-11,
  by the maintainer). `release`'s `prevent_self_review` stays `false`
  until a second reviewer exists — enabling it earlier would let the
  sole maintainer approve their own publish *less* safely, not more.
  Same trigger as SR-5-F2.
- **SR-5-F5 — repository-level SHA-pinning enforcement — FIXED**
  (decided 2026-10-11 by the maintainer). `sha_pinning_required=true`
  is codified in `.github/scripts/setup-repo-protection.sh` (applied by
  an admin after merge; pinned by `setup-repo-protection.test.sh`).
  Every action was already SHA-pinned with zizmor/actionlint in a
  required job — the platform now enforces it too.
- **External human review before GA — OPEN, deferred to the 1.x line**
  (decided 2026-10-11 by the maintainer; owner `maintainers`). The
  v0.1→v0.5 delta had no external review (standing item S13); this
  internal review narrows that gap but is not an independent audit.
  v1.0.0 ships with that statement; an independent audit is planned on
  the 1.x line.

## 8. Sign-off

This was an **internal review** executed by the project's own workers
against the frozen v0.5.0 tree — **not an independent audit**. It
carries the biases of any self-review: the same assumptions that shaped
the design shaped the review, and the supply-chain items in §7 exist
precisely because one project's checks can only go so far. External
human review before general availability remains open — deferred to
the 1.x line by the maintainer's decision of 2026-10-11 (§7).

— TinyCDI maintainers, 2026-10-11
