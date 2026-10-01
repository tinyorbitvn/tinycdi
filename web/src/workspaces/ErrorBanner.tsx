import { t, type MessageKey } from "../i18n";
import { isPortalApiError, type ErrorCode, type PortalApiError } from "../api/errors";

// Stable-code → operator-facing guidance, keyed on `code` per the contract.
// `message` from the server is shown as detail only.
const GUIDANCE_KEYS: Record<ErrorCode, MessageKey> = {
  QUOTA_EXHAUSTED: "errors.code.quotaExhausted",
  INVALID_TEMPLATE: "errors.code.invalidTemplate",
  IDEMPOTENCY_CONFLICT: "errors.code.idempotencyConflict",
  INVALID_STATE: "errors.code.invalidState",
  CONNECTION_IN_USE: "errors.code.connectionInUse",
  FORBIDDEN: "errors.code.forbidden",
  NOT_FOUND: "errors.code.notFound",
  CSRF_FAILED: "errors.code.csrfFailed",
  UNAUTHENTICATED: "errors.code.unauthenticated",
  INVALID_REQUEST: "errors.code.generic",
  RATE_LIMITED: "errors.code.generic",
  UNAVAILABLE: "errors.code.generic",
  INTERNAL: "errors.code.generic",
};

function guidance(e: PortalApiError): string {
  return t(GUIDANCE_KEYS[e.code]);
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
      <strong>{apiErr ? apiErr.code : t("errors.banner.fallback")}</strong>{" "}
      <span>{apiErr ? guidance(apiErr) : detail}</span>
      {apiErr?.message ? <small> {apiErr.message}</small> : null}
      {apiErr?.requestId ? (
        <small> {t("errors.banner.requestId", { id: apiErr.requestId })}</small>
      ) : null}
      {onRetry && apiErr?.retryable ? (
        <button type="button" onClick={onRetry}>
          {t("errors.banner.retry")}
        </button>
      ) : null}
      {onDismiss ? (
        <button
          type="button"
          aria-label={t("errors.banner.dismiss")}
          onClick={onDismiss}
        >
          ×
        </button>
      ) : null}
    </div>
  );
}
