import { describe, expect, it } from "vitest";
import { unwrap } from "../../../src/api/client";
import { KNOWN_CODES, isErrorCode, isPortalApiError, PortalApiError } from "../../../src/api/errors";
import type { components } from "../../../src/api/generated/schema";

type GeneratedCode = components["schemas"]["ErrorCode"];

function failed(status: number, body: unknown) {
  return {
    error: body,
    response: new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": "application/json" },
    }),
  };
}

// Every member of the generated ErrorCode union, checked by the compiler in
// both directions: a code missing here or one the schema no longer has
// fails `tsc`, so this list cannot drift from the contract silently.
const GENERATED: { [K in GeneratedCode]: true } = {
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

describe("api/errors envelope", () => {
  it("unwrap reads body.error.code: PRECONDITION_REQUIRED", () => {
    let caught: unknown;
    try {
      unwrap(
        failed(428, {
          error: {
            code: "PRECONDITION_REQUIRED",
            message: "If-Match required",
            retryable: false,
            requestId: "req_428",
          },
        }),
      );
    } catch (e) {
      caught = e;
    }
    expect(isPortalApiError(caught)).toBe(true);
    const err = caught as PortalApiError;
    expect(err.code).toBe("PRECONDITION_REQUIRED");
    expect(err.httpStatus).toBe(428);
    expect(err.retryable).toBe(false);
    expect(err.requestId).toBe("req_428");
    expect(err.message).toBe("If-Match required");
  });

  it("unwrap carries details from the envelope", () => {
    expect(() =>
      unwrap(
        failed(409, {
          error: {
            code: "QUOTA_EXHAUSTED",
            message: "held",
            retryable: true,
            requestId: "r",
            details: { reason: "release_pending" },
          },
        }),
      ),
    ).toThrow(expect.objectContaining({ code: "QUOTA_EXHAUSTED", details: { reason: "release_pending" } }));
  });

  it("a flat (pre-envelope) body is not read as a code", () => {
    expect(() =>
      unwrap(failed(409, { code: "CONNECTION_IN_USE", message: "x", retryable: false, requestId: "r" })),
    ).toThrow(expect.objectContaining({ code: "INTERNAL", requestId: "" }));
  });

  it("KNOWN_CODES equals the generated ErrorCode enum", () => {
    expect([...KNOWN_CODES].sort()).toEqual(Object.keys(GENERATED).sort());
    expect(isErrorCode("PRECONDITION_REQUIRED")).toBe(true);
    expect(isErrorCode("SOME_FUTURE_CODE")).toBe(false);
  });
});
