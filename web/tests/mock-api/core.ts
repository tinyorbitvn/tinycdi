// Shared core of the contract mock (internal/api/openapi.yaml): request and
// response shapes, the shared contract state, response helpers and the
// MockArea interface every handler file implements.
//
// Composition: handler.ts builds one MockContext, instantiates every area
// listed in areas.ts against it, and routes each request through them. To
// mock a new part of the API, add web/tests/mock-api/<area>.ts exporting a
// MockAreaFactory and register it in areas.ts — no edits to handler.ts.
//
// Erasable-syntax TypeScript only (no enums/parameter properties): Node 22
// type-stripping runs these files directly, and vitest imports them unchanged.

import { createHash } from "node:crypto";
import type {
  RetainedFixture,
  TemplateFixture,
  WorkspaceEventFixture,
  WorkspaceFixture,
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

export interface IdemRecord {
  bodyHash: string;
  status: number;
  body: unknown;
}

export interface TicketRecord {
  workspaceId: string;
  expiresAt: number;
  used: boolean;
}

// Contract state shared by every area. Area-private state belongs in the
// area factory's closure (exposed via MockArea.state when tests need it).
export interface MockState {
  templates: TemplateFixture[];
  invalidTemplates: Set<string>;
  workspaces: Map<string, WorkspaceFixture>;
  events: Map<string, WorkspaceEventFixture[]>;
  retained: Map<string, RetainedFixture>;
  nonces: Map<string, string>;
  idempotency: Map<string, IdemRecord>;
  leases: Map<string, string>;
  tickets: Map<string, TicketRecord>;
  quotaExhausted: boolean;
  requests: { method: string; path: string; headers: Record<string, string>; rawBody: string }[];
  launchRequests: { headers: Record<string, string>; rawBody: string }[];
}

export type IdemCheck =
  | { error: MockResponse }
  | { replay: { status: number; body: unknown } }
  | { record: (status: number, body: unknown) => void };

export interface MockOptions {
  sessionOrigin?: string;
  portalOrigins?: string[];
  // Demo mode seeds a realistic multi-workspace tenant and advances
  // transitional phases on a timer (dev server). Off for tests, which drive
  // every transition explicitly through /_control.
  demo?: boolean;
  // Clock override for deterministic demo-progression tests.
  now?: () => number;
}

export interface MockContext {
  readonly sessionOrigin: string;
  readonly sessionOriginUrl: URL;
  readonly portalOrigins: readonly URL[];
  readonly demo: boolean;
  readonly state: MockState;
  now(): number;
  nowIso(): string;
  nextId(prefix: string): string;
  checkIdempotency(req: MockRequest): IdemCheck;
  // Records a Kubernetes-style event against a workspace (newest first).
  recordEvent(workspaceId: string, ev: Omit<WorkspaceEventFixture, "lastTimestamp"> & { lastTimestamp?: string }): void;
}

export interface MockArea {
  name: string;
  // Re-seeds this area's slice of state (called at startup and on
  // POST /_control/reset, after the core state is cleared).
  reset?(): void;
  // Routes reachable without a portal session: login bootstrap and the
  // session origin (/v1/launch, /desktop/*). Return undefined to pass.
  public?(req: MockRequest): MockResponse | undefined;
  // Authenticated /v1 routes; the composer has already enforced the
  // session cookie and CSRF header. Return undefined to pass.
  api?(req: MockRequest): MockResponse | undefined;
  // Test-control routes under /_control (not part of the contract).
  control?(req: MockRequest): MockResponse | undefined;
  // Demo mode only: advance simulated controllers to time `now`.
  tick?(now: number): void;
  // Area-private state, exposed for tests (api.areas.<name>.state).
  state?: unknown;
}

export type MockAreaFactory = (ctx: MockContext) => MockArea;

let reqSeq = 0;

export function err(status: number, code: string, message: string, retryable: boolean): MockResponse {
  return {
    status,
    headers: { "content-type": "application/json" },
    body: { code, message, retryable, requestId: `req_${String(++reqSeq).padStart(4, "0")}` },
  };
}

export function ok(status: number, body: unknown, headers: Record<string, string | string[]> = {}): MockResponse {
  return {
    status,
    headers: { "content-type": "application/json", ...headers },
    body,
  };
}

// gwErr answers with the session gateway's error body shape
// ({"error": code}), not the portal API shape.
export function gwErr(status: number, code: string): MockResponse {
  return { status, headers: { "content-type": "application/json" }, body: { error: code } };
}

export function parseCookies(header: string | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  for (const part of String(header ?? "").split(";")) {
    const idx = part.indexOf("=");
    if (idx > 0) out[part.slice(0, idx).trim()] = part.slice(idx + 1).trim();
  }
  return out;
}

export const hash = (s: string) => createHash("sha256").update(s).digest("hex");

// parseOriginValue parses a serialized Origin header value:
// scheme://host[:port] with no userinfo, path, query or fragment —
// "null" never parses (same rule as the gateway's parseOrigin).
export function parseOriginValue(o: string | undefined): URL | undefined {
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
export function originsEqual(a: URL, b: URL): boolean {
  const effPort = (u: URL) =>
    u.port || (u.protocol === "https:" ? "443" : u.protocol === "http:" ? "80" : "");
  return (
    a.protocol === b.protocol &&
    a.hostname.toLowerCase() === b.hostname.toLowerCase() &&
    effPort(a) === effPort(b)
  );
}

export function emptyState(): MockState {
  return {
    templates: [],
    invalidTemplates: new Set(),
    workspaces: new Map(),
    events: new Map(),
    retained: new Map(),
    nonces: new Map(),
    idempotency: new Map(),
    leases: new Map(),
    tickets: new Map(),
    quotaExhausted: false,
    requests: [],
    launchRequests: [],
  };
}

// clearState empties the shared state in place so references held by
// areas and tests (api.state) stay valid across resets.
export function clearState(state: MockState): void {
  Object.assign(state, emptyState());
}
