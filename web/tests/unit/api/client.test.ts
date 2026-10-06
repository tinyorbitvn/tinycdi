import { afterEach, describe, expect, it, vi } from "vitest";
import { createApi, setCsrfToken, CSRF_HEADER, POLL_HEADER, POLL_HEADERS, unwrap } from "../../../src/api/client";
import { fetchMe } from "../../../src/app/me";
import { getWorkspace, listWorkspaces } from "../../../src/workspaces/api";

// The CSRF token arrives in the GET /v1/me body (P1/D17) and lives only in
// module state — setCsrfToken installs it, the client echoes it on mutations,
// and nothing ever touches document.cookie.

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function recordingFetch(handler: (req: Request) => Response | Promise<Response>) {
  const calls: Request[] = [];
  const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const req = input instanceof Request ? input : new Request(input, init);
    calls.push(req);
    return handler(req);
  }) as typeof fetch;
  return { calls, fetchImpl };
}

const WS = "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E";
const ME_BODY = {
  subject: "user-1",
  displayName: "Ada",
  tenant: "tenant-1",
  roles: ["user"],
  csrfToken: "tok-new",
  sessionDomain: "session.example.com",
};

afterEach(() => {
  setCsrfToken(undefined);
  vi.restoreAllMocks();
});

describe("api client CSRF", () => {
  it("sends token from setCsrfToken", async () => {
    const { calls, fetchImpl } = recordingFetch(() => json(200, { items: [] }));
    setCsrfToken("tok-abc");
    const client = createApi(fetchImpl);

    await client.POST("/v1/workspaces/{workspaceId}/stop", {
      params: { path: { workspaceId: WS } },
    });
    await client.GET("/v1/workspaces", {});

    const post = calls.find((c) => c.method === "POST");
    const get = calls.find((c) => c.method === "GET");
    expect(post?.headers.get(CSRF_HEADER)).toBe("tok-abc");
    expect(get?.headers.get(CSRF_HEADER)).toBeNull();
  });

  it("never reads document.cookie", async () => {
    const spy = vi.spyOn(Document.prototype, "cookie", "get");
    const { fetchImpl } = recordingFetch(() => json(200, {}));
    setCsrfToken("tok-abc");
    const client = createApi(fetchImpl);

    await client.POST("/v1/workspaces/{workspaceId}/stop", {
      params: { path: { workspaceId: WS } },
    });

    expect(spy).not.toHaveBeenCalled();
  });

  it("refetches /v1/me once on CSRF_FAILED", async () => {
    const { calls, fetchImpl } = recordingFetch((req) => {
      if (new URL(req.url).pathname === "/v1/me") return json(200, ME_BODY);
      if (req.headers.get(CSRF_HEADER) === "tok-new") return json(200, {});
      return json(403, {
        code: "CSRF_FAILED",
        message: "bad token",
        retryable: false,
        requestId: "req_1",
      });
    });
    setCsrfToken("tok-old");
    const client = createApi(fetchImpl);

    unwrap(
      await client.POST("/v1/workspaces/{workspaceId}/stop", {
        params: { path: { workspaceId: WS } },
      }),
    );

    const meCalls = calls.filter((c) => new URL(c.url).pathname === "/v1/me");
    const posts = calls.filter((c) => c.method === "POST");
    expect(meCalls).toHaveLength(1);
    expect(posts).toHaveLength(2);
    expect(posts[1].headers.get(CSRF_HEADER)).toBe("tok-new");
  });

  it("surfaces a second CSRF_FAILED as an error", async () => {
    const { calls, fetchImpl } = recordingFetch((req) => {
      if (new URL(req.url).pathname === "/v1/me") return json(200, ME_BODY);
      return json(403, {
        code: "CSRF_FAILED",
        message: "bad token",
        retryable: false,
        requestId: "req_1",
      });
    });
    setCsrfToken("tok-old");
    const client = createApi(fetchImpl);

    await expect(
      client
        .POST("/v1/workspaces/{workspaceId}/stop", {
          params: { path: { workspaceId: WS } },
        })
        .then(unwrap),
    ).rejects.toMatchObject({ code: "CSRF_FAILED", httpStatus: 403 });

    const meCalls = calls.filter((c) => new URL(c.url).pathname === "/v1/me");
    const posts = calls.filter((c) => c.method === "POST");
    expect(meCalls).toHaveLength(1);
    expect(posts).toHaveLength(2);
  });
});

// FIX-IDLE — background polls carry X-TCDI-Poll: background so the server
// authenticates them without sliding the portal idle window; foreground
// reads send nothing.
describe("background-poll marker", () => {
  it("sends X-TCDI-Poll only on marked reads", async () => {
    const { calls, fetchImpl } = recordingFetch(() => json(200, { items: [] }));
    const client = createApi(fetchImpl);

    await listWorkspaces(client, true);
    await listWorkspaces(client);
    await getWorkspace(client, WS, true);

    const [markedList, plainList, markedGet] = calls;
    expect(markedList.headers.get(POLL_HEADER)).toBe(POLL_HEADERS[POLL_HEADER]);
    expect(plainList.headers.get(POLL_HEADER)).toBeNull();
    expect(markedGet.headers.get(POLL_HEADER)).toBe(POLL_HEADERS[POLL_HEADER]);
  });

  it("fetchMe marks retry attempts", async () => {
    const calls: Request[] = [];
    // fetchMe hits the relative /v1/me, so give the Request a base URL.
    const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? new URL(input, "https://portal.test") : input;
      const req = new Request(url, init);
      calls.push(req);
      return json(200, ME_BODY);
    }) as typeof fetch;

    await fetchMe(true, fetchImpl);
    await fetchMe(false, fetchImpl);

    expect(calls[0].headers.get(POLL_HEADER)).toBe(POLL_HEADERS[POLL_HEADER]);
    expect(calls[1].headers.get(POLL_HEADER)).toBeNull();
  });
});
