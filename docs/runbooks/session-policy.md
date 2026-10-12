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

If a ceiling must take effect everywhere *now*, ask users to stop and
start their workspaces; no platform-side restart action is triggered.

## Verify

- `GET` the endpoint (or reload the admin page): `source` is
  `TenantRow` when a ceiling row exists, `FollowTemplate` when none
  does; `version` is the change token `PUT` echoes.
- Affected workspaces show `spec.sessionPolicy` on their `Workspace`
  custom resource at their next start:

  ```bash
  kubectl -n <tenant-ns> get workspace <name> -o jsonpath='{.spec.sessionPolicy}'
  ```

- Tickets minted after the change record the resolved vector on the
  `launch_ticket.session_policy` column for audit.
