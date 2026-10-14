# ADR 0009 — Session data-transfer policy

Status: Accepted

## Context

Copying data in and out of a remote desktop — clipboard text and files —
is a data-loss channel: a session in a user's browser can move tenant
data to a device, or push content in, over the same stream that carries
the pixels. The templates that define a workspace class are
GitOps-managed CRDs, so the platform needs a separate, tenant-scoped
control: a ceiling over each template's data-transfer policy that a
tenant administrator can set and tighten, and that no template can
exceed.

## Decision

### §1 Policy model: a tenant ceiling

Two channels — clipboard and file transfer — each a direction lattice
named from the user's point of view: `Disabled` is the bottom,
`Bidirectional` the top, and the two single directions are incomparable
(clipboard `Send`/`Receive`, file transfer `Upload`/`Download`;
`Send ∩ Receive = Disabled`, `Upload ∩ Download = Disabled`).

The effective policy is the **meet** of the template's own policy and
the tenant ceiling — the ceiling only narrows, never widens
(`meet`, `internal/provisioning/sessionpolicy.go:58-59`):

| template \ tenant ceiling | FollowTemplate | Disabled | Send / Upload | Receive / Download | Bidirectional |
|---|---|---|---|---|---|
| any `T` | `T` | Disabled | meet(T, one-dir) | meet(T, one-dir) | `T` |

A missing ceiling row — or a NULL channel column — means
`FollowTemplate`: the channel is not narrowed (`PolicyFollowTemplate`,
`sessionpolicy.go:19-23`; `internal/store/sessionpolicy.go:16`). The
file-transfer ceiling's `maxFileBytes` narrows the same way: it may
lower, never raise, the platform bound `-file-transfer-max-bytes`
(`internal/backend/config.go:304`; chart
`backend.fileTransfer.maxFileBytes`, default `104857600`,
`deploy/helm/tinycdi/values.yaml:379`).

### §2 Resolution: one function, recorded result

One resolver — `resolveSessionPolicyTx`
(`internal/provisioning/sessionpolicy.go:126`) — computes the effective
policy for a (tenant, template) pair; consumers never re-implement the
lattice. The resolved vector is recorded wherever a decision is taken:
on the launch-ticket row (what the client was offered), on the
provisioning outbox intent and the Workspace CR's `spec.sessionPolicy`
(what the runtime is built with), and on each file-transfer row (what a
stream was admitted under). The runtime and audit read the record; they
never re-derive it.

### §3 When a change takes effect

A change reaches **new tickets immediately** — every minted ticket
records the resolution at issue time. A running workspace's
**server-side enforcement** changes at its next start: pod specs are
immutable and the posture renders once per boot from the pod env
`TCDI_CLIPBOARD_POLICY` (`internal/runtime/linux/backend.go:177`) and
the helper's direction and cap; until then the client flags restrain an
unmodified client only.

`POST /v1/admin/tenants/{tenant}/session-policy:apply` restarts, now,
the running workspaces that are looser than the ceiling. The body is
`{"dryRun": bool, "includeActive"?: bool}` with `dryRun` required
(`internal/api/adminsessionpolicyapply.go:40-44,90`); workspaces with an
active session are skipped unless `includeActive` is true; at most 500
affected workspaces are listed and restarted per call, the response
carrying `truncated` when more qualify (`maxPolicyApply`,
`adminsessionpolicyapply.go:17-20`). The call requires `If-Match` on the
ceiling row (ADR 0002). Each restart goes through `RestartWorkspace`
(`internal/provisioning/service.go`), the same fence an owner's `:start`
on a recoverable `Failed` workspace uses: it mints a new
`runtimeGeneration`, revokes the previous generation's leases and launch
tickets in one transaction, and is fenced on the generation the apply
observed (TM-16; [lifecycle-reasons.md](../lifecycle-reasons.md)).

### §4 Admin API

`GET`/`PUT /v1/admin/tenants/{tenant}/session-policy` — tenant-admin
role, strong ETag and `If-Match` optimistic concurrency per ADR 0002,
audited per ADR 0010 — and `:apply` (§3). Every ceiling row is
API-managed; unlike quota there is no config declaration
(`internal/api/adminsessionpolicy.go:95-97`).

### §5 File transfer

File transfer moves one file at a time, by name, in the flat folder
`~/transfers` of the workspace home (`internal/xferd/xferd.go:34`);
there is no listing endpoint.

- **Name rule.** A name is 1..255 bytes of valid UTF-8 with no `/`,
  `\`, `;`, `%`, `..`, NUL or control character, and not `.` — required
  at mint (`validTransferName`, `internal/api/filetransfers.go:86-100`)
  and again in the helper (`internal/xferd/xferd.go:85-90`).
- **Who mints.** Only the workspace owner mints a transfer — a tenant
  admin acts on others' workspaces but never moves their files
  (`filetransfers.go:146-148`). The mint route sits behind the
  session-keyed rate limit that sign-out-everywhere uses
  (`filetransfers.go:68-72`).
- **Row lifetimes** (`internal/store/filetransfer.go:15-20`): a minted
  row waits at most 2 minutes for its claim (`FileTransferTTL`), a
  claimed row counts as a live stream for at most 30 minutes
  (`FileTransferClaimDeadAfter`), and finished rows are kept 7 days for
  audit joins (`FileTransferKeep`).
- **Authoritative check.** The broker's `ClaimTransfer`
  (`internal/broker/transfers.go:60-86`) spends a minted grant in one
  conditional update after checking: the lease is live and bound to the
  current incarnation; the grant belongs to the lease's workspace,
  principal and incarnation; the method matches the direction; no other
  transfer stream is live on the workspace; and the policy in force
  *now* still allows the direction and the declared size — a ceiling
  tightened between mint and claim refuses the content request too.
  Success and every refusal are audited in the same transaction, so a
  failed audit insert fails the claim: fail closed.
- **Gateway leg.** The gateway serves `/v1/transfers/{id}/content` on
  the workspace's own session host (`transferPathRE`,
  `internal/gateway/transfer.go:22-32`) — the id is an identifier, not
  a credential — and builds the upstream `/xfer/files/<name>` URL
  itself from the resolved target (`transfer.go:163-168`). The generic
  desktop proxy answers `404` for `/xfer` and for KasmVNC's own
  upload/download paths (`TestTransfer_DesktopProxyStillRefusesXfer`,
  `internal/gateway/transfer_test.go:228`).
- **Browser side.** Downloads use a credentialed CORS `fetch` and then
  a blob with `<a download>` (`web/src/session/transfer.ts:55-90`); a
  top-level navigation would not carry the `Partitioned` session
  cookie. The helper refuses a file larger than the cap.

#### (i) In-pod transfer helper

`tcdi-xferd` owns the pod's only exposed port `:8443` — the
NetworkPolicy admits gateway pods to that one port — and
reverse-proxies the desktop to KasmVNC on `127.0.0.1:8444`, so the
pod's published contract (one HTTPS port, the workspace's own Secret
cert) is unchanged. On `/xfer/` it checks `X-Tcdi-Xfer-Token` in
constant time against the Secret key `xfer-token`
(`internal/xferd/xferd.go:28,146`;
`internal/runtime/linux/backend.go:810`), re-applies the name rule,
enforces the size cap (`xferd.go:198`) and opens names through an
`os.Root` jail on `~/transfers` (`xferd.go:125-140`). A desktop request
without Basic credentials gets the helper's own `401`
(`xferd.go:102-109`), and the gateway's forwarding headers pass through
to KasmVNC unchanged (`xferd.go:63-75`).

#### Trust model

`xfer-token` is readable by the workspace user (the Secret is mounted
0440 with fsGroup 1000, and the desktop runs as uid 1000;
`backend.go:1103,1237,1239`), and `tcdi-xferd` runs as that same user.
A workspace user can therefore read the token and could replace the
helper on `:8443`. The token only proves "the gateway or a process in
this pod"; the broker claim, which a pod cannot mint or forge, is what
authorizes a transfer, and the jail and cap protect the user from the
gateway, not the platform from the user.

## Consequences

- Tightening works on the two clocks of §3: new tickets immediately,
  running workspaces at next start or on `:apply`.
- A looser server-side policy persists on a running workspace until
  restart or `:apply`; until then the client flags restrain an
  unmodified client only.
- Tab memory holds a whole download up to the cap before the browser
  writes it out.
- The trust model above: the transfer credential is readable inside the
  pod it protects; authorization lives at the broker claim.

## Alternatives considered

- **Operator-mediated stream** (`pods/exec` or a sidecar): exec RBAC is
  an arbitrary-code-execution grant on every tenant container, bytes
  ride the control plane, and there is no per-request user
  authorization.
- **KasmVNC's built-in upload/download**: it needs an owner kasmpasswd
  entry, which also opens the management and port-relay surface the
  runtime deliberately closes.
- **Client-side-only transfer**: there is no file channel in the
  client, and a hint is not enforcement.
- **A second pod port for the helper**: the NetworkPolicy admits one
  port, so the helper owns `:8443` and proxies the desktop instead.
- **A top-level download navigation**: it would miss a `partitioned`
  session cookie, so downloads are a credentialed `fetch` plus blob.
