# ADR 0007 — Sign out everywhere (principal-scoped session revocation)

Status: **accepted** (implemented in #117)

Date: 2026-10-05

## Context

v0.4 (#107, threat-model S17) made **plain sign-out** revoke the material of
the *calling* portal session: `POST /v1/logout` deletes the session row,
expires the portal cookie and runs `Broker.RevokePortalSession`, which in
one store transaction revokes every active connection lease and every
outstanding launch ticket bound to that session's `portal_session_digest`.
Every gateway replica's renew loop sees the leases die within one renew
cycle, and a replayed workspace-host cookie resolves to a dead lease (401)
on any replica. That path is **unchanged** by this ADR.

What a single-session sign-out does not cover: a principal with sessions on
several browsers/devices (or a stolen cookie still valid elsewhere) has no
way to end *all* of them. "Sign out everywhere" is the standard remedy and
is the v0.5 SIGNOUT-ALL task.

Scope deliberately excluded: **no IdP back-channel logout** (RFC 7009
token revocation / OIDC back-channel to the provider). The provider session
is affected only through the same RP-initiated `end_session_endpoint`
navigation plain logout already returns for the calling browser.

## Decision

### API shape

`POST /v1/me/sessions:revoke-all` — the principal-scoped bulk variant of
`POST /v1/logout`. Mounted on the app mux behind `RequireAuth` +
`RequireCSRF` (session cookie + `X-CSRF-Token` synchronizer header — the
same guard pair as `/v1/logout`; the endpoint is cookie-authenticated and
state-changing so CSRF is mandatory). The `:revoke-all` verb suffix follows
the collection-action spelling the task brief suggests; the alternative
repo-style `POST /v1/me/revoke-all` was considered, but the `sessions`
segment makes the target (sessions, not leases or tokens) explicit, and no
other public route uses a verb segment anyway.

Answers mirror logout: `204` when the revoke committed and nothing else is
needed; `200` with `LogoutResult` (`{"endSessionUrl"}`) when the deployment
has `oidc.endSession` on and the provider advertises the endpoint — the
portal navigates there exactly as after plain sign-out. The caller's
retained ID token is `Peek`ed *before* the transaction so `id_token_hint`
behaves identically. On store failure the answer is `500`: unlike plain
logout, the operation's whole point is destroying *other* sessions, so a
failed transaction must not be hidden behind a cleared cookie (the tx is
atomic — nothing was destroyed, the caller stays signed in, retrying is
safe).

### Transaction scope and ordering

One new broker operation, `Broker.RevokePrincipalSessions(ctx, tenantID,
issuer, subject)`, runs **one transaction per principal** covering, in
order:

1. `UPDATE launch_ticket SET revoked_at = now WHERE tenant_id = $t AND
   principal_subject = $iss|sub AND consumed_at IS NULL AND revoked_at IS
   NULL` — same serialization trick `RevokePortalSession` relies on:
   `RedeemTicket` holds `FOR UPDATE` on its ticket row for its whole tx, so
   this UPDATE waits out any in-flight redemption of the principal's
   tickets. Whether a redeem commits or aborts, the lease UPDATE below runs
   on a fresh READ-COMMITTED snapshot that sees whatever it committed; a
   redeem starting after this tx finds `revoked_at` — or the deleted session
   row, which `RedeemTicket` re-checks — and never mints.
2. `DELETE FROM sessions WHERE issuer AND subject AND tenant_id` —
   all portal sessions of the principal inside the caller's tenant (the
   `sessions_issuer_subject` index from migration 013 applies; tenant is an
   extra predicate).
3. `UPDATE connection_lease SET state='revoked', closed_at = now WHERE
   tenant_id = $t AND principal_subject = $iss|sub AND state='active'
   RETURNING workspace_id, runtime_generation` — principal-scoped, so it
   also catches leases minted before `portal_session_digest` existed.
4. `closeStreamsTx` per revoked lease — same drain-accounting rule as
   `RevokeLease`/`RevokePortalSession`.

Lock ordering is *tickets → sessions → leases* and it matches the other
mutators' order exactly: `RedeemTicket` takes its ticket row `FOR UPDATE`,
then reads `sessions`, then writes `connection_lease`; `RevokePortalSession`
takes tickets then leases with the caller's session delete up front. No
operation acquires these rows in a different order, so a concurrent redeem
or per-session sign-out can only serialize — never deadlock (covered by a
`-race` interleaving test).

Tenant-scoping is deliberate and user-visible: a principal with sessions in
several tenants revokes **only the caller's tenant**. The API description
and the portal confirm text both say so.

### The caller's own session: revoked too (default)

The transaction deletes **all** the principal's sessions, including the one
making the request; the response then expires the portal cookie exactly like
logout. A `keepSelf` opt-out is *not* offered: "sign out everywhere" is the
action a user reaches for when a session may be compromised, and a default
that silently preserves the calling browser is the surprising, weaker
semantics — a user who wants to keep only this device can simply sign back
in. Returning the RP-initiated `endSessionUrl` (200 path) is precisely what
makes "revoked everywhere" hold for this browser too: without it the IdP
session would transparently re-authenticate the next visit.

### Idempotency

Fully idempotent: a second call finds no session rows, no outstanding
tickets and no active leases, commits, and returns the same response.
Concurrent revoke-all calls for the same principal serialize on the row
locks harmlessly (second one revokes zero rows). Counts are returned for
audit only, never for control flow.

### Audit

One dedicated event, action `session.revoke_all` (distinct from
`session.revoke`, which stays reserved for the per-session path), with the
per-kind counts on success and `ErrorCode` on failure — written through
the same `AuditSink`/`RedactDetails` path as today. The counts travel in
one `counts` detail (`"sessions=3,tickets=1,leases=2"`): Detail *keys*
containing `session`/`ticket` are redacted on write, so per-key counts
would emit `[REDACTED]`. The request-level audit trail stays in the
middleware.

### Metrics

`tinycdi_session_revocations_total{result}` (S17) is unchanged for the
per-session path. Revoke-all counts on a new bounded counter
`tinycdi_session_revoke_all_total{result}` with `result ∈ {ok, error}` —
keeping bulk revocation distinguishable from per-session sign-out without
changing the existing series' label set during rolling upgrade.

### Rate limiting

The route joins the existing login-family limiter via
`RateLimitWithKey(loginLimiter, trusted, metrics, authn.SessionRateLimitKey())`
— the same shared Postgres-window limiter + divided local ceiling as the
session probe, keyed by the validated session digest with the trusted-proxy
client key as anonymous fallback. The operation is self-scoped and
idempotent, so the limiter's job is only bounding repeated DB churn from a
noisy caller; no new budget flag. In split mode it keeps the divided local
bucket per ADR 0006's split-mode rule.

### Portal UX

The account menu gains **"Sign out everywhere"** below "Sign out". It opens
a confirm dialog stating the caller is signed out on **all devices
including this one** and naming the scope: sessions of this tenant only
(`en`/`vi` strings). On confirm it POSTs the endpoint and then follows the
same navigation as `signOut` (`endSessionUrl` → `/signed-out` fallback).
A 401 (session already gone) lands on `/signed-out` like today.

### Index migration

New migration `023_principal_revocation_indexes` (expand-only,
rolling-upgrade safe — replicas predating it just scan; 022 belongs to the
USER-LIMITS task): two partial indexes so revocation never seq-scans the
unpruned append-mostly tables —

- `connection_lease (tenant_id, principal_subject) WHERE state='active'`
- `launch_ticket (tenant_id, principal_subject) WHERE consumed_at IS NULL AND revoked_at IS NULL`

## Implementation notes (as merged in #117)

- **Migration 023** landed as designed:
  `internal/store/migrations/023_principal_revocation_indexes.sql` adds the
  two partial indexes (`connection_lease (tenant_id, principal_subject)
  WHERE state='active'`, `launch_ticket (tenant_id, principal_subject)
  WHERE consumed_at IS NULL AND revoked_at IS NULL`), expand-only and
  idempotent (`TestMigration023_Idempotent`).
- **Lock order** — the coarse *tickets → sessions → leases* order above is
  implemented with an additional deterministic within-kind ordering: each
  sweep first takes `FOR UPDATE` locks in ascending key order via
  `lockByKeysInOrderTx` (tickets by `ticket_hash`, sessions by `id`,
  leases by `id`) before its UPDATE/DELETE
  (`internal/broker/sessions.go:418`). `RevokePortalSession` was switched
  to the same discipline, closing the residual window where two sweeps
  could lock overlapping rows in different index-scan orders. The
  ordering argument is pinned by both redeem/revoke interleavings
  (`TestRevokePrincipalSessions_RedeemCommitThenRevoke` /
  `..._RevokeCommitThenRedeem`), a `-race` three-way run
  (`TestRevokePrincipalSessions_ConcurrentNoDeadlock`), and
  `TestRevokePrincipalSessions_MultiTicketInversion`.
- **Single audit event** — the `audited()` wrapper emits exactly one
  `session.revoke_all` record per call (including denials); the handler
  attaches the per-kind counts to the in-flight event via
  `auditSetDetail(ctx, "counts", "sessions=N,tickets=N,leases=N")`
  (`internal/api/revokeall.go`) — the neutral `counts` key survives the
  sink's credential-key redaction. No second audit write exists.
- **Metric** — `tinycdi_session_revoke_all_total{result}` with
  `result ∈ {ok, error}` as designed (`internal/observability/metrics.go`).
- **Rate limiting** — the production mount applies the session-keyed
  login-family limiter (`internal/backend/wire.go` `MountRevokeAllRoute`).
- The revoke runs detached-but-bounded (`context.WithoutCancel` +
  `sessionRevokeTimeout`): a client disconnect mid-call cannot abort a
  committed decision (`TestRevokeAll_RevokeSurvivesClientDisconnect`).
- Tests: `internal/api/revokeall_test.go`,
  `internal/broker/revokeall_test.go`, `internal/gateway/revokeall_test.go`;
  portal e2e `web/tests/signout.spec.ts` ("sign out everywhere confirms
  the tenant scope, then ends all sessions").

## Consequences

- One `POST` destroys every session-bound artifact of the principal in the
  tenant: streams on any replica close within one renew cycle (the existing
  renew-loop observation is untouched), replayed workspace-host cookies for
  every revoked session 401, and tickets in flight can never redeem.
- `Broker` now *writes* the `sessions` table (it already reads it in
  `RedeemTicket`'s liveness barrier). Keeping the delete inside the same tx
  is what makes the ordering guarantee hold without a second round-trip.
- Slightly wider blast radius per call is mitigated by CSRF + auth +
  per-session-digest rate limiting + the confirm dialog.
- No chart/API breakage; additive endpoint, additive metric, expand-only
  migration.
