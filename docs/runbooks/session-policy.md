# Runbook — Tenant session policy (clipboard and file-transfer ceilings)

A tenant's **session policy** is the ceiling over what data may move
between a user's device and their workspace (ADR 0009). It has two
channels: the **clipboard** and **file transfer**.

## What the ceiling does

Each channel carries one value:

| Ceiling | Effect |
|---|---|
| `FollowTemplate` | the channel is not narrowed — each template's own policy applies (the default) |
| `Disabled` | off for every template in the tenant |
| `Send` | device → workspace only, where the template allows it |
| `Receive` | workspace → device only, where the template allows it |
| `Bidirectional` | both directions, where the template allows it |

The ceiling can only **narrow** a template's `clipboardPolicy`, never
widen it: the effective policy a user gets is the meet of the template's
and the ceiling — `Send ∩ Receive = Disabled`. A tenant with no
session-policy row behaves exactly as before (the upgrade default is
`FollowTemplate` everywhere), and setting every channel back to
`FollowTemplate` deletes the row.

For file transfer the ceiling values mean: `Upload` is device → workspace,
`Download` is workspace → device. The effective policy is the meet of the
template's `spec.fileTransferPolicy` (default `Disabled`) and the ceiling,
so `Upload ∩ Download = Disabled`, and a ceiling never widens a template.

## Change the ceiling

**Admin panel:** open **Admin → Session policy**
(`/admin/session-policy`), pick the clipboard ceiling, and save. The
page previews the effective policy per template — template policy ∧
ceiling — so the result is visible before and after the save.

**API:** `GET` then `PUT /v1/admin/tenants/{tenant}/session-policy` —
tenant-admin only, CSRF header plus `If-Match: <version>` from the GET
(`If-Match: *` creates the row for a tenant that has none; a stale
version answers `412 PRECONDITION_FAILED`). `fileTransfer` is
`{"mode": …, "maxFileBytes": …}` (see File transfer below). Every write is audited
(`admin.session_policy.set` / `admin.session_policy.clear`).

```bash
curl -X PUT "https://<portal>/v1/admin/tenants/<tenant>/session-policy" \
  -H "X-CSRF-Token: <token>" -H "If-Match: <version>" \
  -H "Content-Type: application/json" \
  -d '{"clipboard":"Disabled"}'
```

## File transfer

File transfer moves one file at a time between the browser and the
**Transfers** folder of the workspace home (`~/transfers`). The portal
session page shows an upload control and a download control only for
the directions the workspace's resolved policy allows; with `Disabled`
nothing is shown.

**Enable or disable it.**

1. Allow it on the template: set `spec.fileTransferPolicy` to `Upload`,
   `Download` or `Bidirectional` on the `WorkspaceTemplate` (default
   `Disabled`; `GET /v1/templates` shows it as `fileTransferPolicy`).
2. Check the tenant ceiling. With no row, or `fileTransfer.mode:
   FollowTemplate`, the template's policy applies. To turn file transfer
   off for the whole tenant, open **Admin → Session policy → File
   transfer** and choose `Disabled` (or `PUT` `{"fileTransfer":{"mode":
   "Disabled","maxFileBytes":null}}`). The preview table on that page
   shows the effective policy per template before you save.

**Size cap.** The per-file limit is the smaller of two values:

- the platform bound, chart value `backend.fileTransfer.maxFileBytes`
  (backend flag `-file-transfer-max-bytes`, default 104857600 = 100 MiB,
  allowed 1 to 10737418240);
- the tenant's `fileTransfer.maxFileBytes` (1 up to the platform bound;
  `null` means "use the platform bound").

An ingress controller that caps request bodies must allow at least the
platform bound on the session route (chart README, "Exposure").

**When a change takes effect.** Mint and claim re-resolve the policy for
every transfer, so a narrower ceiling or a lower cap blocks the next
transfer immediately, including on running workspaces. The in-pod helper
enforces the policy and cap it booted with (`TCDI_XFER_POLICY`,
`TCDI_XFER_MAX_BYTES`), so a **wider** policy or a higher cap reaches a
running workspace at its next start.

**Verify.** `kubectl -n <tenant-ns> get workspace <name> -o
jsonpath='{.spec.sessionPolicy}'` shows `fileTransfer` and `maxFileBytes`;
the audit log (`/admin/audit`, actions `file.transfer.upload` and
`file.transfer.download`) records every mint, claim and completion with
the phase, path, byte count and outcome. A file moves only once: each grant is
single use and expires after 2 minutes.

**Who can transfer.** Only the workspace owner can start a transfer; a
tenant administrator gets `404` on another user's workspace. The folder
is flat and there is no listing: a transfer names one file. A second
transfer on the same workspace is refused while one is streaming.

**Trust model.** The workspace user can read the `xfer-token` key and can
interfere with `tcdi-xferd`, because both run as the same user in the same
container; the checks that hold are the owner-only grant, the broker's
claim and the gateway's byte cap, so on the transfer leg a tampered
helper can mishandle only that workspace's own files within the allowed
direction and size. The file-transfer and clipboard policies bind an
unmodified runtime only: a user who replaces the in-pod `:8443` server
can move data either way over the desktop leg with no grant, byte cap or
transfer audit. See
[ADR 0009](../adr/0009-session-data-transfer-policy.md) for the design.

## When does a change take effect?

Two clocks, by design:

- **New connections — immediately.** The next launch ticket minted
  carries the resolved policy; the gateway's redirect flags and the
  session iframe's `allow` attribute follow it.
- **Running workspaces — at their next start.** The server-side KasmVNC
  clipboard enforcement is baked into the pod spec at boot
  (`TCDI_CLIPBOARD_POLICY`); pod specs are immutable, so a running
  workspace keeps its boot-time policy until it restarts. The residual
  exposure in that window is bounded by the template's own policy —
  never wider than what was allowed before the ceiling existed.

If a ceiling must take effect everywhere *now*, apply it: the admin page's
**Apply now** button (shown while a ceiling row exists and the form has no
unsaved edits) or `POST .../session-policy:apply`. Both restart running
workspaces whose applied policy is looser than the ceiling; home volumes
are kept and open desktops reconnect to the new policy.

1. **Dry run.** `{"dryRun": true}` restarts nothing and lists every running
   workspace that is looser than the stored ceiling, with its owner, the
   applied and the resulting policy, and whether a session is live. The
   portal always runs it before the confirm. The API does not enforce the
   order, so a scripted caller must run it itself.
2. **Confirm.** `{"dryRun": false}` restarts the listed workspaces, except
   those with a live session. Add `"includeActive": true` to restart those
   too; their users are disconnected.
3. **Limits.** One call lists and restarts at most 500 workspaces
   (`truncated: true` says more qualify; call again once they have
   restarted). A workspace whose earlier restart is not reconciled yet is
   reported `skipped_moved`, never restarted twice.
4. **Preconditions.** Tenant administrators only, CSRF token, and
   `If-Match` carrying the ceiling's current `ETag` (a stale tag is `412`;
   with no ceiling row the call is `409`).
5. **Audit.** Every call, dry or confirmed, is recorded as
   `admin.session_policy.apply` with `dryRun`, `includeActive`, `affected`
   and `restarted` counts.

```bash
etag=$(curl -sD- -o /dev/null "https://<portal>/v1/admin/tenants/<tenant>/session-policy" \
  -b cookies.txt | awk 'tolower($1)=="etag:"{print $2}' | tr -d '\r')
curl -X POST "https://<portal>/v1/admin/tenants/<tenant>/session-policy:apply" \
  -b cookies.txt -H "X-CSRF-Token: <token>" -H "If-Match: $etag" \
  -H "Content-Type: application/json" \
  -d '{"dryRun":true}'
```

Without Apply now, workspaces pick the ceiling up at their next stop and start.

## Verify

- `GET` the endpoint (or reload the admin page): `source` is
  `TenantRow` when a ceiling row exists, `FollowTemplate` when none
  does; `version` is the change token `PUT` echoes.
- Affected workspaces show `spec.sessionPolicy` on their `Workspace`
  custom resource at their next start (or right after an apply):

  ```bash
  kubectl -n <tenant-ns> get workspace <name> -o jsonpath='{.spec.sessionPolicy}'
  ```

- Tickets minted after the change record the resolved vector on the
  `launch_ticket.session_policy` column for audit.
