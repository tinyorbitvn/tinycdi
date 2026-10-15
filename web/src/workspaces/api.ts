import { newIdempotencyKey, unwrap, type ApiClient } from "../api/client";
import { noteServerDateHeader } from "../progress/derive";
import type { ServerPage } from "../design/useServerPage";
import type { components } from "../api/generated/schema";
import type { WorkspaceEvent, WorkspaceView } from "./helpers";

type WorkspacePhase = components["schemas"]["WorkspacePhase"];

// Workspace-area API calls against the generated schema (GET
// /v1/workspaces?scope=…, GET /v1/workspaces/{id}/events, the :start/:stop
// actions). Each response's
// Date header feeds the progress module's skew correction — the elapsed
// counters anchor on server-side updated_at, not the client clock.

/** Server default order of GET /v1/workspaces: newest first. */
export const WORKSPACE_DEFAULT_SORT = "-createdAt";

export interface WorkspaceListQuery {
  /** `mine` (default here) or `tenant` (tenant-admin only). */
  scope?: "mine" | "tenant";
  phase?: WorkspacePhase;
  /** Page size (server default 50, max 500). */
  limit?: number;
}

/** One page of GET /v1/workspaces under `sort` (`createdAt` | `name`, `-` = descending). */
export async function listWorkspacePage(
  api: ApiClient,
  query: WorkspaceListQuery,
  cursor?: string,
  sort: string = WORKSPACE_DEFAULT_SORT,
): Promise<ServerPage<WorkspaceView>> {
  const res = await api.GET("/v1/workspaces", {
    params: {
      query: {
        scope: query.scope ?? "mine",
        sort,
        ...(query.phase ? { phase: query.phase } : {}),
        ...(query.limit ? { limit: query.limit } : {}),
        ...(cursor ? { cursor } : {}),
      },
    },
  });
  noteServerDateHeader(res.response.headers.get("date"));
  return unwrap(res);
}

export async function getWorkspace(api: ApiClient, workspaceId: string): Promise<WorkspaceView> {
  const res = await api.GET("/v1/workspaces/{workspaceId}", {
    params: { path: { workspaceId } },
  });
  noteServerDateHeader(res.response.headers.get("date"));
  return unwrap(res);
}

/**
 * Curated workspace events, newest first (API order is the render order).
 * The detail page shows the most recent page only (server default 50).
 */
export async function listWorkspaceEvents(
  api: ApiClient,
  workspaceId: string,
): Promise<WorkspaceEvent[]> {
  const res = unwrap(
    await api.GET("/v1/workspaces/{workspaceId}/events", {
      params: { path: { workspaceId } },
    }),
  );
  return res.items ?? [];
}

// Lifecycle intents shared by the workspace detail, the session page and
// the tenant workspaces table. `start` contractually requires an
// Idempotency-Key; `stop` accepts one, which keeps a retried click from
// double-signalling.
export async function startWorkspace(
  api: ApiClient,
  workspaceId: string,
  idempotencyKey: string = newIdempotencyKey(),
): Promise<WorkspaceView> {
  return unwrap(
    await api.POST("/v1/workspaces/{workspaceId}:start", {
      params: { path: { workspaceId }, header: { "Idempotency-Key": idempotencyKey } },
    }),
  );
}

export async function stopWorkspace(
  api: ApiClient,
  workspaceId: string,
  idempotencyKey: string = newIdempotencyKey(),
): Promise<WorkspaceView> {
  return unwrap(
    await api.POST("/v1/workspaces/{workspaceId}:stop", {
      params: { path: { workspaceId }, header: { "Idempotency-Key": idempotencyKey } },
    }),
  );
}
