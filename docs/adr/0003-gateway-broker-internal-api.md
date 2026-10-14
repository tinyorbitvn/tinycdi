# ADR 0003 — Internal gateway↔broker API (mTLS)

Status: Accepted

## Context

The session gateway (ADR 0005) redeems launch tickets and renews/resolves
connection leases against the broker, and the operator revokes and drains
leases when it tears a workspace down. That surface is server-to-server: it
carries opaque tickets in request bodies and returns runtime credentials in
target responses. It must never sit on the public portal mux, must
authenticate the caller as a specific client identity, and must fail closed
when the broker's view of Workspace status is stale.

## Decision

The broker side of the contract is implemented by `internal/broker/httpapi`
and served from `cmd/backend` on a **separate internal listener**
(`-internal-listen`, default `:9443`, `internal/backend/config.go:343`;
env `TCDI_INTERNAL_LISTEN`), never on the public mux.

### Transport and identity

- HTTPS with client certificates verified against the internal CA
  (`tls.Config{ClientAuth: VerifyClientCertIfGiven, ClientCAs: <internal CA>}`):
  a request presenting an untrusted certificate fails the TLS handshake; a
  request presenting none reaches the handler and is answered **401
  UNAUTHENTICATED** in the standard error shape, so clients get a parseable
  error rather than a TLS abort.
- The client identity is the verified leaf certificate's `Subject CN`. An
  optional SPIFFE URI SAN, if present, must equal the CN or the request is
  rejected 403 (`internal/broker/httpapi/server.go:145-172`). The identity
  maps to `broker.GatewayIdentity{ID: CN, Audience: <configured audience>}`.
- The operator identity is the client-certificate CN configured by
  `-operator-cn` (default `operator`, `server.go:48-52`, `config.go:348`).
  There is no CN allowlist: one operator CN, and any other CN the internal
  CA signed is a gateway identity. Per-certificate revocation is ADR 0008.
- `X-Request-Id` is propagated into responses and error bodies.

### Routes

The listener mounts two route families (`server.go:117-125`). Lease-scoped
routes are refused to the operator identity (`requireGateway`,
`server.go:431-440`); workspace-scoped routes admit only the operator CN
(`requireOperator`, `server.go:442-450`).

Lease-scoped gateway routes:

| Route | Purpose |
|---|---|
| `POST /internal/v1/broker/redeem` | redeem a launch ticket; returns the new lease |
| `POST …/leases/{id}/renew` | renew the sliding lease TTL under a matching fence |
| `GET …/leases/{id}/target` | resolve the lease's runtime upstream and credentials |
| `POST …/leases/{id}/revoke` | revoke the addressed lease |
| `POST …/leases/{id}/activity` | report `input`/`connected`/`disconnect` activity |
| `POST …/leases/{id}/transfers/{tid}/claim` | spend a minted file-transfer grant (ADR 0009) |
| `POST …/leases/{id}/transfers/{tid}/complete` | close a claimed transfer with its byte count |

Workspace-scoped operator routes:

| Route | Purpose |
|---|---|
| `POST …/workspaces/{uid}/revoke` | revoke every lease at or below a runtime generation |
| `GET …/workspaces/{uid}/drain` | read the summed open-stream count for teardown |

`Lease`/`Target`/`Fence` serialize the `broker` package structs directly
(`leaseId`, `workspaceUID`, `runtimeGeneration`, `runtimeUID`,
`fencingVersion`, `expiresAt`; `protocol`, `serviceDNS`, `upstreamURL`,
`tlsServerName`, `caPEM`, `username`, `password`). The target response is
the only place credentials appear — never in logs, metrics or audit. Request
logs carry the matched route pattern plus a truncated SHA-256 of the lease
id — never the raw id, which is bearer material on this listener. The 410
code uses the internal-only error code `REVOKED` in the standard `Error`
JSON shape.

### Fail-closed budgets

- Renew cadence 10 s; the gateway treats a session as dead after 30 s
  without a successful renew (`gateway.RevokeDeadline`,
  `internal/gateway/proxy.go:61`).
- The broker refuses issue/renew/resolve when its Workspace-status view
  (informer `ObservedAt`) is older than 15 s (`MaxBindingAge`,
  `internal/broker/tickets.go:73`).

### Ticket/lease security properties (broker)

- `IssueTicket` mints a `tkt_` + 256-bit opaque token; only SHA-256 is
  persisted (`launch_ticket.ticket_hash`). Tickets bind (workspace, tenant,
  owner, runtimeGeneration, runtimeUID, audience, takeover) and live 60 s.
- `RedeemTicket` consumes on first attempt (even expired), enforces the
  single active lease via the partial unique index, and a takeover
  supersedes the old lease **before** inserting the new one
  (`fencing_version` is monotonic per workspace).
- `RenewLease`/`ResolveTarget` require the holding gateway identity, a
  fence equal to the lease's pinned incarnation, and a fresh `Ready`
  binding — a recreated runtime (new `runtimeUID`) auto-fences old access.
- `RevokeTicket`/`RevokeLease` act only on the addressed row: an unknown or
  replayed revoke never touches a live session.

### Activity and revocation semantics

- **Server receipt time.** The broker stamps `receivedAt` from its own
  clock on every accepted activity event; any client timestamp is ignored.
- **Recorded state.** `workspace_activity` is keyed by
  `(workspace_id, runtime_generation)`: `input` refreshes `last_input_at`
  (the only signal that re-anchors the idle clock); `connected` increments
  `open_streams` and clears `disconnected_since` (a reconnect inside the
  grace window cancels the pending disconnect stop); `disconnect`
  decrements `open_streams` and anchors `disconnected_since` on the first
  transition to zero. Stale generation/runtimeUID/fencing events are
  refused — a dead incarnation can neither keep alive nor stop a newer
  runtime. `ReportActivity` does not enforce the sliding lease TTL: the
  idle/disconnect clocks keep working through a renew hiccup; a revoked,
  superseded or expired lease is refused.
- **Revoke.** `workspaces/{uid}/revoke` revokes every live lease bound to a
  generation `<= runtimeGeneration` (the operator's observed generation; a
  lease for a newer generation survives — stale teardown never fences a
  restarted runtime) and records a `workspace_revocation` row that blocks
  `IssueTicket`/`RedeemTicket` for covered generations. The fence is
  bounded server-side: a `runtimeGeneration` beyond the workspace's
  recorded generation plus one is refused — a revocation row is
  undeletable, so covering a generation that was never recorded would fence
  every future runtime permanently.
- **Drain.** `drain` returns the summed `open_streams` the gateways have
  reported. Stream state derives from the connected/disconnect activity
  events themselves — a gateway crash may leak a count, which the
  finalizer's bounded drain window absorbs by construction.
- **Expiry path.** `ExpiryPlanner.Scan` evaluates running workspaces
  (desired Running + phase Ready, `status.startedAt` anchor for the
  max-duration cap; idle anchored at last input — or at the generation's
  start when it never saw input). Deadlines become durable `stop_intent`
  rows via `RequestStop`, which re-checks the generation against the
  current binding — a stale generation is refused with no side effects.
  The periodic sweep drains those intents into the provisioning outbox:
  `desired_state`/`phase` flip to Stopped/Stopping and
  `provisioning.AppendIntent(IntentStop)` commit in one transaction, only
  while the workspace row still pins the intent's generation — expiry is
  never a direct CR mutation.

### Deployment

In the chart's (only) mode the session gateway runs inside the backend
process and calls the broker in process through `broker.LocalGateway`
(`internal/broker/gatewayshim.go`), so the operator is the only remote
client of this listener. A backend started with `-broker-url` (split/test
mode, `config.go:373`) is a remote gateway client of the lease routes; the
chart refuses `broker-url` in `backend.extraArgs`
(`deploy/helm/tinycdi/templates/_helpers.tpl` `denyExtraArgs`). The listener
always mounts both route families — the lease routes exist for split mode
and tests even though the chart never reaches them over the network.

## Consequences

- `cmd/backend` gains `-internal-listen` + `-internal-tls-cert/-key` +
  `-internal-client-ca` flags (env `TCDI_INTERNAL_*`); when unset the
  listener is disabled and remote clients cannot redeem (warned at
  startup).
- The public API gains `POST /v1/workspaces/{id}/connections`
  (`internal/api/connections.go`) returning `{workspaceId, ticket,
  launchUrl, expiresAt}`; the ticket never enters URLs or logs.
- The broker's `BindingSource` is a controller-runtime informer on
  Workspace CRs scoped to managed namespaces (`-tenant-namespaces`);
  credentials resolve from the per-workspace Secret `ws-<uid>-rt` read in
  the tenant's managed namespace only.
- In-process and wire callers share one `BrokerClient` contract, so the
  same lease rules are enforced identically in both topologies.
