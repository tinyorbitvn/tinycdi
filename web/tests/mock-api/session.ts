// Session-origin area of the contract mock: /v1/launch (ticket redeem with
// the gateway's ADR-0004 origin gate) and the cookie-gated /desktop/*.

import {
  err,
  gwErr,
  originsEqual,
  parseCookies,
  parseOriginValue,
  type MockArea,
  type MockContext,
  type MockRequest,
  type MockResponse,
} from "./core.ts";

export function sessionArea(ctx: MockContext): MockArea {
  const { state } = ctx;

  // launchOriginOK is the gateway's launchOriginOK (ADR 0004): the Origin
  // must exactly match a configured portal origin, or the session origin
  // itself on the request's authority (the public-origin leg).
  function launchOriginOK(req: MockRequest): boolean {
    const o = parseOriginValue(req.headers["origin"]);
    if (!o) return false;
    if (ctx.portalOrigins.some((p) => originsEqual(o, p))) return true;
    const host = req.headers["host"] ?? "";
    const reqHostname = host.startsWith("[") ? host.slice(0, host.indexOf("]") + 1) : host.split(":")[0];
    return o.hostname === reqHostname && originsEqual(o, ctx.sessionOriginUrl);
  }

  function launch(req: MockRequest): MockResponse {
    state.launchRequests.push({ headers: req.headers, rawBody: req.rawBody ?? "" });
    // Fetch-metadata/Origin gate, same order as the real gateway: it runs
    // before the method and ticket checks, so a rejected launch never
    // consumes the ticket. A browser labels the launch POST with
    // Sec-Fetch-Site; then an absent or "null" Origin (what the portal's
    // old Referrer-Policy: no-referrer produced) is bad_origin.
    // A request with neither header is a non-browser client and stays
    // allowed; a present Origin is always checked.
    const sfs = req.headers["sec-fetch-site"];
    const origin = req.headers["origin"];
    if (sfs) {
      if (sfs !== "same-origin" && sfs !== "same-site" && sfs !== "cross-site") {
        return gwErr(403, "bad_fetch_site");
      }
      if (!origin || origin === "null" || !launchOriginOK(req)) {
        return gwErr(403, "bad_origin");
      }
    } else if (origin && !launchOriginOK(req)) {
      return gwErr(403, "bad_origin");
    }
    if (req.method !== "POST") return err(404, "NOT_FOUND", "method not allowed", false);
    const params = new URLSearchParams(req.rawBody ?? "");
    const ticket = params.get("ticket") ?? "";
    const rec = state.tickets.get(ticket);
    if (!rec || rec.used || rec.expiresAt < ctx.now()) {
      return err(403, "FORBIDDEN", "ticket invalid, expired or already redeemed", false);
    }
    rec.used = true;
    return {
      status: 302,
      headers: {
        location: `/desktop/${rec.workspaceId}`,
        // SameSite=Lax like the real gateway (internal/gateway/launch.go):
        // the launch POST is cross-site by design, so a Strict cookie
        // would never be sent on this redirect chain.
        "set-cookie": `tcdi_desktop=${ctx.nextId("dsk_")}; Path=/; HttpOnly; Secure; SameSite=Lax`,
      },
      body: "",
    };
  }

  function desktop(req: MockRequest): MockResponse {
    if (req.method !== "GET") return err(404, "NOT_FOUND", "method not allowed", false);
    // Cookie-gated like the real gateway: without the session cookie the
    // desktop is unauthorized — this is what proves the browser sent the
    // cookie on the cross-site POST→302 redirect chain.
    if (!parseCookies(req.headers["cookie"])["tcdi_desktop"]) {
      return { status: 401, headers: { "content-type": "text/plain" }, body: "unauthorized" };
    }
    const id = req.path.split("/").pop();
    return {
      status: 200,
      headers: { "content-type": "text/html" },
      body: `<html><body><h1>desktop session ${id}</h1></body></html>`,
    };
  }

  function publicRoutes(req: MockRequest): MockResponse | undefined {
    if (req.path === "/v1/launch") return launch(req);
    if (req.path.startsWith("/desktop/")) return desktop(req);
    return undefined;
  }

  function control(req: MockRequest): MockResponse | undefined {
    if (req.path === "/_control/launchRequests" && req.method === "GET") {
      return { status: 200, headers: { "content-type": "application/json" }, body: { requests: state.launchRequests } };
    }
    return undefined;
  }

  return { name: "session", public: publicRoutes, control };
}
