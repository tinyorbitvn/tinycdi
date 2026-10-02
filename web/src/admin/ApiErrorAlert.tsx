import { Alert, Button } from "../design";
import { isPortalApiError, type ErrorCode, type PortalApiError } from "../api/errors";
import { t, type MessageKey } from "../i18n";

// Stable-code → actionable text for the admin and data views; the server's
// `message` is shown only as secondary detail (not part of the contract).
// Keys are looked up statically so every catalog entry stays greppable.
const GUIDANCE: Partial<Record<ErrorCode, MessageKey>> = {
  FORBIDDEN: "admin.errors.forbidden",
  NOT_FOUND: "admin.errors.notFound",
  INVALID_STATE: "admin.errors.invalidState",
  INVALID_REQUEST: "admin.errors.invalidRequest",
  INVALID_TEMPLATE: "admin.errors.invalidTemplate",
  QUOTA_EXHAUSTED: "admin.errors.quotaExhausted",
  QUOTA_NOT_CONFIGURED: "admin.errors.quotaNotConfigured",
  IDEMPOTENCY_CONFLICT: "admin.errors.idempotencyConflict",
  CSRF_FAILED: "admin.errors.csrfFailed",
  UNAUTHENTICATED: "admin.errors.unauthenticated",
};

export function errorGuidance(e: PortalApiError): string {
  return t(GUIDANCE[e.code] ?? "admin.errors.default");
}

export function ApiErrorAlert({
  error,
  title,
  onRetry,
  onDismiss,
}: {
  error: unknown;
  title?: string;
  onRetry?: () => void;
  onDismiss?: () => void;
}) {
  if (!error) return null;
  const apiErr = isPortalApiError(error) ? error : null;
  const text = apiErr ? errorGuidance(apiErr) : error instanceof Error ? error.message : String(error);
  const canRetry = onRetry && (!apiErr || apiErr.retryable);
  return (
    <Alert
      tone="danger"
      title={title ?? (apiErr ? apiErr.code : t("admin.errors.title"))}
      {...(onDismiss ? { onDismiss } : {})}
      {...(canRetry
        ? {
            actions: (
              <Button size="sm" onClick={onRetry}>
                {t("admin.errors.retry")}
              </Button>
            ),
          }
        : {})}
    >
      <p>{text}</p>
      {apiErr?.requestId ? <p className="tc-admin-muted">{t("admin.errors.requestId", { id: apiErr.requestId })}</p> : null}
    </Alert>
  );
}
