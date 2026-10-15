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
import { Buffer } from "node:buffer";
import type {
  RetainedFixture,
  TemplateFixture,
  WorkspaceEventFixture,
  WorkspaceFixture,
} from "./fixtures.ts";

export const SESSION_COOKIE = "tcdi_session";
export const CSRF_HEADER = "x-csrf-token";
export const CSRF_TOKEN_VALUE = "csrf-token-01J4ZD";
export const SESSION_PRINCIPAL = "session-01J4ZD";

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
  requests: { method: string; path: string; query: string; headers: Record<string, string>; rawBody: string }[];
  launchRequests: { headers: Record<string, string>; rawBody: string }[];
}

export type IdemCheck =
  | { error: MockResponse }
  | { replay: { status: number; body: unknown } }
  | { record: (status: number, body: unknown) => void };

export interface MockOptions {
  sessionOrigin?: string;
  // The host[:port] /v1/me publishes as sessionDomain and launch URLs are
  // built under (D9/D17). Defaults to the session origin's host — pass a
  // wildcard-resolvable name (e.g. localhost:<port>) when a real browser
  // must reach ws-<label> hosts.
  sessionDomain?: string;
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
  readonly sessionDomain: string;
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
    body: { error: { code, message, retryable, requestId: `req_${String(++reqSeq).padStart(4, "0")}` } },
  };
}

// Sort comparators by allowlisted field name; `-field` reverses one.
export type SortFields<T> = Record<string, (a: T, b: T) => number>;

// paginate answers a /v1 list the way the contract pages: `limit` (default
// 50, max 500), `sort` (allowlisted field, `-` prefix descending, anything
// else 400) and an opaque `cursor` bound to the sort it was minted under
// (a cursor from another sort is 400). Rows tie-break on `id`. The cursor
// is an offset in this mock — the real server is keyset-based.
export function paginate<T>(
  req: MockRequest,
  rows: readonly T[],
  fields: SortFields<T>,
  defaultSort: string,
  idOf: (row: T) => string = (row) => String((row as { id?: string }).id ?? ""),
): MockResponse {
  const sort = req.query.get("sort") || defaultSort;
  const field = sort.startsWith("-") ? sort.slice(1) : sort;
  const cmp = fields[field];
  if (!cmp) return err(400, "INVALID_REQUEST", `sort must be one of ${Object.keys(fields).join(", ")}`, false);
  const dir = sort.startsWith("-") ? -1 : 1;
  const sorted = [...rows].sort((a, b) => dir * (cmp(a, b) || idOf(a).localeCompare(idOf(b))));
  const rawLimit = req.query.get("limit");
  const limit = rawLimit === null ? 50 : Number(rawLimit);
  if (!Number.isInteger(limit) || limit < 1 || limit > 500) {
    return err(400, "INVALID_REQUEST", "limit must be 1..500", false);
  }
  let offset = 0;
  const cursor = req.query.get("cursor");
  if (cursor) {
    const [rawOffset, cursorSort] = Buffer.from(cursor, "base64url").toString().split("@");
    offset = Number(rawOffset);
    if (cursorSort !== sort || !Number.isInteger(offset) || offset < 0) {
      return err(400, "INVALID_REQUEST", "cursor does not match this sort", false);
    }
  }
  const page = sorted.slice(offset, offset + limit);
  const next =
    offset + limit < sorted.length ? Buffer.from(`${offset + limit}@${sort}`).toString("base64url") : undefined;
  return ok(200, next ? { items: page, nextCursor: next } : { items: page });
}

export const byString =
  <T>(get: (row: T) => string) =>
  (a: T, b: T): number =>
    get(a).localeCompare(get(b));

// GET /v1/workspaces sorts (default `-createdAt`).
export const WORKSPACE_SORTS: SortFields<{ createdAt: string; name: string }> = {
  createdAt: byString((w) => w.createdAt),
  name: byString((w) => w.name),
};

// GET /v1/data sorts (default `-retainedAt`); `name` is the source
// workspace name.
export const DATA_SORTS: SortFields<{ retainedAt: string; sourceWorkspaceName: string }> = {
  retainedAt: byString((r) => r.retainedAt),
  name: byString((r) => r.sourceWorkspaceName),
};

// Strong entity tags: a quoted opaque token, compared byte for byte.
export function etagOf(tag: string): string {
  return `"${tag}"`;
}

// checkIfMatch applies the admin write precondition: missing If-Match →
// 428 PRECONDITION_REQUIRED; `*` only while the resource does not exist;
// anything but the current tag → 412 PRECONDITION_FAILED.
export function checkIfMatch(req: MockRequest, current: string | undefined): MockResponse | undefined {
  const ifMatch = req.headers["if-match"];
  if (!ifMatch) return err(428, "PRECONDITION_REQUIRED", "If-Match is required", false);
  if (ifMatch === "*" ? current === undefined : ifMatch === current) return undefined;
  return err(412, "PRECONDITION_FAILED", "the resource changed since it was read", false);
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

/** ws_<suffix> -> ws-<suffix>, the mapping internal/sessionhost.Label pins. */
export function sessionHostLabel(workspaceId: string): string {
  return workspaceId.replaceAll("_", "-").toLowerCase();
}

// Absolute launchUrl for a workspace (D9): the real backend always emits
// https://ws-<label>.<sessionDomain>/v1/launch; the mock keeps its session
// listener's scheme so a browser e2e on loopback can reach it.
export function sessionLaunchUrl(ctx: MockContext, workspaceId: string): string {
  return `${ctx.sessionOriginUrl.protocol}//${sessionHostLabel(workspaceId)}.${ctx.sessionDomain}/v1/launch`;
}

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
