import type { ApiClient } from "../api/client";
import { unwrap } from "../api/client";
import type { components } from "../api/generated/schema";
export { templateFamily } from "../templates/family";

// Admin/data-area API surface: GET /v1/me, GET /v1/quota and the `scope` list
// filter, called through the same openapi-fetch client (CSRF middleware,
// same-origin credentials).

export type WorkspaceView = components["schemas"]["WorkspaceView"];
export type WorkspacePhase = components["schemas"]["WorkspacePhase"];
export type RetainedDataView = components["schemas"]["RetainedDataView"];

export const TENANT_ADMIN = "tenant-admin";

export interface Owner {
  subject: string;
  displayName: string;
}

export interface Me {
  subject: string;
  displayName: string;
  email?: string;
  tenant: string;
  roles: string[];
}

export type QuotaAmounts = components["schemas"]["QuotaAmounts"];
export type UserUsage = components["schemas"]["UserUsage"];
export type QuotaView = components["schemas"]["QuotaView"];
export type QuotaSource = components["schemas"]["QuotaSource"];
export type AdminQuotaLimits = components["schemas"]["AdminQuotaLimits"];
export type AdminQuotaView = components["schemas"]["AdminQuotaView"];

export type Scope = "mine" | "tenant";

export type ScopedWorkspace = WorkspaceView;
export type ScopedRetainedData = RetainedDataView;
export type AdminTemplateView = components["schemas"]["TemplateView"];

interface Page<T> {
  items: T[];
  nextPageToken?: string;
}

type Query = Record<string, string | number | undefined>;

// openapi-fetch's runtime accepts any path; only its typing is bound to the
// generated `paths`. This narrow view covers the not-yet-generated reads.
interface LooseGet {
  GET(
    path: string,
    init: { params?: { query?: Query } },
  ): Promise<{ data?: unknown; error?: unknown; response: Response }>;
}

async function get<T>(api: ApiClient, path: string, query?: Query): Promise<T> {
  const loose = api as unknown as LooseGet;
  return unwrap(await loose.GET(path, query ? { params: { query } } : {})) as T;
}

export function isTenantAdmin(me: Pick<Me, "roles"> | null | undefined): boolean {
  return !!me && me.roles.includes(TENANT_ADMIN);
}

export function fetchMe(api: ApiClient): Promise<Me> {
  return get<Me>(api, "/v1/me");
}

export function fetchQuota(api: ApiClient): Promise<QuotaView> {
  return get<QuotaView>(api, "/v1/quota");
}

// Admin quota management (GET/PUT /v1/admin/tenants/{tenant}/quota): the
// tenant-admin-only view that also reports which layer owns the limits and
// writes them back when the platform configuration does not.
export async function fetchAdminQuota(api: ApiClient, tenant: string): Promise<AdminQuotaView> {
  return unwrap(
    await api.GET("/v1/admin/tenants/{tenant}/quota", { params: { path: { tenant } } }),
  );
}

export async function putAdminQuota(
  api: ApiClient,
  tenant: string,
  limits: AdminQuotaLimits,
): Promise<AdminQuotaView> {
  return unwrap(
    await api.PUT("/v1/admin/tenants/{tenant}/quota", {
      params: { path: { tenant } },
      body: limits,
    }),
  );
}

// Lists stop following cursors at `maxPages` so a runaway server cannot pin
// the tab; the admin views show a "partial" hint when that happens.
export const MAX_PAGES = 20;
const PAGE_LIMIT = 200;

async function listAll<T>(
  api: ApiClient,
  path: string,
  query: Query,
): Promise<{ items: T[]; truncated: boolean }> {
  const items: T[] = [];
  let pageToken: string | undefined;
  for (let i = 0; i < MAX_PAGES; i++) {
    const page = await get<Page<T>>(api, path, { ...query, limit: PAGE_LIMIT, pageToken });
    items.push(...page.items);
    if (!page.nextPageToken) return { items, truncated: false };
    pageToken = page.nextPageToken;
  }
  return { items, truncated: true };
}

export function listScopedWorkspaces(api: ApiClient, scope: Scope, phase?: WorkspacePhase) {
  return listAll<ScopedWorkspace>(api, "/v1/workspaces", { scope, phase });
}

export function listScopedData(api: ApiClient, scope: Scope) {
  return listAll<ScopedRetainedData>(api, "/v1/data", { scope });
}

export function listTemplates(api: ApiClient) {
  return listAll<AdminTemplateView>(api, "/v1/templates", {});
}
