// English message catalog. v0.2 is English-only (D33): every user-visible
// string lives here, keyed area.screen.element; placeholders are {name} and
// substituted by t() in ./index. Copy states what happened and the next
// step; no emoji. This file is append-only — each screen owns a key prefix,
// later tasks add keys under their own prefix and never rename existing ones.
export const en = {
  "app.notFound.title": "Not found",
  "app.notFound.body": "No page for {path}.",

  "auth.gate.apiUnreachable": "Could not reach the workspace API: {error}",
  "auth.gate.checking": "Checking session…",

  "common.cancel": "Cancel",

  "data.list.title": "Retained data",
  "data.list.intro":
    "Disks kept after their workspace was deleted with dataPolicy Retain. Purge destroys a disk permanently — Delete only removes a workspace.",
  "data.list.loading": "Loading…",
  "data.list.empty": "No retained disks.",
  "data.list.label": "retained data",
  "data.list.col.id": "ID",
  "data.list.col.workspace": "From workspace",
  "data.list.col.runtime": "Runtime",
  "data.list.col.size": "Size",
  "data.list.col.state": "State",
  "data.list.col.retainedAt": "Retained at",
  "data.list.size": "{size} GiB",
  "data.purge.action": "Purge",
  "data.purge.label": "purge retained disk",
  "data.purge.warning":
    "Purging {id} ({size} GiB from {name}) destroys the disk permanently. Type {name} to confirm.",
  "data.purge.confirmLabel": "Confirmation",
  "data.purge.confirm": "Purge permanently",
  "data.purge.confirming": "Purging…",

  "errors.banner.fallback": "ERROR",
  "errors.banner.requestId": "(request {id})",
  "errors.banner.retry": "Retry",
  "errors.banner.dismiss": "dismiss error",
  "errors.code.quotaExhausted":
    "Quota exhausted — delete an unused workspace or ask an administrator for more quota.",
  "errors.code.invalidTemplate":
    "That template is not available (unpublished or disallowed for your tenant). Pick another template.",
  "errors.code.idempotencyConflict":
    "The create request conflicted with a previous attempt. Review the list before trying again.",
  "errors.code.invalidState":
    "The resource is not in a state that allows this right now — it may have changed; refresh and retry.",
  "errors.code.connectionInUse":
    "Another session is already connected to this workspace.",
  "errors.code.forbidden": "You are not allowed to do that on this resource.",
  "errors.code.notFound":
    "This resource does not exist (or is not visible to you).",
  "errors.code.csrfFailed":
    "Your session token expired. Reload the page and try again.",
  "errors.code.unauthenticated":
    "Your session expired. You will be asked to sign in again.",
  "errors.code.generic":
    "The service could not complete the request; it is safe to retry.",

  "nav.newWorkspace": "New workspace",
  "nav.templates": "Template catalog",
  "nav.data": "Retained data",
  "nav.allWorkspaces": "← All workspaces",

  "templates.catalog.title": "Template catalog",
  "templates.catalog.loading": "Loading catalog…",
  "templates.catalog.empty": "No templates published.",
  "templates.catalog.revision": "({id} rev {revision})",
  "templates.catalog.field.runtime": "Runtime",
  "templates.catalog.field.experience": "Experience",
  "templates.catalog.field.resources": "Resources",
  "templates.catalog.resources": "{cpu}m CPU · {memory} MiB · {storage} GiB",
  "templates.catalog.field.dataPolicy": "Data policy default",
  "templates.catalog.field.clipboard": "Clipboard",
  "templates.catalog.create": "Create workspace",

  "workspaces.list.title": "Workspaces",
  "workspaces.list.loading": "Loading…",
  "workspaces.list.empty": "No workspaces yet.",
  "workspaces.list.count": "{n} workspaces",
  "workspaces.list.col.name": "Name",
  "workspaces.list.col.template": "Template",
  "workspaces.list.col.phase": "Phase",
  "workspaces.list.col.desired": "Desired",
  "workspaces.list.col.dataPolicy": "Data policy",
  "workspaces.list.waitingConnection": "(waiting for ConnectionReady)",
  "workspaces.list.manage": "Manage",

  "workspaces.detail.loading": "Loading workspace…",
  "workspaces.detail.field.id": "ID",
  "workspaces.detail.field.template": "Template",
  "workspaces.detail.template": "{name}@{revision} ({runtime} / {experience})",
  "workspaces.detail.field.desired": "Desired state",
  "workspaces.detail.field.dataPolicy": "Data policy",
  "workspaces.detail.field.failure": "Failure",
  "workspaces.detail.field.created": "Created",
  "workspaces.detail.field.updated": "Updated",
  "workspaces.detail.conditions.title": "Conditions",
  "workspaces.detail.action.start": "Start",
  "workspaces.detail.action.starting": "Starting…",
  "workspaces.detail.action.retryStart": "Retry start",
  "workspaces.detail.action.stop": "Stop",
  "workspaces.detail.action.stopping": "Stopping…",
  "workspaces.detail.connectStatus": "connect status",
  "workspaces.detail.connectUnavailable": "Connect unavailable: {reason}",
  "workspaces.detail.blocker.stopped": "desired state is Stopped",
  "workspaces.detail.blocker.connectionReady":
    "ConnectionReady={status} ({reason})",
  "workspaces.detail.blocker.noConnection":
    "no ConnectionReady condition reported yet",

  "workspaces.conditions.label": "conditions",
  "workspaces.conditions.empty": "No conditions reported yet.",
  "workspaces.conditions.col.type": "Type",
  "workspaces.conditions.col.status": "Status",
  "workspaces.conditions.col.reason": "Reason",
  "workspaces.conditions.col.message": "Message",
  "workspaces.conditions.col.since": "Since",

  "workspaces.create.title": "New workspace",
  "workspaces.create.loading": "Loading templates…",
  "workspaces.create.nameLabel": "Name",
  "workspaces.create.namePlaceholder": "research-desktop",
  "workspaces.create.templateLabel": "Template",
  "workspaces.create.templatePlaceholder": "Select a template",
  "workspaces.create.templateOption": "{name} (rev {revision}, {runtime})",
  "workspaces.create.dataPolicyLabel": "Data policy",
  "workspaces.create.dataPolicyDefault": "Template default",
  "workspaces.create.dataPolicyDefaultNamed": "Template default ({policy})",
  "workspaces.create.dataPolicyRetain": "Retain — keep disk on stop/delete",
  "workspaces.create.dataPolicyEphemeral":
    "Ephemeral — destroy data on stop/delete",
  "workspaces.create.startNow": "Start immediately",
  "workspaces.create.submit": "Create workspace",
  "workspaces.create.submitting": "Creating…",

  "workspaces.delete.action": "Delete",
  "workspaces.delete.label": "delete workspace",
  "workspaces.delete.confirm":
    "Delete {name}? Access is revoked and the runtime is removed.",
  "workspaces.delete.dataRetain":
    "Its disk moves to the retained inventory — it is not destroyed here (use Purge on the Retained data page for that).",
  "workspaces.delete.dataEphemeral":
    "Its data is Ephemeral and will be destroyed.",
  "workspaces.delete.confirmButton": "Confirm delete",
  "workspaces.delete.confirming": "Deleting…",

  "workspaces.connect.action": "Connect",
  "workspaces.connect.connecting": "Connecting…",
  "workspaces.connect.inUse.label": "session in use",
  "workspaces.connect.inUse.body":
    "Another session is already connected to this workspace. Taking over disconnects it.",
  "workspaces.connect.inUse.takeover": "Take over session",
} as const;
