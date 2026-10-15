import type { components } from "./generated/schema";

export type ErrorCode = components["schemas"]["ErrorCode"];

// The schema-generated Error.details shape (release_pending reason plus
// the IMAGE_STALE templateName/ageDays/limitDays/pinned context).
export type ErrorBody = components["schemas"]["ErrorBody"];
export type ErrorDetails = NonNullable<ErrorBody["details"]>;
/** The wire envelope every non-2xx response carries: `{"error": ErrorBody}`. */
export type ErrorEnvelope = components["schemas"]["Error"];

export class PortalApiError extends Error {
  readonly code: ErrorCode;
  readonly httpStatus: number;
  readonly retryable: boolean;
  readonly requestId: string;
  readonly details: ErrorDetails | undefined;
  /** Server-asked retry delay (429 Retry-After), in ms, when present. */
  readonly retryAfterMs: number | undefined;

  constructor(
    httpStatus: number,
    body: {
      code?: string | undefined;
      message?: string | undefined;
      retryable?: boolean | undefined;
      requestId?: string | undefined;
      details?: ErrorDetails | undefined;
    },
    retryAfterMs?: number,
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
    this.retryAfterMs = retryAfterMs;
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

// Keyed by the generated ErrorCode union, so the compiler rejects a missing
// or extra member: the runtime set cannot drift from the contract.
const CODE_TABLE: { readonly [K in ErrorCode]: true } = {
  UNAUTHENTICATED: true,
  CSRF_FAILED: true,
  FORBIDDEN: true,
  NOT_FOUND: true,
  INVALID_REQUEST: true,
  INVALID_TEMPLATE: true,
  INVALID_STATE: true,
  IDEMPOTENCY_CONFLICT: true,
  QUOTA_EXHAUSTED: true,
  QUOTA_NOT_CONFIGURED: true,
  QUOTA_MANAGED_BY_CONFIG: true,
  QUOTA_ABOVE_CEILING: true,
  PRECONDITION_REQUIRED: true,
  PRECONDITION_FAILED: true,
  CONNECTION_IN_USE: true,
  IMAGE_STALE: true,
  RATE_LIMITED: true,
  UNAVAILABLE: true,
  INTERNAL: true,
};

export const KNOWN_CODES: ReadonlySet<string> = new Set(Object.keys(CODE_TABLE));

/** The `error` member of a response body, or {} when the body is not an envelope. */
export function errorBodyOf(body: unknown): Record<string, unknown> {
  if (typeof body !== "object" || body === null) return {};
  const inner = (body as Record<string, unknown>).error;
  return typeof inner === "object" && inner !== null ? (inner as Record<string, unknown>) : {};
}

export function isErrorCode(code: string): boolean {
  return KNOWN_CODES.has(code);
}

export function isPortalApiError(e: unknown): e is PortalApiError {
  return e instanceof PortalApiError;
}
