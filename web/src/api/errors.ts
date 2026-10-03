import type { components } from "./generated/schema";

export type ErrorCode = components["schemas"]["ErrorCode"];

// The schema-generated Error.details shape (release_pending reason plus
// the IMAGE_STALE templateName/ageDays/limitDays/pinned context).
export type ErrorDetails = NonNullable<components["schemas"]["Error"]["details"]>;
export type ErrorBody = components["schemas"]["Error"];

export class PortalApiError extends Error {
  readonly code: ErrorCode;
  readonly httpStatus: number;
  readonly retryable: boolean;
  readonly requestId: string;
  readonly details: ErrorDetails | undefined;

  constructor(
    httpStatus: number,
    body: {
      code?: string | undefined;
      message?: string | undefined;
      retryable?: boolean | undefined;
      requestId?: string | undefined;
      details?: ErrorDetails | undefined;
    },
  ) {
    const rawCode = body.code ?? "INTERNAL";
    const known = isErrorCode(rawCode);
    const code: ErrorCode = known ? (rawCode as ErrorCode) : "INTERNAL";
    super(body.message ?? `request failed with status ${httpStatus}`);
    this.name = "PortalApiError";
    this.code = code;
    this.httpStatus = httpStatus;
    // Unknown codes are treated as INTERNAL (retryable) per the contract.
    this.retryable = known ? body.retryable === true : true;
    this.requestId = body.requestId ?? "";
    this.details = body.details;
  }
}

// isReleasePending reports whether a QUOTA_EXHAUSTED refusal is the
// transient teardown-pending kind (details.reason=release_pending): the
// refused quota is held only by a workspace still shutting down and frees
// on the next recovery pass, so the request may be retried.
export function isReleasePending(e: unknown): boolean {
  return (
    isPortalApiError(e) &&
    e.code === "QUOTA_EXHAUSTED" &&
    e.details?.reason === "release_pending"
  );
}

const KNOWN_CODES: ReadonlySet<string> = new Set([
  "UNAUTHENTICATED",
  "CSRF_FAILED",
  "FORBIDDEN",
  "NOT_FOUND",
  "INVALID_REQUEST",
  "INVALID_TEMPLATE",
  "INVALID_STATE",
  "IDEMPOTENCY_CONFLICT",
  "QUOTA_EXHAUSTED",
  "QUOTA_NOT_CONFIGURED",
  "CONNECTION_IN_USE",
  "IMAGE_STALE",
  "RATE_LIMITED",
  "UNAVAILABLE",
  "INTERNAL",
]);

export function isErrorCode(code: string): boolean {
  return KNOWN_CODES.has(code);
}

export function isPortalApiError(e: unknown): e is PortalApiError {
  return e instanceof PortalApiError;
}
