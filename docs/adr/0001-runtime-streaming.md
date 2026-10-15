# ADR 0001 — Runtime streaming

Status: Accepted

## Context

Users open an interactive session to a workload on Kubernetes from the
browser: a Linux desktop, or a single locked-down browser window. One
workspace admits one authorized interactive connection at a time, and the
platform does not depend on the proprietary Kasm Workspaces product. A
Windows desktop needs a different streaming stack (RDP with NLA): the
RDP/Guacamole path is not implemented in v1.0.0 — the only runtime backend
is the Linux container (`internal/runtime/linux`).

## Decision

- **KasmVNC behind the session gateway.** The workspace pod runs upstream
  KasmVNC (pinned by digest) with TLS over its WebSocket transport —
  KasmVNC 1.5.0 answers `/websockify` only and has no HTTP tunnel fallback.
  KasmVNC sits behind the authenticating session gateway (ADR 0005): the
  gateway injects the runtime credential server-side and the browser never
  holds it.
- **In-pod endpoint.** The pod's streaming port is `:8443`, served by
  `tcdi-xferd` in front of KasmVNC on loopback; the same port carries the
  `/xfer/` file-transfer leg (ADR 0009). The contract a runtime image must
  meet is [docs/images.md](../images.md#runtime-contract); the shipped
  runtime images are `linux-base`, `linux-desktop`, `browser` and the
  `kasm-adapter` path under `build/`.
- **Ticket.** A launch is a single-use 60 s ticket (`TicketTTL`,
  `internal/broker/tickets.go`) minted by
  `POST /v1/workspaces/{id}/connections` and redeemed by a form POST to the
  workspace's own session host (ADR 0004). A ticket is consumed on the first
  redeem attempt — even an expired one — and a revoked ticket can never
  redeem.
- **Lease and fence.** A redeemed ticket becomes a connection lease with a
  30 s sliding TTL that the gateway renews (`LeaseTTL`, `tickets.go`).
  The lease is fenced to the runtime incarnation by
  `(workspaceUID, runtimeGeneration, runtimeUID)` plus a monotonic fencing
  version (ADR 0003): recreating the runtime changes `runtimeUID` and
  automatically fences every older ticket and lease.
- **One stream per lease.** A second claim on a live lease replaces the old
  stream (sequential replacement across replicas, ADR 0005); a claim while
  another handshake is in flight is refused `409 stream_in_use`
  (`internal/gateway/proxy.go`). A new ticket is a takeover: the old
  session is revoked before the new one activates.
- **Network boundary.** The runtime pod runs default-deny
  (`ws-<uid>-boundary`, `internal/runtime/linux/backend.go`): ingress
  only from the gateway pods, egress only to cluster DNS and the workspace
  template's approved network profile.

## Consequences

- The session gateway is the single mandatory authorization point; a
  runtime pod never trusts a direct client connection.
- A reconnect inside the same lease (reload) uses the session cookie and
  fences the previous stream — no new ticket. A reconnect after an
  incarnation replacement (`runtimeUID` changed) needs a fresh ticket; the
  existing 60 s ticket flow covers it, so no second mechanism exists.
- Windows desktops have no streaming path in v1.0.0. The ticket/lease/fence
  contract is independent of the streaming protocol, so it is the contract
  any further runtime stack sits behind.

## Alternatives considered

- **Every desktop as a KubeVirt VM (Linux included).** Stronger isolation,
  but slower boot and more RAM/disk per workspace — rejected for the
  container runtime.
- **Port KasmVNC to Windows.** Would mean maintaining a
  display/input/encoding stack on Windows — rejected.
- **Bridge guacd to KasmVNC as VNC/RFB.** KasmVNC's protocol differs from
  traditional RFB and compatibility was never assumed — rejected.

## References

- [KasmVNC](https://github.com/kasmtech/KasmVNC) ·
  [KasmVNC server-side auth](https://www.kasmweb.com/kasmvnc/docs/latest/serverside.html)
- [Guacamole architecture](https://guacamole.apache.org/doc/gug/guacamole-architecture.html)
  · [Guacamole custom auth](https://guacamole.apache.org/doc/gug/custom-auth.html)
