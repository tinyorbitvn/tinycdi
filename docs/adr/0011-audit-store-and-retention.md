# ADR 0011 — Audit store and retention

Status: accepted (F6 of the v1.0.0 W1 slate; implemented with the audit
viewer)

Date: 2026-10-14

## Context

Audit events were a stdout JSONL stream only (`observability.JSONSink`
behind `GuardedSink`). Administrators could not read them in the product,
and a write failure was swallowed by design.

## Decision

- **Store.** Domain audit events (every route in `auditedRoutes` except
  `session.touch`, plus the file-transfer claim/complete records the broker
  writes) are persisted to `audit_event` (`internal/store/audit.go`). The
  stdout record is unchanged and still written for every event.
- **One function, fail closed.** `audited()` writes both. On a mutating
  request it installs a `store.TxHook`; the first transaction of the
  request that commits inserts the row in the same transaction, so the
  mutation and its record commit or roll back together. The row is
  amended with the final outcome after the handler returns. A request that
  commits no transaction inserts its row before the buffered response is
  released; if the insert fails the client gets `500 INTERNAL` and nothing
  the handler produced. A mutation that reaches only Kubernetes cannot be
  rolled back — its response fails closed instead.
- **Pseudonymization.** `actor_ref` is `observability.ActorRef(issuer,
  subject)` (`oidc:` + 8 bytes of SHA-256, hex), or `anonymous`. A target
  user appears only as the same pseudonym in the event's details
  (`owner` or `target`, via `targetActorRef`). No raw subject, email,
  display name, source address, token or cookie is written; detail keys
  that look like credentials are redacted before storage
  (`observability.RedactDetails`). File-transfer events record the file
  name and byte count, never content. The viewer accepts an `issuer|sub`
  filter and pseudonymizes it server-side, so an administrator can search
  by a known user without the table ever holding the identity. The
  pseudonym is a join key across events, not a secret: anyone who can guess
  an `issuer|sub` can compute it.
- **Retention.** `-audit-retention-days` (chart
  `backend.retention.auditDays`, default 90, 1..3650). A backend leader
  singleton prunes older rows once at election and then daily, in batches
  of 1000.
- **Read surface.** `GET /v1/admin/tenants/{tenant}/audit`, tenant-admin
  only, newest first, keyset-paged on `(created_at, id)`, filtered by
  action, actor, target and time range. Reading the log is itself audited
  (`admin.audit.get`).

## Alternatives considered

- **Stdout only, with a cluster log shipper.** Rejected: the product could
  not show the log, and a lost line could not fail the mutation.
- **A separate audit database.** Rejected: it cannot share the mutation's
  transaction, which is what makes "no record, no change" true.

## Consequences

- Every audited request does one extra insert; the table is indexed on
  `(tenant, created_at desc, id desc)` and `(tenant, action, created_at desc, id desc)`.
- An audit-store outage makes audited routes fail instead of running
  unrecorded. Unaudited reads keep working.
- Gateway-local events (launch, stream) stay stdout-only.
