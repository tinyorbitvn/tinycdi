# ADR 0007 — KasmVNC endpoint brute-force posture (S18)

Status: **accepted — no build required** (analysis note; closes S18)

Date: 2026-10-05

## Decision

Nothing to build. The open half of threat-model item S18 — "whether
pod-side brute-force protection is meaningful behind the authenticating
gateway" — resolves as: **yes, it is correctly keyed and works, but the
attack it defends against is unreachable by construction.** No client
class — unauthenticated, other-tenant, or the workspace's own lease
holder — can present even one credential guess to the KasmVNC endpoint.
Recommendation: keep the existing controls unchanged (option A); S18 is
closed.

## Context

KasmVNC authenticates at its HTTPS layer: HTTP Basic against a
`kasmvncpasswd` file (`-SecurityTypes None` at the RFB layer — the VNC
port is never published and carries no auth). Because the gateway
terminates the client and re-originates upstream, the brute-force
question splits into two: can a client reach the endpoint at all, and
can a reaching client influence the credential KasmVNC checks?

## Attack surface

- **Endpoint** — pod `:8443`, HTTPS websocket only (no HTTP tunnel
  fallback in KasmVNC 1.5.0), ClusterIP Service `ws-<uid>.<ns>.svc`.
  KasmVNC 1.4.x also binds `:8443/udp` with no off-switch; the
  NetworkPolicy's TCP-only ingress means nothing can reach it
  (`build/kasm-adapter/entrypoint.sh` policy note).
- **One account** — `kasm_user`, write-only/non-owner
  (`kasmvncpasswd -w`, never `-o`): owner rights are what unlock the
  `/api/*` management surface, so those routes deny every request even
  if reached (`build/linux-base/kasmvnc.yaml`,
  `build/kasm-adapter/entrypoint.sh:80-90`). The gateway path allowlist
  never forwards `/api/*` regardless.
- **Credential** — 24 random bytes → base64url (~144-bit), generated per
  workspace into Secret `ws-<uid>-rt`, immutable for the workspace's
  life (`generateCredentials`,
  `internal/runtime/linux/backend.go:610-650`). It is never logged and
  never exposed to the client.

## Reachability

**Direct pod path: none.** Per-workspace NetworkPolicy
`ws-<uid>-boundary` is default-deny with ingress TCP:8443 only from
`role=gateway` pods in the platform namespace (SEC-29; `buildNetPol`,
`internal/runtime/linux/backend.go:1172-1258`; `backend_netpol_test.go`).
CNI enforcement is the same assumption the rest of the boundary already
rests on.

**Via the gateway** (`serveProxy`, `internal/gateway/proxy.go`):

1. `ticket_in_query` rejected outright.
2. `cleanedProxyPath` + `upstreamPathOK` — cleaned-path allowlist of the
   stock client assets and `/websockify` only; no `/api/*`, no login or
   management endpoints; traversal and parser-differential encodings die
   first (SEC-20, `internal/gateway/authorization.go:474-522`).
3. `lookupSession` — a valid session cookie resolving to a live lease
   (renewed ~10 s, fail-closed on revoke/broker-loss). No cookie → 401.
4. Workspace host binding — a lease minted for a different `workspaceUID`
   answers 401 as absent (`host_mismatch` audit, D11; the cookie is
   host-only so a browser cannot even present it cross-host).

The session cookie itself exists only after redeeming a one-use 60 s
launch ticket, which requires an authenticated portal POST `/v1/launch`
behind the login session, CSRF, and the S14 app-layer rate limit
(ADR 0006). So:

- **Unauthenticated client** → 401 at step 3; never reaches upstream.
- **Other-tenant / other-workspace lease holder** → 401 at step 4.
- **Same-workspace lease holder** → proxied — but cannot attempt
  credentials (below).

## Credential attempts — the crux

`direct()` rewrites every proxied request: `Authorization` is **set** to
the broker-resolved per-workspace credential (or deleted when the target
carries none — fail closed), and `Cookie` is deleted
(`internal/gateway/proxy.go:778-806`). Pinned by
`TestProxy_StripsClientAuth` (`internal/gateway/proxy_test.go:362-396`).

KasmVNC 1.5.0's HTTPS auth is Basic only — there is no query-param,
token, or form credential channel server-side. Query strings do pass
through to upstream, but the only parameter with wire meaning is the
gateway's own `tcdi_tab` stream-owner correlator, explicitly "an opaque
correlator, never a credential" (`internal/gateway/authorization.go`).
Nothing the client sends can carry a password guess.

Net: **zero client-influenced credential attempts can reach KasmVNC.**
And even hypothetically, the lease holder already possesses exactly the
access the pod password gates — guessing it would buy nothing.

## Pod-side protection — meaningful?

- **Configured:** `security.brute_force_protection` threshold 5 /
  timeout 10 (KasmVNC schema: initial seconds; upstream defaults) in
  `build/linux-base/kasmvnc.yaml:74-77` and in the adapter-written
  `~/.vnc/kasmvnc.yaml` — rewritten every boot so the persistent home
  can never carry a weaker policy (`build/kasm-adapter/entrypoint.sh:188-190`).
- **Correctly keyed:** KasmVNC takes the client address from
  `X-Forwarded-For` when present. The gateway rebuilds
  XFF/`Forwarded`/`X-Real-IP` from `ratelimit.ClientKey` — the socket
  peer, or the right-most untrusted entry when the peer is inside
  `TrustedProxies` (`rewriteForwarded`, `internal/gateway/proxy.go:830`;
  FX-R26, `internal/gateway/forwarded_test.go`). The blacklist therefore
  keys on the derived real client: a spoofed XFF can neither launder an
  attacker behind the gateway IP nor pin the blacklist onto a victim.
- **Meaningful but near-inert:** the mechanism is proven live — the
  readiness-probe contract tests exist precisely because anonymous
  probes DO trip the blacklist (`ReadinessProbeIsNotAnAuthFailure` in
  `tests/integration/linux_runtime_test.go` and
  `tests/integration/kasm_adapter_test.go`). But through the only
  ingress, the injected credential is always the single valid one, so a
  client cannot induce failures at all. The blacklist's residual value
  is defense-in-depth: a credential-injection regression, a
  NetPol-bypassing cluster-internal path, or any future ingress would
  meet a correctly keyed blacklist rather than a gateway-IP-keyed one a
  single actor could weaponize to DoS the whole workspace.

## Is there a real gap?

**No.** Repeated credential attempts need a credential channel plus
reachability; the architecture gives no client either. The secrets a
client CAN guess (session cookie, launch ticket, lease-bound tokens)
are the gateway's own auth surface — covered by S2/S3, one-use 60 s
tickets, digest-bound leases, and the S14 rate limit — not KasmVNC's.

## Options considered

- **A — nothing needed (recommended).** Cost: zero. The 5/10 blacklist
  stays as free defense-in-depth; `TestProxy_StripsClientAuth` and the
  FX-R26 forwarded-chain tests already pin the two load-bearing
  invariants (credentials server-injected, client address derived).
- **B — gateway per-lease failed-auth limiter** (count upstream 401s per
  session, kill after N). Cost: new per-session response accounting on
  the hijacked-stream path plus a kill trigger. Guards a corner that
  cannot occur: the injected credential is immutable per workspace, and
  a mid-lease Secret rewrite would still fail closed, not leak guesses.
  Marginal benefit, real complexity.
- **C — per-lease request rate limit on the session path.** Bounds
  asset-fetch churn and bandwidth, not credential attempts — there is
  no credential vector to limit. Revisit only if upstream request
  volume becomes a DoS concern; today one lease's traffic can only load
  its own pod.
- **D — drop the pod blacklist.** Rejected: it costs nothing and covers
  the NetPol-bypass / future-ingress hypotheticals.

## Recommendation

Option A. Close S18; no code changes. Keep `brute_force_protection`
5/10 and the pinning tests. Re-open this question only if a credential
channel other than Basic auth is ever enabled on KasmVNC, or a
non-gateway ingress is ever added to the workspace NetworkPolicy.

## Consequences

- `docs/security/threat-model.md` S18 marked closed, referencing this
  ADR; the Boundary-5 open-questions line drops the S18 remainder.
- No new components, state, flags, or config.
