// Admin-area messages (T3.6). Spread into the catalog by i18n/en.ts;
// keys follow the `area.screen.element` convention. Copy states what
// happened and the next step; no emoji (D33).

export default {
  // Section nav and access gate.
  "admin.nav.ariaLabel": "Admin sections",
  "admin.nav.overview": "Overview",
  "admin.nav.workspaces": "Workspaces",
  "admin.nav.quota": "Quota",
  "admin.nav.templates": "Templates",
  "admin.eyebrow": "Admin",
  "admin.eyebrow.tenant": "Admin · {tenant}",
  "admin.gate.checking": "Checking permissions",
  "admin.gate.title": "Tenant administrators only",
  "admin.gate.body":
    "Your account does not have the tenant-admin role. Ask an administrator if you need access.",
  "admin.gate.back": "Back to my workspaces",

  // Route titles (document title suffixes).
  "admin.route.overview": "Tenant overview",
  "admin.route.workspaces": "Tenant workspaces",
  "admin.route.quota": "Quota",
  "admin.route.templates": "Template catalog",

  "admin.action.refresh": "Refresh",

  // Overview page.
  "admin.overview.title": "Tenant overview",
  "admin.overview.description": "Capacity and health of every workspace in the tenant.",
  "admin.overview.loading": "Loading overview",
  "admin.overview.capacity": "Capacity",
  "admin.overview.usageByUser": "Usage by user",
  "admin.overview.byPhase": "Workspaces by phase",
  "admin.overview.allWorkspaces": "All workspaces",
  "admin.overview.empty": "No workspaces in this tenant yet.",
  "admin.overview.failed.one": "{n} failed workspace",
  "admin.overview.failed.other": "{n} failed workspaces",
  "admin.overview.failedAgo": "failed {age}",

  // Quota page.
  "admin.quota.title": "Quota",
  "admin.quota.description": "Tenant limits, current usage and who is using it.",
  "admin.quota.loading": "Loading quota",
  "admin.quota.limits.title": "Tenant limits",
  "admin.quota.limits.description": "Current usage against the limits of tenant {tenant}.",
  "admin.quota.users.title": "Usage by user",
  "admin.quota.users.caption": "Usage by user",
  "admin.quota.users.empty": "No usage recorded.",
  "admin.quota.column.user": "User",
  "admin.quota.amount.workspaces": "Workspaces",
  "admin.quota.amount.runningWorkspaces": "Running workspaces",
  "admin.quota.amount.cpuMillicores": "CPU",
  "admin.quota.amount.memoryMib": "Memory",
  "admin.quota.amount.storageGib": "Storage",
  "admin.quota.atLimit": "at limit",
  "admin.quota.meter": "{used} of {limit} ({pct}%)",
  "admin.quota.notConfigured":
    "No quota configured. New workspaces are refused until a tenant quota is set.",
  "admin.quota.noLimit": "{used} used · No limit",
  "admin.quota.source.config": "Platform configuration",
  "admin.quota.source.api": "Admin API",
  "admin.quota.source.none": "Not configured",
  "admin.quota.source.configNote":
    "These limits are declared in the platform configuration and can only change there.",
  "admin.quota.source.apiNote": "These limits were set through the admin API.",
  "admin.quota.source.noneNote":
    "No quota is set. New workspaces are refused until limits are saved.",
  "admin.quota.edit.action": "Edit limits",
  "admin.quota.edit.title": "Set tenant limits",
  "admin.quota.edit.description":
    "Limits below current usage are allowed: running workspaces keep their reservations and new ones are refused.",
  "admin.quota.edit.field.runningWorkspaces": "Running workspaces",
  "admin.quota.edit.field.cpu": "CPU (vCPU)",
  "admin.quota.edit.field.memory": "Memory (GiB)",
  "admin.quota.edit.field.storage": "Storage (GiB)",
  "admin.quota.edit.save": "Save limits",
  "admin.quota.edit.cancel": "Cancel",
  "admin.quota.edit.invalid":
    "Enter numbers of 0 or more; running workspaces and storage must be whole numbers.",

  // Per-user limits section (running-workspace cap per principal).
  "admin.userLimits.title": "Per-user limits",
  "admin.userLimits.description":
    "How many workspaces one user may run at once, on top of the tenant limits.",
  "admin.userLimits.loading": "Loading per-user limits",
  "admin.userLimits.caption": "Per-user running limits",
  "admin.userLimits.empty": "No users yet.",
  "admin.userLimits.default": "Default per-user limit:",
  "admin.userLimits.unlimited": "Unlimited",
  "admin.userLimits.inherit": "Default",
  "admin.userLimits.column.running": "Running",
  "admin.userLimits.column.limit": "Override",
  "admin.userLimits.column.effective": "Effective",
  "admin.userLimits.editDefault": "Set default",
  "admin.userLimits.edit.action": "Set limit",
  "admin.userLimits.edit.userTitle": "Running limit — {user}",
  "admin.userLimits.edit.defaultTitle": "Default per-user limit",
  "admin.userLimits.edit.descriptionUser":
    "Most workspaces this user may run at once. Leave empty to inherit the tenant default.",
  "admin.userLimits.edit.descriptionDefault":
    "Most workspaces each user may run at once. Leave empty for no default limit.",
  "admin.userLimits.edit.field": "Running workspaces",
  "admin.userLimits.edit.save": "Save limit",
  "admin.userLimits.edit.invalid": "Enter a whole number of 0 or more, or leave empty.",

  // Template catalog page.
  "admin.templates.title": "Template catalog",
  "admin.templates.description":
    "Templates published to this tenant and how many workspaces use each. Templates are published as WorkspaceTemplate resources by platform administrators.",
  "admin.templates.caption": "Template catalog",
  "admin.templates.empty": "No templates are published to this tenant.",
  "admin.templates.column.template": "Template",
  "admin.templates.column.kind": "Kind",
  "admin.templates.column.resources": "Resources",
  "admin.templates.column.image": "Image",
  "admin.templates.column.policy": "Policies",
  "admin.templates.column.lifecycle": "Idle / grace / max",
  "admin.templates.column.usage": "Workspaces",
  "admin.templates.column.published": "Published",
  "admin.templates.revision": "r{n}",
  "admin.templates.usageLabel": "{inUse} workspaces, {running} running",
  "admin.templates.usage.running": "({n} running)",
  "admin.templates.badge.data": "Data: {value}",
  "admin.templates.badge.clipboard": "Clipboard: {value}",
  "admin.templates.badge.network": "Network: {value}",
  "admin.templates.imageStale": "Stale",
  "admin.templates.imageStaleHint": "The runtime image is older than the freshness SLO.",
  "admin.templates.imageBlocked": "Blocked",
  "admin.templates.imageBlockedHint":
    "The runtime image is over the block limit — creates and starts are refused (409 IMAGE_STALE).",
  "admin.templates.engine.chromium": "Chromium",
  "admin.templates.engine.firefox": "Firefox ESR",
  "admin.templates.imageUnknown": "Unknown",

  // Tenant workspaces page.
  "admin.workspaces.title": "Tenant workspaces",
  "admin.workspaces.description": "Every workspace in the tenant, across all users.",
  "admin.workspaces.caption": "Tenant workspaces",
  "admin.workspaces.filter.label": "Filter",
  "admin.workspaces.filter.placeholder": "Name, owner, template or ID",
  "admin.workspaces.phase.label": "Phase",
  "admin.workspaces.phase.all": "All phases",
  "admin.workspaces.truncated":
    "Showing the first {n} workspaces; narrow the phase filter to see the rest.",
  "admin.workspaces.empty.filtered": "No workspaces match the filters.",
  "admin.workspaces.empty.all": "No workspaces in this tenant yet.",
  "admin.workspaces.shown": "{shown} of {total} workspaces shown",
  "admin.workspaces.column.workspace": "Workspace",
  "admin.workspaces.column.owner": "Owner",
  "admin.workspaces.column.template": "Template",
  "admin.workspaces.column.phase": "Phase",
  "admin.workspaces.column.age": "Age",
  "admin.workspaces.column.actions": "Actions",
  "admin.workspaces.action.stop": "Stop",
  "admin.workspaces.action.delete": "Delete",
  "admin.workspaces.action.stopLabel": "Stop {name}",
  "admin.workspaces.action.deleteLabel": "Delete {name}",
  "admin.workspaces.notice.stop": "Stop requested for {name} ({owner}).",
  "admin.workspaces.notice.delete": "Delete requested for {name} ({owner}).",
  "admin.workspaces.stop.title": "Stop {name}?",
  "admin.workspaces.stop.fallbackTitle": "Stop workspace",
  "admin.workspaces.stop.confirm": "Stop workspace",
  "admin.workspaces.stop.body":
    "The workspace belongs to {owner}. Stopping it ends any open session immediately; unsaved work in the desktop is lost.",
  "admin.workspaces.stop.retain": "Its disk is kept and the owner can start it again.",
  "admin.workspaces.stop.ephemeral": "Its data is ephemeral and is discarded on stop.",
  "admin.workspaces.delete.title": "Delete {name}?",
  "admin.workspaces.delete.fallbackTitle": "Delete workspace",
  "admin.workspaces.delete.confirm": "Delete workspace",
  "admin.workspaces.delete.body":
    "This removes the workspace owned by {owner} and revokes access.",
  "admin.workspaces.delete.retain":
    "Its disk moves to the retained data inventory; purge it there to destroy it.",
  "admin.workspaces.delete.ephemeral": "Its data is ephemeral and will be destroyed.",
  "admin.workspaces.delete.typeConfirm": "Type {name} to confirm",
  "admin.workspaces.dialog.cancel": "Cancel",

  // Error guidance (stable code -> next step; server messages are detail).
  "admin.errors.title": "Error",
  "admin.errors.retry": "Retry",
  "admin.errors.requestId": "Request ID: {id}",
  "admin.errors.forbidden":
    "You do not have permission for this. Tenant-wide views need the tenant-admin role.",
  "admin.errors.notFound": "It no longer exists or is not visible to you. Refresh the list.",
  "admin.errors.invalidState":
    "Its state changed and no longer allows this. Refresh and try again once it settles.",
  "admin.errors.invalidRequest":
    "The request was rejected. If you were confirming a purge, reopen the dialog to get a fresh confirmation.",
  "admin.errors.invalidTemplate":
    "That template is unavailable or does not match the disk's runtime. Pick another template.",
  "admin.errors.quotaExhausted": "Quota exhausted. Free resources or raise the quota first.",
  "admin.errors.quotaNotConfigured": "No quota is configured for your tenant. Ask an administrator to set one.",
  "admin.errors.quotaManagedByConfig":
    "This tenant's quota is managed by the platform configuration and cannot be changed here.",
  "admin.errors.preconditionFailed":
    "Someone else changed these limits. The current values were reloaded — review them and save again.",
  "admin.errors.idempotencyConflict":
    "This conflicted with an earlier attempt. Refresh and check before retrying.",
  "admin.errors.csrfFailed": "Your session token expired. Reload the page and try again.",
  "admin.errors.unauthenticated": "Your session expired. Reload the page to sign in again.",
  "admin.errors.default": "The service could not complete the request. It is safe to retry.",

  // Compact time and unit formats.
  "admin.time.justNow": "just now",
  "admin.time.ago": "{age} ago",
  "admin.time.s": "{n}s",
  "admin.time.m": "{n}m",
  "admin.time.h": "{n}h",
  "admin.time.hm": "{h}h {m}m",
  "admin.time.d": "{n}d",
  "admin.time.dh": "{d}d {h}h",
  "admin.unit.vcpu": "{n} vCPU",
  "admin.unit.mib": "{n} MiB",
  "admin.unit.gib": "{n} GiB",
  "admin.unit.tib": "{n} TiB",
} as const;
