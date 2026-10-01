import { afterEach, describe, expect, it } from "vitest";
import { createApi, setCsrfToken, unwrap } from "../../src/api/client";
import { isPortalApiError } from "../../src/api/errors";
import {
  createMockApi,
  stubFetch,
  loginCookies,
  clearCookies,
  CSRF_TOKEN_VALUE,
} from "./helpers";
import { TEMPLATE_LINUX } from "../mock-api/fixtures.ts";

afterEach(() => {
  setCsrfToken(undefined);
});

// v0.2 (P1/D17): the synchronizer token is published by GET /v1/me and held
// in module state via setCsrfToken — there is no tcdi_csrf cookie. The mock
// composer enforces the published token on every mutation.

describe("CSRF token handling", () => {
  it("sends X-CSRF-Token from the /v1/me token on every mutation", async () => {
    const api = createMockApi();
    loginCookies();
    setCsrfToken(CSRF_TOKEN_VALUE);
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

  it("heals a stale token through /v1/me and retries once", async () => {
    const api = createMockApi();
    loginCookies();
    setCsrfToken("stale-token");
    const client = createApi(stubFetch(api));

    unwrap(
      await client.POST("/v1/workspaces/{workspaceId}/stop", {
        params: { path: { workspaceId: "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E" } },
      }),
    );

    const posts = api.state.requests.filter((r) => r.method === "POST");
    const mes = api.state.requests.filter((r) => r.path === "/v1/me");
    expect(posts).toHaveLength(2);
    expect(mes).toHaveLength(1);
    expect(posts[0].headers["x-csrf-token"]).toBe("stale-token");
    expect(posts[1].headers["x-csrf-token"]).toBe(CSRF_TOKEN_VALUE);
  });

  it("surfaces CSRF_FAILED when the /v1/me refresh cannot fix the token", async () => {
    const api = createMockApi();
    loginCookies();
    setCsrfToken("stale-token");
    // The refresh fetch fails: the retry goes out with the old token and a
    // second CSRF_FAILED surfaces to the caller.
    api.handle({
      method: "POST",
      path: "/_control/admin/fail",
      query: new URLSearchParams(),
      headers: {},
      body: { path: "/v1/me", status: 503, code: "UNAVAILABLE" },
    });
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
