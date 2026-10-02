// Session area of the contract mock (v0.2): the session-origin routes
// /v1/launch (ticket redeem with the gateway's ADR-0004 origin gate) and the
// cookie-gated /desktop/*, plus the portal-side session surface:
// GET /v1/workspaces/{id}/connection (the passive poll endpoint, P4) and the
// per-workspace launchUrl the connections endpoint answers (D9).
//
// Contract notes:
// - /v1/me fields (csrfToken, sessionDomain) are added by the admin area
//   from ctx.sessionDomain and core.CSRF_TOKEN_VALUE — /v1/me stays where
//   the principal state lives.
// - The CSRF check in handler.ts compares X-CSRF-Token against the token
//   /v1/me publishes; the v0.1 tcdi_csrf / tcdi_session_origin cookies are
//   gone (D17).
// - launchUrl is https://ws-<label>.<sessionDomain>/v1/launch like the real
//   backend; on a loopback session domain the mock keeps its listener's
//   scheme so browser e2e can reach it.

import { createHash } from "node:crypto";
import {
  CSRF_TOKEN_VALUE,
  err,
  gwErr,
  ok,
  originsEqual,
  parseCookies,
  parseOriginValue,
  type MockArea,
  type MockContext,
  type MockRequest,
  type MockResponse,
} from "./core.ts";

export { sessionHostLabel, sessionLaunchUrl } from "./core.ts";

/** GET /v1/workspaces/{id}/connection body (openapi.yaml ConnectionStatus). */
export interface MockConnectionStatus {
  state: "none" | "connected" | "disconnected" | "stale";
  leaseActive: boolean;
  lastRenewedAt?: string;
  /** First 16 hex chars of SHA-256 of the active lease ID (absent without a lease). */
  leaseRef?: string;
  /** The lease's stream epoch; advances when a new stream opens. */
  streamEpoch?: number;
}

/** What the API publishes as leaseRef: SHA-256(lease ID), first 16 hex chars. */
export function leaseRefOf(leaseId: string): string {
  return createHash("sha256").update(leaseId).digest("hex").slice(0, 16);
}

export interface SessionAreaState {
  /** Test scripting for /v1/workspaces/{id}/connection (P4). */
  connection: {
    set(workspaceId: string, status: MockConnectionStatus): void;
    clear(workspaceId: string): void;
  };
  /** The token /v1/me publishes and the composer enforces on mutations. */
  csrfToken: string;
  sessionDomain: string;
}

export function sessionArea(ctx: MockContext): MockArea {
  const { state } = ctx;
  const scripted = new Map<string, MockConnectionStatus>();
  // Stream epoch per lease ID: 1 for a lease's first stream; the control
  // route below advances it like a new stream opening would.
  const epochs = new Map<string, number>();

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

  // GET /v1/workspaces/{id}/connection — passive (never slides the idle
  // timer server-side; a mock has no clock). Visibility matches
  // GET /v1/workspaces/{id}: an unknown workspace is 404.
  function connection(req: MockRequest): MockResponse | undefined {
    const m = req.path.match(/^\/v1\/workspaces\/([^/]+)\/connection$/);
    if (!m || req.method !== "GET") return undefined;
    if (!state.workspaces.get(m[1])) {
      return err(404, "NOT_FOUND", "workspace not found", false);
    }
    const fixed = scripted.get(m[1]);
    if (fixed) return ok(200, fixed);
    const leaseId = state.leases.get(m[1]);
    const leaseActive = leaseId !== undefined;
    return ok(200, {
      state: leaseActive ? "connected" : "none",
      leaseActive,
      lastRenewedAt: ctx.nowIso(),
      ...(leaseActive
        ? { leaseRef: leaseRefOf(leaseId), streamEpoch: epochs.get(leaseId) ?? 1 }
        : {}),
    });
  }

  function api(req: MockRequest): MockResponse | undefined {
    return connection(req);
  }

  function control(req: MockRequest): MockResponse | undefined {
    if (req.path === "/_control/launchRequests" && req.method === "GET") {
      return { status: 200, headers: { "content-type": "application/json" }, body: { requests: state.launchRequests } };
    }
    // POST /_control/session/stream {workspaceId} — a new stream opened on
    // the workspace's current lease: its epoch advances by one.
    if (req.path === "/_control/session/stream" && req.method === "POST") {
      const id = String(req.body?.workspaceId ?? "");
      const leaseId = state.leases.get(id);
      if (leaseId === undefined) return err(404, "NOT_FOUND", "no active lease", false);
      const next = (epochs.get(leaseId) ?? 1) + 1;
      epochs.set(leaseId, next);
      return ok(200, { leaseRef: leaseRefOf(leaseId), streamEpoch: next });
    }
    // POST /_control/session/connection {workspaceId, state?, leaseActive?,
    // lastRenewedAt?, leaseRef?, streamEpoch?} — script the poll response; a
    // missing/absent "state" clears the override. GET lists the scripted entries.
    if (req.path === "/_control/session/connection") {
      if (req.method === "GET") {
        return ok(200, { scripted: Object.fromEntries(scripted) });
      }
      if (req.method === "POST") {
        const id = String(req.body?.workspaceId ?? "");
        if (req.body?.state === undefined) {
          scripted.delete(id);
          return ok(200, { cleared: id });
        }
        scripted.set(id, {
          state: req.body.state as MockConnectionStatus["state"],
          leaseActive: req.body.leaseActive === true,
          ...(typeof req.body?.lastRenewedAt === "string"
            ? { lastRenewedAt: req.body.lastRenewedAt }
            : {}),
          ...(typeof req.body?.leaseRef === "string" ? { leaseRef: req.body.leaseRef } : {}),
          ...(typeof req.body?.streamEpoch === "number"
            ? { streamEpoch: req.body.streamEpoch }
            : {}),
        });
        return ok(200, { scripted: id });
      }
      return err(404, "NOT_FOUND", "no control route", false);
    }
    return undefined;
  }

  const sessionState: SessionAreaState = {
    connection: { set: (id, s) => scripted.set(id, s), clear: (id) => scripted.delete(id) },
    csrfToken: CSRF_TOKEN_VALUE,
    sessionDomain: ctx.sessionDomain,
  };

  // A scripted /connection answer or a stream epoch must not outlive the
  // test that set it (POST /_control/reset).
  function reset(): void {
    scripted.clear();
    epochs.clear();
  }

  return { name: "session", reset, public: publicRoutes, api, control, state: sessionState };
}
