import { POLL_HEADERS, unwrap, type ApiClient } from "../api/client";
import { noteServerDateHeader } from "../progress/derive";
import type { WorkspaceEvent, WorkspaceView } from "./helpers";

// Workspace-area API calls against the generated schema (GET
// /v1/workspaces?scope=…, GET /v1/workspaces/{id}/events). Each response's
// Date header feeds the progress module's skew correction — the elapsed
// counters anchor on server-side updated_at, not the client clock.
//
// `background` marks a read as an automated poll (POLL_HEADERS): the
// server authenticates it without sliding the portal idle window. Only
// useResource's scheduled ticks pass true.

const MAX_PAGES = 10;

/** All of the caller's workspaces (`scope=mine`; the tenant view is admin's). */
export async function listWorkspaces(api: ApiClient, background = false): Promise<WorkspaceView[]> {
  const items: WorkspaceView[] = [];
  let pageToken: string | undefined;
  for (let i = 0; i < MAX_PAGES; i++) {
    const res = await api.GET("/v1/workspaces", {
      params: { query: { scope: "mine", limit: 200, ...(pageToken ? { pageToken } : {}) } },
      ...(background ? { headers: POLL_HEADERS } : {}),
    });
    noteServerDateHeader(res.response.headers.get("date"));
    const page = unwrap(res);
    items.push(...page.items);
    pageToken = page.nextPageToken;
    if (!pageToken) break;
  }
  return items;
}

export async function getWorkspace(
  api: ApiClient,
  workspaceId: string,
  background = false,
): Promise<WorkspaceView> {
  const res = await api.GET("/v1/workspaces/{workspaceId}", {
    params: { path: { workspaceId } },
    ...(background ? { headers: POLL_HEADERS } : {}),
  });
  noteServerDateHeader(res.response.headers.get("date"));
  return unwrap(res);
}

/** Curated workspace events, newest first (API order is the render order). */
export async function listWorkspaceEvents(
  api: ApiClient,
  workspaceId: string,
  background = false,
): Promise<WorkspaceEvent[]> {
  const res = unwrap(
    await api.GET("/v1/workspaces/{workspaceId}/events", {
      params: { path: { workspaceId } },
      ...(background ? { headers: POLL_HEADERS } : {}),
    }),
  );
  return res.items ?? [];
}
