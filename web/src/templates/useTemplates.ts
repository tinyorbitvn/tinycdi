import { useCallback } from "react";
import { useApi } from "../api/context";
import { unwrap, type ApiClient } from "../api/client";
import { ALL_PAGE_LIMIT, fetchAllPages, type ServerPage } from "../design/useServerPage";
import { useResource } from "../workspaces/resource";
import type { RuntimeKind, TemplateView } from "./types";

/** Server default order of GET /v1/templates. */
export const TEMPLATE_DEFAULT_SORT = "name";

export interface TemplateListQuery {
  runtime?: RuntimeKind;
  /** Page size (server default 50, max 500). */
  limit?: number;
}

/** One page of GET /v1/templates under `sort` (`name` | `-name`). */
export async function listTemplatePage(
  api: ApiClient,
  query: TemplateListQuery,
  cursor?: string,
  sort: string = TEMPLATE_DEFAULT_SORT,
): Promise<ServerPage<TemplateView>> {
  return unwrap(
    await api.GET("/v1/templates", {
      params: {
        query: {
          sort,
          ...(query.runtime ? { runtime: query.runtime } : {}),
          ...(query.limit ? { limit: query.limit } : {}),
          ...(cursor ? { cursor } : {}),
        },
      },
    }),
  );
}

/** Every template matching `query`, read page by page (catalogs, pickers, id lookups). */
export function listAllTemplates(api: ApiClient, query: Omit<TemplateListQuery, "limit"> = {}): Promise<TemplateView[]> {
  return fetchAllPages((cursor, sort) => listTemplatePage(api, { ...query, limit: ALL_PAGE_LIMIT }, cursor, sort));
}

/** Published templates visible to the caller (every page; no polling). */
export function useTemplates() {
  const api = useApi();
  const load = useCallback(() => listAllTemplates(api), [api]);
  return useResource(load, null);
}
