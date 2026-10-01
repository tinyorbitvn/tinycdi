// Contract-faithful mock of the tinycdi public API (internal/api/openapi.yaml).
// Shared between the node HTTP server (Playwright) and the vitest fetch stub.
// Portal endpoints under /v1, session-origin endpoints (/v1/launch, /desktop/*),
// and test-control endpoints under /_control (not part of the contract).
//
// Erasable-syntax TypeScript only (no enums/parameter properties): Node 22
// type-stripping runs this file directly, and vitest imports it unchanged.

import { createHash, randomBytes } from "node:crypto";
import {
  TEMPLATE_LINUX,
  TEMPLATE_BROWSER,
  RETAINED_DISK,
  makeWorkspace,
  type RetainedFixture,
  type TemplateFixture,
  type WorkspaceFixture,
} from "./fixtures.ts";

export const SESSION_COOKIE = "tcdi_session";
export const CSRF_COOKIE = "tcdi_csrf";
export const CSRF_HEADER = "x-csrf-token";
export const CSRF_TOKEN_VALUE = "csrf-token-01J4ZD";
export const SESSION_PRINCIPAL = "session-01J4ZD";
// Mirrors SESSION_ORIGIN_COOKIE in src/api/client.ts: the login flow
// publishes the configured session origin so the SPA can pin launchUrl.
export const SESSION_ORIGIN_COOKIE = "tcdi_session_origin";

export interface MockRequest {
  method: string;
  path: string;
  query: URLSearchParams;
  headers: Record<string, string>;
  body?: Record<string, unknown>;
  rawBody?: string;
}

export interface MockResponse {
  status: number;
  headers: Record<string, string | string[]>;
  body: unknown;
}

interface IdemRecord {
  bodyHash: string;
  status: number;
  body: unknown;
}

interface TicketRecord {
  workspaceId: string;
  expiresAt: number;
  used: boolean;
}

interface MockState {
  templates: TemplateFixture[];
  invalidTemplates: Set<string>;
  workspaces: Map<string, WorkspaceFixture>;
  retained: Map<string, RetainedFixture>;
  nonces: Map<string, string>;
  idempotency: Map<string, IdemRecord>;
  leases: Map<string, string>;
  tickets: Map<string, TicketRecord>;
  quotaExhausted: boolean;
  requests: { method: string; path: string; headers: Record<string, string>; rawBody: string }[];
  launchRequests: { headers: Record<string, string>; rawBody: string }[];
}

let seq = 0;
const nextId = (prefix: string) => `${prefix}${String(++seq).padStart(4, "0")}`;

function err(status: number, code: string, message: string, retryable: boolean): MockResponse {
  return {
    status,
    headers: { "content-type": "application/json" },
    body: { code, message, retryable, requestId: nextId("req_") },
  };
}

function ok(status: number, body: unknown, headers: Record<string, string | string[]> = {}): MockResponse {
  return {
    status,
    headers: { "content-type": "application/json", ...headers },
    body,
  };
}

function parseCookies(header: string | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const part of String(header ?? "").split(";")) {
    const idx = part.indexOf("=");
    if (idx > 0) out[part.slice(0, idx).trim()] = part.slice(idx + 1).trim();
  }
  return out;
}

const hash = (s: string) => createHash("sha256").update(s).digest("hex");

// ---- launch origin policy (ADR 0004, mirrored from the real gateway in
// internal/gateway/launch.go so the e2e suites exercise the same gate) ----

// parseOriginValue parses a serialized Origin header value:
// scheme://host[:port] with no userinfo, path, query or fragment —
// "null" never parses (same rule as the gateway's parseOrigin).
function parseOriginValue(o: string | undefined): URL | undefined {
  if (!o) return undefined;
  let u: URL;
  try {
    u = new URL(o);
  } catch {
    return undefined;
  }
  if (
    (u.protocol !== "https:" && u.protocol !== "http:") ||
    !u.hostname ||
    u.username !== "" ||
    u.password !== "" ||
    u.pathname !== "/" ||
    u.search !== "" ||
    u.hash !== ""
  ) {
    return undefined;
  }
  return u;
}

// originsEqual compares two origins exactly on scheme+host+port, default
// ports normalized and hostnames case-insensitive (the gateway's
// originsEqual — browsers omit :443/:80 in Origin).
function originsEqual(a: URL, b: URL): boolean {
  const effPort = (u: URL) =>
    u.port || (u.protocol === "https:" ? "443" : u.protocol === "http:" ? "80" : "");
  return (
    a.protocol === b.protocol &&
    a.hostname.toLowerCase() === b.hostname.toLowerCase() &&
    effPort(a) === effPort(b)
  );
}

// gwErr answers with the session gateway's error body shape
// ({"error": code}), not the portal API shape used by the routes below.
function gwErr(status: number, code: string): MockResponse {
  return { status, headers: { "content-type": "application/json" }, body: { error: code } };
}

type IdemCheck =
  | { error: MockResponse }
  | { replay: { status: number; body: unknown } }
  | { record: (status: number, body: unknown) => void };

export function createMockApi(opts: { sessionOrigin?: string; portalOrigins?: string[] } = {}) {
  const sessionOrigin = opts.sessionOrigin ?? "http://127.0.0.1:4311";

  // The launch allowlist mirrors Config.PortalOrigins (ADR 0004) plus the
  // gateway's own public origin leg; a malformed entry is a config error,
  // not an empty allowlist.
  const mustOrigin = (raw: string, what: string): URL => {
    const u = parseOriginValue(raw);
    if (!u) throw new Error(`mock: bad ${what} ${raw}`);
    return u;
  };
  const sessionOriginUrl = mustOrigin(sessionOrigin, "sessionOrigin");
  const portalOrigins = (opts.portalOrigins ?? []).map((p) => mustOrigin(p, "portalOrigin"));

  const state: MockState = {
    templates: [],
    invalidTemplates: new Set(),
    workspaces: new Map(),
    retained: new Map(),
    nonces: new Map(),
    idempotency: new Map(),
    leases: new Map(),
    tickets: new Map(),
    quotaExhausted: false,
    requests: [],
    launchRequests: [],
  };

  function reset(): void {
    state.templates = [structuredClone(TEMPLATE_LINUX), structuredClone(TEMPLATE_BROWSER)];
    state.invalidTemplates = new Set();
    state.workspaces = new Map();
    const seed = makeWorkspace();
    state.workspaces.set(seed.id, seed);
    state.retained = new Map([[RETAINED_DISK.id, structuredClone(RETAINED_DISK)]]);
    state.nonces = new Map();
    state.idempotency = new Map();
    state.leases = new Map();
    state.tickets = new Map();
    state.quotaExhausted = false;
    state.requests = [];
    state.launchRequests = [];
  }
  reset();

  function freshNonce(dataId: string): string {
    const nonce = `nonce-${randomBytes(8).toString("hex")}`;
    state.nonces.set(dataId, nonce);
    return nonce;
  }

  function checkIdempotency(req: MockRequest): IdemCheck {
    const key = req.headers["idempotency-key"];
    if (!key) {
      return { error: err(400, "INVALID_REQUEST", "Idempotency-Key header is required", false) };
    }
    const bodyHash = hash(JSON.stringify(req.body ?? null));
    const prior = state.idempotency.get(key);
    if (prior) {
      if (prior.bodyHash !== bodyHash) {
        return {
          error: err(409, "IDEMPOTENCY_CONFLICT", "Idempotency-Key reused with a different body", false),
        };
      }
      return { replay: { status: prior.status, body: prior.body } };
    }
    return { record: (status, body) => state.idempotency.set(key, { bodyHash, status, body }) };
  }

  function handle(req: MockRequest): MockResponse {
    const { method, path } = req;
    if (path.startsWith("/_control/")) return control(req);
    if (path === "/v1/login") return login(req);
    if (path === "/v1/launch") return launch(req);
    if (path.startsWith("/desktop/")) return desktop(req);

    state.requests.push({
      method,
      path,
      headers: req.headers,
      rawBody: req.rawBody ?? "",
    });
    const cookies = parseCookies(req.headers.cookie);
    if (!cookies[SESSION_COOKIE]) {
      return err(401, "UNAUTHENTICATED", "no session", false);
    }
    if (method !== "GET" && method !== "HEAD") {
      if (!cookies[CSRF_COOKIE] || req.headers[CSRF_HEADER] !== cookies[CSRF_COOKIE]) {
        return err(403, "CSRF_FAILED", "missing or mismatched CSRF token", false);
      }
    }

    const wsMatch = path.match(/^\/v1\/workspaces\/([^/]+)(\/start|\/stop|\/connections)?$/);
    if (path === "/v1/workspaces" && method === "GET") return listWorkspaces(req);
    if (path === "/v1/workspaces" && method === "POST") return createWorkspace(req);
    if (wsMatch) {
      const ws = state.workspaces.get(wsMatch[1]);
      if (!ws) return err(404, "NOT_FOUND", "workspace not found", false);
      const sub = wsMatch[2] ?? "";
      if (!sub && method === "GET") return ok(200, ws);
      if (!sub && method === "DELETE") return deleteWorkspace(ws);
      if (sub === "/start" && method === "POST") return startWorkspace(req, ws);
      if (sub === "/stop" && method === "POST") return stopWorkspace(ws);
      if (sub === "/connections" && method === "POST") return createConnection(req, ws);
    }
    if (path === "/v1/templates" && method === "GET") return listTemplates(req);
    if (path === "/v1/data" && method === "GET") return listRetained();
    const dataMatch = path.match(/^\/v1\/data\/([^/]+)(\/attach|\/purge)?$/);
    if (dataMatch) {
      const rec = state.retained.get(dataMatch[1]);
      if (!rec) return err(404, "NOT_FOUND", "retained data record not found", false);
      if (dataMatch[2] === "/attach" && method === "POST") return attachData(req, rec);
      if (dataMatch[2] === "/purge" && method === "POST") return purgeData(req, rec);
    }
    return err(404, "NOT_FOUND", `no route ${method} ${path}`, false);
  }

  function listWorkspaces(req: MockRequest): MockResponse {
    const phase = req.query.get("phase");
    let items = [...state.workspaces.values()].sort((a, b) =>
      a.createdAt.localeCompare(b.createdAt),
    );
    if (phase) items = items.filter((w) => w.phase === phase);
    return ok(200, { items });
  }

  function createWorkspace(req: MockRequest): MockResponse {
    const idem = checkIdempotency(req);
    if ("error" in idem) return idem.error;
    if ("replay" in idem) return ok(idem.replay.status, idem.replay.body);
    if (state.quotaExhausted) {
      return err(409, "QUOTA_EXHAUSTED", "tenant quota has no headroom", false);
    }
    const { name, templateRef, desiredState, dataPolicy, retainedDataRef } = req.body ?? {};
    const tpl = state.templates.find((t) => t.id === templateRef);
    if (!tpl || state.invalidTemplates.has(String(templateRef))) {
      return err(422, "INVALID_TEMPLATE", "templateRef is unknown, unpublished or disallowed", false);
    }
    if (retainedDataRef !== undefined && !state.retained.has(String(retainedDataRef))) {
      return err(400, "INVALID_REQUEST", "retainedDataRef not found", false);
    }
    const running = desiredState === "Running";
    const ws = makeWorkspace({
      id: `${nextId("ws_")}X8KQ2M9X`,
      name: String(name ?? ""),
      template: {
        id: tpl.id,
        name: tpl.name,
        revision: tpl.revision,
        runtime: tpl.runtime,
        experience: tpl.experience,
      },
      phase: running ? "Provisioning" : "Pending",
      desiredState: running ? "Running" : "Stopped",
      dataPolicy: (typeof dataPolicy === "string"
        ? dataPolicy
        : tpl.dataPolicyDefault) as WorkspaceFixture["dataPolicy"],
      conditions: [
        {
          type: "Admitted",
          status: "True",
          reason: "QuotaReserved",
          lastTransitionTime: new Date().toISOString(),
        },
      ],
      ...(typeof retainedDataRef === "string" ? { retainedDataRef } : {}),
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
    });
    state.workspaces.set(ws.id, ws);
    idem.record(201, ws);
    return ok(201, ws);
  }

  function deleteWorkspace(ws: WorkspaceFixture): MockResponse {
    ws.phase = "Terminating";
    ws.updatedAt = new Date().toISOString();
    state.leases.delete(ws.id);
    return ok(202, ws);
  }

  function startWorkspace(req: MockRequest, ws: WorkspaceFixture): MockResponse {
    const idem = checkIdempotency(req);
    if ("error" in idem) return idem.error;
    if ("replay" in idem) return ok(idem.replay.status, idem.replay.body);
    if (ws.desiredState === "Running" && ws.phase !== "Terminating") return ok(200, ws);
    if (ws.phase !== "Stopped" && ws.phase !== "Failed") {
      return err(409, "INVALID_STATE", `cannot start while ${ws.phase}`, true);
    }
    ws.phase = "Provisioning";
    ws.desiredState = "Running";
    ws.updatedAt = new Date().toISOString();
    idem.record(200, ws);
    return ok(200, ws);
  }

  function stopWorkspace(ws: WorkspaceFixture): MockResponse {
    if (ws.phase !== "Stopped" && ws.phase !== "Stopping") {
      ws.phase = "Stopping";
    }
    ws.desiredState = "Stopped";
    ws.updatedAt = new Date().toISOString();
    state.leases.delete(ws.id);
    return ok(200, ws);
  }

  function createConnection(req: MockRequest, ws: WorkspaceFixture): MockResponse {
    if (!(ws.phase === "Ready" && ws.desiredState === "Running")) {
      return err(409, "INVALID_STATE", "workspace is not connectable", true);
    }
    const takeover = req.body?.takeover === true;
    if (state.leases.has(ws.id) && !takeover) {
      return err(
        409,
        "CONNECTION_IN_USE",
        "a live interactive lease exists; pass takeover to replace it",
        false,
      );
    }
    const ticket = `tkt_${randomBytes(24).toString("base64url")}`;
    state.tickets.set(ticket, {
      workspaceId: ws.id,
      expiresAt: Date.now() + 60_000,
      used: false,
    });
    state.leases.set(ws.id, nextId("lease_"));
    return ok(201, {
      workspaceId: ws.id,
      ticket,
      launchUrl: `${sessionOrigin}/v1/launch`,
      expiresAt: new Date(Date.now() + 60_000).toISOString(),
    });
  }

  function listTemplates(req: MockRequest): MockResponse {
    const runtime = req.query.get("runtime");
    let items = state.templates;
    if (runtime) items = items.filter((t) => t.runtime === runtime);
    return ok(200, { items });
  }

  function listRetained(): MockResponse {
    const items = [...state.retained.values()].map((r) => ({
      ...r,
      purgeConfirmationNonce: freshNonce(r.id),
    }));
    return ok(200, { items });
  }

  function attachData(req: MockRequest, rec: RetainedFixture): MockResponse {
    const idem = checkIdempotency(req);
    if ("error" in idem) return idem.error;
    if ("replay" in idem) return ok(idem.replay.status, idem.replay.body);
    if (rec.state !== "Retained") {
      return err(409, "INVALID_STATE", `disk is ${rec.state}`, true);
    }
    const tpl = state.templates.find((t) => t.id === req.body?.templateRef);
    if (!tpl || tpl.runtime !== rec.runtime) {
      return err(
        422,
        "INVALID_TEMPLATE",
        "template is unknown or incompatible with the disk runtime",
        false,
      );
    }
    rec.state = "Attaching";
    const ws = makeWorkspace({
      id: `${nextId("ws_")}ATTACH9X`,
      name: String(req.body?.name ?? "restored-desktop"),
      template: {
        id: tpl.id,
        name: tpl.name,
        revision: tpl.revision,
        runtime: tpl.runtime,
        experience: tpl.experience,
      },
      phase: req.body?.desiredState === "Running" ? "Provisioning" : "Pending",
      desiredState: req.body?.desiredState === "Running" ? "Running" : "Stopped",
      dataPolicy: "Retain",
      retainedDataRef: rec.id,
      conditions: [],
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
    });
    rec.consumingWorkspaceId = ws.id;
    state.workspaces.set(ws.id, ws);
    idem.record(201, ws);
    return ok(201, ws);
  }

  function purgeData(req: MockRequest, rec: RetainedFixture): MockResponse {
    const nonce = req.body?.confirmationNonce;
    if (!nonce || nonce !== state.nonces.get(rec.id)) {
      return err(400, "INVALID_REQUEST", "missing or stale confirmation nonce", false);
    }
    if (rec.state !== "Retained") {
      return err(409, "INVALID_STATE", `disk is ${rec.state}; only Retained records can be purged`, true);
    }
    rec.state = "Purging";
    const purged = { ...rec, purgeConfirmationNonce: freshNonce(rec.id) };
    return ok(202, purged);
  }

  // ---- session origin ----

  // launchOriginOK is the gateway's launchOriginOK (ADR 0004): the Origin
  // must exactly match a configured portal origin, or the session origin
  // itself on the request's authority (the public-origin leg).
  function launchOriginOK(req: MockRequest): boolean {
    const o = parseOriginValue(req.headers["origin"]);
    if (!o) return false;
    if (portalOrigins.some((p) => originsEqual(o, p))) return true;
    const host = req.headers["host"] ?? "";
    const reqHostname = host.startsWith("[") ? host.slice(0, host.indexOf("]") + 1) : host.split(":")[0];
    return o.hostname === reqHostname && originsEqual(o, sessionOriginUrl);
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
    if (!rec || rec.used || rec.expiresAt < Date.now()) {
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
        "set-cookie": `tcdi_desktop=${nextId("dsk_")}; Path=/; HttpOnly; Secure; SameSite=Lax`,
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

  // ---- auth bootstrap (dev/test login, not part of the public contract) ----

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
          "set-cookie": [
            `${SESSION_COOKIE}=${SESSION_PRINCIPAL}; Path=/; HttpOnly; SameSite=Strict`,
            `${CSRF_COOKIE}=${CSRF_TOKEN_VALUE}; Path=/; SameSite=Strict`,
            `${SESSION_ORIGIN_COOKIE}=${sessionOrigin}; Path=/; SameSite=Strict`,
          ],
        },
        body: "",
      };
    }
    return err(405, "INVALID_REQUEST", "method not allowed", false);
  }

  // ---- test control (not part of the contract) ----

  function control(req: MockRequest): MockResponse {
    const p = req.path;
    if (p === "/_control/health" && req.method === "GET") {
      return ok(200, { ok: true });
    }
    if (p === "/_control/reset" && req.method === "POST") {
      reset();
      return ok(200, { reset: true });
    }
    if (p === "/_control/requests" && req.method === "GET") {
      return ok(200, { requests: state.requests });
    }
    if (p === "/_control/requests/clear" && req.method === "POST") {
      state.requests = [];
      return ok(200, { cleared: true });
    }
    if (p === "/_control/launchRequests" && req.method === "GET") {
      return ok(200, { requests: state.launchRequests });
    }
    if (p === "/_control/quota" && req.method === "POST") {
      state.quotaExhausted = req.body?.exhausted === true;
      return ok(200, { quotaExhausted: state.quotaExhausted });
    }
    if (p === "/_control/templates/invalidate" && req.method === "POST") {
      state.invalidTemplates.add(String(req.body?.id));
      return ok(200, { invalid: [...state.invalidTemplates] });
    }
    const wsm = p.match(/^\/_control\/workspaces\/([^/]+)$/);
    if (wsm && req.method === "POST") {
      const ws = state.workspaces.get(wsm[1]);
      if (!ws) return err(404, "NOT_FOUND", "no such workspace", false);
      Object.assign(ws, req.body ?? {});
      return ok(200, ws);
    }
    if (p === "/_control/lease" && req.method === "POST") {
      const workspaceId = String(req.body?.workspaceId ?? "");
      if (req.body?.active) state.leases.set(workspaceId, nextId("lease_"));
      else state.leases.delete(workspaceId);
      return ok(200, { leases: [...state.leases.keys()] });
    }
    const retm = p.match(/^\/_control\/data\/([^/]+)$/);
    if (retm && req.method === "POST") {
      const rec = state.retained.get(retm[1]);
      if (!rec) return err(404, "NOT_FOUND", "no such record", false);
      Object.assign(rec, req.body ?? {});
      return ok(200, rec);
    }
    return err(404, "NOT_FOUND", `no control route ${req.method} ${p}`, false);
  }

  return { handle, state, reset };
}
