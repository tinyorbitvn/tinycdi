// Auth bootstrap area of the contract mock: the dev/test login flow
// (/v1/login — not part of the public contract) that sets the session
// cookie. v0.2 (D17): the CSRF token and session domain come from
// GET /v1/me, so the tcdi_csrf / tcdi_session_origin cookies are gone.

import {
  SESSION_COOKIE,
  SESSION_PRINCIPAL,
  err,
  ok,
  parseCookies,
  type MockArea,
  type MockContext,
  type MockRequest,
  type MockResponse,
} from "./core.ts";

export function authArea(_ctx: MockContext): MockArea {
  // The end-session URL POST /v1/logout answers with (RP-initiated logout);
  // null = the provider has no end_session_endpoint, so 204. Set through
  // POST /_control/auth/endSession {url}; cleared by reset.
  let endSessionUrl: string | null = null;

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

  // POST /v1/logout (openapi.yaml logout): the composer already enforced
  // the session cookie and CSRF header, so this only ends the session.
  function logout(): MockResponse {
    const clear = `${SESSION_COOKIE}=; Path=/; HttpOnly; SameSite=Strict; Max-Age=0`;
    if (endSessionUrl === null) return { status: 204, headers: { "set-cookie": clear }, body: "" };
    return ok(200, { endSessionUrl }, { "set-cookie": clear, "cache-control": "no-store" });
  }

  return {
    name: "auth",
    reset() {
      endSessionUrl = null;
    },
    api: (req) => {
      // POST /v1/me/sessions:revoke-all (openapi.yaml revokeAllSessions):
      // same answer shape as logout — the caller's session dies too.
      if (req.path === "/v1/me/sessions:revoke-all" && req.method === "POST") return logout();
      if (req.path === "/v1/logout" && req.method === "POST") return logout();
      return undefined;
    },
    control: (req) => {
      if (req.path === "/_control/auth/endSession" && req.method === "POST") {
        const url = req.body?.url;
        endSessionUrl = typeof url === "string" && url !== "" ? url : null;
        return ok(200, { endSessionUrl });
      }
      return undefined;
    },
    public: (req) => {
      if (req.path === "/v1/login") return login(req);
      // GET /v1/session: the anonymous, passive probe (openapi.yaml
      // SessionProbe) — 200 {authenticated} for everyone, never a 401.
      if (req.path === "/v1/session" && req.method === "GET") {
        return ok(200, { authenticated: !!parseCookies(req.headers.cookie)[SESSION_COOKIE] }, { "cache-control": "no-store" });
      }
      return undefined;
    },
  };
}
