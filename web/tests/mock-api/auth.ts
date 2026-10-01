// Auth bootstrap area of the contract mock: the dev/test login flow
// (/v1/login — not part of the public contract) that sets the session
// cookie. v0.2 (D17): the CSRF token and session domain come from
// GET /v1/me, so the tcdi_csrf / tcdi_session_origin cookies are gone.

import {
  SESSION_COOKIE,
  SESSION_PRINCIPAL,
  err,
  type MockArea,
  type MockContext,
  type MockRequest,
  type MockResponse,
} from "./core.ts";

export function authArea(_ctx: MockContext): MockArea {
  function login(req: MockRequest): MockResponse {
    const returnTo = req.query.get("returnTo") || "/";
    if (req.method === "GET") {
      return {
        status: 200,
        headers: { "content-type": "text/html" },
        body: `<html><body><h1>Sign in</h1><form method="POST" action="/v1/login?returnTo=${encodeURIComponent(returnTo)}"><button type="submit">Log in with SSO</button></form></body></html>`,
      };
    }
    if (req.method === "POST") {
      return {
        status: 302,
        headers: {
          location: returnTo,
          // v0.2 (D17): only the session cookie remains — tcdi_csrf and
          // tcdi_session_origin are gone; the CSRF token and session domain
          // come from GET /v1/me.
          "set-cookie": `${SESSION_COOKIE}=${SESSION_PRINCIPAL}; Path=/; HttpOnly; SameSite=Strict`,
        },
        body: "",
      };
    }
    return err(405, "INVALID_REQUEST", "method not allowed", false);
  }

  return {
    name: "auth",
    public: (req) => (req.path === "/v1/login" ? login(req) : undefined),
  };
}
