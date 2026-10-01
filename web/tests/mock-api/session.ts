// v0.2 session surface of the contract mock: GET /v1/me carrying the
// derived CSRF token and the session domain (P1/D17), the passive
// GET /v1/workspaces/{id}/connection endpoint (P4), and per-workspace
// launch URLs on ws-<label>.<sessionDomain> (D9).
//
// Wraps createMockApi: the underlying handler still performs the portal
// auth check and all v0.1 routes; this module layers the G2 contract on
// top. CSRF moves from a JS-readable cookie to the token published by
// /v1/me, so mutations delegate to the inner handler with a matching
// tcdi_csrf cookie injected — the inner check passes whenever this
// layer's newer check already did.

import {
  createMockApi,
  CSRF_COOKIE,
  CSRF_HEADER,
  SESSION_COOKIE,
  type MockRequest,
  type MockResponse,
} from "./handler.ts";

/** Default session domain the mock's /v1/me publishes. */
export const SESSION_DOMAIN = "session.example.com";

/** CSRF token the mock's /v1/me publishes until rotated via setCsrfToken. */
export const ME_CSRF_TOKEN = "csrf-01J4ZD9000MOCK";

export interface MockMe {
  subject: string;
  displayName: string;
  email?: string;
  tenant: string;
  roles: string[];
  csrfToken: string;
  sessionDomain: string;
}

export const ME: MockMe = {
  subject: "user-01J4ZD",
  displayName: "Ada Example",
  email: "ada@example.com",
  tenant: "tenant-01",
  roles: ["user"],
  csrfToken: ME_CSRF_TOKEN,
  sessionDomain: SESSION_DOMAIN,
};

export type ConnectionStateValue = "none" | "connected" | "disconnected" | "stale";

export interface MockConnectionStatus {
  state: ConnectionStateValue;
  leaseActive: boolean;
  lastRenewedAt?: string;
}

function parseCookies(header: string | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const part of String(header ?? "").split(";")) {
    const idx = part.indexOf("=");
    if (idx > 0) out[part.slice(0, idx).trim()] = part.slice(idx + 1).trim();
  }
  return out;
}

function json(status: number, body: unknown): MockResponse {
  return { status, headers: { "content-type": "application/json" }, body };
}

function apiErr(status: number, code: string, message: string): MockResponse {
  return json(status, { code, message, retryable: false, requestId: "req_mock" });
}

/** ws_<suffix> -> ws-<suffix>, the mapping internal/sessionhost.Label pins. */
export function sessionHostLabel(workspaceId: string): string {
  return workspaceId.replaceAll("_", "-").toLowerCase();
}

export interface SessionMockApi {
  handle: (req: MockRequest) => MockResponse;
  state: ReturnType<typeof createMockApi>["state"];
  reset: () => void;
  sessionDomain: string;
  /** Rotate the published CSRF token (simulates a session refresh). */
  setCsrfToken: (token: string) => void;
  connection: {
    /** Script the /connection response for a workspace. */
    set: (workspaceId: string, status: MockConnectionStatus) => void;
    clear: (workspaceId: string) => void;
  };
}

export function createSessionMockApi(
  opts: { sessionDomain?: string; csrfToken?: string; me?: Partial<MockMe> } = {},
): SessionMockApi {
  const sessionDomain = opts.sessionDomain ?? SESSION_DOMAIN;
  let csrfToken = opts.csrfToken ?? ME_CSRF_TOKEN;
  const inner = createMockApi();
  const scripted = new Map<string, MockConnectionStatus>();

  function meBody(): MockMe {
    return { ...ME, csrfToken, sessionDomain, ...(opts.me ?? {}) };
  }

  function authenticated(req: MockRequest): boolean {
    return parseCookies(req.headers.cookie)[SESSION_COOKIE] !== undefined;
  }

  function me(req: MockRequest): MockResponse {
    if (!authenticated(req)) return apiErr(401, "UNAUTHENTICATED", "no session");
    return json(200, meBody());
  }

  // Passive endpoint: authenticates but must never slide the idle timer.
  // The mock has no clock, so "passive" only means the route exists and is
  // scriptable here.
  function connection(req: MockRequest, workspaceId: string): MockResponse {
    if (!authenticated(req)) return apiErr(401, "UNAUTHENTICATED", "no session");
    if (!inner.state.workspaces.has(workspaceId)) {
      return apiErr(404, "NOT_FOUND", "workspace not found");
    }
    const fixed = scripted.get(workspaceId);
    if (fixed) return json(200, fixed);
    const leaseActive = inner.state.leases.has(workspaceId);
    return json(200, {
      state: leaseActive ? "connected" : "none",
      leaseActive,
      lastRenewedAt: new Date().toISOString(),
    });
  }

  function handle(req: MockRequest): MockResponse {
    if (req.path === "/v1/me" && req.method === "GET") return me(req);
    const connMatch = req.path.match(/^\/v1\/workspaces\/([^/]+)\/connection$/);
    if (connMatch && req.method === "GET") return connection(req, connMatch[1]);
    if (!req.path.startsWith("/v1/")) return inner.handle(req);

    // v0.2 CSRF contract: the token comes from /v1/me, no cookie exists.
    if (!authenticated(req)) return apiErr(401, "UNAUTHENTICATED", "no session");
    if (req.method !== "GET" && req.method !== "HEAD") {
      if (req.headers[CSRF_HEADER] !== csrfToken) {
        return apiErr(403, "CSRF_FAILED", "missing or mismatched CSRF token");
      }
      // Satisfy the wrapped handler's legacy cookie-vs-header check.
      req = {
        ...req,
        headers: {
          ...req.headers,
          cookie: `${req.headers.cookie ?? ""}; ${CSRF_COOKIE}=${csrfToken}`,
        },
      };
    }
    const resp = inner.handle(req);
    const connPost = req.path.match(/^\/v1\/workspaces\/([^/]+)\/connections$/);
    if (
      connPost &&
      req.method === "POST" &&
      resp.status === 201 &&
      typeof resp.body === "object" &&
      resp.body !== null
    ) {
      // Per-workspace launch URL (D9): https://ws-<label>.<sessionDomain>/v1/launch
      const host = `${sessionHostLabel(connPost[1])}.${sessionDomain}`;
      return {
        ...resp,
        body: { ...(resp.body as Record<string, unknown>), launchUrl: `https://${host}/v1/launch` },
      };
    }
    return resp;
  }

  return {
    handle,
    state: inner.state,
    reset: () => {
      scripted.clear();
      inner.reset();
    },
    sessionDomain,
    setCsrfToken: (token) => {
      csrfToken = token;
    },
    connection: {
      set: (workspaceId, status) => scripted.set(workspaceId, status),
      clear: (workspaceId) => scripted.delete(workspaceId),
    },
  };
}
