# Security test inventory

Companion to [`threat-model.md`](threat-model.md): the automated evidence that
exists today for the v1.0 security review. Lists security-relevant tests and
CI scanners **with paths** — presence in CI does not imply the reviewer should
skip reading the code (the post-MVP review's own lesson: green tests are not a
substitute for review).

## 1. How the tests run

| Suite | Command | Where it runs |
|---|---|---|
| Unit + envtest (race) | `go vet ./...` + `go test -race ./...` | `ci.yml` job `go vet + test -race (envtest)` |
| Integration (Postgres + envtest) | `go test -tags=integration ./tests/integration` | `ci.yml` job `integration` (Postgres service); a cluster-free subset runs per PR |
| Chart | `helm lint --strict`, `promtool`, `go test ./deploy/helm/...` | `ci.yml` job `chart` |
| Frontend server | `go test ./build/frontend` | folded into the Go test run |
| Portal web | `npm ci`, `tsc`, `vitest`, `vite build` | `ci.yml` job `portal ui` |
| Soak harness | `npm ci`, `tsc`, vitest + drills dry-run | `ci.yml` job `soak harness`; live soak out of band |
| Go fuzz | `go test -fuzz=<target>` | `ci.yml` job `go fuzz`: 30 s per target on PRs touching the fuzzed packages, 10 m per target on the weekly schedule; crashers uploaded as artifacts |
| Quickstart e2e | `hack/quickstart/up.sh` on kind + Playwright portal smoke | `ci.yml` job `quickstart` |
| Partitioned-cookie e2e | quickstart + `values-partitioned.yaml` overlay + `partitioned.spec.ts` | `ci.yml` job `partitioned` (gated + weekly) |
| Upgrade drill | previous release → tree on kind | `ci.yml` job `upgrade` |

## 2. Auth, session and API surface

- `internal/api/auth_test.go` — OIDC login/callback, sealed login cookie
  (SEC-03), `requiredGroups` gating (SEC-I3), session lifecycle.
- `internal/api/middleware_test.go` — `RequireCSRF` accept/reject matrix,
  `RequireTrustedOrigin` (single-Origin allowlist, `Sec-Fetch-Site`
  fallback), request-ID/audit middleware.
- `internal/api/loginstate/sealer_test.go` — AEAD seal/open, expiry, wrong-
  purpose AAD rejection, key rotation (first seals, all open).
- `internal/api/idtoken.go` exercised via `auth_test.go` /
  `logout_test.go` and `tests/integration/session_idtoken_test.go` —
  sealed `id_token` at rest; unopenable blob degrades logout, never fails it.
- `internal/api/ratelimit_test.go`, `ratelimit_key_test.go` — limiter
  behaviour and the FX-R30 key resolvers (valid session → session-digest
  bucket; valid OIDC state → state-digest bucket; forged values fall back to
  the client-IP bucket; per-IP ceiling on the callback).
- `internal/api/connection_status_test.go` — passive auth path: polling must
  not extend idle (A6-S6).
- `internal/api/session_probe_test.go` — the unauthenticated session probe.
- `internal/api/logout_test.go` — sign-out destroys the session and expires
  cookies; `endSessionURL` built only from discovery+config (no open
  redirect); `TestLogout_RevokesSessionBoundMaterial` /
  `TestLogout_RevokeFailureStillSignsOut` — sign-out revokes session-bound
  leases/tickets at the store, audits `session.revoke`, and a store failure
  never blocks the sign-out (S17).
- `internal/api/tenant_scope_test.go`, `events_test.go`, `statusview_test.go`,
  `me_test.go`, `owner_test.go`, `principal.go` — tenant scoping, curated
  events, principal directory fallbacks.
- `internal/api/workspaces_handlers_test.go` — SEC-01 (retainedDataRef owner
  bypass) and SEC-I7 (malformed pageToken) regression coverage.
- `internal/api/adminquota_test.go`, `quota_test.go`,
  `internal/backend/tenantquota_test.go` — quota enforcement paths.
- `internal/api/openapi_contract_test.go` — public API contract vs
  `openapi.yaml`.

## 3. Session gateway (listener :8444)

- `internal/gateway/launch_test.go` — ticket redemption contract: body-only
  ticket (`?ticket=` → 403), one-use, expired still consumed, takeover,
  cookie attributes.
- `internal/gateway/launch_origin_test.go` — ADR 0004 Origin allowlist:
  cross-site portal origin, same-site, session origin, unknown/null Origin
  rejected without consuming the ticket, `Sec-Fetch-Site` agreement,
  no-browser-headers case, WebSocket upgrade origin.
- `internal/gateway/launch_ratelimit_test.go` — a rate-limited launch does
  not consume the ticket; session-keyed buckets behind NAT (FX-R30);
  limiter disabled by default.
- `internal/gateway/hostbinding_test.go` — launch on own vs other host,
  cookie replayed on a sibling workspace host (host_mismatch), two parallel
  workspaces, unknown host → 421, control surface only on control hosts,
  pinned session headers, upstream-cannot-override, partitioned cookie mode,
  per-host `connect-src` CSP, KasmVNC probe allowances.
- `internal/gateway/proxy_test.go` — response policy pinning (SEC-07),
  upstream path canonicalization + allowlist (SEC-20), control-surface
  bearer auth and revoke isolation (SEC-1/2), strict upgrade detection
  (SEC-3), lease/cookie plumbing.
- `internal/gateway/rehydrate_test.go` — digest → lease rehydration,
  singleflight per digest, expired/revoked rejection.
- `internal/gateway/authorization.go` fencing covered by
  `wsclose_test.go` (orderly WS close), `owner_tab_test.go` (owner-tab
  takeover), `activity_test.go`/`activity_internal_test.go`.
- `internal/gateway/forwarded_test.go` — client XFF never reaches the
  runtime; XFF honored only from trusted-proxy CIDRs (FX-R26 / A6-S18).
- `internal/gateway/framing_test.go`, `framereload_test.go` — iframe
  embedding policy, `frame-ancestors`, frame reload behaviour.
- `internal/gateway/e2e_test.go` — end-to-end session flow in-process.
- `internal/gateway/signout_test.go` — S17 propagation: a store-level
  revoke ends a live stream on a sibling replica within one renew cycle,
  and the workspace cookie replays to 401 on any replica.

### Fuzz targets (stdlib `testing.F`, run by the `go fuzz` CI job)

- `internal/api/fuzz_callback_test.go` — `FuzzCallbackParsing`: OIDC
  callback query params (state/code/error), raw Cookie header →
  AEAD login-state open (test keys) and the code-exchange boundary
  against the in-process issuer; asserts exact rejection order and
  determinism, and that the callback rate-limit key is minted only for a
  validated state.
- `internal/gateway/fuzz_test.go` — `FuzzHostClassify`: arbitrary Host
  headers (ports, IPv6 literals, IDNA/punycode, trailing dots, case) ×
  request paths through `ServeHTTP`; asserts the host-class contract
  (session host / control host / foreign → 421) is total and
  deterministic.
- `internal/gateway/fuzz_test.go` — `FuzzLaunchRedeem`: arbitrary launch
  methods, query strings and form bodies through ticket redemption on an
  in-memory digest store (tickets keyed by SHA-256 like
  `launch_ticket.ticket_hash`); asserts the bounded status set, single
  use, host binding, and that a 303 implies a well-formed cookie + clean
  redirect with no ticket leakage.
- `internal/ratelimit/fuzz_test.go` — `FuzzClientKey`: arbitrary
  RemoteAddr, X-Forwarded-For lines and `-trusted-proxies` CIDR lists;
  asserts the right-most-untrusted-entry contract end to end plus
  deterministic, canonical-CIDR-only proxy parsing.
- `internal/sessionhost/fuzz_test.go` — `FuzzDomainMatch`: arbitrary
  session-domain config strings and Host headers through `ParseDomain`,
  `Match`, `Label`/`WorkspaceID`; asserts determinism and
  accept-implies-well-formed round-trips.

## 4. Broker, tickets, leases, internal mTLS

- `internal/broker/tickets_test.go` — issue/redeem contract: TTL, single-use,
  hash storage, audience/tenant/subject binding, `ErrConnectionInUse`,
  clipboard-policy recording.
- `internal/broker/leases_test.go`, `connection_state_test.go`,
  `sessions_test.go`, `bindings_envtest_test.go`/`bindings_internal_test.go`,
  `targets_internal_test.go` — lease lifecycle, fencing/freshness, stale
  binding, target resolution.
- `internal/broker/signout_test.go` — `RevokePortalSession` semantics (S17):
  digest-bound leases revoked with drain accounting, outstanding tickets
  revoked, redemption denied once the issuing session's row is gone,
  idempotent; migration 020 indexes.
- `internal/broker/credentials_envtest_test.go` — per-workspace Secret
  credential reads.
- `internal/broker/operator_stopped_test.go` +
  `operator_stopped_envtest_test.go` — behaviour while the operator is down.
- `internal/broker/httpapi/server_test.go` — internal API routes require a
  peer certificate and the right client role (SEC-41).
- `internal/broker/httpapi/opclient/client_test.go`,
  `verify_internal_test.go` — `VerifyConnection` chain+hostname checks;
  `ca_reload_test.go` — client cert/CA hot reload (FX-R33, A6-S15).
- `internal/tlsreload/reloader_test.go`, `capool_test.go` — file-watch reload
  incl. Kubernetes atomic Secret-volume swaps.
- `internal/backend/mtls_reload_test.go` — internal listener cert + client-CA
  hot reload.
- `internal/backend/listeners_test.go` — listener timeout budgets (SEC-23).
- `internal/backend/config_test.go` — DSN `sslmode` refusal without
  `-dev-insecure-db`, trusted-proxies parsing.
- `internal/backend/leader_lock_test.go`, `leader_test.go`,
  `migrate_lock_test.go` — leader/migration locking.
- `internal/ratelimit/ratelimit_test.go` — token buckets, LRU eviction,
  `PerReplica` division (RL-1), `ClientKey`/`PeerIP` derivation.
- `internal/ratelimit/shared_test.go` — the Postgres-window limiter (ADR
  0006): fixed-window bound, local ceiling consulted first
  (min(shared, local)), fail-open to the divided bucket on store error,
  edge-triggered fallback logs + per-error metric, disabled-rate and
  nil-store paths, and the circuit breaker — 500 ms per-call deadline
  under a stalled store, no store contact inside the 10 s cool-down,
  probe-triggered recovery, single-flight probe under -race.
- `internal/backend/ratelimit_test.go` — the expired-window sweep runs
  only on the Postgres-leader replica (advisory-lock singleton).
- `tests/integration/rate_limit_pg_test.go` — store-level window upsert
  and sweep semantics on real Postgres, plus the B6 gate: two backend
  replicas sharing one database admit one hammered key exactly R+B per
  window (window-edge bound documented in the README), and a Postgres
  outage fails open to the divided local limiter and recovers —
  `tinycdi_rate_limit_store_errors_total{route}` asserted on /metrics.

## 5. Store

- `internal/store/sessions_test.go` — digest-keyed session rows (SEC-27);
  raw cookie values are never persisted.
- `internal/store/errors_test.go`, migration tests in
  `tests/integration/migration017_test.go`, `migration018_test.go`.

## 6. Runtime pods, operator, chart

- `internal/runtime/linux/backend_netpol_test.go` — per-workspace
  `ws-<uid>-boundary` NetworkPolicy (SEC-02, SEC-11, SEC-29, SEC-30).
- `internal/runtime/linux/backend_test.go`,
  `apparmor_setting_test.go` — pod SecurityContext (non-root, drop ALL,
  seccomp/AppArmor), `requireRuntimeDefault` opt-out (A6-S16).
- `internal/runtime/linux/backend_kasm_test.go`,
  `backend_kasm_quota_test.go` — kasm adapter injection, wrapper/policy
  mounts, read-only rootfs, quota interaction.
- `internal/operator/snapshot_verify_test.go`, `resnapshot_test.go` —
  template snapshot image-digest verification (SEC-10).
- `internal/operator/retained_claim_test.go`, `status_test.go`,
  `delete_vanished_test.go`, `workspace_controller_test.go` — retained-disk
  ownership and lifecycle edges.
- `deploy/helm/chart_test.go` — chart hardening/render guards (SEC-02, 06,
  08, 09, 29, 31–37): ServiceAccount scoping, TLS-required edges,
  NetworkPolicy rendering, metrics exposure, schema guards.
- `deploy/helm/chart_hardening_test.go`, `chart_edgepolicy_test.go`,
  `chart_sessiondomain_test.go`, `chart_sessiondomain_case_test.go`,
  `chart_signout_test.go`, `chart_tenantquota_test.go`,
  `kasm_adapter_test.go`, `chart_branding_test.go`,
  `chart_explicitdefaults_test.go`, `chart_topologyspread_test.go`.
- `build/frontend/main_test.go` — security headers (SEC-22, SEC-26), server
  timeouts (SEC-23), CSP/COOP, no `/v1` proxy, branding-dir
  traversal/listing/fallback rules, TLS hot reload, drain.

## 7. Integration tests (tests/integration, `-tags=integration`)

Security-relevant subset:

- `sec_retainedref_test.go` — SEC-01/SEC-21/SEC-I7 permanent regression
  (retained-disk owner bypass, idempotency key scoping).
- `session_cookie_liveness_test.go` — two-replica cookie survival through
  streaming + restarts (FX-R26).
- `session_idtoken_test.go` — sealed id_token round trip on real Postgres.
- `api_admission_test.go` — admission/authn boundary incl. SEC-03/SEC-27.
- `principal_directory_test.go` — directory-driven display identity.
- `kasm_adapter_test.go` — adapter contract vs real kasmweb images.
- `postgres_outage_test.go`, `backend_restart_test.go`,
  `pre_stop_drain_test.go`, `pre_stop_drain_window_test.go` — fail-closed
  posture under infra loss.
- `connection_stable_test.go`, `session_touch_test.go` — lease/stream
  stability.
- `retained_attach_mount_test.go`, `retention_test.go`,
  `delete_finalization_test.go` — data-plane ownership/teardown.
- `quota_admin_test.go`, `quota_restart_test.go` — quota persistence.
- `operator_leader_test.go`, `operator_stopped_test.go` — operator
  availability edges.
- `linux_runtime_test.go`, `linux_desktop_test.go`,
  `linux_runtime_firefox_test.go` — runtime profile pods end to end.
- `template_update_test.go`, `imagestale`-adjacent coverage — image
  freshness signals (see also `internal/api/imageblock_test.go`,
  `imagestale_test.go`, `imagestale_r4_test.go`).

## 8. CI scanners and gates

`.github/workflows/ci.yml` (PRs, main pushes, weekly):

| Job | Gate |
|---|---|
| `go vet + test -race (envtest)` | vet, gofmt, `go test -race` |
| `integration` | Postgres-service integration suite incl. restart-drill evidence |
| `portal ui` | tsc, vitest, build; **`npm audit` prod deps high+** |
| `soak harness` | tsc + unit; **`npm audit` prod deps high+** |
| `chart` | `helm lint --strict`, promtool rules check, chart Go tests |
| `govulncheck` | `govulncheck ./...` fails on reachable vulns (weekly too) |
| `dependency review` | `actions/dependency-review-action` on PRs |
| `workflow-policy` | `.github/tests/*.test.sh` regression suite, `.trivyignore` expiry policy (`check-trivyignore.sh`), kasm-catalog policy (`check-kasm-catalog.sh`) |
| `kasm-contract` | adapter contract test + catalog scan (trivy gate + engine freshness floor); weekly + kasm-relevant PRs |
| `quickstart` | kind e2e + Playwright portal smoke; `shellcheck` on quickstart scripts |
| `partitioned` | kind e2e with `backend.sessionCookieMode: partitioned` — CHIPS Set-Cookie attributes, in-frame reconnect across a backend rollout (digest rehydrate), revoked-lease cookie rejection, logout; `partitioned.spec.ts` on the pinned Chromium (CHIPS ≥ 118) |
| `upgrade` | previous release → tree upgrade drill on kind |
| `workflow lint` | actionlint + yamllint + zizmor on all workflows (sha256-pinned tools) |

`.github/workflows/images.yml` (main pushes, publishes):

- Build jobs push **by digest only**; syft SBOM per image in the build job
  (SUPR-2); per-image `scan` job runs **trivy** with the effective
  `.trivyignore` and an empty `--config` (SUPR-8), gating fixable
  CRITICAL/HIGH; `sign + promote` job cosign-keyless-signs and attaches the
  SBOM attestation **before** `imagetools create` promotes any tag
  (SEC-04/SEC-17, SUPR-13).

`.github/workflows/release.yml` (`v*` tags):

- Version validation, release-notes review-status check, image/chart build +
  scan chain, then a minimal-permission `publish` job: cosign signs +
  attests digests (plus `attest-build-provenance` when enabled), signs the
  Helm chart, and only then promotes tags / drafts the release
  (`docs/security/provenance.md`).

`.github/workflows/runtime-images.yml` — the runtime image train: daily
check → pin-bump PR → build/trivy-scan/cosign-sign/publish `runtime-*`
releases. Intentionally no human gate on publish (A6-S12).

`.github/workflows/runtime-freshness.yml` — daily: fails while a pinned
Chromium/Firefox-ESR build is stale (opens a pin-bump PR); fails when the
newest `runtime-*` release is older than the 14-day SLO.

## 9. Repo-policy automation

- `.github/scripts/setup-repo-protection.sh` — codifies required checks and
  enforce-admins (govulncheck, workflow lint, etc. listed verbatim).
- `.github/scripts/check-trivyignore.sh` — every `.trivyignore` entry needs a
  `exp:YYYY-MM-DD`; enforced in `workflow-policy` **and** inside the scan
  jobs themselves.
- `.github/scripts/check-kasm-catalog.sh` — every `kasmweb/*` ref must be a
  digest-pinned catalog entry meeting the documented engine floor.
- `.github/scripts/check-browser-freshness.sh`,
  `check-runtime-image-age.sh`, `bump-browser-pin.sh` — freshness SLO
  machinery.
- `.github/tests/*.test.sh` — regression tests for all of the above plus
  `pipefail.test.sh`, `script-checkout.test.sh`,
  `workflow-expressions.test.sh`, `release-topology.test.sh`,
  `publish-inputs.test.sh`, `setup-repo-protection.test.sh`,
  `validate-release-version.test.sh`, `release-notes-review-status.test.sh`,
  `preflight.test.sh`.
- `hack/preflight/preflight.sh` — pre-install environment checks (includes
  node/AppArmor posture detection relevant to A6-S16).

## 10. Finding-tag index

Regression tests cite the finding ID they close. Quick index of tags seen in
`*_test.go` (grep `SEC-`, `SEC-I`, `SUPR-`, `KASM-`, `FX-R`, `RL-` for
context):

| Tag | Meaning / where |
|---|---|
| SEC-01 | retainedDataRef owner bypass — `workspaces_handlers_test.go`, `tests/integration/sec_retainedref_test.go` |
| SEC-02 | netpol/workspace boundary — `backend_netpol_test.go`, `chart_test.go`, `api_admission_test.go` |
| SEC-03 | sealed OIDC login state binds browser — `auth_test.go`, `api_admission_test.go` |
| SEC-04 | sign-before-tag supply chain — `images.yml` promote job |
| SEC-06/08/09 | chart RBAC/Secret/manager-binding guards — `chart_test.go` |
| SEC-1/2/3 | control-revoke isolation, control-surface bearer auth, strict WS upgrade detection — `proxy_test.go` (`isUpgrade`, `controlAuth`) |
| SEC-07 | pinned session CSP/headers; hostile upstream headers dropped — `proxy_test.go`, `hostbinding_test.go` |
| SEC-10 | template snapshot image-digest verification — `snapshot_verify_test.go` |
| SEC-11/29/30 | workspace ingress selector / netpol details — `backend_netpol_test.go`, `chart_test.go` |
| SEC-17 | SBOM attestation on the digest — `images.yml`, `release.yml` |
| SEC-20 | upstream path canonicalization/traversal + allowlist (`cleanedProxyPath`) — `proxy_test.go`, `authorization.go` |
| SEC-21 | idempotency key principal binding — `sec_retainedref_test.go` |
| SEC-22/23/26 | frontend headers/timeouts/cookie flags — `build/frontend/main_test.go` |
| SEC-27 | digest-keyed session rows — `sessions_test.go`, `api_admission_test.go` |
| SEC-29/31–37 | chart hardening set — `chart_test.go` |
| SEC-41 | internal API requires peer cert/role — `server_test.go` |
| SEC-I3/I7 | requiredGroups gate; malformed pageToken — `auth_test.go`, `sec_retainedref_test.go` |
| SUPR-2/8/13 | SBOM in build job; empty trivy config; sign before tag — `images.yml`, `release.yml` |
| KASM-1..9 | adapter/wrapper/policy/catalog items — `backend_kasm_test.go`, `kasm_adapter_test.go`, `check-kasm-catalog.sh` |
| FX-R26 | XFF hygiene + multi-replica cookie liveness — `forwarded_test.go`, `session_cookie_liveness_test.go` |
| FX-R30 | authenticated rate-limit keying — `ratelimit_key.go` + launch/login tests |
| FX-R33 | mTLS client cert/CA hot reload — `opclient/ca_reload_test.go`, `mtls_reload_test.go` |
| RL-1 | per-replica rate-limit division — `internal/ratelimit`, `wire.go` |

## 11. Gaps a reviewer will notice

- `partitioned` cookie mode now has kind e2e coverage (`ci.yml` job
  `partitioned`); a cross-SITE deployment shape (portal and session on
  different registrable domains) is still not exercised — kind resolves
  everything under one domain.
- Soak/drill harness exists (`tests/soak/`) but runs out of band, not per PR.
- Rate-limit multi-replica coverage now exists for the login window
  (`tests/integration/rate_limit_pg_test.go`, B6); `/v1/launch` shares
  the same limiter construction but has no two-replica abuse drill of
  its own.
- Branch-protection drift is only as good as the last run of
  `setup-repo-protection.sh`.
