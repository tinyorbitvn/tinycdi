# ADR 0003 — Internal gateway↔broker API (mTLS)

Status: accepted (contract fixed in design review; both sides implement it verbatim)

Date: 2026-09-30

> **v0.2 update:** [ADR 0005](0005-backend-frontend-operator.md) (proposed)
> merges the session gateway into the backend binary: the gateway↔broker
> calls below become in-process, and the mTLS listener keeps only the
> operator routes (`workspaces/{uid}/revoke`, `workspaces/{uid}/drain`).
> The wire contract itself is unchanged — a backend run as a standalone
> session gateway still speaks it as a remote client.

## Context

The session gateway (design §6) redeems launch tickets and renews/resolves
connection leases against the broker. That surface is server-to-server: it
carries opaque tickets in request bodies and returns runtime credentials in
target responses. It must never sit on the public portal mux, must
authenticate the caller as a specific gateway identity, and must fail closed
when the broker's view of Workspace status is stale.

## Decision

The broker side of the contract is implemented by `internal/broker/httpapi`
and served from `cmd/api` on a **separate internal listener**
(`-internal-listen`, `TCDI_INTERNAL_LISTEN`), never on the public mux.

### Transport & identity

- HTTPS with mTLS. The listener is configured with
  `tls.Config{ClientAuth: VerifyClientCertIfGiven, ClientCAs: <gateway CA>}`:
  a request presenting an untrusted certificate fails the TLS handshake; a
  request presenting none reaches the handler and is answered **401
  UNAUTHENTICATED** through the standard error model (`internal/api/errors.go`
  shape), so gateways always get a parseable error rather than a TLS abort.
- Gateway identity derives from the verified leaf certificate: `Subject CN`
  is the gateway ID. An optional URI SAN `spiffe://cdi.tinyorbit.vn/gateway/<id>`
  must equal the CN or the request is rejected 403. The identity maps to
  `broker.GatewayIdentity{ID: CN, Audience: <configured audience>}`; the
  audience is the deployment's session origin (`-gateway-audience`, default
  the `-session-origin` host).
- `X-Request-Id` is propagated into responses and error bodies.

### Routes

| Route | Body | Success | Errors |
|---|---|---|---|
| `POST /internal/v1/broker/redeem` | `{"ticket":"<opaque>"}` | 200 `Lease` | 401 invalid/expired ticket, 403 denied/revoked, 409 in-use/stale |
| `POST /internal/v1/broker/leases/{id}/renew` | `{"fence":{"version":N,...}}` | 200 `Lease` | 403 foreign gateway, 404 unknown lease, 409 fenced/stale, 410 revoked |
| `GET /internal/v1/broker/leases/{id}/target` | — | 200 `Target` | 403/404/409/410 as above |
| `POST /internal/v1/broker/leases/{id}/revoke` | — | 204 | — |

`Lease`/`Target`/`Fence` serialize the `broker` package structs directly
(`leaseId`, `workspaceUID`, `runtimeGeneration`, `runtimeUID`,
`fencingVersion`, `expiresAt`; `protocol`, `serviceDNS`, `upstreamURL`,
`tlsServerName`, `caPEM`, `username`, `password`). The target response is the
only place credentials appear — never in logs, metrics or audit. Request
logs carry the matched route pattern plus a truncated sha256 of the lease
id — never the raw id, which is bearer material on this listener (SEC-41).
The 410
code uses the internal-only error code `REVOKED` in the standard `Error`
JSON shape (the public `ErrorCode` enum has no 410 mapping).

### Fail-closed budgets

- Renew cadence 10 s; the gateway treats a session as dead after 30 s
  without a successful renew (`gateway.RevokeDeadline`).
- The broker refuses issue/renew/resolve when its Workspace-status view
  (informer `ObservedAt`) is older than 15 s → 503 `UNAVAILABLE`
  (`retryable: true`).

### Ticket/lease security properties (broker)

- `IssueTicket` mints a `tkt_` + 256-bit opaque token; only SHA-256 is
  persisted (`launch_ticket.ticket_hash`). Tickets bind
  (workspace, tenant, owner, runtimeGeneration, runtimeUID, audience,
  takeover) and live 60 s.
- `RedeemTicket` consumes on first attempt (even expired), enforces the
  single active lease via the partial unique index, and a takeover
  supersedes the old lease **before** inserting the new one
  (fencing_version is monotonic per workspace).
- `RenewLease`/`ResolveTarget` require the holding gateway identity, a
  fence equal to the lease's pinned incarnation, and a fresh `Ready`
  binding — a recreated runtime (new runtimeUID) auto-fences old access.
- `RevokeTicket`/`RevokeLease` act only on the addressed row (SEC-1: an
  unknown/replayed revoke never touches a live session).

## Consequences

- `cmd/api` gains `-internal-listen` + `-internal-tls-cert/-key` +
  `-internal-client-ca` flags (env `TCDI_INTERNAL_*`); when unset the
  listener is disabled and gateways cannot redeem (warned at startup).
- The public API gains `POST /v1/workspaces/{id}/connections`
  (`internal/api/connections.go`) returning `{workspaceId, ticket,
  launchUrl, expiresAt}`; the ticket never enters URLs or logs.
- The broker's `BindingSource` is a controller-runtime informer on
  Workspace CRs scoped to managed namespaces (`-tenant-namespaces`);
  credentials resolve from the per-workspace Secret `ws-<uid>-rt` read in
  the tenant's managed namespace only.

---

## Addendum: session activity + operator revocation

Status: contract extension fixed in design review.

### Routes

| Route | Caller | Body | Success | Errors |
|---|---|---|---|---|
| `POST /internal/v1/broker/leases/{id}/activity` | gateway | `{"fence":{...},"type":"input"\|"connected"\|"disconnect"}` | 204 | 400 unknown type, 403 foreign gateway, 409 fenced/stale, 410 dead lease |
| `POST /internal/v1/broker/workspaces/{uid}/revoke` | operator | `{"runtimeGeneration":N}` | 200 `{"revokedLeases":n}` | 403 non-operator |
| `GET /internal/v1/broker/workspaces/{uid}/drain` | operator | — | 200 `{"openStreams":n,"drained":b}` | 403 non-operator |

### Identity split

The same mTLS listener now serves two client populations. The operator
identity is the client-certificate CN configured by `-operator-cn` /
`TCDI_OPERATOR_CN` (default `operator`): the `workspaces/*` routes require
it and every lease-scoped route (redeem/renew/target/revoke/activity)
refuses it — a gateway cert cannot call the operator routes and vice versa.

### Semantics

- **Server receipt time.** The broker stamps `receivedAt` from its own
  clock on every accepted activity event; any client timestamp is ignored.
- **Recorded state.** `workspace_activity` is keyed by
  `(workspace_id, runtime_generation)`: `input` refreshes
  `last_input_at` (the only signal that re-anchors the 30 min idle clock);
  `connected` increments `open_streams` and clears `disconnected_since`
  (reconnect inside the grace window cancels the pending disconnect stop);
  `disconnect` decrements `open_streams` and anchors `disconnected_since`
  on the FIRST transition to zero. Stale generation/runtimeUID/fencing
  events are refused (409) — a dead incarnation can neither keep alive nor
  stop a newer runtime. `ReportActivity` does not enforce the sliding
  lease TTL: the idle/disconnect clocks keep working through a renew
  hiccup; a revoked/superseded/expired lease is refused (410).
- **Revoke.** `workspaces/{uid}/revoke` revokes every live lease bound to
  a generation `<= runtimeGeneration` (the operator's observed generation;
  a lease for a NEWER generation survives — stale teardown never fences a
  restarted runtime) and records a `workspace_revocation` row that blocks
  `IssueTicket`/`RedeemTicket` for covered generations.
- **Drain.** `drain` returns the summed `open_streams` the gateways have
  reported. Stream state derives from the connected/disconnect activity
  events themselves — no separate periodic stream-count report was added;
  a gateway crash may leak a count, which the finalizer's bounded
  (≤45 s) drain window absorbs by construction.
- **Expiry path.** `ExpiryPlanner.Scan` evaluates running workspaces
  (desired Running + phase Ready, `status.startedAt` anchor for the 8 h
  max-duration cap; idle anchored at last input — or at the generation's
  start when it never saw input). Deadlines become durable `stop_intent`
  rows via `RequestStop` (which re-checks the generation against the
  CURRENT binding — a stale generation is refused with no side effects).
  The periodic sweep drains those intents into the provisioning outbox:
  `desired_state`/`phase` flip to Stopped/Stopping and
  `provisioning.AppendIntent(IntentStop)` commit in one transaction, only
  while the workspace row still pins the intent's generation — expiry is
  never a direct CR mutation.
