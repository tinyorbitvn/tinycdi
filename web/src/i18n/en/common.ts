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
  "errors.code.quotaNotConfigured":
    "No quota is configured for your tenant. Ask an administrator to set one.",
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
} as const;
