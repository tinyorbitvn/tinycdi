import { newIdempotencyKey, unwrap, type ApiClient } from "../api/client";
import type { components } from "../api/generated/schema";
import { get, listAll, type Scope } from "../app/me";
import { getWorkspace } from "../workspaces/api";
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

export type AdminUserLimitsView = components["schemas"]["AdminUserLimitsView"];
export type AdminUserLimitEntry = components["schemas"]["AdminUserLimitEntry"];
export type SetUserLimitRequest = components["schemas"]["SetUserLimitRequest"];
export type SetUserLimitDefaultRequest = components["schemas"]["SetUserLimitDefaultRequest"];

// Per-user running-workspace limits
// (GET/PUT /v1/admin/tenants/{tenant}/user-limits[...]). Writes are upserts
// — a `null` limit clears the row back to the tenant default (or
// unlimited), so no If-Match precondition exists.
export async function fetchAdminUserLimits(api: ApiClient, tenant: string): Promise<AdminUserLimitsView> {
  return unwrap(
    await api.GET("/v1/admin/tenants/{tenant}/user-limits", { params: { path: { tenant } } }),
  );
}

export async function putAdminUserLimit(
  api: ApiClient,
  tenant: string,
  ownerRef: string,
  limit: number | null,
): Promise<AdminUserLimitsView> {
  const body: SetUserLimitRequest = { ownerRef, limit };
  return unwrap(
    await api.PUT("/v1/admin/tenants/{tenant}/user-limits", {
      params: { path: { tenant } },
      body,
    }),
  );
}

export async function putAdminUserLimitDefault(
  api: ApiClient,
  tenant: string,
  limit: number | null,
): Promise<AdminUserLimitsView> {
  const body: SetUserLimitDefaultRequest = { limit };
  return unwrap(
    await api.PUT("/v1/admin/tenants/{tenant}/user-limits/default", {
      params: { path: { tenant } },
      body,
    }),
  );
}

export function listScopedWorkspaces(api: ApiClient, scope: Scope, phase?: WorkspacePhase) {
  return listAll<ScopedWorkspace>(api, "/v1/workspaces", { scope, phase });
}

export function listScopedData(api: ApiClient, scope: Scope) {
  return listAll<ScopedRetainedData>(api, "/v1/data", { scope });
}

// Workspace lifecycle intents shared by the tenant workspaces table.
// `start` contractually requires an Idempotency-Key; `stop` accepts one,
// which keeps a retried click from double-signalling.
export async function startWorkspace(api: ApiClient, workspaceId: string): Promise<ScopedWorkspace> {
  return unwrap(
    await api.POST("/v1/workspaces/{workspaceId}/start", {
      params: {
        path: { workspaceId },
        header: { "Idempotency-Key": newIdempotencyKey() },
      },
    }),
  );
}

export async function stopWorkspace(api: ApiClient, workspaceId: string): Promise<ScopedWorkspace> {
  return unwrap(
    await api.POST("/v1/workspaces/{workspaceId}/stop", {
      params: {
        path: { workspaceId },
        header: { "Idempotency-Key": newIdempotencyKey() },
      },
    }),
  );
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

export function listTemplates(api: ApiClient) {
  return listAll<AdminTemplateView>(api, "/v1/templates", {});
}
