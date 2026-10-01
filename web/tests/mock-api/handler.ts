// Contract-faithful mock of the tinycdi public API (internal/api/openapi.yaml).
// Shared between the node HTTP server (Playwright, dev server) and the vitest
// fetch stub. This file is the composer only: it owns the shared context
// (session/CSRF gate, idempotency, request log, reset) and routes requests
// through the areas registered in areas.ts — portal API under /v1, session
// origin (/v1/launch, /desktop/*), and test control under /_control (not
// part of the contract).
//
// Erasable-syntax TypeScript only (no enums/parameter properties): Node 22
// type-stripping runs this file directly, and vitest imports it unchanged.

import {
  CSRF_COOKIE,
  CSRF_HEADER,
  CSRF_TOKEN_VALUE,
  SESSION_COOKIE,
  clearState,
  emptyState,
  err,
  hash,
  ok,
  parseCookies,
  parseOriginValue,
  type IdemCheck,
  type MockArea,
  type MockAreaFactory,
  type MockContext,
  type MockOptions,
  type MockRequest,
  type MockResponse,
} from "./core.ts";
import { AREAS } from "./areas.ts";

export {
  CSRF_COOKIE,
  CSRF_HEADER,
  CSRF_TOKEN_VALUE,
  SESSION_COOKIE,
  SESSION_ORIGIN_COOKIE,
  SESSION_PRINCIPAL,
} from "./core.ts";
export type { MockArea, MockAreaFactory, MockContext, MockOptions, MockRequest, MockResponse } from "./core.ts";

export function createMockApi(opts: MockOptions & { areas?: readonly MockAreaFactory[] } = {}) {
  const sessionOrigin = opts.sessionOrigin ?? "http://127.0.0.1:4311";

  // The launch allowlist mirrors Config.PortalOrigins (ADR 0004) plus the
  // gateway's own public origin leg; a malformed entry is a config error,
  // not an empty allowlist.
  const mustOrigin = (raw: string, what: string): URL => {
    const u = parseOriginValue(raw);
    if (!u) throw new Error(`mock: bad ${what} ${raw}`);
    return u;
  };

  const state = emptyState();
  let seq = 0;
  const now = opts.now ?? (() => Date.now());

  const ctx: MockContext = {
    sessionOrigin,
    sessionOriginUrl: mustOrigin(sessionOrigin, "sessionOrigin"),
    sessionDomain: opts.sessionDomain ?? mustOrigin(sessionOrigin, "sessionOrigin").host,
    portalOrigins: (opts.portalOrigins ?? []).map((p) => mustOrigin(p, "portalOrigin")),
    demo: opts.demo === true,
    state,
    now,
    nowIso: () => new Date(now()).toISOString(),
    nextId: (prefix) => `${prefix}${String(++seq).padStart(4, "0")}`,
    checkIdempotency,
    recordEvent(workspaceId, ev) {
      const list = state.events.get(workspaceId) ?? [];
      const ts = ev.lastTimestamp ?? ctx.nowIso();
      const head = list[0];
      // Collapse repeats like the Kubernetes event recorder does.
      if (head && head.reason === ev.reason && head.message === ev.message && head.type === ev.type) {
        head.count = (head.count ?? 1) + 1;
        head.firstTimestamp ??= head.lastTimestamp;
        head.lastTimestamp = ts;
      } else {
        list.unshift({ ...ev, lastTimestamp: ts });
      }
      state.events.set(workspaceId, list.slice(0, 50));
    },
  };

  const areas: MockArea[] = (opts.areas ?? AREAS).map((f) => f(ctx));
  const byName: Record<string, MockArea> = Object.fromEntries(areas.map((a) => [a.name, a]));

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

  function reset(): void {
    clearState(state);
    for (const a of areas) a.reset?.();
  }
  reset();

  function firstMatch(fn: (a: MockArea) => MockResponse | undefined): MockResponse | undefined {
    for (const a of areas) {
      const r = fn(a);
      if (r) return r;
    }
    return undefined;
  }

  function handle(req: MockRequest): MockResponse {
    const { method, path } = req;
    if (ctx.demo) for (const a of areas) a.tick?.(now());
    if (path.startsWith("/_control/")) return control(req);

    const pub = firstMatch((a) => a.public?.(req));
    if (pub) return pub;

    state.requests.push({ method, path, headers: req.headers, rawBody: req.rawBody ?? "" });
    const cookies = parseCookies(req.headers.cookie);
    if (!cookies[SESSION_COOKIE]) {
      return err(401, "UNAUTHENTICATED", "no session", false);
    }
    // v0.2 (P1/D17): X-CSRF-Token is the synchronizer token /v1/me
    // publishes, compared directly — there is no tcdi_csrf cookie anymore.
    if (method !== "GET" && method !== "HEAD") {
      if (req.headers[CSRF_HEADER] !== CSRF_TOKEN_VALUE) {
        return err(403, "CSRF_FAILED", "missing or mismatched CSRF token", false);
      }
    }
    return firstMatch((a) => a.api?.(req)) ?? err(404, "NOT_FOUND", `no route ${method} ${path}`, false);
  }

  function control(req: MockRequest): MockResponse {
    const p = req.path;
    if (p === "/_control/health" && req.method === "GET") return ok(200, { ok: true });
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
    return (
      firstMatch((a) => a.control?.(req)) ??
      err(404, "NOT_FOUND", `no control route ${req.method} ${p}`, false)
    );
  }

  return { handle, state, reset, areas: byName, ctx };
}

export type MockApi = ReturnType<typeof createMockApi>;
