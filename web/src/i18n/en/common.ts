// Shared strings: cross-screen copy (dialogs, the error banner). Owned by
// T3.2 after T3.3 seeds it; append-only.
export default {
  "common.cancel": "Cancel",
  "common.close": "Close",
  "common.dismiss": "Dismiss",
  "common.toast.dismiss": "Dismiss notification",
  "common.toast.errors": "Error notifications",
  "common.toast.notifications": "Notifications",

  "errors.banner.fallback": "ERROR",
  "errors.banner.requestId": "(request {id})",
  "errors.banner.retry": "Retry",
  "errors.banner.dismiss": "dismiss error",
  "errors.code.quotaExhausted":
    "Quota exhausted — delete an unused workspace or ask an administrator for more quota.",
  "errors.code.quotaReleasePending":
    "A workspace is still shutting down; its quota is released within about 30 s. Try again in a moment.",
  "errors.code.quotaNotConfigured":
    "No quota is configured for your tenant. Ask an administrator to set one.",
  "errors.code.userLimitReached":
    "You have reached your running-workspace limit ({current} of {limit}). Stop or delete a workspace, or ask an administrator to raise your limit.",
  "errors.code.invalidTemplate":
    "That template is not available (unpublished or disallowed for your tenant). Pick another template.",
  "errors.code.idempotencyConflict":
    "The create request conflicted with a previous attempt. Review the list before trying again.",
  "errors.code.invalidState":
    "The resource is not in a state that allows this right now — it may have changed; refresh and retry.",
  "errors.code.connectionInUse":
    "Another session is already connected to this workspace.",
  "errors.code.imageStale":
    "The runtime image is older than the freshness limit. Ask an administrator to refresh it.",
  "errors.code.imageStaleNamed":
    "Template {template}'s runtime image is {ageDays} days old — over the {limitDays}-day freshness limit. Ask an administrator to refresh it.",
  "errors.code.imageStalePinned":
    "The workspace is pinned to this revision — it can start again only after an administrator publishes a fresh image.",
  "errors.code.forbidden": "You are not allowed to do that on this resource.",
  "errors.code.notFound":
    "This resource does not exist (or is not visible to you).",
  "errors.code.csrfFailed":
    "Your session token expired. Reload the page and try again.",
  "errors.code.unauthenticated":
    "Your session expired. You will be asked to sign in again.",
  "errors.code.generic":
    "The service could not complete the request; it is safe to retry.",
} as const;
