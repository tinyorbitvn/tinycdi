import type { components } from "./generated/schema";

export type ErrorCode = components["schemas"]["ErrorCode"];
export type ErrorBody = components["schemas"]["Error"];

export class PortalApiError extends Error {
  readonly code: ErrorCode;
  readonly httpStatus: number;
  readonly retryable: boolean;
  readonly requestId: string;

  constructor(
    httpStatus: number,
    body: {
      code?: string | undefined;
      message?: string | undefined;
      retryable?: boolean | undefined;
      requestId?: string | undefined;
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
  }
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
  "CONNECTION_IN_USE",
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
