# Runbook — Audit log

TinyCDI writes every audited action twice — one row in the `audit_event`
table in Postgres and one JSON line on the backend's stdout. This runbook
covers the stored log; the log stream is covered in
[observability.md](observability.md#audit-events).

## What is recorded

- Every audited API route (`auditedRoutes`,
  [`internal/api/auditroutes.go`](../../internal/api/auditroutes.go))
  except `session.touch`, plus the file-transfer claim and completion
  rows the broker writes, is stored
  ([ADR 0010](../adr/0010-audit-store-and-retention.md)).
- The stored actions, and when each is written:

  | Group | Action | When it is written |
  |---|---|---|
  | User | `workspace.create` | `POST /v1/workspaces` |
  | User | `workspace.start` | `POST /v1/workspaces/{workspaceId}:start` |
  | User | `workspace.stop` | `POST /v1/workspaces/{workspaceId}:stop` |
  | User | `workspace.delete` | `DELETE /v1/workspaces/{workspaceId}` |
  | User | `connection.create` | `POST /v1/workspaces/{workspaceId}/connections` (launch-ticket mint) |
  | User | `session.logout` | `POST /v1/logout` |
  | User | `session.revoke_all` | `POST /v1/me/sessions:revoke-all` (sign out everywhere) |
  | Data | `data.attach` | `POST /v1/data/{dataId}:attach` |
  | Data | `data.purge` | `POST /v1/data/{dataId}:purge` |
  | File transfer | `file.transfer.upload` | grant mint on `POST /v1/workspaces/{workspaceId}/file-transfers` (the attempt action before the direction decodes), then the broker's claim and completion rows as the stream runs |
  | File transfer | `file.transfer.download` | same route once the direction decodes, then the broker's claim and completion rows |
  | Admin | `admin.quota.get` | `GET /v1/admin/tenants/{tenant}/quota` |
  | Admin | `admin.quota.set` | `PUT /v1/admin/tenants/{tenant}/quota` |
  | Admin | `admin.user_limit.get` | `GET /v1/admin/tenants/{tenant}/user-limits` |
  | Admin | `admin.user_limit.set` | `PUT /v1/admin/tenants/{tenant}/user-limits/{ownerRef}` |
  | Admin | `admin.user_limit.clear` | `DELETE /v1/admin/tenants/{tenant}/user-limits/{ownerRef}` |
  | Admin | `admin.user_limit.default.get` | `GET /v1/admin/tenants/{tenant}/user-limits/default` |
  | Admin | `admin.user_limit.default.set` | `PUT /v1/admin/tenants/{tenant}/user-limits/default` with a `limit` |
  | Admin | `admin.user_limit.default.clear` | `PUT /v1/admin/tenants/{tenant}/user-limits/default` with `"limit": null` |
  | Admin | `admin.session_policy.get` | `GET /v1/admin/tenants/{tenant}/session-policy` |
  | Admin | `admin.session_policy.set` | `PUT /v1/admin/tenants/{tenant}/session-policy` |
  | Admin | `admin.session_policy.clear` | `PUT /v1/admin/tenants/{tenant}/session-policy` with an all-`FollowTemplate` body |
  | Admin | `admin.session_policy.apply` | `POST /v1/admin/tenants/{tenant}/session-policy:apply` |
  | Admin | `admin.audit.get` | `GET /v1/admin/tenants/{tenant}/audit` — reading this log |
  | Admin | `admin.user.list` | `GET /v1/admin/tenants/{tenant}/users` |
  | Admin | `admin.user.get` | `GET /v1/admin/tenants/{tenant}/users/{ownerRef}` |
  | Admin | `admin.session.list` | `GET /v1/admin/tenants/{tenant}/sessions` |
  | Admin | `admin.session.revoke` | `POST /v1/admin/tenants/{tenant}/users/{ownerRef}/sessions:revoke-all` |
  | Admin | `admin.overview.get` | `GET /v1/admin/tenants/{tenant}/overview` |

- Not stored, log stream only: the gateway's events (`launch.redeem`,
  `launch.host_mismatch`, `session.lookup`, `session.stream`,
  `session.list`, `session.revoke`, `session.host_mismatch`),
  `session.touch`, `http.request`, the sign-out `session.revoke` record
  ([`internal/api/auth.go`](../../internal/api/auth.go)) and
  `admin.quota.config_apply`
  ([`internal/backend/tenantquota.go`](../../internal/backend/tenantquota.go)).
- Each row: tenant, actor, action, target kind and id, request id,
  outcome (`success`, `failure`, `denied`), error code, details, time.
  File-transfer rows carry the file name and byte count, never content.
- The actor is a pseudonym, `oidc:` plus 16 hex characters of the SHA-256
  of issuer and subject, or `anonymous`. It is a join key, not a secret:
  anyone who knows a user's issuer and subject can compute it. No raw
  subject, email, display name, source address, token or cookie is
  stored; detail keys that look like credentials are redacted.

## Reading it

- Portal: **Admin → Audit** (`/admin/audit`), tenant admins only.
- API: `GET /v1/admin/tenants/{tenant}/audit`, newest first, with
  `action`, `actor`, `target`, `since` (inclusive), `until` (exclusive)
  and the paging parameters of
  [ADR 0002](../adr/0002-api-conventions.md) (`limit`, `cursor`; sort
  `-createdAt` only). `actor` takes a pseudonym, or an `issuer|sub`
  owner reference that the server pseudonymizes, so you can search for a
  known user.
- Reading the log is itself audited (`admin.audit.get`).

## Retention

- `-audit-retention-days` (chart `backend.retention.auditDays`), default
  90, range 1 to 3650; it cannot be turned off. The backend replica that
  holds the leader lock deletes older rows once when it becomes leader
  and then every 24 hours, 1000 rows per batch.

## When the store is unavailable

- An audited request whose row cannot be inserted fails with
  `500 INTERNAL`, and nothing the handler produced reaches the client.
  For a mutation that runs in a database transaction, the change and the
  row commit or roll back together; a change that reaches only
  Kubernetes cannot be rolled back, so its response fails instead.
  Unaudited reads keep working.
- Every failed insert or amend is logged at error level with the request
  id and counted on `tinycdi_audit_write_errors_total{event}`. Alert on
  it.

## Exporting for long-term retention

- The log stream carries every event, stored or not. Ship it to your log
  store to keep events beyond the database window
  ([observability.md](observability.md#log-integrity--what-tinycdi-guarantees-vs-what-you-must-provide)).
