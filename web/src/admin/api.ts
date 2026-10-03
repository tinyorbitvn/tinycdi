import { unwrap, type ApiClient } from "../api/client";
import type { components } from "../api/generated/schema";
import { get, listAll, type Scope } from "../app/me";
export { templateFamily } from "../templates/family";

// Admin-area API surface: GET /v1/quota, the /v1/admin/tenants/{tenant}/quota
// pair and the `scope` list filter, called through the same openapi-fetch
// client (CSRF middleware, same-origin credentials). The principal reads
// (Me/useLoader/listAll plumbing) are shared and live in ../app/me.

export type WorkspaceView = components["schemas"]["WorkspaceView"];
export type WorkspacePhase = components["schemas"]["WorkspacePhase"];
export type RetainedDataView = components["schemas"]["RetainedDataView"];
export type Owner = components["schemas"]["Owner"];

export type QuotaAmounts = components["schemas"]["QuotaAmounts"];
export type UserUsage = components["schemas"]["UserUsage"];
export type QuotaView = components["schemas"]["QuotaView"];
export type QuotaSource = components["schemas"]["QuotaSource"];
export type AdminQuotaLimits = components["schemas"]["AdminQuotaLimits"];
export type AdminQuotaView = components["schemas"]["AdminQuotaView"];

export type { Scope };
export type ScopedWorkspace = WorkspaceView;
export type ScopedRetainedData = RetainedDataView;
export type AdminTemplateView = components["schemas"]["TemplateView"];

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

// `version` is the change token the GET returned — sent as If-Match so a
// concurrent write lands as 412 PRECONDITION_FAILED, not a silent clobber.
// Pass "*" when the GET reported no row (version absent) to create it.
export async function putAdminQuota(
  api: ApiClient,
  tenant: string,
  limits: AdminQuotaLimits,
  version: string | undefined,
): Promise<AdminQuotaView> {
  return unwrap(
    await api.PUT("/v1/admin/tenants/{tenant}/quota", {
      params: { path: { tenant }, header: { "If-Match": version ?? "*" } },
      body: limits,
    }),
  );
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
