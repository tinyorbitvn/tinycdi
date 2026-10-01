import { isPortalApiError, type PortalApiError } from "../api/errors";

// Stable-code → operator-facing guidance. `message` from the server is shown
// as detail only; the actionable text is keyed on `code` per the contract.
function guidance(e: PortalApiError): string {
  switch (e.code) {
    case "QUOTA_EXHAUSTED":
      return "Quota exhausted — delete an unused workspace or ask an administrator for more quota.";
    case "INVALID_TEMPLATE":
      return "That template is not available (unpublished or disallowed for your tenant). Pick another template.";
    case "IDEMPOTENCY_CONFLICT":
      return "The create request conflicted with a previous attempt. Review the list before trying again.";
    case "INVALID_STATE":
      return "The resource is not in a state that allows this right now — it may have changed; refresh and retry.";
    case "CONNECTION_IN_USE":
      return "Another session is already connected to this workspace.";
    case "FORBIDDEN":
      return "You are not allowed to do that on this resource.";
    case "NOT_FOUND":
      return "This resource does not exist (or is not visible to you).";
    case "CSRF_FAILED":
      return "Your session token expired. Reload the page and try again.";
    case "UNAUTHENTICATED":
      return "Your session expired. You will be asked to sign in again.";
    case "RATE_LIMITED":
    case "UNAVAILABLE":
    case "INTERNAL":
    default:
      return "The service could not complete the request; it is safe to retry.";
  }
}

export function ErrorBanner({
  error,
  onRetry,
  onDismiss,
}: {
  error: unknown;
  onRetry?: () => void;
  onDismiss?: () => void;
}) {
  if (!error) return null;
  const apiErr = isPortalApiError(error) ? error : null;
  const detail = error instanceof Error ? error.message : String(error);
  return (
    <div role="alert" className="error-banner">
      <strong>{apiErr ? apiErr.code : "ERROR"}</strong>{" "}
      <span>{apiErr ? guidance(apiErr) : detail}</span>
      {apiErr?.message ? <small> {apiErr.message}</small> : null}
      {apiErr?.requestId ? <small> (request {apiErr.requestId})</small> : null}
      {onRetry && apiErr?.retryable ? (
        <button type="button" onClick={onRetry}>
          Retry
        </button>
      ) : null}
      {onDismiss ? (
        <button type="button" aria-label="dismiss error" onClick={onDismiss}>
          ×
        </button>
      ) : null}
    </div>
  );
}
