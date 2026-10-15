# Security test inventory

Companion to [`threat-model.md`](threat-model.md): the automated evidence
that exists for the v1.0 security review. Lists security-relevant tests
and CI scanners **with paths** — presence in CI does not imply the
reviewer should skip reading the code (green tests are not a substitute
for review). Each entry names the `TM-NN` register row(s) of
[`threat-model.md`](threat-model.md) §6 it is evidence for. Older
tracking tags kept in code comments (`SEC-…`, `SR-…`, `FX-R…`, `RL-…`,
`KASM-…`, `SUPR-…`) appear beside the register rows they map onto;
§10 explains the families.

## 1. How the tests run

| Suite | Command | Where it runs |
|---|---|---|
| Unit + envtest (race) | `go vet ./...` + `go test -race ./...` | `ci.yml` job `go vet + test -race (envtest)` |
| Integration (Postgres + envtest) | `go test -tags=integration ./tests/integration` | `ci.yml` job `integration` (postgres:18.0 service); a cluster-free subset runs per PR |
| Chart | `helm lint --strict`, `promtool`, `go test ./deploy/helm/...` | `ci.yml` job `chart` |
| Frontend server | `go test ./build/frontend` | folded into the Go test run |
| Portal web | `npm ci`, `tsc`, `vitest`, `vite build`, `npm run test:e2e` (Playwright against the real Go binaries) | `ci.yml` job `portal ui` |
| Soak harness | `npm ci`, `tsc`, vitest + drills dry-run | `ci.yml` job `soak harness`; live soak out of band |
| Go fuzz | `go test -fuzz=<target>` | `ci.yml` job `go fuzz`: 30 s per target on PRs touching the fuzzed packages, 10 m per target on the weekly schedule; crashers uploaded as artifacts |
| Quickstart e2e | `hack/quickstart/up.sh` on kind + Playwright portal smoke + the cold-connect gate (20 cold boots, p95 < 20 s, `hack/quickstart/smoke/cold-connect.spec.ts`) | `ci.yml` job `quickstart` |
| Partitioned-cookie e2e | quickstart + `values-partitioned.yaml` overlay + `hack/quickstart/smoke/partitioned.spec.ts` | `ci.yml` job `partitioned` (gated + weekly) |

## 2. Auth, session and API surface

- `internal/api/auth_test.go` — OIDC login/callback, the AEAD-sealed login
  cookie and PKCE (TM-19), `requiredGroups` gating (SEC-I3, TM-11),
  login cookie attributes (`TestLoginCookieAttributes`, TM-01), session
  fixation (`TestSessionFixationPrevented`, TM-19); also pins the
  dead-knob guard: `TestAuthConfig_NoDeadSessionKnobs` fails if
  `AuthConfig` regrows a dead session knob.
- `internal/api/me_test.go` — `TestLogin_OnlyHostPrefixedCookies` pins
  that the portal sets only `__Host-`-prefixed cookies (TM-01);
  `TestInputActivity_*` pins that broker-reported desktop input slides
  only the bound session's idle window, cannot revive an
  already-idle-dead session and leaves other principals untouched
  (TM-03).
- `internal/api/middleware_test.go` — `RequireCSRF` accept/reject matrix,
  `RequireTrustedOrigin` (single-Origin allowlist, `Sec-Fetch-Site`
  fallback), request-ID/audit middleware (TM-02).
- `internal/api/loginstate/sealer_test.go` — AEAD seal/open, expiry,
  wrong-purpose AAD rejection, key rotation (first seals, all open)
  (TM-19).
- `internal/api/idtoken.go` exercised via `auth_test.go` /
  `logout_test.go` and `tests/integration/session_idtoken_test.go` —
  sealed `id_token` at rest; an unopenable blob degrades logout, never
  fails it (`TestPGSessionStore_IDTokenAbsentIsNull`, TM-23).
- `internal/api/ratelimit_test.go`, `ratelimit_key_test.go` — limiter
  behaviour and the FX-R30 key resolvers (TM-06): valid session →
  session-digest bucket; valid OIDC state → state-digest bucket; forged
  values fall back to the client-IP bucket; per-IP ceiling on the
  callback (`TestCallback_ValidatedStateCeiling`).
- `internal/api/connection_status_test.go` — passive auth path: polling
  must not extend idle (TM-03).
- `internal/api/idlepoll_test.go` — the idle-window contract (TM-03):
  every cookie-authenticated GET is passive, so timer-driven portal
  polls can never hold a session open (`TestPolledReads_DoNotSlideIdleWindow`,
  `TestMeRead_DoesNotSlideIdleWindow`); mutations still slide
  (`TestMutation_StillSlidesIdleWindow`); the explicit activity beat
  `POST /v1/session:touch` slides only after the CSRF check
  (`TestSessionTouch_SlidesIdleWindow`).
- `internal/api/inputhook_throttle_test.go` — broker-reported desktop
  input is throttled before it touches the session store
  (`TestInputHookThrottle_Bounded`, TM-03).
- `internal/api/session_probe_test.go` — the unauthenticated session
  probe (TM-03).
- `internal/api/logout_test.go` — sign-out destroys the session and
  expires cookies; `endSessionURL` built only from discovery+config (no
  open redirect, TM-20); `TestLogout_RevokesSessionBoundMaterial` /
  `TestLogout_RevokeFailureStillSignsOut` — sign-out revokes
  session-bound leases/tickets at the store, audits `session.revoke`,
  and a store failure never blocks the sign-out (TM-07). Fail-closed
  sign-out: `TestLogout_StoreDeleteFailureReturns503` — a session-store
  `Delete` error answers retryable 503 + `Retry-After` with nothing torn
  down (no cookie expiry, no lease/ticket revoke). Post-logout redirect
  is https-only off-loopback:
  `TestNewAuthenticator_RejectsBadPostLogoutRedirect`,
  `TestNewAuthenticator_AllowsLoopbackPostLogoutRedirect` (TM-20).
- `internal/api/revokeall_test.go` — sign-out-everywhere (ADR 0007,
  TM-08): `POST /v1/me/sessions:revoke-all` hands the caller's (tenant,
  issuer, subject) to the principal revoker — the caller's own session
  included — audits `session.revoke_all` with per-kind counts, expires
  the cookie, returns the same `endSessionUrl` as logout, and answers
  500 leaving every session intact when the store transaction fails
  (503 when unwired); `TestRevokeAll_StoreFailureThenRetrySignsOut`
  pins that a retry after a store failure completes the sign-out.
- `internal/api/tenant_scope_test.go`, `events_test.go`,
  `statusview_test.go`, `owner_test.go`, `principal.go` — tenant
  scoping, curated events, principal directory fallbacks (TM-04);
  `TestProjectConditions_IntentBehind` covers the intent-drift condition
  projection (TM-22).
- `internal/api/workspaces_handlers_test.go` — retainedDataRef owner
  bypass (SEC-01) and malformed pageToken (SEC-I7) regression coverage
  (TM-04).
- `internal/api/connections_test.go` — connection creation decodes via
  the shared `decodeJSON` helper:
  `TestCreateConnection_InvalidBodyGenericMessage` (the decoder detail
  is never echoed) and `TestCreateConnection_TrailingJSONRejected`
  (TM-10).
- `internal/api/adminquota_test.go`, `quota_test.go`,
  `internal/backend/tenantquota_test.go` — tenant quota enforcement
  paths and their tenant-admin authz (TM-09, TM-11).
- `internal/api/adminuserlimit_test.go`,
  `internal/provisioning/userlimit_test.go` — per-principal running
  limits (TM-09): tenant-admin authz and cross-tenant denial,
  set/clear/default writes with audit (`admin.user_limit.*`, incl.
  denied attempts), `UserLimitReached` → 409 mapping, enforcement
  inside the reservation tx (`TestUserLimit_ConcurrentLaunches`,
  `TestUserLimit_ReleasePending`, `TestUserLimit_RetainDiskOnlyHold`,
  `TestUserLimit_NoRowsUnlimited`,
  `TestUserLimit_MissingWorkspaceRowFailsClosed`), caller-facing
  `userLimits` view on `GET /v1/quota` (`TestQuota_UserLimitFields`).
- `internal/api/auditroutes_test.go` — dedicated audit events on every
  mutating + `/v1/admin/` route (TM-50):
  `TestAuditCoverage_SpecMatchesTable` pins the openapi↔table pairing,
  `TestAuditCoverage_MountedMuxEmitsEventPerRoute` replays the mounted
  mux and requires exactly one domain event per mutating/admin spec
  route — a plainly-mounted route emits none and fails, denied admin
  attempts record `denied`+actor, `role=tenant-admin` marks elevated
  use; `TestAudit_SinkFailureStillSucceeds` /
  `TestAudit_HandlerPanicStillEmits` pin the sink-failure and panic
  paths.
- `internal/api/auditstore_test.go` — the durable audit sink (ADR 0010,
  TM-50): `TestAudited_PersistFailureFailsClosed` and
  `TestAudited_NoTxMutationPersistFailureFailsClosed` pin that an
  audited write answers 500 when the audit row cannot be persisted —
  including a write with no DB transaction whose change already applied;
  `TestAudited_TxHookPersistsOnceThenAmends`,
  `TestAudited_PanicAfterCommitAmendsFailure`,
  `TestAudited_AmendFailureIsReported`,
  `TestAudited_PanicPersistFailureIsReported` cover the tx-hook
  persist/amend and panic paths; `TestAudited_TouchStaysStdoutOnly`
  pins `session.touch` to the stdout stream;
  `TestDurableAuditSink_ReportsPersistError`,
  `TestAudited_PlainSinkDoesNotBuffer`,
  `TestLateAuditSink_ForwardsPersistence`,
  `TestAudited_NoTxPathPersistsAfterHandler` and `TestAuditTargetKind`
  cover the sink plumbing.
- `internal/observability/audit_test.go` — `JSONSink` schema, `ActorRef`
  pseudonymisation, sensitive-key redaction, and `GuardedSink` failure
  signalling (`TestGuardedSink_ReportsFailureNeverPropagates`, TM-50);
  `internal/observability/metrics_test.go` pins the audit-write-error
  metric label bounds (`TestIncAuditWriteError`).
- `internal/api/openapi_contract_test.go` — public API contract vs
  `openapi.yaml`, incl. `TestClipboardPolicyDirectionSemantics`
  (TM-36).

The v1.0 API surface added in the W0/W1 waves:

- `internal/api/actions_test.go` — the `:verb` action router (TM-16,
  TM-17): `TestActionRouter_DispatchesVerb` routes `:start`-style verbs
  to their handlers; `TestActionRouter_UnknownVerbAndColonInID` refuses
  unknown verbs and colons inside ids.
- `internal/api/etag_test.go` — the strong ETag + `If-Match`
  precondition the admin mutations rely on (TM-17):
  `TestETagOf_StrongQuotedStable`, `TestCheckIfMatch_Matrix` (428/412).
- `internal/api/page_test.go` — list pagination (TM-04):
  `TestParsePage_DefaultsAndBounds` (`limit` default 50, max 500),
  `TestCursor_RoundTrip`, `TestCursor_RejectsSortMismatchAndTamper`.
- `internal/api/templates_test.go` — `TestTemplatesList_Paged` pages the
  template catalog (TM-04).
- `internal/api/workspaces_test.go` — `TestWorkspacesList_PagingStableUnderInsert`,
  `TestWorkspacesList_SortDescCreatedAt` and
  `TestWorkspaceActions_VerbPaths` pin the owner-scoped list ordering
  and verb paths (TM-04).
- `internal/api/statusview_settled_test.go` —
  `TestK8sStatusView_IntentSettledAndGeneration` pins that the projected
  status carries the intent-settled flag and the generation the
  recovery paths fence on (TM-16, TM-22).
- `internal/api/adminsessions_test.go` — the admin session view (TM-14):
  `TestAdminSessions_ListHidesLeaseIDs` and
  `TestAdminSessions_PagingCursorHasNoLeaseID` pin that the list and
  its cursor expose `leaseRef` hashes, never a lease id;
  `TestAdminSessions_ListRefusals` and
  `TestAdminSessions_RevokeAllRefusals` pin the authz and input
  refusals; `TestAdminSessions_RevokeAllTargetsTheNamedUser` scopes the
  revoke to the named user; `TestAdminSessions_RevokeFailureIs500`
  answers 500 on a store failure; `TestAudit_AdminSessionRevoke` pins
  the audit event (TM-50).
- `internal/api/adminaudit_test.go` — the tenant-admin audit read
  (TM-12): `TestAdminAudit_ListPagesAndFilters`,
  `TestAdminAudit_RejectsBadInput`, `TestAdminAudit_Authz` — the read is
  itself audited.
- `internal/api/adminsessionpolicyapply_test.go` —
  `POST /v1/admin/session-policy:apply` (TM-17):
  `TestApplySessionPolicy_Preconditions` pins the `If-Match` ceiling
  version; `TestApplySessionPolicy_DryRunListsLooserOnly`,
  `TestApplySessionPolicy_ConfirmSkipsLiveUnlessIncluded`,
  `TestApplySessionPolicy_MovedGeneration`,
  `TestApplySessionPolicy_UnsettledIntentIsNotRestarted` and
  `TestApplySessionPolicy_TruncatesAtCap` pin the apply semantics;
  `TestApplySessionPolicy_AuthzAndTenantIsolation` the tenant boundary.
- `internal/api/adminoverview_test.go` — the admin overview (TM-15):
  `TestAdminOverview_AggregatesAndCaches`,
  `TestAdminOverview_CacheIsPerTenant`,
  `TestAdminOverview_NonAdminRefused`.
- `internal/api/adminusers_test.go` — the user directory (TM-13):
  `TestAdminUsers_ListIncludesUsersWithoutWorkspacesOrLimits`,
  `TestAdminUsers_DetailNotFoundAndBadRef` (the detail route is
  addressed by an owner reference),
  `TestAdminUsers_CursorCarriesSortKey`.
- `internal/api/filetransfers_test.go` — minting transfer grants
  (TM-18): `TestMintFileTransfer_RejectsBadNames` (single file names
  only), `TestMintFileTransfer_PolicyOwnerAndReadiness` (owner only, on
  a `Ready` workspace, within the resolved policy),
  `TestMintFileTransfer_AuditsMintAndRefusal`.
- `internal/api/recovery_test.go` — owner `:start` on a `Failed`
  workspace (TM-16):
  `TestStart_FailedRecoverableRestartsWithObservedGeneration` restarts
  at a new generation fenced on the observed one;
  `TestStart_FailedUnrecoverableIs409WithReason`;
  `TestStart_StaleOrUnsettledStatusIsAPlainStart` and
  `TestStart_StoppedFailedWorkspaceIsAPlainStart` keep the plain-start
  behaviour; `TestStart_RestartNotRunningIs409`;
  `TestStart_ForeignFailedWorkspaceIs404BeforeStatus` pins that the
  owner check runs before the CR state is read.

Portal web (vitest under `web/tests/unit/`, Playwright under
`web/tests-portal/`; both run in the `ci.yml` job `portal ui`):

- `web/tests/unit/admin/audit.test.tsx` — the audit page lists
  newest-first, shows the denied outcome and sends the action filter
  (TM-12).
- `web/tests/unit/admin/overview.test.tsx` — the overview cards show
  live sessions, boot p95, the stuck-deletion warning and
  running-per-template (TM-15).
- `web/tests/unit/session/coldConnect.test.ts` — `streamConfirmed`
  counts only an open stream on a live lease; `nextColdStep` backs off
  1 s, 2 s, 4 s and gives up inside the confirm budget (TM-24).
- `web/tests/unit/session/filetransfer.test.tsx` — the transfer UI
  renders nothing on a `Disabled` policy, offers only the allowed
  directions and refuses an over-limit file before minting (TM-18,
  TM-30).
- `web/tests/unit/session/transfer.test.ts` — `validTransferName`
  accepts single file names only; `checkedContentUrl` accepts only the
  workspace's own session origin and route; content requests PUT with
  credentials and surface the gateway error code (TM-30, TM-31).
- `web/tests/unit/api/errors.test.ts` — the portal reads
  `body.error.code` from the error envelope (`PRECONDITION_REQUIRED`
  included) and never mistakes a flat body for a code (TM-17).
- `web/tests/unit/design/useServerPage.test.tsx` — keyset pagination:
  `nextCursor` appends the next page, a page arriving after `reset` is
  dropped, `refresh` re-reads the loaded pages (TM-04).
- `web/tests-portal/session-transfer.spec.ts` — upload then download
  through the session host against the real backend; a `Disabled`
  policy hides the controls; an over-limit file is refused before
  anything is minted; a refused direction is not offered (TM-30,
  TM-31).

## 3. Session gateway (listener :8444)

- `internal/gateway/launch_test.go` — ticket redemption contract
  (TM-24, TM-25): body-only ticket (`?ticket=` → 403), one-use, expired
  still consumed, takeover, cookie attributes; `TestLaunch_RequiresOwnerTab`
  pins that a launch without a valid `tcdi_tab` answers
  `400 owner_tab_required`.
- `internal/gateway/launch_origin_test.go` — ADR 0004 Origin allowlist
  (TM-02): cross-site portal origin, same-site, session origin,
  unknown/null Origin rejected without consuming the ticket,
  `Sec-Fetch-Site` agreement, no-browser-headers case, WebSocket
  upgrade origin.
- `internal/gateway/launch_ratelimit_test.go` — a rate-limited launch
  does not consume the ticket; session-keyed buckets behind NAT
  (FX-R30, TM-06); limiter disabled by default.
- `internal/gateway/hostbinding_test.go` — launch on own vs other host,
  cookie replayed on a sibling workspace host (host_mismatch), two
  parallel workspaces, unknown host → 421, control surface only on
  control hosts, pinned session headers, upstream-cannot-override,
  partitioned cookie mode, per-host `connect-src` CSP, KasmVNC probe
  allowances (TM-01, TM-25, TM-26).
- `internal/gateway/proxy_test.go` — response policy pinning (SEC-07,
  TM-01), upstream path canonicalization + allowlist (SEC-20, TM-31),
  control-surface bearer auth and revoke isolation (SEC-1/2, TM-26),
  strict upgrade detection (SEC-3, TM-24), lease/cookie plumbing;
  `TestProxy_StripsClientAuth` pins the broker-injected
  `Authorization` overwrite (TM-33, the KasmVNC credential
  brute-force control) and `TestControlAudit_OperatorSurface` the
  control-surface audit events incl. bearer denials (TM-50).
- `internal/gateway/rehydrate_test.go` — digest → lease rehydration,
  singleflight per digest, expired/revoked rejection (TM-23);
  `TestRehydrate_NilDirectorySingleProcess` pins the no-directory
  fast path (TM-23, TM-27).
- `internal/gateway/lookup_limit_test.go`,
  `lookup_limit_config_test.go` — the unknown-cookie session-directory
  lookup bound (TM-27): spray → bounded lookups + 429/`Retry-After`,
  known/rehydrated/cookie-less requests never limited, a 121-cookie
  concurrent valid-session storm at defaults admits exactly 120 and
  refuses the overflow with `Retry-After` (valid-but-uncached cookies
  pay the pre-lookup gate), injected bucket + disable path, refusal
  audits `session.lookup`.
- `internal/gateway/unattached_test.go` — ghost-session TTL (TM-28):
  redeem-then-abandon reaped with a lease revoke, attached sessions
  survive, a lease in use on a sibling replica is never revoked, reap
  proceeds on revoke failure.
- `internal/gateway/unattached_race_test.go` — attach/reap
  serialization (TM-28): an attach concurrent with the reap's
  in-flight revoke loses deterministically; the attach-vs-tick hammer
  asserts no end state ever shows a live session on a revoked lease.
- `internal/gateway/coop_test.go` — `Cross-Origin-Opener-Policy:
  same-origin` pinned on every session-listener response and
  upstream-supplied COOP stripped (TM-29); frame-ancestors unchanged.
- `internal/gateway/authorization.go` fencing covered by
  `wsclose_test.go` (orderly WS close), `owner_tab_test.go` (owner-tab
  takeover, `tcdi_tab` enforcement), `activity_test.go` /
  `activity_internal_test.go` (TM-24).
- `internal/gateway/streamcap_test.go`, `streamcap_internal_test.go`
  — the per-workspace per-replica live-upstream cap (TM-35): exact
  bound, mixed HTTP and WebSocket accounting, slot release on close, a
  concurrent storm without leaks, refusal before upgrade admission.
- `internal/gateway/connheaders_test.go` — header-injection ordering
  (TM-32): client `Connection:` token lists can never strip the
  broker-injected headers
  (`TestProxy_ConnectionTokensCannotStripInjectedHeaders`,
  `TestProxy_UpgradeConnectionTokensCannotStripInjected`), the whole
  client-supplied forwarding family plus vendor client-address headers
  is deleted before the verified rebuild
  (`TestProxy_ClientForwardedFamilyStripped`,
  `TestProxy_VendorClientAddressHeadersStripped`).
- `internal/gateway/upstreamtimeout_test.go` — the session upstream
  transport bounds a hostile/stalled runtime
  (`TestProxy_UpstreamResponseHeaderTimeout`); also pins the
  connection-token accounting — dropped tokens count on
  `tinycdi_gateway_connection_tokens_dropped_total`
  (`TestProxy_ConnectionTokensDroppedMetric`) with a bound on the
  debug-log rate (`TestProxy_ConnectionTokenLogRateLimited`)
  (TM-32, TM-35).
- `internal/gateway/forwarded_test.go` — client XFF never reaches the
  runtime; XFF honored only from trusted-proxy CIDRs (FX-R26, TM-32);
  `TestProxy_IPv6ForwardsExactAddrKeyFolds` pins that the forwarded
  rebuild carries the client's exact IPv6 address while the limiter
  key folds to /64; `TestClientAddr_NonIPSelectedEntryFallsBackToPeer`
  pins that a non-IP selected claim collapses to the socket peer on
  the forwarded path too.
- `internal/gateway/framing_test.go`, `framereload_test.go` — iframe
  embedding policy, `frame-ancestors`, frame reload behaviour (TM-01).
- `internal/gateway/transfer_test.go` — the `/xfer/` transfer leg
  (TM-30): `TestTransfer_UploadRoundTrip` and
  `TestTransfer_DownloadRoundTrip` pin the round trips after a broker
  claim; `TestTransfer_UploadOverCapRefused` and
  `TestTransfer_DownloadOverCapAborts` the byte cap;
  `TestTransfer_Refusals` and `TestTransfer_BrokerRefusalsMapped` the
  refusal surface; `TestTransfer_DesktopProxyStillRefusesXfer` pins
  that the desktop proxy path still refuses `/xfer/` (TM-31).
- `internal/gateway/dial_test.go` — the upstream dial retry that makes
  the restart-to-Ready path honest (TM-16):
  `TestProxy_DialRetriesUntilUpstreamListens` pins that a refused
  first dial racing the Service route is retried inside
  `UpstreamDialBudget`; `TestProxy_DialBudgetExhaustedIs502` pins the
  bounded failure.
- `internal/gateway/e2e_test.go` — end-to-end session flow in-process
  (TM-24); `TestWS_RevokeClosesStream_EndToEnd` also covers TM-07
  propagation.
- `internal/gateway/signout_test.go` — TM-07 propagation: a
  store-level revoke ends a live stream on a sibling replica within
  one renew cycle, and the workspace cookie replays to 401 on any
  replica.
- `internal/gateway/revokeall_test.go` — TM-08 propagation: a
  principal-scoped revoke ends every revoked session's stream within
  one renew cycle on the replica holding it, and each session's
  cookie replays to 401 — including on a replica that never saw
  either session.

### Fuzz targets (stdlib `testing.F`, run by the `go fuzz` CI job)

- `internal/api/fuzz_callback_test.go` — `FuzzCallbackParsing` (TM-19):
  OIDC callback query params (state/code/error), raw Cookie header →
  AEAD login-state open (test keys) and the code-exchange boundary
  against the in-process issuer; asserts exact rejection order and
  determinism, and that the callback rate-limit key is minted only for
  a validated state (TM-06).
- `internal/gateway/fuzz_test.go` — `FuzzHostClassify` (TM-25, TM-26):
  arbitrary Host headers (ports, IPv6 literals, IDNA/punycode,
  trailing dots, case) × request paths through `ServeHTTP`; asserts
  the host-class contract (session host / control host / foreign →
  421) is total and deterministic.
- `internal/gateway/fuzz_test.go` — `FuzzLaunchRedeem` (TM-24, TM-25):
  arbitrary launch methods, query strings and form bodies through
  ticket redemption on an in-memory digest store (tickets keyed by
  SHA-256 like `launch_ticket.ticket_hash`); asserts the bounded
  status set, single use, host binding, and that a 303 implies a
  well-formed cookie + clean redirect with no ticket leakage.
- `internal/ratelimit/fuzz_test.go` — `FuzzClientKey` (TM-06, TM-32):
  arbitrary RemoteAddr, X-Forwarded-For lines and `-trusted-proxies`
  CIDR lists; asserts the right-most-untrusted-entry contract end to
  end plus deterministic, canonical-CIDR-only proxy parsing.
- `internal/sessionhost/fuzz_test.go` — `FuzzDomainMatch` (TM-25):
  arbitrary session-domain config strings and Host headers through
  `ParseDomain`, `Match`, `Label`/`WorkspaceID`; asserts determinism
  and accept-implies-well-formed round-trips.

## 4. Broker, tickets, leases, internal mTLS

- `internal/broker/tickets_test.go` — issue/redeem contract (TM-24):
  TTL, single-use, hash storage, audience/tenant/subject binding,
  `ErrConnectionInUse`, clipboard-policy recording
  (`TestTicket_ClipboardPolicyRecordedOnRedeem`, TM-36).
- `internal/broker/leases_test.go`, `connection_state_test.go`,
  `sessions_test.go`, `bindings_envtest_test.go` /
  `bindings_internal_test.go`, `targets_internal_test.go` — lease
  lifecycle, fencing/freshness (TM-24), stale binding, target
  resolution.
- `internal/broker/signout_test.go` — `RevokePortalSession` semantics
  (TM-07): digest-bound leases revoked with drain accounting,
  outstanding tickets revoked, redemption denied once the issuing
  session's row is gone, idempotent; the baseline schema carries the
  revoke indexes (`TestBaseline_MigrateIsIdempotent`, §5).
- `internal/broker/revokeall_test.go` — `RevokePrincipalSessions`
  semantics (ADR 0007, TM-08): all of the principal's sessions in the
  tenant deleted (caller's included), leases revoked principal-scoped
  with drain accounting, outstanding tickets revoked, replayed
  cookies resolve to dead leases, tenant boundary held, idempotent,
  both redeem/revoke interleavings pinned on real row locks, a
  `-race` revoke-all × redeem × per-session-sign-out run proving no
  deadlock and no live lease left for a destroyed session, and
  `TestRevokePrincipalSessions_MultiTicketInversion` racing the sweep
  against a multi-ticket redeem (the deterministic
  `lockByKeysInOrderTx` order).
- `internal/broker/lease_session_test.go` — bound-portal-session
  re-check on every live-lease read (TM-07 defence-in-depth):
  renew/attach/claim revoke a lease whose session row was deleted,
  epoch-staled (restored dump, TM-21) or absolutely expired, with
  drain accounting and the
  `tinycdi_lease_session_missing_total{reason}` counter;
  foreign-gateway and multi-replica shapes. The portal idle window is
  honoured too: `TestLeaseSession_IdleDeadSessionRevokesOnRenew`,
  `TestLeaseSession_RedeemRejectsIdleDeadSession`, and
  `TestLeaseSession_IdleCheckDisabledWithoutOption` pins the
  option-off behaviour (TM-03).
- `internal/broker/credentials_envtest_test.go` — per-workspace Secret
  credential reads (the per-workspace credential boundary, TM-37).
- `internal/broker/operator_stopped_test.go` +
  `operator_stopped_envtest_test.go` — behaviour while the operator is
  down; a held workspace still gets the stricter of template/defaults
  policy, never the mirror annotation's caps (SEC-10, TM-43).
- `internal/broker/httpapi/server_test.go` — internal API routes
  require a peer certificate and the right client role (SEC-41,
  TM-40); the `identify` refusal observability
  (`TestIdentify_RejectionsLogged`, `TestIdentify_RejectedPathRedacted`,
  `TestIdentify_RejectionsRateLimited`,
  `TestIdentify_RejectionsCounted`) and the generation bound on
  workspace revoke (`TestRevokeWorkspace_GenerationUnbounded`, TM-39);
  `internal/broker/activity_e2e_test.go` pins the bound end to end
  (`TestRevokeWorkspaceLeases_GenerationBounded`).
- `internal/broker/httpapi/opclient/client_test.go`,
  `verify_internal_test.go` — `VerifyConnection` chain+hostname
  checks; `ca_reload_test.go` — client cert/CA hot reload (FX-R33,
  TM-38).
- `internal/tlsreload/reloader_test.go`, `capool_test.go` — file-watch
  reload incl. Kubernetes atomic Secret-volume swaps (TM-38).
- `internal/tlsreload/blocklist_test.go` — the revocation list
  (SR-3-F5, TM-38): format/bounds parse (one bad line rejects the
  file), fail-closed boot, keep-last-good reload with the error
  observer, the conn registry's track/sever/drop, and the
  swap-vs-registration serialization under `-race`.
- `internal/backend/mtls_reload_test.go` — internal listener cert +
  client-CA hot reload (TM-38).
- `internal/backend/mtls_revocation_test.go` — the revocation list
  end-to-end on the production internal-listener stack (TM-38):
  blocked (issuer, serial) aborts the handshake, sibling +
  cross-issuer certs pass, an appended entry lands on poll, resumed
  handshakes are refused too, a live conn on a newly blocked serial
  is severed, `ConnState` close/hijack drops the registration,
  feature-off installs no revocation `VerifyConnection`,
  absent-cert still reaches the 401 path, WARN line + the three
  metrics, and the flag/env/validation surface.
- `internal/broker/transfers_test.go` — the transfer claim and
  completion (TM-30): `TestClaimTransfer_HappyPathIsSingleUse`,
  `TestClaimTransfer_PolicyTightenedAfterMint` (a tightened ceiling
  applies at claim time),
  `TestClaimTransfer_RejectsMismatches`,
  `TestClaimFileTransfer_OnePerWorkspaceConcurrent`,
  `TestCompleteTransfer_BoundToLeaseWorkspaceAndSurvivesRevoke`;
  `internal/broker/httpapi/server_test.go` maps the claim errors on
  the wire (`TestTransferClaim_OK_AndErrorMapping`).
- `deploy/helm/chart_revocation_test.go` — `revokedSerials` render
  legs (TM-38): off = no arg/mount/volume (golden unchanged), on =
  arg + read-only mount + single-key Secret projection to the fixed
  filename.
- `internal/backend/listeners_test.go` — listener timeout budgets
  (SEC-23, TM-35).
- `internal/backend/config_test.go` — DSN `sslmode` refusal without
  `-dev-insecure-db`, trusted-proxies parsing (TM-32).
- `internal/backend/leader_lock_test.go`, `leader_test.go`,
  `migrate_lock_test.go` — leader/migration locking; the advisory
  lock the post-restore procedure and the rate-limit sweep take
  (TM-21, TM-06).
- `internal/backend/postrestore_test.go` — `backend post-restore`
  (runbook kill-steps, TM-21): dry-run writes nothing, epoch rotation
  rejects every restored session (TM-07), lease/ticket denial, CR
  intent alignment, refusal with zero writes while the leader lock is
  held, the tool holding the lock for the whole apply (a concurrent
  lock attempt fails mid-run) and releasing it on success and on a
  mid-run error, refusal while backend connections remain in
  pg_stat_activity plus the `-i-know-backends-are-running` override,
  the no-Kubernetes SQL fallback.
- `internal/ratelimit/ratelimit_test.go` — token buckets, LRU
  eviction, `PerReplica` division (RL-1, TM-06),
  `ClientKey`/`PeerIP` derivation; key canonicalisation:
  `TestClientKey_IPv6FoldedTo64`, `TestClientKey_MappedFormsUnified`;
  `TestClientKey_NonIPSelectedEntryFallsBackToPeer` pins that a
  non-IP selected XFF claim never becomes a limiter key;
  `TestShared_IPv6WindowAndLocalShareKey` pins the same folded key in
  the local bucket and the shared Postgres window.
- `internal/ratelimit/shared_test.go` — the Postgres-window limiter
  (ADR 0006 + RL-CEILING, TM-06): fixed-window bound, undivided
  healthy ceiling consulted first, uneven-split bound (15/5, 20/0
  across two instances), degraded-mode division to the per-pod floor,
  healthy↔degraded transition bounds with no bucket resets, fail-open
  on store error, edge-triggered fallback logs + per-error metric,
  disabled-rate and nil-store paths, and the circuit breaker —
  500 ms per-call deadline under a stalled store, no store contact
  inside the 10 s cool-down, probe-triggered recovery, single-flight
  probe under -race. `internal/backend/wire_test.go` pins the wired
  defaults (`TestRateLimitDefaults_Budgets`).
- `internal/backend/ratelimit_test.go` — the expired-window sweep runs
  only on the Postgres-leader replica (advisory-lock singleton,
  TM-06).
- `tests/integration/rate_limit_pg_test.go` — store-level window
  upsert and sweep semantics on real Postgres, plus the B6 gate: two
  backend replicas sharing one database admit one hammered key
  exactly R+B per window — including an uneven 15/5 and 20/0 spread,
  the RL-CEILING regression — (window-edge bound documented in the
  README), and a Postgres outage fails open to the divided local
  limiter and recovers —
  `tinycdi_rate_limit_store_errors_total{route}` asserted on /metrics
  (TM-06).

## 5. Store

- `internal/store/sessions_test.go` — digest-keyed session rows
  (SEC-27, TM-23); raw cookie values are never persisted.
- `internal/store/baseline_test.go` — the one baseline schema
  (TM-21): `TestBaseline_SchemaMatchesSnapshot` pins the schema to
  the checked-in snapshot, `TestBaseline_MigrateIsIdempotent` the
  migrate path, `TestBaseline_MigrateRefusesUnknownMigration` that
  the backend refuses an unknown or older database, and
  `TestBaseline_PortalSessionDigestNotNull` the session-digest
  invariant.
- `internal/store/audit_test.go` — `TestAudit_InsertListFilterCursor`
  (tenant-scoped keyset paging, TM-12),
  `TestAudit_OutcomeVocabulary`, `TestAudit_AmendAndPrune` (TM-50).
- `internal/store/filetransfer_test.go` —
  `TestFileTransfer_InsertAndSweep` — grant rows are single use and
  expire (TM-18).
- `internal/store/leases_test.go` —
  `TestLiveLeases_ListFiltersAndPages` — the admin lease listing the
  `leaseRef` view is built on (TM-14).
- `internal/store/overview_test.go` —
  `TestOverview_LiveSessionsAndP95` and `TestMarkBootReady_FirstWinsAndPrune`
  — the overview aggregation and the boot-time bookkeeping the p95
  reads (TM-15).
- `internal/store/txhook_test.go` —
  `TestWithTx_TxHookFailureRollsBackMutation` — a failing audit
  tx-hook rolls the mutation back (TM-50).
- `internal/store/users_test.go` —
  `TestTenantUsers_ListSortsPagesAndCounts`,
  `TestTenantUsers_DisplayNameFallsBackToSubject` — the
  tenant-scoped directory listing (TM-13).
- `internal/store/userlimit_test.go` —
  `TestUserLimitStore_OverrideConditionalWrites`,
  `TestUserLimitStore_DefaultConditionalWrites` — conditional writes
  the `If-Match` admin routes rely on (TM-09).
- `internal/store/sessionpolicy_test.go` — the per-tenant session
  policy ceiling the `:apply` route and intent resolver read (TM-17).
- `internal/store/retention_test.go` — terminal connection-row
  pruning keeps the tables bounded (TM-50).
- `internal/store/errors_test.go` — transient-error classification
  the retry and fail-open paths rely on (TM-06).

## 6. Runtime pods, operator, chart

- `internal/runtime/linux/backend_netpol_test.go` — per-workspace
  `ws-<uid>-boundary` NetworkPolicy (SEC-02, SEC-11, SEC-29, SEC-30;
  TM-33, TM-34).
- `internal/runtime/linux/backend_test.go`,
  `apparmor_setting_test.go` — pod SecurityContext (non-root, drop
  ALL, seccomp/AppArmor), `requireRuntimeDefault` opt-out (TM-41);
  `TestEnsureSecret_CarriesXferTokenAndNeverRotatesIt` pins the
  per-workspace `xfer-token` Secret key (TM-37);
  `TestClusterOnlyEgress_DefaultRestricted` and
  `TestClusterOnlyEgress_UnrestrictedOptIn` pin the pod-level egress
  default and the explicit opt-in (TM-34).
- `internal/runtime/linux/backend_kasm_test.go`,
  `backend_kasm_quota_test.go` — kasm adapter injection,
  wrapper/policy mounts, read-only rootfs, quota interaction (TM-48,
  TM-36).
- `internal/xferd/xferd_test.go` — the in-pod transfer helper
  (TM-37): `TestXferd_TokenPolicyAndCap` (constant-time token,
  direction policy, byte cap),
  `TestXferd_JailRefusesEscapes` (the `os.Root` jail confines names
  to `~/transfers`),
  `TestXferd_DownloadAndUnknownPolicyFailsClosed`,
  `TestXferd_ProxiesDesktopAndStripsToken` (the token never reaches
  KasmVNC, TM-33), `TestXferd_ProxiesWebSocketUpgrade`.
- `api/v1/failure_test.go` —
  `TestFailureClassesAreDisjointAndComplete` — the recoverable and
  unrecoverable failure classes partition the lifecycle reasons
  (TM-16).
- `internal/operator/crd_v1_envtest_test.go` —
  `TestCRDv1_SessionPolicyRequiredAndFileTransferDefault` — the
  shipped CRD `v1` schema makes `sessionPolicy` required and
  defaults `fileTransfer` (envtest, TM-30, TM-36).
- `internal/operator/endpoint_ready_test.go` —
  `TestReconcile_WaitingForEndpoint` — `Ready` waits for a ready
  EndpointSlice endpoint, so a reported-Ready workspace accepts the
  first connection (TM-16).
- `internal/operator/recovery_test.go` — pod-loss recovery (TM-16):
  `TestRecovery_DeletedPodGoesRuntimeLostWithoutRebuild`,
  `TestRecovery_NewGenerationAfterRuntimeLostComesBackReady`,
  `TestRecovery_UnrecoverableReasonsFailImmediatelyAndStick`,
  `TestRecovery_ImagePolicyDeniedOnlyFromPolicyPullMessages`,
  `TestAdmissionImageDenied` (only image-policy admission messages
  map to `ImagePolicyDenied`).
- `internal/runtime/linux/home_volume_test.go` — the retained-home
  checks (TM-16): `TestEnsure_RetainHomeMissingAfterFirstBootIsLost`
  (a missing retained home after first boot is `HomeVolumeLost`),
  `TestEnsure_RetainHomeCacheLagIsNotLost`,
  `TestEnsure_RetainFirstBootCreatesTheHome`, `TestWaitingMessage`.
- `internal/operator/snapshot_verify_test.go`, `resnapshot_test.go`,
  `snapshot_adopt_test.go` — template snapshot image-digest
  verification, status-record authority over the annotation, and
  status-loss adoption from the pod-stamped template identity or the
  pre-stamp candidate match — identity from pod metadata, content
  always from the LIVE revision object, the annotation never a
  source (`TemplateRevisionGone` holds for unprovable identities:
  pruned/republished/rotated/webhook-altered or forged builds,
  foreign pods); the forged-annotation path is pinned by
  `TestStatusSnapshotAuthoritativeOverForgedAnnotation`,
  `TestForgedSnapshotStructurallyValidRejected`,
  `TestStatusSnapshotRecordedAtFirstAdmit`, the `TestAdoptSnapshot_*`
  series (incl.
  `TestAdoptSnapshot_StampedPodWithoutHashHolds`) and
  `TestBuildPod_AnnotationNoLongerHonoured` (TM-43);
  `internal/broker/operator_stopped_envtest_test.go` pins that held
  workspaces get the stricter of template/defaults, never annotation
  caps (SEC-10, TM-43).
- `internal/operator/hold_phase_test.go` + `internal/broker/
  held_expiry_test.go` — the held-phase contract (TM-43): a held
  workspace whose operator-owned pod serves stays `Ready` +
  `Degraded(reason)` and keeps lifecycle enforcement
  (`TestHoldWithPodStaysReady`, `TestHoldWithoutServingPodStaysPending`,
  `TestHeldWorkspace_EnforcedAtStricterCaps`,
  `TestK8sRunningSource_OperatorHeldWorkspaceStaysEnforced`).
- `internal/operator/snapshot_seccomp_test.go` +
  `internal/runtime/linux/seccomp_profile_test.go` —
  `seccomp-profile` localhost-name validation at template admission,
  snapshot recording and pod build (TM-47).
- `internal/provisioning/template_family_test.go` — the start-time
  template guard's security-field coverage (TM-46):
  `TestUpdate_SkippedOnSecurityFieldDrift` end to end through
  `K8sTemplateCatalog`, `TestUpdate_SkippedReasonMatrix`;
  `internal/api/events_test.go` `TestEvents_TemplateSkipReasons`
  curates the seven reason tokens.
- `internal/provisioning/boot_test.go` —
  `TestAppendIntent_RecordsBootRequestPerGeneration` — a boot intent
  is recorded per generation (TM-16).
- `internal/provisioning/restart_test.go` — the generation-fenced
  restart (TM-16):
  `TestRestartWorkspace_BumpsGenerationRevokesOldAndAppendsStart`
  (new generation, old generation's leases and tickets revoked),
  `TestRestartWorkspace_KeyReplaysAcrossRestartAndPlainStart`,
  `TestRestartWorkspace_GenerationFenced`,
  `TestRestartWorkspace_RefusesStoppedAndForeignAndRollsBackOnRevokeError`;
  `TestListRunningWorkspaces_TenantScopedRunningOnly` pins the
  tenant-scoped running list (TM-04).
- `internal/provisioning/list_page_test.go` —
  `TestListWorkspaces_Keyset`, `TestListRetained_Keyset` — keyset
  pagination on the workspace and retained-data lists (TM-04).
- `internal/backend/bootrecorder_test.go` —
  `TestBootReadyTransition` — the boot bookkeeping the
  admin-overview p95 reads (TM-15).
- `internal/backend/retention_test.go` —
  `TestAuditRetentionLoop_PrunesOldRowsAtStart` — the 90-day audit
  retention loop runs at boot and on its interval (TM-50).
- `deploy/helm/chart_filetransfer_test.go` —
  `TestFileTransferBoundDefaultAndCustom` — the chart renders
  `backend.fileTransfer.maxFileBytes` (default 104857600) into
  `-file-transfer-max-bytes` (TM-30, TM-37).
- `deploy/helm/chart_rbac_test.go` —
  `TestOperatorRoleReadsEndpointSlices` — the operator RBAC grants
  exactly the EndpointSlice read the Ready gate needs, and nothing
  more (TM-16, TM-42).
- `deploy/helm/chart_test.go` — also pins that `workspaces/status`
  is writable only by the operator service account (TM-43):
  `TestWorkspaceStatusWritableOnlyByOperator`,
  `TestKustomizeWorkspaceStatusWritableOnlyByManager`.
- `deploy/helm/chart_clusteronly_egress_test.go`,
  `chart_namespacequota_test.go`, `chart_defaultrender_test.go` —
  the opt-in hardening renders (TM-34, TM-44): ClusterOnly egress
  selector/ports rendering + schema
  (`TestClusterOnlyEgressRestrictionOnPolicy`,
  `TestClusterOnlyEgressPortsOnlyStaysPodOnly` in
  `internal/runtime/linux/backend_netpol_test.go`,
  `TestClusterOnlyEgressDefaultRestricted` and
  `TestClusterOnlyEgressUnrestrictedOptInOnPolicy` beside them,
  `TestParseClusterOnlyEgress` in `cmd/operator/main_test.go`
  covering fail-loud flag validation), per-managed-namespace
  ResourceQuota/LimitRange objects, and the whole-render byte
  golden (`TestDefaultRenderUnchangedGolden`) proving the default
  render is unchanged;
  `TestClusterOnlyEgress_DefaultRendersNoFlagAndNoWarning` and
  `TestClusterOnlyEgress_UnrestrictedRendersFlag` pin that the
  chart renders no operator flag by default and the
  `unrestricted` flag only on the explicit opt-in (SR-2-F8,
  TM-34).
- `internal/operator/snapshot_annotation_vap_envtest_test.go` —
  envtest enforcement proof for the optional
  ValidatingAdmissionPolicy sample (`config/admissionpolicy/`,
  TM-45): a real apiserver denies non-operator create/update
  touching the `template-snapshot` annotation inside the managed
  namespaces and stays inert outside them.
- `internal/operator/retained_claim_test.go`, `status_test.go`,
  `delete_vanished_test.go`, `workspace_controller_test.go` —
  retained-disk ownership and lifecycle edges (TM-04, TM-16);
  `intent_drift_test.go` (`TestIntentBehindDrift`) covers the
  `IntentBehind` condition + edge-triggered Warning event on
  intent-fence drift (TM-22), and
  `internal/provisioning/k8sapplier_test.go`
  (`TestK8sApplierIntentDrift`) the `intent-behind` marker stamping
  on a dropped drifted intent.
- `deploy/helm/chart_test.go` — chart hardening/render guards
  (SEC-02, 06, 08, 09, 29, 31–37; TM-42): ServiceAccount scoping,
  TLS-required edges, NetworkPolicy rendering, metrics exposure,
  schema guards.
- `deploy/helm/chart_guards_test.go` — render guards (TM-42):
  `TestOperatorLeaderElectionGuard` fails `leaderElect=false` with
  `replicas>1`, `TestMetricsListenerIsolation` pins the metrics port
  to `allow-metrics-scrape`/`prometheusPeers` in every edgeIngress
  mode.
- `deploy/helm/chart_hardening_test.go`, `chart_edgepolicy_test.go`,
  `chart_sessiondomain_test.go`, `chart_sessiondomain_case_test.go`,
  `chart_signout_test.go`, `chart_tenantquota_test.go`,
  `kasm_adapter_test.go`, `chart_branding_test.go`,
  `chart_explicitdefaults_test.go`, `chart_topologyspread_test.go`,
  `chart_streamcap_test.go` (TM-35:
  `TestSessionStreamCapBelowOneRejected`),
  `chart_admissionpolicy_test.go` (TM-45: opt-in VAP off by default,
  rendered objects, operator-SA exemption, Deny binding) — the
  remaining chart render guards (TM-42, TM-48).
- `build/frontend/main_test.go` — security headers (SEC-22, SEC-26;
  TM-01), server timeouts (SEC-23, TM-35), CSP/COOP, no `/v1` proxy,
  branding-dir traversal/listing/fallback rules (TM-05), TLS hot
  reload, drain.

## 7. Integration tests (tests/integration, `-tags=integration`)

Security-relevant subset:

- `sec_retainedref_test.go` — SEC-01/SEC-21/SEC-I7 permanent
  regression (retained-disk owner bypass, idempotency key scoping)
  (TM-04).
- `session_cookie_liveness_test.go` — two-replica cookie survival
  through streaming + restarts (FX-R26, TM-23).
- `session_idtoken_test.go` — sealed id_token round trip on real
  Postgres (TM-23), incl. `TestPGSessionStore_IDTokenAbsentIsNull`.
- `api_admission_test.go` — admission/authn boundary (SEC-03/SEC-27;
  TM-19, TM-23); `TestSessionEpochRotation` proves
  `platform_meta.session_epoch` rotation kills every restored
  session row (TM-21), `TestIntentRevisionOrdering` the intent-fence
  ordering on real Postgres (TM-22).
- `principal_directory_test.go` — directory-driven display identity
  (TM-13).
- `kasm_adapter_test.go` — adapter contract vs real kasmweb images
  (TM-48).
- `postgres_outage_test.go`, `backend_restart_test.go`,
  `pre_stop_drain_test.go`, `pre_stop_drain_window_test.go` —
  fail-closed posture under infra loss (TM-07, TM-23).
- `connection_stable_test.go`, `session_touch_test.go` —
  lease/stream stability (TM-24); `session_touch_test.go` also pins
  the digest-scoped input touch on real Postgres
  (`TestTouchSessionDigest_ScopesToBoundSession`, TM-03).
- `retained_attach_mount_test.go`, `retention_test.go`,
  `delete_finalization_test.go` — data-plane ownership/teardown
  (TM-04).
- `quota_admin_test.go`, `quota_restart_test.go` — quota persistence
  (TM-09).
- `operator_leader_test.go`, `operator_stopped_test.go` — operator
  availability edges (TM-22, TM-43).
- `linux_runtime_test.go`, `linux_desktop_test.go`,
  `linux_runtime_firefox_test.go` — runtime profile pods end to end
  (TM-33, TM-34).
- `template_update_test.go`, `imagestale`-adjacent coverage — image
  freshness signals (TM-46, TM-48; see also
  `internal/api/imageblock_test.go`, `imagestale_test.go`,
  `imagestale_r4_test.go`).

Browser-driven end-to-end coverage (Playwright, not `-tags=integration`):

- `hack/quickstart/smoke/cold-connect.spec.ts` — the cold-connect
  gate (TM-16): 20 cold boots measure launch→connected p95 < 20 s in
  the `quickstart` CI job.
- `web/tests-portal/session-transfer.spec.ts` — see §2 (TM-30,
  TM-31).

Pattern note — "vacuous test" traps: a test that severs, revokes or
deletes must assert it acted on >= 1 object, otherwise a broken
selector silently passes on an empty set. Example: `severDB` in
`backend_restart_test.go` counts `pg_terminate_backend` rows and
fails when it terminated zero connections — a wrong
`application_name` tag can never produce a vacuous "severed" pass.
Apply the same fail-when-nothing-happens rule to any new kill/revoke
drill.

## 8. CI scanners and gates

`.github/workflows/ci.yml` (PRs, main pushes, weekly):

| Job | Gate |
|---|---|
| `go vet + test -race (envtest)` | vet, gofmt, `go test -race` |
| `integration` | Postgres-service integration suite incl. restart-drill evidence |
| `portal ui` | tsc, vitest, build, Playwright e2e (`web/tests`, `web/tests-portal`); **`npm audit` prod deps high+** |
| `soak harness` | tsc + unit; **`npm audit` prod deps high+** |
| `chart` | `helm lint --strict`, promtool rules check, chart Go tests |
| `govulncheck` | `govulncheck ./...` fails on reachable vulns (weekly too) |
| `dependency review` | `actions/dependency-review-action` on PRs |
| `workflow-policy` | `.github/tests/*.test.sh` regression suite, `.trivyignore` expiry policy (`check-trivyignore.sh`), kasm-catalog policy (`check-kasm-catalog.sh`) |
| `kasm-contract` | adapter contract test + catalog scan (trivy gate + engine freshness floor); weekly + kasm-relevant PRs |
| `quickstart` | kind e2e + Playwright portal smoke; the cold-connect gate (20 runs, p95 < 20 s); `shellcheck` on quickstart scripts |
| `partitioned` | kind e2e with `backend.sessionCookieMode: partitioned` — CHIPS Set-Cookie attributes, in-frame reconnect across a backend rollout (digest rehydrate), revoked-lease cookie rejection, logout; `hack/quickstart/smoke/partitioned.spec.ts` on the pinned Chromium (CHIPS ≥ 118) |
| `workflow lint` | actionlint + yamllint + zizmor on all workflows (sha256-pinned tools) |

`.github/workflows/images.yml` (main pushes, publishes):

- Build jobs push **by digest only**; syft SBOM per image in the
  build job (SUPR-2, TM-49); per-image `scan` job runs **trivy** with
  the effective `.trivyignore` and an empty `--config` (SUPR-8),
  gating fixable CRITICAL/HIGH; `sign + promote` job
  cosign-keyless-signs and attaches the SBOM attestation **before**
  `imagetools create` promotes any tag (SEC-04/SEC-17, SUPR-13;
  TM-49).

`.github/workflows/release.yml` (`v*` tags):

- Version validation, release-notes review-status check, image/chart
  build + scan chain, then a minimal-permission `publish` job:
  cosign signs + attests digests (plus `attest-build-provenance`
  when enabled), signs the Helm chart, and only then promotes tags /
  drafts the release (`docs/security/provenance.md`). Dedicated SPDX
  SBOMs cover the packaged chart (`sbom-chart.spdx.json`) and the
  static binaries (`sbom-binaries.spdx.json`) — validated by
  `collect-publish-inputs.sh` and signed like every other asset
  (TM-49).

`.github/workflows/runtime-images.yml` — the runtime image train:
daily check → pin-bump PR → build/trivy-scan/cosign-sign/publish
`runtime-*` releases; `runtime-images.json` carries a
`cosign sign-blob` Sigstore bundle, and GitHub build-provenance legs
run under the `TRAIN_ATTESTATIONS_ENABLED` variable (off by
default). The publish job runs under the `runtime-train`
environment — a required reviewer makes every publish wait on a
recorded approval (SR-5-F2 mitigation, TM-49; an unapproved run
expires and the freshness check flags the images stale).

`.github/workflows/runtime-freshness.yml` — daily: fails while a
pinned Chromium/Firefox-ESR build is stale (opens a pin-bump PR);
fails when the newest `runtime-*` release is older than the 14-day
SLO (TM-49).

## 9. Repo-policy automation

- `.github/scripts/setup-repo-protection.sh` — codifies required
  checks and enforce-admins (govulncheck, workflow lint, etc. listed
  verbatim).
- `.github/scripts/check-trivyignore.sh` — every `.trivyignore`
  entry needs a `exp:YYYY-MM-DD`; enforced in `workflow-policy`
  **and** inside the scan jobs themselves.
- `.github/scripts/check-kasm-catalog.sh` — every `kasmweb/*` ref
  must be a digest-pinned catalog entry meeting the documented
  engine floor (TM-48).
- `.github/scripts/check-doc-links.py` — every relative Markdown
  link resolves, anchors included; nothing outside the planning
  folder may link into it.
- `.github/scripts/check-doc-versions.sh` — the published docs
  carry no internal version history or PR references;
  `.github/tests/docs-versions.allow` is the reviewed allowlist
  (stale entries fail the check).
- `.github/scripts/allowlist-grep.sh` — the shared allowlist engine
  the grep gates run through.
- `.github/tests/legacy-shims.test.sh` — the no-compatibility-shim
  gate: greps the `legacy`/`deprecated` markers and the alpha API
  version name in Go, TS and chart sources against
  `.github/tests/legacy-shims.allow` (each entry with a reason;
  stale entries fail).
- `.github/tests/docs-gates.test.sh` — the docs gates end to end:
  link-checker fixtures, the planning-folder fence fixtures, the
  version-gate fixtures, then the live link check, the live fence
  and the live version gate over the whole docs scope.
- `.github/tests/adr-refs.test.sh` — the ADR set is numbered without
  gaps and every `ADR NNNN` / `docs/adr/NNNN-….md` reference
  resolves.
- `.github/tests/threat-model-refs.test.sh` — the threat register is
  gap-free `TM-01..TM-N`, every row has a Control and a Pinned-by
  cell, every cited Go test exists, every cited `TM-NN` resolves,
  and no `S<n>`/legacy ids are left in the tree.
- `.github/tests/xfer-entrypoint.test.sh` — the in-pod transfer
  helper wiring (TM-37): both runtime entrypoints start
  `tcdi-xferd`, supervise it next to Xvnc and stop it on shutdown;
  KasmVNC listens on `127.0.0.1:8444` only; the images ship the
  helper.
- `.github/tests/*.test.sh` — regression tests for all of the above
  plus `pipefail.test.sh`, `script-checkout.test.sh`,
  `workflow-expressions.test.sh`, `release-topology.test.sh`,
  `publish-inputs.test.sh`, `setup-repo-protection.test.sh`,
  `validate-release-version.test.sh`,
  `release-notes-review-status.test.sh`,
  `supply-chain-hardening.test.sh`, `preflight.test.sh`,
  `e2e-smoke.test.sh`, `clipboard-policy.test.sh`,
  `check-browser-freshness.test.sh`,
  `check-runtime-image-age.test.sh`, `check-trivyignore.test.sh`,
  `failed-digest-cleanup.test.sh`, `kasm-catalog.test.sh`,
  `next-rt-tag.test.sh`, `write-runtime-manifest.test.sh`.
- `.github/scripts/check-browser-freshness.sh`,
  `check-runtime-image-age.sh`, `bump-browser-pin.sh` — freshness
  SLO machinery (TM-49).
- `hack/preflight/preflight.sh` — pre-install environment checks
  (includes node/AppArmor posture detection relevant to TM-41).

## 10. Tag glossary

Besides `TM-NN`, code comments carry older review and design finding
ids; the comment beside each tag states the rule it came from. A tag
documents provenance — the `TM-NN` register row is the canonical id.
Families seen in the tree (count = hits of the family pattern in
`*.go`, `*.ts`, `*.tsx`, `*.yaml`, `*.tpl`, `*.sh` outside `docs/`):

| Tag family | Count | Meaning / typical sites |
|---|---|---|
| `SEC-N`, `SEC-IN` | 215 (+19 `SEC-I…`) | pre-launch security review findings — `SEC-01` retainedDataRef owner bypass, `SEC-27` digest-keyed sessions, `SEC-41` internal-mTLS peer role |
| `SR-N`, `SR-N-FN` | 54 | security-review follow-ups — `SR-3-F5` certificate revocation on internal mTLS, `SR-5-F2` publish-approval environment |
| `FX-RN` | 187 | fix-review rows — `FX-R26` XFF hygiene, `FX-R30` authenticated rate-limit keying, `FX-R33` client-cert/CA hot reload |
| `R-VN` | 18 | review-round items |
| `V3.N` | 104 | design-review items |
| `DN` | 210 | design-decision ids |
| `EN` | 185 | edge-case ids |
| `RL-…` | 19 | rate-limit findings — `RL-1` per-replica division, `RL-CEILING` |
| `KASM-N` | 35 | kasm adapter/wrapper/policy/catalog findings — `backend_kasm_test.go`, `check-kasm-catalog.sh` |
| `SUPR-N` | 90 (incl. `*.yml`) | supply-chain findings — `SUPR-2` SBOM in build job, `SUPR-8` empty trivy config, `SUPR-13` sign before tag |

## 11. Gaps a reviewer will notice

- `partitioned` cookie mode has kind e2e coverage (`ci.yml` job
  `partitioned`), but a cross-SITE deployment shape (portal and
  session on different registrable domains) is still not exercised —
  kind resolves everything under one domain (the TM-01 residual).
- Soak/drill harness exists (`tests/soak/`) but runs out of band,
  not per PR.
- Rate-limit multi-replica coverage exists for the login window
  (`tests/integration/rate_limit_pg_test.go`, the B6 gate);
  `/v1/launch` shares the same limiter construction but has no
  two-replica abuse drill of its own (TM-06).
- The post-restore drill (`hack/quickstart/restore-drill.sh`,
  TM-21) is a manual kind runbook script — executed and recorded
  once (`docs/runbooks/disaster-recovery.md` "Restore drill —
  executed"), not a per-PR or scheduled CI gate, so bit-rot in the
  drill/tool pair is only caught when someone runs it.
- The audit stream is stdout JSONL behind `GuardedSink`: emission
  and shape are guaranteed, sink loss is signalled via
  `tinycdi_audit_write_errors_total`, but durability and
  tamper-evidence depend entirely on the operator's log pipeline —
  there is no signed/hashed audit log test because the product does
  not produce one (the TM-50 residual; runbook "Log integrity").
- No in-repo test drives a real KasmVNC client through the session
  gateway: the owner-tab contract (`400 owner_tab_required`,
  TM-24) is covered by unit and in-process e2e tests only, so a
  client that mishandles a reconnect on the gated stream would only
  be caught by the browser checklist.
- Branch-protection drift is only as good as the last run of
  `setup-repo-protection.sh` (TM-49).
- `podPidsLimit` is checked by preflight, not enforced by the product (TM-53).

## 12. Security-review fix tests

Tests added by the fixes for the internal v1.0 security review
([report](review-v1.0.md)). Each was written to fail before its fix.
Tests that need a built runtime image run in CI only (§7).

| Finding | Tests | File | Row |
|---|---|---|---|
| R1-01 | `TestSettleQuota_StartRaceKeepsReservation`, `TestSettleQuota_StoppedWorkspaceStillReleases` | `internal/provisioning/recovery_test.go` | TM-09 |
| R1-02 | `TestAdminQuota_PutAboveOperatorCeilingRefused`, `TestAdminQuota_UndeclaredTenantCreateRefused`, `TestAdminQuota_PutAtCeilingAccepted`, `TestTenantQuotaCeilingsRenderFlag`, `TestTenantQuotaCeilingsFlagAbsentByDefault`, `TestSchemaRejectsBadQuotaCeilings`, `TestParseFlags_TenantQuotaCeilings` | `internal/api/adminquota_test.go`, `deploy/helm/chart_tenantquota_test.go`, `internal/backend/tenantquota_test.go` | TM-51 |
| R1-03 | `TestAppListener_UnknownCookieSprayBounded`, `TestAppListener_ValidSessionsBehindOneClientNotThrottled`, `TestAppListener_UnknownCookieSprayRefused`, `TestAppListener_StoreErrorsKeepLookupSpent`, `TestLimiter_RefundReturnsOneToken`, `TestSessionStore_MissNeverWrites`, `TestPruneDeadSessions_RemovesOnlyDeadRows` | `internal/api/middleware_test.go`, `internal/backend/backend_test.go`, `internal/ratelimit/ratelimit_test.go`, `internal/store/sessions_pg_test.go` | TM-06 |
| R1-04 | `TestResponsePolicy_Upstream1xxCannotStripPinnedHeaders`, `TestProxy_WebSocketUpgradeSurvives1xxGuard` | `internal/gateway/proxy_1xx_test.go` | TM-29 |
| R1-05 | `TestSessionListener_StalledBodyBounded`, `TestSessionListener_AdmittedStreamOutlivesReadBound`, `TestSessionListener_TransferUploadOutlivesReadBound` | `internal/backend/session_readbound_test.go`, `internal/gateway/readbound_test.go` | TM-52 |
| R1-06 | `TestLinuxRuntimeClipboardParamsNotOverridable` (image IT, run locally with the images built; the adapter subtest `TestKasmAdapterChromium/ClipboardParamsNotOverridable` runs in CI's `kasm-contract` job), `clipboard-policy.test.sh` | `tests/integration/linux_runtime_test.go`, `tests/integration/kasm_adapter_test.go`, `.github/tests/clipboard-policy.test.sh` | TM-36 |
| R1-07 | `preflight.test.sh`, `security-doc-claims.test.sh` | `.github/tests/preflight.test.sh`, `.github/tests/security-doc-claims.test.sh` | TM-53 |
| R1-08 | `TestWorkspaceTemplate_BrowserPolicyFieldRemoved` | `api/v1/validation_test.go` | TM-46 |
| R1-09 | `write-runtime-manifest.test.sh`, `check-scan-age.test.sh`, `publish-inputs.test.sh`, `release-topology.test.sh` | `.github/tests/` | TM-49 |
| R1-10 | `TestWrapApp_PortalBrowserHeaders`, `TestAPIBrowserHeaders_SetOnEveryResponse` | `internal/backend/backend_test.go`, `internal/api/headers_test.go` | TM-01 |
| R1-11 | `TestOpenAPI_SessionCookieMatchesAuthDefaults` | `internal/api/openapi_contract_test.go` | TM-01 |
| R1-12 | `TestOpenAPI_LogoutDocumentsIDTokenHint` | `internal/api/openapi_contract_test.go` | TM-20 |
| R1-13 | `TestBrandingDir_OnlyAllowlistedNames` | `build/frontend/main_test.go` | TM-05 |
| R1-14 | `TestLogin_SamePrincipalKeepsLiveSession` | `internal/api/auth_test.go` | TM-07 |
| R1-15 | `TestNotesWarnUnrestrictedEdgeOpen`, `security-doc-claims.test.sh` | `deploy/helm/chart_edgepolicy_test.go`, `.github/tests/security-doc-claims.test.sh` | TM-06 |
| R1-16 | `TestAudit_RequestLogNeverContainsRawOwnerRef` | `internal/api/auditroutes_test.go` | TM-13 |
| R1-17 | `security-doc-claims.test.sh` | `.github/tests/security-doc-claims.test.sh` | TM-11 |
| R1-18 | `TestCreateConnection_ForeignOwnerIs404` | `internal/broker/tickets_test.go` | TM-04 |
| R1-19 | `TestListWorkspaces_BadPhaseIs400`, `TestAdminUsers_ForgedCursorIs400`, `TestParsePage_CursorTextMustBePostgresSafe` | `internal/api/workspaces_handlers_test.go`, `internal/api/adminusers_test.go`, `internal/api/page_test.go` | TM-04 |
| R1-20 | `TestCreateWorkspace_ReservesExactTemplateBytes`, `TestStart_RevisionMoveReservesNewVector` | `internal/api/workspaces_handlers_test.go`, `internal/provisioning/template_family_test.go` | TM-09 |
| R1-21 | `security-doc-claims.test.sh` | `.github/tests/security-doc-claims.test.sh` | TM-17 |
| R1-22 | `TestAudited_TargetSanitizedAndBounded`, `TestAudit_InsertSanitizedTarget` | `internal/api/auditstore_test.go`, `internal/store/audit_test.go` | TM-50 |
| R1-23 | `TestAdminUserLimit_AuditFailureRollsBackWrite` | `internal/api/adminuserlimit_test.go` | TM-50 |
| R1-24 | `TestRetainedAttach_LabelIsCRName` | `internal/provisioning/retained_applier_test.go` | TM-09 |
| R1-25 | `TestRedeemTicket_EveryRefusalConsumes` | `internal/broker/ticket_consume_test.go` | TM-23 |
| R1-26 | `TestSessionListener_HealthOnlyOnControlHosts`, `TestControlToken_MinLength`, `TestControlAuth_FailuresLimitedPerClient`, `TestLimiter_PeekDoesNotSpend` | `internal/backend/session_health_test.go`, `internal/backend/config_test.go`, `internal/gateway/control_limit_test.go`, `internal/ratelimit/ratelimit_test.go` | TM-26 |
| R1-27 | `TestDesktopLeg_ReplacedHelperRelaysBytesWithoutDisposition`, `security-doc-claims.test.sh` | `internal/gateway/desktopleg_trust_test.go`, `.github/tests/security-doc-claims.test.sh` | TM-37 |
| R1-28 | `TestClusterOnlyEgressDefaultSameTenantOnly` | `internal/runtime/linux/backend_netpol_test.go` | TM-34 |
| R1-29 | `TestEnsureUsernsProfileRequiresHostUsersFalse`, `TestSeededUsernsProfileRequiresHostUsersFalse`, `security-doc-claims.test.sh` | `internal/runtime/linux/userns_profile_test.go`, `deploy/helm/chart_userns_profile_test.go`, `.github/tests/security-doc-claims.test.sh` | TM-47 |
| R1-30 | `TestInternalListener_RemovedCASeversLiveConn` | `internal/backend/mtls_reload_test.go` | TM-38 |
| R1-31 | `TestBlocklistIssuerCanonicalMatch` | `internal/tlsreload/blocklist_test.go` | TM-38 |
| R1-32 | `TestUnknownCN_Refused`, `TestParseFlags_InternalGatewayCNs` | `internal/broker/httpapi/server_test.go`, `internal/backend/config_test.go` | TM-54 |
| R1-33 | `TestRoleMismatchCounted`, `TestIdentify_HandshakeRejectionsCounted` | `internal/broker/httpapi/server_test.go`, `internal/backend/mtls_revocation_test.go` | TM-40 |
| R1-34 | `TestExtraArgsCannotOverrideRenderedFlags` | `deploy/helm/chart_extraenv_test.go` | TM-42 |
| R1-35 | `TestEnsure_NetPolPrecedesPod` | `internal/runtime/linux/backend_netpol_test.go` | TM-34 |
| R1-36 | `TestPodSecurityValuesAllReferenced` | `deploy/helm/chart_hardening_test.go` | TM-41 |
| R1-37 | `release-topology.test.sh`, `security-doc-claims.test.sh`, `failed-digest-cleanup.test.sh` | `.github/tests/` | TM-48 |
| R1-38 | `publish-gating.test.sh` | `.github/tests/publish-gating.test.sh` | TM-49 |
| R1-39 | `setup-repo-protection.test.sh` | `.github/tests/setup-repo-protection.test.sh` | TM-49 |
| R1-40 | `push-sign-chart.test.sh` | `.github/tests/push-sign-chart.test.sh` | TM-49 |
| R1-41 | `release-topology.test.sh`, `security-doc-claims.test.sh` | `.github/tests/` | TM-49 |
