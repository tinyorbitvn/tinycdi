import { useCallback } from "react";
import { useApi } from "../api/context";
import { unwrap } from "../api/client";
import { useResource } from "../workspaces/resource";
import type { TemplateView } from "./types";

const MAX_PAGES = 10;

/** Published templates visible to the caller (all pages; no polling). */
export function useTemplates() {
  const api = useApi();
  const load = useCallback(async () => {
    const items: TemplateView[] = [];
    let pageToken: string | undefined;
    for (let i = 0; i < MAX_PAGES; i++) {
      const res = unwrap(
        await api.GET("/v1/templates", {
          params: { query: { limit: 200, ...(pageToken ? { pageToken } : {}) } },
        }),
      );
      items.push(...(res.items as TemplateView[]));
      pageToken = res.nextPageToken;
      if (!pageToken) break;
    }
    return items;
  }, [api]);
  return useResource(load, null);
}
