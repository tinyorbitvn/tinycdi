import type { ApiClient } from "../api/client";
import type { components } from "../api/generated/schema";
import { get, listAll, type Scope } from "../app/me";
export { templateFamily } from "../templates/family";

// Admin-area API surface: GET /v1/quota and the `scope` list filter, called
// through the same openapi-fetch client (CSRF middleware, same-origin
// credentials). The principal reads (Me/useLoader/listAll plumbing) are
// shared and live in ../app/me.

export type WorkspaceView = components["schemas"]["WorkspaceView"];
export type WorkspacePhase = components["schemas"]["WorkspacePhase"];
export type RetainedDataView = components["schemas"]["RetainedDataView"];
export type Owner = components["schemas"]["Owner"];

export type QuotaAmounts = components["schemas"]["QuotaAmounts"];
export type UserUsage = components["schemas"]["UserUsage"];
export type QuotaView = components["schemas"]["QuotaView"];

export type { Scope };
export type ScopedWorkspace = WorkspaceView;
export type ScopedRetainedData = RetainedDataView;
export type AdminTemplateView = components["schemas"]["TemplateView"];

export function fetchQuota(api: ApiClient): Promise<QuotaView> {
  return get<QuotaView>(api, "/v1/quota");
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
