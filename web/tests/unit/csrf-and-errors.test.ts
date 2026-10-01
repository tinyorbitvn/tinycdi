import { describe, expect, it } from "vitest";
import { createApi, unwrap } from "../../src/api/client";
import { isPortalApiError } from "../../src/api/errors";
import {
  createMockApi,
  stubFetch,
  loginCookies,
  clearCookies,
  CSRF_TOKEN_VALUE,
} from "./helpers";
import { TEMPLATE_LINUX } from "../mock-api/fixtures.ts";

describe("CSRF token handling", () => {
  it("sends X-CSRF-Token from the tcdi_csrf cookie on every mutation", async () => {
    const api = createMockApi();
    loginCookies();
    const client = createApi(stubFetch(api));

    unwrap(
      await client.POST("/v1/workspaces", {
        params: { header: { "Idempotency-Key": "key-1-testtest" } },
        body: { name: "w1", templateRef: TEMPLATE_LINUX.id },
      }),
    );

    const posts = api.state.requests.filter((r) => r.method === "POST");
    expect(posts).toHaveLength(1);
    expect(posts[0].headers["x-csrf-token"]).toBe(CSRF_TOKEN_VALUE);
  });

  it("mutation without a valid CSRF cookie fails with CSRF_FAILED", async () => {
    const api = createMockApi();
    document.cookie = "tcdi_session=session-01J4ZD; path=/"; // no csrf cookie
    const client = createApi(stubFetch(api));

    await expect(
      client
        .POST("/v1/workspaces/{workspaceId}/stop", {
          params: { path: { workspaceId: "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E" } },
        })
        .then(unwrap),
    ).rejects.toMatchObject({ code: "CSRF_FAILED", httpStatus: 403 });
  });

  it("unauthenticated calls surface UNAUTHENTICATED", async () => {
    clearCookies();
    const client = createApi(stubFetch(createMockApi()));
    await expect(
      client.GET("/v1/workspaces", {}).then(unwrap),
    ).rejects.toMatchObject({ code: "UNAUTHENTICATED", httpStatus: 401 });
  });
});

describe("stable error-code mapping", () => {
  it("keeps known codes verbatim", async () => {
    const fetchImpl = (async () =>
      new Response(
        JSON.stringify({
          code: "CONNECTION_IN_USE",
          message: "busy",
          retryable: false,
          requestId: "req_x",
        }),
        { status: 409, headers: { "content-type": "application/json" } },
      )) as typeof fetch;
    const client = createApi(fetchImpl);
    try {
      unwrap(
        await client.POST("/v1/workspaces/{workspaceId}/connections", {
          params: { path: { workspaceId: "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E" } },
          body: { takeover: false },
        }),
      );
      expect.unreachable();
    } catch (e) {
      expect(isPortalApiError(e)).toBe(true);
      const err = e as { code: string; retryable: boolean; requestId: string };
      expect(err.code).toBe("CONNECTION_IN_USE");
      expect(err.retryable).toBe(false);
      expect(err.requestId).toBe("req_x");
    }
  });

  it("maps unknown codes to INTERNAL (retryable) per the contract", async () => {
    const fetchImpl = (async () =>
      new Response(
        JSON.stringify({
          code: "SOME_FUTURE_CODE",
          message: "new server code",
          retryable: false,
          requestId: "req_y",
        }),
        { status: 500, headers: { "content-type": "application/json" } },
      )) as typeof fetch;
    const client = createApi(fetchImpl);
    await expect(
      client.GET("/v1/workspaces", {}).then(unwrap),
    ).rejects.toMatchObject({ code: "INTERNAL", retryable: true, httpStatus: 500 });
  });
});
