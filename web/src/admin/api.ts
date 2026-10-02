import type { ApiClient } from "../api/client";
import { unwrap } from "../api/client";
import type { components } from "../api/generated/schema";

// Admin/data-area API surface. GET /v1/me, GET /v1/quota, the `owner` field
// and the `scope` list filter are contract additions (backend); they are typed
// here until the generated schema carries them, and the calls go through the
// same openapi-fetch client (CSRF middleware, same-origin credentials).

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

export interface QuotaAmounts {
  workspaces: number;
  runningWorkspaces: number;
  cpuMillicores: number;
  memoryMib: number;
  storageGib: number;
}

export interface UserUsage {
  subject: string;
  displayName: string;
  usage: QuotaAmounts;
}

export interface QuotaView {
  tenant: string;
  // Absent when the tenant has no quota row; `workspaces: 0` means no count limit.
  limits?: Partial<QuotaAmounts>;
  usage: QuotaAmounts;
  userLimits?: QuotaAmounts;
  users: UserUsage[];
}

export type Scope = "mine" | "tenant";

export type ScopedWorkspace = WorkspaceView & { owner?: Owner };
export type ScopedRetainedData = RetainedDataView & { owner?: Owner };

// networkProfile, imageBuiltAt and imageStale are additive contract fields
// (template networkProfile from the CRD enum; imageBuiltAt/imageStale are the
// stale-image fields resolved from the template's image-built-at annotation).
// Optional here so the views compile against both schemas.
export type AdminTemplateView = components["schemas"]["TemplateView"] & {
  networkProfile?: string;
  imageBuiltAt?: string;
  imageStale?: boolean;
};

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
