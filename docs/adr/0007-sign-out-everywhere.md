# ADR 0007 — Sign out everywhere

Status: Accepted

## Context

Plain sign-out revokes the material of the *calling* portal session:
`POST /v1/logout` deletes the session row, expires the portal cookie and
runs `Broker.RevokePortalSession`, which in one store transaction revokes
every active connection lease and every outstanding launch ticket bound
to that session's `portal_session_digest`. Every gateway replica's renew
loop sees the leases die within one renew cycle, and a replayed
workspace-host cookie resolves to a dead lease (401) on any replica.

What a single-session sign-out does not cover: a principal with sessions
on several browsers/devices (or a stolen cookie still valid elsewhere)
has no way to end *all* of them. "Sign out everywhere" is the remedy.

Scope deliberately excluded: **no IdP back-channel logout** (RFC 7009
token revocation / OIDC back-channel to the provider). The provider
session is affected only through the same RP-initiated
`end_session_endpoint` navigation plain logout already returns for the
calling browser.

## Decision

### API shape

`POST /v1/me/sessions:revoke-all` — the principal-scoped bulk variant of
`POST /v1/logout`. It is mounted on the app mux behind `RequireAuth` +
`RequireCSRF` (session cookie + `X-CSRF-Token` synchronizer header — the
same guard pair as `/v1/logout`; the endpoint is cookie-authenticated and
state-changing so CSRF is mandatory). The `:revoke-all` verb suffix is the
collection-action spelling of ADR 0002.

The tenant-admin variant
`POST /v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all`
does the same for a named user in the admin's tenant
(`internal/api/adminsessions.go`). The admin session list
(`GET /v1/admin/tenants/{tenant}/sessions`) shows `leaseRef` — 16
lowercase hex characters, a SHA-256 prefix of the lease id
(`internal/api/openapi.yaml`, `AdminLiveSession.leaseRef`) — never lease
ids.
Both routes are audited
(`internal/api/auditroutes.go` `routeSessionRevokeAll`,
`routeAdminUserRevokeAll`).

Answers mirror logout: `204` when the revoke committed and nothing else is
needed; `200` with `LogoutResult` (`{"endSessionUrl"}`) when the
deployment has `oidc.endSession` on and the provider advertises the
endpoint — the portal navigates there exactly as after plain sign-out.
The caller's retained ID token is `Peek`ed *before* the transaction so
`id_token_hint` behaves identically. On store failure the answer is
`500`: unlike plain logout, the operation's whole point is destroying
*other* sessions, so a failed transaction is never hidden behind a
cleared cookie (the transaction is atomic — nothing was destroyed, the
caller stays signed in, retrying is safe).

### Transaction scope and ordering

`Broker.RevokePrincipalSessions(ctx, tenantID, issuer, subject)` runs
**one transaction per principal** covering, in order:

1. `UPDATE launch_ticket SET revoked_at = now WHERE tenant_id = $t AND
   principal_subject = $iss|sub AND consumed_at IS NULL AND revoked_at IS
   NULL` — the same serialization trick `RevokePortalSession` relies on:
   `RedeemTicket` holds `FOR UPDATE` on its ticket row for its whole tx, so
   this UPDATE waits out any in-flight redemption of the principal's
   tickets. Whether a redeem commits or aborts, the lease UPDATE below runs
   on a fresh READ-COMMITTED snapshot that sees whatever it committed; a
   redeem starting after this tx finds `revoked_at` — or the deleted
   session row, which `RedeemTicket` re-checks — and never mints.
2. `DELETE FROM sessions WHERE issuer AND subject AND tenant_id` —
   all portal sessions of the principal inside the caller's tenant.
3. `UPDATE connection_lease SET state='revoked', closed_at = now WHERE
   tenant_id = $t AND principal_subject = $iss|sub AND state='active'
   RETURNING workspace_id, runtime_generation` — principal-scoped, so it
   also catches leases minted without a `portal_session_digest`.
4. `closeStreamsTx` per revoked lease — the same drain-accounting rule as
   `RevokeLease`/`RevokePortalSession`.

Lock ordering is *tickets → sessions → leases* and matches the other
mutators' order exactly: `RedeemTicket` takes its ticket row `FOR UPDATE`,
then reads `sessions`, then writes `connection_lease`;
`RevokePortalSession` takes tickets then leases with the caller's session
delete up front. Within each kind the sweeps take `FOR UPDATE` locks in
ascending key order through `lockByKeysInOrderTx` (tickets by
`ticket_hash`, sessions by `id`, leases by `id`,
`internal/broker/sessions.go`), so overlapping sweeps serialize on the
first contested row instead of crossing waits into a deadlock. The
ordering is pinned by the redeem/revoke interleaving tests
(`TestRevokePrincipalSessions_RedeemCommitThenRevoke`,
`TestRevokePrincipalSessions_RevokeCommitThenRedeem`,
`internal/broker/revokeall_test.go`) and the handler-level tests in
`internal/api/revokeall_test.go`.

Tenant-scoping is deliberate and user-visible: a principal with sessions
in several tenants revokes **only the caller's tenant**. The API
description and the portal confirm text both say so
(`TestRevokePrincipalSessions_TenantScoped`).

### The caller's own session: revoked too

The transaction deletes **all** the principal's sessions, including the
one making the request; the response then expires the portal cookie
exactly like logout. A `keepSelf` opt-out is not offered: "sign out
everywhere" is the action a user reaches for when a session may be
compromised, and a default that silently preserves the calling browser is
the surprising, weaker semantics — a user who wants to keep only this
device can simply sign back in. Returning the RP-initiated
`endSessionUrl` (200 path) is precisely what makes "revoked everywhere"
hold for this browser too: without it the IdP session would transparently
re-authenticate the next visit.

The revoke runs detached-but-bounded (`context.WithoutCancel` +
`sessionRevokeTimeout`): a client disconnect mid-call cannot abort a
committed decision (`TestRevokeAll_RevokeSurvivesClientDisconnect`).

### Idempotency

Fully idempotent: a second call finds no session rows, no outstanding
tickets and no active leases, commits, and returns the same response.
Concurrent revoke-all calls for the same principal serialize on the row
locks harmlessly (second one revokes zero rows —
`TestRevokePrincipalSessions_Idempotent`). Counts are returned for audit
only, never for control flow.

### Audit

One dedicated event per call, action `session.revoke_all` (distinct from
`session.revoke`, which stays reserved for the per-session path),
including denials, with the per-kind counts on success and `ErrorCode` on
failure — written through the same `AuditSink`/`RedactDetails` path as
every audited route (ADR 0010). The handler attaches the counts to the
in-flight event via `auditSetDetail(ctx, "counts",
"sessions=N,tickets=N,leases=N")` (`internal/api/revokeall.go`): the
neutral `counts` key survives the sink's credential-key redaction —
detail *keys* containing `session`/`ticket` are redacted on write, so
per-key counts would emit `[REDACTED]`. The request-level audit trail
stays in the middleware.

### Metrics

`tinycdi_session_revocations_total{result}` (S17) is unchanged for the
per-session path. Revoke-all counts on the bounded counter
`tinycdi_session_revoke_all_total{result}` with `result ∈ {ok, error}`
(`internal/observability/metrics.go`) — keeping bulk revocation
distinguishable from per-session sign-out.

### Rate limiting

The route joins the login-family limiter via
`RateLimitWithKey(loginLimiter, trusted, metrics, authn.SessionRateLimitKey())`
— the same shared Postgres-window limiter + divided local ceiling as the
session probe (ADR 0006), keyed by the validated session digest with the
trusted-proxy client key as anonymous fallback. The operation is
self-scoped and idempotent, so the limiter's job is only bounding
repeated DB churn from a noisy caller; there is no new budget flag. In
split mode it keeps the divided local bucket per ADR 0006's split-mode
rule.

### Portal UX

The account menu offers **"Sign out everywhere"** below "Sign out". It
opens a confirm dialog stating the caller is signed out on **all devices
including this one** and naming the scope: sessions of this tenant only.
On confirm it POSTs the endpoint and follows the same navigation as
`signOut` (`endSessionUrl` → `/signed-out` fallback). A 401 (session
already gone) lands on `/signed-out` like sign-out. The portal e2e is
`web/tests/signout.spec.ts`.

### Indexes

Two partial indexes in the baseline schema keep revocation from
seq-scanning the append-mostly tables
(`internal/store/migrations/0001_baseline.sql`):

- `connection_lease_principal_active` —
  `connection_lease (tenant_id, principal_subject) WHERE state='active'`
- `launch_ticket_principal_outstanding` —
  `launch_ticket (tenant_id, principal_subject) WHERE consumed_at IS NULL
  AND revoked_at IS NULL`

## Consequences

- One `POST` destroys every session-bound artifact of the principal in
  the tenant: streams on any replica close within one renew cycle,
  replayed workspace-host cookies for every revoked session 401, and
  tickets in flight can never redeem.
- `Broker` *writes* the `sessions` table (it already reads it in
  `RedeemTicket`'s liveness barrier). Keeping the delete inside the same
  transaction is what makes the ordering guarantee hold without a second
  round-trip.
- Slightly wider blast radius per call is mitigated by CSRF + auth +
  per-session-digest rate limiting + the confirm dialog.
- The endpoint, the metric and the indexes are additive; no other route
  changes.
