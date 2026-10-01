import { useCallback, useEffect, useRef, useState } from "react";
import { useApi } from "../api/context";
import { unwrap, type ApiClient } from "../api/client";
import type { components } from "../api/generated/schema";

// Data-area API surface. `owner` on records and the `?scope=mine|tenant`
// filter on GET /v1/data are contract additions (T3.4); they are typed here
// until the generated schema carries them, and the calls go through the same
// openapi-fetch client (CSRF middleware, same-origin credentials).

export type WorkspaceView = components["schemas"]["WorkspaceView"];
export type TemplateView = components["schemas"]["TemplateView"];
export type RetainedDataView = components["schemas"]["RetainedDataView"];
export type RetainedDataState = components["schemas"]["RetainedDataState"];

export const TENANT_ADMIN = "tenant-admin";

export interface Owner {
  subject: string;
  displayName: string;
}

export interface Me {
  subject: string;
  displayName: string;
  email?: string;
  tenant: string;
  roles: string[];
}

export type Scope = "mine" | "tenant";

export type ScopedRetainedData = RetainedDataView & { owner?: Owner };

export interface AttachDataBody {
  name: string;
  templateRef: string;
  desiredState?: "Running" | "Stopped";
}

interface Page<T> {
  items: T[];
  nextPageToken?: string;
}

type Query = Record<string, string | number | undefined>;

// openapi-fetch's runtime accepts any path; only its typing is bound to the
// generated `paths`. This narrow view covers the not-yet-generated reads
// (/v1/me, ?scope= on /v1/data, owner on records).
interface LooseGet {
  GET(
    path: string,
    init: { params?: { query?: Query } },
  ): Promise<{ data?: unknown; error?: unknown; response: Response }>;
}

async function get<T>(api: ApiClient, path: string, query?: Query): Promise<T> {
  const loose = api as unknown as LooseGet;
  return unwrap(await loose.GET(path, query ? { params: { query } } : {})) as T;
}

export function isTenantAdmin(me: Pick<Me, "roles"> | null | undefined): boolean {
  return !!me && me.roles.includes(TENANT_ADMIN);
}

export function fetchMe(api: ApiClient): Promise<Me> {
  return get<Me>(api, "/v1/me");
}

// Lists stop following cursors at MAX_PAGES so a runaway server cannot pin
// the tab; the views show a "partial" hint when that happens.
export const MAX_PAGES = 20;
const PAGE_LIMIT = 200;

async function listAll<T>(
  api: ApiClient,
  path: string,
  query: Query,
): Promise<{ items: T[]; truncated: boolean }> {
  const items: T[] = [];
  let pageToken: string | undefined;
  for (let i = 0; i < MAX_PAGES; i++) {
    const page = await get<Page<T>>(api, path, {
      ...query,
      limit: PAGE_LIMIT,
      pageToken,
    });
    items.push(...page.items);
    if (!page.nextPageToken) return { items, truncated: false };
    pageToken = page.nextPageToken;
  }
  return { items, truncated: true };
}

// GET /v1/data. `Purged` records are excluded server-side; the filter here
// keeps the list honest if an old backend still returns them.
export async function listRetainedData(
  api: ApiClient,
  scope: Scope,
): Promise<{ items: ScopedRetainedData[]; truncated: boolean }> {
  const page = await listAll<ScopedRetainedData>(api, "/v1/data", { scope });
  return { ...page, items: page.items.filter((r) => r.state !== "Purged") };
}

// There is no GET /v1/data/{id}: the detail view resolves a record from the
// caller-visible list (existence is never leaked — a foreign id reads as
// "not found", matching the API's 404 convention).
export async function getRetainedData(
  api: ApiClient,
  id: string,
): Promise<ScopedRetainedData | null> {
  const { items } = await listRetainedData(api, "mine");
  const mine = items.find((r) => r.id === id);
  if (mine) return mine;
  const tenant = await listRetainedData(api, "tenant").catch(() => null);
  return tenant?.items.find((r) => r.id === id) ?? null;
}

export async function attachRetainedData(
  api: ApiClient,
  dataId: string,
  body: AttachDataBody,
  idempotencyKey: string,
): Promise<WorkspaceView> {
  return unwrap(
    await api.POST("/v1/data/{dataId}/attach", {
      params: {
        path: { dataId },
        header: { "Idempotency-Key": idempotencyKey },
      },
      body,
    }),
  );
}

export async function purgeRetainedData(
  api: ApiClient,
  dataId: string,
  confirmationNonce: string,
  idempotencyKey?: string,
): Promise<RetainedDataView> {
  return unwrap(
    await api.POST("/v1/data/{dataId}/purge", {
      params: {
        path: { dataId },
        ...(idempotencyKey
          ? { header: { "Idempotency-Key": idempotencyKey } }
          : {}),
      },
      body: { confirmationNonce },
    }),
  );
}

// Templates eligible for attach are constrained to the disk's runtime
// (attach returns 422 INVALID_TEMPLATE otherwise).
export function listTemplates(api: ApiClient, runtime?: string) {
  return listAll<TemplateView>(api, "/v1/templates", { runtime }).then((p) => ({
    ...p,
    items: p.items.filter(
      (tpl) => tpl.publishedAt && (runtime === undefined || tpl.runtime === runtime),
    ),
  }));
}

// ---- shared async state -------------------------------------------------

export interface Loaded<T> {
  data: T | undefined;
  error: unknown;
  loading: boolean;
  reload: () => void;
}

// useLoader runs `load` on mount and whenever `key` changes; reload() re-runs
// it. A response that arrives after a newer request started is dropped, so a
// slow first page cannot overwrite a fresher reload.
export function useLoader<T>(
  load: (api: ApiClient) => Promise<T>,
  key: string,
): Loaded<T> {
  const api = useApi();
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(true);
  const [tick, setTick] = useState(0);
  const loadRef = useRef(load);
  loadRef.current = load;

  useEffect(() => {
    let current = true;
    setLoading(true);
    loadRef.current(api).then(
      (d) => {
        if (!current) return;
        setData(d);
        setError(null);
        setLoading(false);
      },
      (e: unknown) => {
        if (!current) return;
        setError(e);
        setLoading(false);
      },
    );
    return () => {
      current = false;
    };
  }, [api, key, tick]);

  const reload = useCallback(() => setTick((t) => t + 1), []);
  return { data, error, loading, reload };
}

// One /v1/me request per API client: the data views ask for the principal to
// decide whether the tenant-scope switcher shows, and the answer only
// changes on re-login (a full page load).
const meCache = new WeakMap<ApiClient, Promise<Me>>();

export function loadMe(api: ApiClient): Promise<Me> {
  let p = meCache.get(api);
  if (!p) {
    p = fetchMe(api);
    // A failed probe is not cached, so a later page can retry.
    p.catch(() => meCache.delete(api));
    meCache.set(api, p);
  }
  return p;
}

export function useMe(): Loaded<Me> {
  const load = useCallback((a: ApiClient) => loadMe(a), []);
  return useLoader(load, "me");
}
