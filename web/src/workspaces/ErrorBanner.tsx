import { Alert, Button } from "../design";
import { t, type MessageKey } from "../i18n";
import {
  isPortalApiError,
  isReleasePending,
  type ErrorCode,
  type PortalApiError,
} from "../api/errors";

// Stable-code → operator-facing guidance, keyed on `code` per the contract.
// `message` from the server is shown as detail only.
const GUIDANCE_KEYS: Record<ErrorCode, MessageKey> = {
  QUOTA_EXHAUSTED: "errors.code.quotaExhausted",
  QUOTA_NOT_CONFIGURED: "errors.code.quotaNotConfigured",
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
  // A teardown-pending quota refusal resolves on the next recovery pass —
  // distinct copy from a real exhaustion.
  if (isReleasePending(e)) return t("errors.code.quotaReleasePending");
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
    <Alert
      tone="danger"
      title={apiErr ? apiErr.code : t("errors.banner.fallback")}
      actions={
        <>
          {onRetry && (!apiErr || apiErr.retryable) ? (
            <Button size="sm" variant="secondary" onClick={onRetry}>
              {t("errors.banner.retry")}
            </Button>
          ) : null}
          {onDismiss ? (
            <Button size="sm" variant="secondary" onClick={onDismiss}>
              {t("errors.banner.dismiss")}
            </Button>
          ) : null}
        </>
      }
    >
      {apiErr ? guidance(apiErr) : detail}
      {apiErr?.message ? ` ${apiErr.message}` : null}
      {apiErr?.requestId
        ? ` ${t("errors.banner.requestId", { id: apiErr.requestId })}`
        : null}
    </Alert>
  );
}
