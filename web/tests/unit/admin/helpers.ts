import type { MockApi } from "../../mock-api/handler.ts";
import { ok, type MockArea, type MockContext } from "../../mock-api/core.ts";

// Drives a /_control route on the mock (unit-test sugar for the same routes
// Playwright specs hit over HTTP).
export function control(api: MockApi, path: string, body?: Record<string, unknown>) {
  return api.handle({
    method: "POST",
    path,
    query: new URLSearchParams(),
    headers: {},
    rawBody: JSON.stringify(body ?? null),
    ...(body !== undefined ? { body } : {}),
  });
}

// Makes the mock principal a tenant admin (roles: ["user", "tenant-admin"]).
export function makeAdmin(api: MockApi): void {
  const res = control(api, "/_control/admin/me", { roles: ["user", "tenant-admin"] });
  if (res.status !== 200) throw new Error(`makeAdmin failed: ${res.status}`);
}

// Seeds other users' workspaces and retained disks (Grace Hopper, Linus
// Pauling) so tenant-scope views have rows to show.
export function seedTenant(api: MockApi): void {
  const res = control(api, "/_control/admin/seed");
  if (res.status !== 200) throw new Error(`seedTenant failed: ${res.status}`);
}

// Arms a one-shot failure for the next GET of `path` (POST
// /_control/admin/fail).
export function failNextGet(api: MockApi, path: string, status = 503, code = "UNAVAILABLE"): void {
  const res = control(api, "/_control/admin/fail", { path, status, code });
  if (res.status !== 200) throw new Error(`failNextGet failed: ${res.status}`);
}

// Minimal /v1/templates area for unit tests that run without the workspaces
// area (which owns the real template routes). Serves ctx.state.templates
// verbatim so tests can inject imageBuiltAt/imageStale fields.
export function templatesStubArea(ctx: MockContext): MockArea {
  return {
    name: "templates-stub",
    api: (req) =>
      req.method === "GET" && req.path === "/v1/templates"
        ? ok(200, { items: ctx.state.templates })
        : undefined,
  };
}
