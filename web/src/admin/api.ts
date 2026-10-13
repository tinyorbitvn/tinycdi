import { unwrap, type ApiClient } from "../api/client";
import type { components } from "../api/generated/schema";
import { get, type Scope } from "../app/me";
import { SCAN_PAGE_LIMIT, scanCollection, type ServerPage } from "../design/useServerPage";
import { getWorkspace, listWorkspacePage, startWorkspace, stopWorkspace } from "../workspaces/api";
import { listRetainedDataPage } from "../data/api";
import { listTemplatePage, scanTemplates } from "../templates/useTemplates";
export { templateFamily } from "../templates/family";
export { startWorkspace, stopWorkspace };

// Admin-area API surface: GET /v1/quota, the /v1/admin/tenants/{tenant}/*
// resources and the `scope` list filter, called through the same
// openapi-fetch client (CSRF middleware, same-origin credentials). The
// principal reads (Me/useLoader) are shared and live in ../app/me.
//
// Mutable admin resources carry a strong ETag (response header, or `etag`
// on a user-limit item). Every PUT/DELETE echoes the tag it was read with
// as If-Match, so a concurrent write answers 412 PRECONDITION_FAILED
// instead of being silently clobbered; `*` creates a resource that does not
// exist yet.

export type WorkspaceView = components["schemas"]["WorkspaceView"];
export type WorkspacePhase = components["schemas"]["WorkspacePhase"];
export type RetainedDataView = components["schemas"]["RetainedDataView"];
export type Owner = components["schemas"]["Owner"];

export type QuotaAmounts = components["schemas"]["QuotaAmounts"];
export type QuotaView = components["schemas"]["QuotaView"];
export type QuotaSource = components["schemas"]["QuotaSource"];
export type AdminQuotaLimits = components["schemas"]["AdminQuotaLimits"];
export type AdminQuotaView = components["schemas"]["AdminQuotaView"];

export type { Scope };
export type ScopedWorkspace = WorkspaceView;
export type ScopedRetainedData = RetainedDataView;
export type AdminTemplateView = components["schemas"]["TemplateView"];

/** A resource read together with the strong ETag its response carried. */
export type Tagged<T> = T & {
  /** The `ETag` response header, echoed as If-Match on the next write. */
  etag?: string;
};

function tagged<T>(res: { data?: T; error?: unknown; response: Response }): Tagged<T> {
  const body = unwrap(res);
  const etag = res.response.headers.get("ETag");
  return etag ? { ...body, etag } : (body as Tagged<T>);
}

export function fetchQuota(api: ApiClient): Promise<QuotaView> {
  return get<QuotaView>(api, "/v1/quota");
}

// Admin quota management (GET/PUT /v1/admin/tenants/{tenant}/quota): the
// tenant-admin-only view that also reports which layer owns the limits and
// writes them back when the platform configuration does not.
export async function fetchAdminQuota(api: ApiClient, tenant: string): Promise<Tagged<AdminQuotaView>> {
  return tagged(await api.GET("/v1/admin/tenants/{tenant}/quota", { params: { path: { tenant } } }));
}

// `etag` is the tag the GET returned; absent (no quota row yet) sends "*"
// to create the row.
export async function putAdminQuota(
  api: ApiClient,
  tenant: string,
  limits: AdminQuotaLimits,
  etag: string | undefined,
): Promise<Tagged<AdminQuotaView>> {
  return tagged(
    await api.PUT("/v1/admin/tenants/{tenant}/quota", {
      params: { path: { tenant }, header: { "If-Match": etag ?? "*" } },
      body: limits,
    }),
  );
}

export type AdminUserLimitEntry = components["schemas"]["AdminUserLimitEntry"];
export type AdminUserLimitDefault = components["schemas"]["AdminUserLimitDefault"];

/** Server default (and only) order of the per-user limits collection. */
export const USER_LIMIT_DEFAULT_SORT = "ownerRef";

// Per-user running-workspace limits: GET .../user-limits pages one item
// per principal with usage or an override (each override row carries its
// own `etag`); PUT/DELETE .../user-limits/{ownerRef} write one override;
// GET/PUT .../user-limits/default the tenant fallback.
export async function listAdminUserLimitPage(
  api: ApiClient,
  tenant: string,
  cursor?: string,
  sort: string = USER_LIMIT_DEFAULT_SORT,
  limit?: number,
): Promise<ServerPage<AdminUserLimitEntry>> {
  return unwrap(
    await api.GET("/v1/admin/tenants/{tenant}/user-limits", {
      params: {
        path: { tenant },
        query: { sort, ...(limit ? { limit } : {}), ...(cursor ? { cursor } : {}) },
      },
    }),
  );
}

export type AdminTenantUser = components["schemas"]["AdminTenantUser"];

// GET .../users: everyone who signed in to the tenant (the principal
// directory). `sort` is ownerRef | displayName | lastSeen, `-` = descending.
export async function listAdminUserPage(
  api: ApiClient,
  tenant: string,
  cursor?: string,
  sort?: string,
  limit?: number,
): Promise<ServerPage<AdminTenantUser>> {
  return unwrap(
    await api.GET("/v1/admin/tenants/{tenant}/users", {
      params: {
        path: { tenant },
        query: { ...(sort ? { sort } : {}), ...(limit ? { limit } : {}), ...(cursor ? { cursor } : {}) },
      },
    }),
  );
}

export async function getAdminUser(api: ApiClient, tenant: string, ownerRef: string): Promise<AdminTenantUser> {
  return unwrap(await api.GET("/v1/admin/tenants/{tenant}/users/{ownerRef}", { params: { path: { tenant, ownerRef } } }));
}

// Sets one principal's override. `etag` is the entry's tag; an entry
// without one has no override row yet, so "*" creates it.
export async function putAdminUserLimit(
  api: ApiClient,
  tenant: string,
  ownerRef: string,
  limit: number,
  etag: string | undefined,
): Promise<Tagged<AdminUserLimitEntry>> {
  return tagged(
    await api.PUT("/v1/admin/tenants/{tenant}/user-limits/{ownerRef}", {
      params: { path: { tenant, ownerRef }, header: { "If-Match": etag ?? "*" } },
      body: { limit },
    }),
  );
}

// Removes one principal's override: the tenant default applies again.
export async function deleteAdminUserLimit(
  api: ApiClient,
  tenant: string,
  ownerRef: string,
  etag: string,
): Promise<void> {
  unwrap(
    await api.DELETE("/v1/admin/tenants/{tenant}/user-limits/{ownerRef}", {
      params: { path: { tenant, ownerRef }, header: { "If-Match": etag } },
    }),
  );
}

export async function fetchAdminUserLimitDefault(
  api: ApiClient,
  tenant: string,
): Promise<Tagged<AdminUserLimitDefault>> {
  return tagged(
    await api.GET("/v1/admin/tenants/{tenant}/user-limits/default", { params: { path: { tenant } } }),
  );
}

// `limit: null` clears the default back to unlimited.
export async function putAdminUserLimitDefault(
  api: ApiClient,
  tenant: string,
  limit: number | null,
  etag: string | undefined,
): Promise<Tagged<AdminUserLimitDefault>> {
  return tagged(
    await api.PUT("/v1/admin/tenants/{tenant}/user-limits/default", {
      params: { path: { tenant }, header: { "If-Match": etag ?? "*" } },
      body: { limit },
    }),
  );
}

export type AdminSessionPolicyView = components["schemas"]["AdminSessionPolicyView"];
export type PutAdminSessionPolicy = components["schemas"]["PutAdminSessionPolicy"];
export type ClipboardCeiling = components["schemas"]["ClipboardCeiling"];
export type AdminSessionPolicySource = components["schemas"]["AdminSessionPolicySource"];

// Tenant session data-transfer ceiling
// (GET/PUT /v1/admin/tenants/{tenant}/session-policy, ADR 0010): a
// ceiling narrows every template's own transfer policy, never widens.
// No row (no ETag) writes with "*" to create one.
export async function fetchAdminSessionPolicy(
  api: ApiClient,
  tenant: string,
): Promise<Tagged<AdminSessionPolicyView>> {
  return tagged(
    await api.GET("/v1/admin/tenants/{tenant}/session-policy", {
      params: { path: { tenant } },
    }),
  );
}

export async function putAdminSessionPolicy(
  api: ApiClient,
  tenant: string,
  policy: PutAdminSessionPolicy,
  etag: string | undefined,
): Promise<Tagged<AdminSessionPolicyView>> {
  return tagged(
    await api.PUT("/v1/admin/tenants/{tenant}/session-policy", {
      params: { path: { tenant }, header: { "If-Match": etag ?? "*" } },
      body: policy,
    }),
  );
}

// Scoped collections. Tables page through them with useServerTable; the
// scan* readers serve views that aggregate across the whole tenant
// (overview counts, per-template and per-user tallies), bounded by
// SCAN_MAX_PAGES: past it they reject instead of returning a partial list.
export function listScopedWorkspacePage(
  api: ApiClient,
  scope: Scope,
  phase: WorkspacePhase | undefined,
  cursor?: string,
  sort?: string,
): Promise<ServerPage<ScopedWorkspace>> {
  return listWorkspacePage(api, { scope, ...(phase ? { phase } : {}) }, cursor, sort);
}

export function scanScopedWorkspaces(api: ApiClient, scope: Scope): Promise<ScopedWorkspace[]> {
  return scanCollection((cursor, sort) => listWorkspacePage(api, { scope, limit: SCAN_PAGE_LIMIT }, cursor, sort));
}

export function listScopedDataPage(
  api: ApiClient,
  scope: Scope,
  cursor?: string,
  sort?: string,
): Promise<ServerPage<ScopedRetainedData>> {
  return listRetainedDataPage(api, scope, cursor, sort);
}

export function scanScopedData(api: ApiClient, scope: Scope): Promise<ScopedRetainedData[]> {
  return scanCollection((cursor, sort) => listRetainedDataPage(api, scope, cursor, sort, SCAN_PAGE_LIMIT));
}

export type RestartStep = "stopping" | "waiting" | "starting";

// Restart is client-side sequencing — the API has no composite restart
// intent: POST stop, poll the workspace until it reaches a startable
// terminal phase (Stopped; Failed also accepts a start), then POST start.
export interface RestartOptions {
  /** Workspace read used between polls (injectable for tests). */
  fetch?: (api: ApiClient, workspaceId: string) => Promise<ScopedWorkspace>;
  sleep?: (ms: number) => Promise<void>;
  now?: () => number;
  pollMs?: number;
  timeoutMs?: number;
  /** Progress for the confirm dialog's live status line. */
  onStep?: (step: RestartStep) => void;
}

const RESTART_POLL_MS = 1_500;
const RESTART_TIMEOUT_MS = 120_000;

// The stop half of a restart never finished inside the deadline.
export class RestartTimeoutError extends Error {
  constructor() {
    super("restart wait timed out");
    this.name = "RestartTimeoutError";
  }
}

// The workspace left the lifecycle entirely (delete won the race) while a
// restart waited for Stopped — starting is no longer possible.
export class RestartAbortedError extends Error {
  readonly phase: WorkspacePhase;
  constructor(phase: WorkspacePhase) {
    super(`workspace became ${phase}`);
    this.name = "RestartAbortedError";
    this.phase = phase;
  }
}

export async function restartWorkspace(
  api: ApiClient,
  workspaceId: string,
  opts: RestartOptions = {},
): Promise<ScopedWorkspace> {
  const fetchWs = opts.fetch ?? ((a: ApiClient, id: string) => getWorkspace(a, id));
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
  const now = opts.now ?? Date.now;
  const pollMs = opts.pollMs ?? RESTART_POLL_MS;
  const deadline = now() + (opts.timeoutMs ?? RESTART_TIMEOUT_MS);

  opts.onStep?.("stopping");
  await stopWorkspace(api, workspaceId);

  opts.onStep?.("waiting");
  let ws = await fetchWs(api, workspaceId);
  while (ws.phase !== "Stopped" && ws.phase !== "Failed") {
    if (ws.phase === "Terminating") throw new RestartAbortedError(ws.phase);
    if (now() >= deadline) throw new RestartTimeoutError();
    await sleep(pollMs);
    ws = await fetchWs(api, workspaceId);
  }

  opts.onStep?.("starting");
  return startWorkspace(api, workspaceId);
}

export function listTemplatePageAdmin(
  api: ApiClient,
  cursor?: string,
  sort?: string,
): Promise<ServerPage<AdminTemplateView>> {
  return listTemplatePage(api, {}, cursor, sort);
}

export function scanAdminTemplates(api: ApiClient): Promise<AdminTemplateView[]> {
  return scanTemplates(api);
}

export type AdminAuditEntry = components["schemas"]["AdminAuditEntry"];
export type AuditFilter = { action?: string; actor?: string; target?: string; since?: string; until?: string };

// GET .../audit: newest first (the only order), filtered server-side. Empty
// filter fields are omitted. exactOptionalPropertyTypes is on, so the query
// is built with conditional spreads, not undefined values.
export async function listAdminAuditPage(
  api: ApiClient,
  tenant: string,
  filter: AuditFilter,
  cursor?: string,
): Promise<ServerPage<AdminAuditEntry>> {
  return unwrap(
    await api.GET("/v1/admin/tenants/{tenant}/audit", {
      params: {
        path: { tenant },
        query: {
          sort: "-createdAt",
          ...(filter.action ? { action: filter.action } : {}),
          ...(filter.actor ? { actor: filter.actor } : {}),
          ...(filter.target ? { target: filter.target } : {}),
          ...(filter.since ? { since: filter.since } : {}),
          ...(filter.until ? { until: filter.until } : {}),
          ...(cursor ? { cursor } : {}),
        },
      },
    }),
  );
}
