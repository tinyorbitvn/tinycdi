# ADR 0010 — Session data-transfer policy: tenant ceilings for clipboard and file transfer

Status: accepted (design review settled the policy model, the resolution
path, the propagation semantics, the admin API shape and the file-transfer
transport; implementation is planned as two change sets — T10 in v1.2.x on
top of the v1.2 clipboard fix, T11 in v1.3)

Date: 2026-10-12

## Context

The requested feature is: *a tenant administrator can enable and disable
clipboard and file transfer between a user's device and the workspace*.

Today the platform's data-transfer surface is:

- **Clipboard** — a per-template `spec.clipboardPolicy`
  (`Disabled|Send|Receive|Bidirectional`, direction semantics defined once
  in `internal/api/openapi.yaml`). As of the v1.2 clipboard fix the policy
  is enforced at three layers: the session server (operator stamps
  `TCDI_CLIPBOARD_POLICY` on the pod, both entrypoints render the KasmVNC
  `data_loss_prevention.clipboard` block — `-SendCutText`/`-AcceptCutText`/
  `-SendPrimary`), the gateway's post-redemption redirect
  (`clipboard_up`/`clipboard_down`/`clipboard_seamless`, driven by the
  policy recorded on the launch ticket), and the portal iframe `allow`
  attribute. Client flags are least-privilege hints; enforcement is the
  server's DLP flags (threat-model S40).
- **File transfer** — absent by design (architecture §6: "file transfer,
  drive mapping and recording are not in the MVP"). The runtime's KasmVNC
  account is deliberately **non-owner** (`kasmvncpasswd -w` without `-o`,
  `build/linux-base/entrypoint.sh`): owner rights unlock the management/API
  surface — port relay and the built-in upload/download endpoints — so a
  write-only user denies every one of those requests. The gateway's
  upstream path allowlist (`internal/gateway/authorization.go`) exposes
  only the stock client assets and `/websockify`; a file-management route
  would be denied there first anyway.

The natural place for an admin toggle — a field on `WorkspaceTemplate` —
is not available to a tenant administrator: templates are CRDs delivered
by the chart and managed through GitOps, the public surface offers only
`GET /v1/templates`, and any UI-side write would be reverted by the next
GitOps sync. Tenant-scoped, mutable-by-admin configuration in this
platform is Postgres-backed and exposed under `/v1/admin/tenants/{tenant}`
— exactly the shape of tenant quota (`tenant_quota`, `PUT` with If-Match
optimistic concurrency) and per-user running limits
(`tenant_user_limit_default`/`user_session_limit`, migration 022).

Two properties make this more than a settings page:

1. **Two control planes, one session.** The clipboard ceiling has to
   reach *two* consumers that read different stores: the API, which mints
   the launch ticket (client flags), and the operator, which renders the
   pod (server DLP env). A resolution split across them would let the
   ticket and the pod disagree.
2. **The gateway cannot write into the pod.** For file transfer, bytes
   must cross the workspace's network boundary — one ClusterIP service on
   TCP 8443, ingress-admitted only from gateway pods in the platform
   namespace (`buildNetPol`, `internal/runtime/linux/backend.go`). Any
   transport that needs the platform to *initiate* writes inside the
   tenant's pod is a new trust-boundary decision, not an implementation
   detail.

## Decision

### 1. Policy model — a tenant *ceiling*, never an override

The tenant policy is a **ceiling** applied over the template's own
policy, not a replacement for it. Templates stay the GitOps-owned
statement of what a workspace class may do; the tenant row can only
narrow it. There is no per-template override and no admin "force enable"
in v1 — a ceiling can never widen past the template.

Each channel uses the same direction lattice, named from the user's
point of view (the openapi.yaml convention):

```
            Bidirectional
              /        \
           Send       Receive        clipboard
          (Upload)   (Download)      fileTransfer
              \        /
             Disabled
```

`Disabled` is bottom, `Bidirectional` is top, the two single directions
are incomparable, and `meet(Send, Receive) = Disabled` (likewise
`Upload ∩ Download = Disabled`). The **effective policy is the meet of
the template policy and the tenant ceiling**:

| template \ tenant ceiling | FollowTemplate | Disabled | Send / Upload | Receive / Download | Bidirectional |
|---|---|---|---|---|---|
| any `T` | `T` | Disabled | meet(T, one-dir) | meet(T, one-dir) | `T` |

Concretely: `Bidirectional ∩ Send = Send`,
`Bidirectional ∩ Receive = Receive`, `Send ∩ Receive = Disabled`, and
`Disabled` met with anything is `Disabled`.

- **Channel values.**
  `clipboard ∈ {Disabled, Send, Receive, Bidirectional}` — the same enum
  as `spec.clipboardPolicy`.
  `fileTransfer.mode ∈ {Disabled, Upload, Download, Bidirectional}`:
  `Upload` = device → workspace home, `Download` = workspace → device.
  `fileTransfer.maxFileBytes` bounds a single transferred file (see §5).
- **`FollowTemplate` is the default and is represented by *no row* (or a
  NULL column).** A tenant with no session-policy row resolves to the
  template policy unchanged — **zero behaviour change on upgrade**, which
  is what keeps this additive.
- For `fileTransfer` to have a template leg to meet against, v1.3 adds
  `spec.fileTransferPolicy` to `WorkspaceTemplate` (same lattice, default
  `Disabled` — today's posture). A tenant ceiling of `Bidirectional` over
  a `Disabled` template still resolves `Disabled`: turning the feature on
  always takes a published template, and turning it off never does.

### 2. One resolution function, recorded result

A single backend function resolves the effective policy:

```go
// resolveSessionPolicy returns the effective data-transfer policy for a
// (tenant, template) pair: the meet of the template spec and the tenant
// ceiling row (absent = FollowTemplate). It is the ONLY place the meet
// is computed — consumers never re-implement the lattice.
func resolveSessionPolicy(ctx, tenantID string, tpl TemplateCatalogEntry) (SessionPolicy, error)
```

Both consumers read its result:

- **Ticket issue** (`internal/api/connections.go` → `broker.IssueTicket`).
  The existing `WithClipboardSource` lookup becomes a call into
  `resolveSessionPolicy`; the minted `launch_ticket` row records the
  **resolved** vector — the existing `clipboard_policy` column plus a new
  `session_policy` JSON column carrying the whole result — so the
  gateway's `desktopPath` flags reflect the ceiling and the ticket row
  itself shows what applied. The resolved vector is also returned on the
  connection response (additive field) so the portal renders the iframe
  `allow` from what the ticket actually recorded, not from the template
  it happened to read earlier.
- **Workspace CR → pod env.** The API stamps the resolved policy onto the
  Workspace CR it writes at create/start intents — a new optional
  `spec.sessionPolicy` field (v1alpha1, additive, CEL-validated; the CRD
  schema is explicitly outside the 1.0 stability contract). The
  operator's `buildPod` renders `TCDI_CLIPBOARD_POLICY` — and, with T11,
  `TCDI_XFER_*` — from `spec.sessionPolicy` when present, falling back to
  the snapshot's `spec.clipboardPolicy` for CRs that predate the field.
  An admin-authored annotation was rejected in favour of a spec field:
  spec is the API-authoritative input path the operator already trusts
  for `templateRef`/`desiredState`; annotations on Workspace objects are
  documented mirrors a workspace writer can touch (threat-model S30/S36),
  and pod-building inputs should not ride them.

The resolved value is therefore **recorded wherever a decision was
taken**: on the `launch_ticket` row (what the client was offered), on the
Workspace CR (what the runtime was built with), and — for T11 — on each
`session_file_transfer` row. Audit answers "which policy applied" from
the record, not by re-deriving it.

### 3. Propagation and tightening semantics

The ceiling's two clipboard legs propagate on different clocks; the ADR
fixes them so operators can reason about what "disable" means:

- **Client side is immediate for new connections.** Every minted ticket
  records the resolution at issue time, so a tightened ceiling reaches
  the next connection's `clipboard_*` flags and iframe `allow` with no
  restart. An *established* session keeps its already-negotiated client
  behaviour until it reconnects — nothing pushes mid-stream.
- **Server DLP lands at next start.** Pod specs are immutable and the DLP
  block renders once per boot from the pod env, so a running workspace's
  `SendCutText`/`AcceptCutText` stay where they were launched until the
  workspace stops and starts. This is the deliberate trade of the v1.2
  clipboard fix, unchanged: pod env is the only write path the runtime
  contract offers, and mutating a live session's VNC server is out of
  scope. Consequence, stated plainly: between the PUT and a workspace's
  next start, the server still permits the old policy — the client flags
  restrain an unmodified client only. An admin who cannot wait uses the
  existing controls (stop the workspace, or revoke leases) — **v1 adds no
  "restart affected workspaces" action**; if it is ever added it is a
  separate decision about lifecycle UX, not policy semantics.
- **File transfer is enforced per request at the backend.** A tightened
  ceiling refuses new transfer mints and new content requests
  immediately, independent of pod state — see §5. A stream admitted
  before the change runs to completion under the policy recorded on its
  transfer row (bounded by its size cap); the alternative — aborting
  in-flight bytes — buys a tighter bound on an already-audited,
  already-bounded transfer at the cost of mid-stream bookkeeping that v1
  does not need.

### 4. Admin API surface

One tenant-scoped resource, modelled on the quota routes
(`internal/api/adminquota.go`):

```
GET /v1/admin/tenants/{tenant}/session-policy   → 200 AdminSessionPolicyView
PUT /v1/admin/tenants/{tenant}/session-policy   → 200 AdminSessionPolicyView
```

- `GET` rides `RequireAuthPassive` inside the audit wrapper; `PUT` rides
  `RequireAuth` + `RequireCSRF` + audit — the same mounting as
  `MountAdminQuotaRoutes`. Both require the **tenant-admin** role on the
  named tenant; there is **no platform-level ceiling in v1** (a chart
  value bounding every tenant is a possible later addition, deliberately
  not specified here).
- `PUT` takes the same `If-Match` discipline as quota: the `version`
  token from the last `GET` (RFC3339Nano `updated_at`), or the literal
  `*` when the tenant has no row; a stale token answers
  `412 PRECONDITION_FAILED`. The write is a single conditional statement
  — the check and the write cannot be raced. Every write is audited.
- A PUT that sets every channel to `FollowTemplate` **deletes the row**:
  "no row" and "all-defaults row" are the same state, and the store keeps
  exactly one representation of it.
- Additive only: new routes, new schemas, no existing operation, field or
  enum value changes meaning (docs/versioning.md).
- Schema sketch (component names final in the implementing PR):

  ```yaml
  AdminSessionPolicyView:
    tenant: string
    clipboard:     FollowTemplate|Disabled|Send|Receive|Bidirectional
    fileTransfer:
      mode:        FollowTemplate|Disabled|Upload|Download|Bidirectional
      maxFileBytes: integer|null   # null = platform default bound; see §5
    source: TenantRow|FollowTemplate
    version: string                # present iff a row exists
  ```

The route name `session-policy` was chosen over `clipboard`, `dlp` or
`transfer-limits`: it names the resource (the tenant's session
data-transfer policy) rather than one channel or mechanism, and sits
naturally beside `/quota` and `/user-limits` under the same
`/v1/admin/tenants/{tenant}` prefix.

### 5. File transfer — how bytes reach and leave the home volume

Two transports were compared. **Adopt (i): a ticket-protected transfer
helper inside the workspace pod, behind the existing single-port
boundary.** Reject (ii): operator-mediated streams.

#### (i) In-pod transfer helper — adopted

A small helper process (`tcdi-xferd`), started by the runtime entrypoint
as the same non-root uid and shipped in the same image train, serves the
workspace's transfer endpoints over HTTPS. Constraints and how the design
meets them:

- **Single admitted port.** The NetworkPolicy admits gateway pods to TCP
  8443 only, and no second port is opened. The reference shape has the
  helper **own :8443** and reverse-proxy the desktop traffic to Xvnc,
  which the entrypoint rebinds to a loopback-only port — the pod's
  published contract (one HTTPS port, the same Secret cert the gateway
  already pins) stays byte-identical. The permitted variant, if the added
  in-pod hop measurably hurts the video path, is a second listener plus
  one added port line in the workspace's *own* ingress rule — the peer
  set (gateway pods in the platform namespace) is identical either way,
  so the boundary decision is the same and the choice is an
  implementation-time performance call, not a security one.
- **mTLS posture unchanged — server side.** The helper terminates TLS
  with the workspace's own mounted certificate (the same Secret
  `tls.crt`/`tls.key` the entrypoint already stages), so the gateway's
  pinned-CA upstream validation works unmodified. No new port, no new
  identity.
- **Non-owner KasmVNC posture unchanged.** The helper is a separate,
  purpose-bounded server; the VNC account stays write-only and KasmVNC's
  own upload/download and management endpoints stay denied (see
  "rejected" below).
- **Authorization is enforced at the backend, per request** — the
  immediate-tightening property of §3. Control-plane reachability is not
  part of the runtime egress contract (cluster DNS plus the template's
  profile; `Isolated` admits nothing else and `InternetOnly` excepts the
  cluster ranges), so the helper is not designed to verify tickets
  against the store itself and holds no way to do so:

  1. `POST /v1/workspaces/{id}/file-transfers` (session cookie + CSRF)
     resolves the effective policy **now**, checks direction and size
     against it, and creates a `session_file_transfer` row — opaque id,
     bound to (workspaceUID, runtimeGeneration, runtimeUID, principal,
     direction, declared path, declared size), `pending`, single-use,
     short TTL (minutes).
  2. The browser then issues the content request on the **session host**
     (`https://<ws-host>/v1/transfers/{id}/content` for downloads; the
     upload leg PUTs the body to the same route). The transfer id in the
     URL is an *identifier*, not a credential: authorization is the
     session cookie + live lease + the row's single-use claim, so nothing
     bearer-equivalent enters a URL, preserving the launch-ticket hygiene
     rule. The URL is itself the "download link" — single-use and
     expiring, safe to hand to the browser's download manager.
  3. The gateway claims the row atomically (single-use), re-checks the
     recorded lease binding **and re-resolves the effective policy** — a
     ceiling tightened between mint and claim refuses the content request
     too — then streams the body to/from the helper's
     `/xfer/` endpoint, injecting the workspace's transfer credential —
     a per-workspace Secret file mounted into the pod (same convention as
     the KasmVNC password) that the helper requires on every request. The
     credential is defence in depth against a *network* peer that is not
     the gateway; the NetworkPolicy already makes that only gateway pods.
  4. The helper path-jails under a fixed root inside the home volume
     (e.g. `$HOME/transfers/`), refuses `..`/absolute/symlink escapes,
     enforces the byte cap hard (env `TCDI_XFER_MAX_BYTES`, set at pod
     start from the resolved policy, as the *local floor* — the
     authoritative check ran at mint), writes to a temp file and renames,
     and answers the streamed request with bytes transferred.

- **Rate and size bounds.** The declared size is checked at mint; the
  streamed body is counted and aborted at `maxFileBytes`; the helper
  truncates hard at its env floor. `maxFileBytes` is tenant-settable
  within a platform bound (default in the tens-to-hundreds of MiB; exact
  constants are T11's to choose, the mechanism is what's decided here).
  Minting shares a per-principal token bucket, at most one transfer
  stream is active per workspace at a time, and pending rows expire
  unconsumed. In-flight streams count against the workspace's stream cap.
- **Both directions audited.** Mint, claim, completion and refusal each
  emit an audit event (`file.transfer.upload` / `file.transfer.download`)
  carrying actor, workspaceUID, direction, path name, declared and
  transferred byte counts, and the resolved policy — denials included, so
  a refused transfer is as visible as a completed one.
- **`dataPolicy` interaction — none new.** Transfer bytes land on the
  home volume, so they inherit the workspace's existing lifecycle:
  `Ephemeral` dies with the workspace, `Retain` persists on the disk and
  leaves with it into retained inventory, and purge deletes them with
  everything else — no transfer bytes live anywhere else (the helper
  spools only on-volume temp files). Storage consumed by uploads was
  already reserved against the workspace's PVC size at admission; the
  cap additionally keeps a single upload from filling the volume.
- **Stopped workspace.** The helper dies with the pod. New mints are
  refused (`INVALID_STATE` — a stopped workspace has no Ready
  incarnation, same gate as connection tickets); pending unconsumed rows
  expire; an in-flight stream dies with the pod. A `Retain` workspace's
  files are reachable again at next start — there is **no** path to pull
  bytes off a stopped retained disk through this feature; that is what
  the attach flow (`POST /v1/data/{id}/attach`) is for.
- **Non-goals (v1).** No malware scanning, no content inspection, no
  file-type allowlisting — this is a *transport* policy, and like the
  clipboard policy it is not a DLP system (architecture §6 already
  declines to call the clipboard one). Tenants needing content inspection
  front the traffic with their own DLP/AV tooling.

#### Threat analysis of (i)

| Threat | Posture |
|---|---|
| Direct pod access bypassing gateway checks | NetworkPolicy admits only gateway pods on the port; helper additionally requires the injected per-workspace credential — same boundary model as the desktop stream |
| Credential/cookie theft replaying a link | Transfer id is not a credential; claim needs the session cookie + live lease + unclaimed row; single-use atomic claim; short TTL |
| Tenant user exceeding policy mid-session | Mint resolves the policy fresh; claim re-resolves it, so a tightened ceiling refuses transfers minted before the change; an already-streaming transfer completes under the policy recorded at its claim, bounded by its size cap |
| Path escape into home/system | Jail root + lexical + symlink checks inside the helper, non-root uid, read-only rootfs posture where the adapter provides one |
| Disk exhaustion via upload | `maxFileBytes` at mint + counted stream + helper floor; PVC size already bounds the volume |
| Helper compromise | Runs as the session uid inside the same pod — its blast radius is the workspace it serves; it holds no cluster credentials (automount SA token off) and cannot reach the control plane (egress DNS+profile) |
| New image surface | One small Go process in images we already ship; the alternative adds a strictly wider privilege (below) |

#### (ii) Operator-mediated stream — rejected

Moving bytes through the operator (`pods/exec`/attach streaming `tar`/`cat`,
or a helper sidecar container) was rejected:

- **`pods/exec` is an arbitrary-code-execution grant.** RBAC for exec is
  all-or-nothing per namespace: an operator service account that can exec
  to move files can exec to do *anything* inside every tenant container.
  Today the operator manages objects and cannot open a shell in any of
  them; this feature would turn operator-credential compromise — or any
  confused-deputy bug in the exec-brokering path — into remote code
  execution inside tenant workloads. That is a materially wider
  privilege than the helper's single-purpose, single-workspace endpoint.
- **No per-request authorization.** The apiserver authorizes the
  *operator identity*, not the end user's direction/size/path intent; the
  platform would re-implement user-level checks inside an exec'd shell
  command, un-audited and un-bounded.
- **Bytes through the control plane.** SPDY/websocket streams through the
  apiserver carry multi-hundred-MiB files with the cluster's control
  plane in the middle — poor backpressure, timeouts, and audit that
  records "an exec happened", not "these bytes moved this direction".
- **Sidecar variants are worse still.** A second always-on container in
  every pod to serve a sometimes-feature (image surface, memory, and it
  cannot be added to an already-running pod, so tightening would need a
  restart the feature was designed to avoid); or an on-demand transfer
  pod mounting the PVC, which deadlocks against `ReadWriteOnce` volumes
  while the workspace runs — exactly when transfers must work.

#### Also rejected

- **KasmVNC's built-in upload/download.** Enabling it requires an
  *owner* kasmpasswd entry — the same grant that opens the management and
  **port-relay** surface the entrypoint deliberately closes. It cannot be
  split (no "files but not relay" privilege level), offers no
  per-request direction/size policy, and its audit is the DLP log the
  platform pins off. The posture stays exactly as shipped.
- **Client-side-only file transfer** (browser APIs over the stream).
  There is no channel: KasmVNC cut text is clipboard, not files, and any
  client-injected path is a hint, not enforcement — the same lesson S40
  taught for clipboard.

## Compatibility

- `/v1` HTTP API: additive only — two new admin routes (T10), one new
  transfer route family (T11), new optional fields on catalog/ticket
  views. No existing operation, field or enum changes.
- Chart values: none required for T10; T11 may add a bound for
  `maxFileBytes` — additive with a safe default if so.
- `Workspace`/`WorkspaceTemplate` CRDs: additive optional fields
  (`spec.sessionPolicy`, `spec.fileTransferPolicy`) at `v1alpha1` —
  outside the 1.0 stable-surface contract by design.
- Upgrade: no row ⇒ `FollowTemplate` ⇒ behaviour identical to today.
  Running workspaces are untouched; ceilings apply to new tickets and to
  the next start, per §3.

## Implementation plan

### T10 — tenant clipboard ceiling + admin toggle (v1.2.0/1.2.1)

Built on the merged clipboard fix (pod env `TCDI_CLIPBOARD_POLICY` + DLP
render). Scope: the `clipboard` channel end to end; `fileTransfer` fields
land in the schema as inert defaults so T11 is purely additive.

- **API sketch.** `GET`/`PUT /v1/admin/tenants/{tenant}/session-policy`
  as in §4; `AdminSessionPolicyView`/`PutAdminSessionPolicy` schemas and
  the `SessionPolicy` ceiling enums in `internal/api/openapi.yaml` +
  regenerated types; handler modelled on `adminquota.go`
  (passive-audited GET, CSRF+If-Match PUT, 412 on stale version,
  row-delete on all-`FollowTemplate`).
- **Migration sketch** (next free number):
  ```sql
  CREATE TABLE tenant_session_policy (
      tenant_id      text PRIMARY KEY,
      clipboard      text CHECK (clipboard IN
                         ('Disabled','Send','Receive','Bidirectional')),
      file_transfer  text CHECK (file_transfer IN
                         ('Disabled','Upload','Download','Bidirectional')),
      file_max_bytes bigint CHECK (file_max_bytes IS NULL OR file_max_bytes > 0),
      updated_at     timestamptz NOT NULL DEFAULT now()
  );
  -- NULL columns = FollowTemplate; the write path deletes the row when
  -- every ceiling column is NULL, so "no row" stays the single
  -- representation of the default.
  ```
  Plus `ALTER TABLE launch_ticket ADD COLUMN session_policy text` — the
  resolved vector recorded at mint (`clipboard_policy` keeps its existing
  meaning for the gateway redirect; the JSON column is the audit record).
  Migration → e2e run before push, per the repo's migration rule.
- **Resolution + propagation.** `resolveSessionPolicy` in the backend;
  `WithClipboardSource` rewired to it; `spec.sessionPolicy` added to the
  Workspace CRD (CEL enum), written by the applier at create/start;
  `buildPod` env prefers it over the snapshot field.
- **UI sketch.** A new "Session policy" tab in the admin section switcher
  (`ADMIN_SECTIONS`, e.g. `/admin/session-policy`, consistent with the
  existing Tabs pattern): a card with the clipboard select (five values
  incl. `FollowTemplate`), the file-transfer control rendered
  disabled/"available in v1.3" (T10 ships the read model, not the
  toggle), If-Match save flow like the quota page, en+vi strings
  (`admin.policy.*`), a11y + playwright smoke in both themes.
- **Tests.** Migration test; resolver meet-matrix unit tests (all
  template × ceiling pairs); handler tests (403 non-admin, CSRF,
  If-Match, delete-on-default, audit); ticket-records-resolved test;
  `buildPod` env precedence + pre-field CR fallback; openapi contract
  test; strings parity; playwright.
- **Docs to touch.** openapi.yaml; `docs/releases/v1.2.0.md` (additive
  API note per the contract); threat-model Boundary-6 clipboard bullet +
  a row for the ceiling/residual; a runbook or admin-docs section on the
  toggle; docs/README.md index if a runbook lands.

### T11 — file transfer + admin toggle (v1.3)

- **API sketch.** `spec.fileTransferPolicy` on WorkspaceTemplate (default
  `Disabled`, surfaced on `GET /v1/templates`); `fileTransfer` live in
  the session-policy schema; `POST /v1/workspaces/{id}/file-transfers`
  (mint) + session-host content route `GET|PUT
  /v1/transfers/{id}/content`; `session_policy` on the ticket already
  records the resolved mode.
- **Migration sketch.** `session_file_transfer` table (id, workspace,
  tenant, principal, generation+runtimeUID binding, direction, path,
  declared bytes, status `pending|claimed|completed|failed|expired`,
  resolved policy, bytes done, created/expires/claimed/completed
  timestamps); single-use = atomic `pending→claimed` conditional update.
- **Runtime/helper.** `tcdi-xferd` in `build/linux-base` (and the
  kasm-adapter contract): owns :8443 and proxies the desktop to loopback
  Xvnc (or the second-port variant per §5), requires the injected
  per-workspace transfer credential, jails to the transfer root, hard
  caps at `TCDI_XFER_MAX_BYTES`; pod env gains the resolved file-transfer
  values; `TCDI_CLIPBOARD_POLICY` posture unchanged.
- **Gateway.** Upstream allowlist gains the `/xfer/` prefix (through
  `cleanedProxyPath`); claim-and-proxy leg with the lease binding check;
  bytes counted against the cap and the per-workspace stream cap.
- **UI sketch.** The T10 "Session policy" tab's file-transfer controls go
  live; the session/workspace page gains upload/download affordances
  rendered from the recorded resolved policy (hidden when `Disabled`);
  en+vi strings; download uses the single-use content URL.
- **Tests.** Transfer-table migration test; mint/claim/expiry/single-use
  API tests; policy recheck-on-claim; cap enforcement at mint/stream/
  helper; helper jail + credential tests; gateway allowlist + proxy
  tests; entrypoint render contract (shellcheck-clean, `.github/tests`
  script like the clipboard one); kind smoke upload→download round-trip;
  strings parity; playwright.
- **Docs to touch.** openapi.yaml; `docs/releases/v1.3.0.md`; threat-model
  (new boundary bullets for the helper + audit rows); `docs/images.md`
  runtime contract (the `/xfer/` surface, env vars, credential
  convention); runbook for admin enable/disable + size cap; this ADR's
  status line.

## Consequences

- `docs/README.md` ADR index gains this entry.
- A tenant administrator can narrow clipboard per tenant without touching
  GitOps, and — from v1.3 — file transfer exists at all, behind a
  published-template opt-in plus a tenant ceiling.
- Accepted residuals, stated for the record: clipboard tightening lags a
  running session's server-side DLP until next start (§3); an in-flight
  transfer completes under its minted policy; file transfer is a
  transport policy, not content DLP; and the transfer helper adds one
  process to the runtime image surface (weighed above against the wider
  `pods/exec` alternative).
- Nothing in this ADR changes what a tenant can do *today*: every default
  resolves to the shipped behaviour.
