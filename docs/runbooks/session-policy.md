# Runbook — Tenant session policy (clipboard ceiling)

A tenant's **session policy** is the ceiling over what data may move
between a user's device and their workspace (ADR 0010). In v1.2 the only
live channel is the **clipboard**; the `fileTransfer` channel exists in
the schema for v1.3 and is inert.

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

## Change the ceiling

**Admin panel:** open **Admin → Session policy**
(`/admin/session-policy`), pick the clipboard ceiling, and save. The
page previews the effective policy per template — template policy ∧
ceiling — so the result is visible before and after the save.

**API:** `GET` then `PUT /v1/admin/tenants/{tenant}/session-policy` —
tenant-admin only, CSRF header plus `If-Match: <version>` from the GET
(`If-Match: *` creates the row for a tenant that has none; a stale
version answers `412 PRECONDITION_FAILED`). `fileTransfer` accepts only
its defaults in v1.2. Every write is audited
(`admin.session_policy.set` / `admin.session_policy.clear`).

```bash
curl -X PUT "https://<portal>/v1/admin/tenants/<tenant>/session-policy" \
  -H "X-CSRF-Token: <token>" -H "If-Match: <version>" \
  -H "Content-Type: application/json" \
  -d '{"clipboard":"Disabled"}'
```

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
   applied and the resulting policy, and whether a session is live.
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
