import { useCallback } from "react";
import { useApi } from "../api/context";
import { unwrap, type ApiClient } from "../api/client";
import { SCAN_PAGE_LIMIT, scanCollection, useServerPage, type ServerPage } from "../design/useServerPage";
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

/**
 * Every template matching `query`, bounded by SCAN_MAX_PAGES (rejects past
 * it). For id lookups and pickers only: the API has no GET by id.
 */
export function scanTemplates(api: ApiClient, query: Omit<TemplateListQuery, "limit"> = {}): Promise<TemplateView[]> {
  return scanCollection((cursor, sort) => listTemplatePage(api, { ...query, limit: SCAN_PAGE_LIMIT }, cursor, sort));
}

/** Published templates visible to the caller, one server page at a time (catalog). */
export function useTemplatePages() {
  const api = useApi();
  const fetchPage = useCallback((cursor?: string, sort?: string) => listTemplatePage(api, {}, cursor, sort), [api]);
  return useServerPage(fetchPage, { sort: TEMPLATE_DEFAULT_SORT });
}

/** Templates for a lookup or picker (bounded scan; no polling). */
export function useTemplateLookup() {
  const api = useApi();
  const load = useCallback(() => scanTemplates(api), [api]);
  return useResource(load, null);
}
