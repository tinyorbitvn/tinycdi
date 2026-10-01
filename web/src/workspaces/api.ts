import { unwrap, type ApiClient } from "../api/client";
import type { WorkspaceEvent, WorkspaceView } from "./helpers";

// Workspace-area API calls against the generated schema (GET
// /v1/workspaces?scope=…, GET /v1/workspaces/{id}/events).

const MAX_PAGES = 10;

/** All of the caller's workspaces (`scope=mine`; the tenant view is admin's). */
export async function listWorkspaces(api: ApiClient): Promise<WorkspaceView[]> {
  const items: WorkspaceView[] = [];
  let pageToken: string | undefined;
  for (let i = 0; i < MAX_PAGES; i++) {
    const res = unwrap(
      await api.GET("/v1/workspaces", {
        params: { query: { scope: "mine", limit: 200, ...(pageToken ? { pageToken } : {}) } },
      }),
    );
    items.push(...res.items);
    pageToken = res.nextPageToken;
    if (!pageToken) break;
  }
  return items;
}

export async function getWorkspace(api: ApiClient, workspaceId: string): Promise<WorkspaceView> {
  return unwrap(
    await api.GET("/v1/workspaces/{workspaceId}", {
      params: { path: { workspaceId } },
    }),
  );
}

/** Curated workspace events, newest first (API order is the render order). */
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
