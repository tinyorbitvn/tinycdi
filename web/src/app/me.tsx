import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { setCsrfToken, unwrap, type ApiClient } from "../api/client";
import { useApi } from "../api/context";
import type { components } from "../api/generated/schema";

// Signed-in principal from GET /v1/me (D17): the bootstrap payload carries
// the session-bound CSRF token (P1) and the session domain launch URLs are
// built under. Until the backend ships the endpoint (404) the shell runs on
// a stub principal with no roles, so admin nav stays hidden.

export const TENANT_ADMIN_ROLE = "tenant-admin";

// The contract type is components["schemas"]["Me"]; the parse below stays
// tolerant (optional fields may be absent on older backends), so the local
// shape widens roles and marks the two bootstrap extras optional.
type GeneratedMe = components["schemas"]["Me"];

export interface Me extends Omit<GeneratedMe, "roles" | "csrfToken" | "sessionDomain"> {
  roles: string[];
  /** Synchronizer token echoed as X-CSRF-Token on mutations (P1). */
  csrfToken?: string;
  /** host[:port] under which per-workspace session hosts live (D9). */
  sessionDomain?: string;
  /** True when /v1/me is not available yet and this is a placeholder. */
  stub?: boolean;
}

export type MeState =
  | { status: "loading"; me: null }
  | { status: "ready"; me: Me }
  | { status: "error"; me: null; error: string };

export const STUB_ME: Me = { subject: "", displayName: "Signed in", tenant: "", roles: [], stub: true };

function str(v: unknown): string | undefined {
  return typeof v === "string" && v !== "" ? v : undefined;
}

/** Tolerant decode of the /v1/me body (field names settle with the backend). */
export function parseMe(body: unknown): Me {
  const b = (typeof body === "object" && body !== null ? body : {}) as Record<string, unknown>;
  const tenantRaw = b.tenant;
  const tenant =
    str(tenantRaw) ??
    (typeof tenantRaw === "object" && tenantRaw !== null
      ? (str((tenantRaw as Record<string, unknown>).name) ?? str((tenantRaw as Record<string, unknown>).id))
      : undefined) ??
    str(b.tenantId) ??
    "";
  const subject = str(b.subject) ?? str(b.sub) ?? "";
  const email = str(b.email);
  const csrfToken = str(b.csrfToken);
  const sessionDomain = str(b.sessionDomain);
  return {
    subject,
    displayName: str(b.displayName) ?? str(b.name) ?? email ?? (subject || "Signed in"),
    ...(email ? { email } : {}),
    tenant,
    roles: Array.isArray(b.roles) ? b.roles.filter((r): r is string => typeof r === "string") : [],
    ...(csrfToken ? { csrfToken } : {}),
    ...(sessionDomain ? { sessionDomain } : {}),
  };
}

/** A non-OK /v1/me response; `status` decides whether a retry can help. */
export class MeHttpError extends Error {
  readonly status: number;
  constructor(status: number) {
    super(`GET /v1/me failed: ${status}`);
    this.status = status;
  }
}

/** Transient /v1/me failures: 429, 5xx, and network errors — never a sign-out. */
export function isTransientMeError(e: unknown): boolean {
  if (e instanceof MeHttpError) return e.status === 429 || e.status >= 500;
  return e instanceof TypeError;
}

export async function fetchMe(fetchImpl: typeof fetch = fetch): Promise<Me> {
  const res = await fetchImpl("/v1/me", { credentials: "same-origin", headers: { Accept: "application/json" } });
  if (res.status === 404 || res.status === 501) return STUB_ME;
  if (!res.ok) throw new MeHttpError(res.status);
  return parseMe(await res.json());
}

export function isTenantAdmin(me: Me | null | undefined): boolean {
  return !!me && me.roles.includes(TENANT_ADMIN_ROLE);
}

const MeContext = createContext<MeState>({ status: "loading", me: null });

export function MeProvider({
  children,
  load = fetchMe,
}: {
  children: ReactNode;
  /** Injectable for tests. */
  load?: () => Promise<Me>;
}) {
  const [state, setState] = useState<MeState>({ status: "loading", me: null });
  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let failures = 0;
    const attempt = async () => {
      try {
        const me = await load();
        if (cancelled) return;
        // The API client reads the token from module state, not from a
        // cookie (D17) — install what /v1/me published.
        setCsrfToken(me.csrfToken);
        setState({ status: "ready", me });
      } catch (e) {
        if (cancelled) return;
        // A backend blip (503 UNAVAILABLE etc.) retries with backoff and
        // keeps the shell in "loading" — it is never a sign-out (V3.27).
        if (isTransientMeError(e)) {
          failures += 1;
          timer = setTimeout(() => void attempt(), Math.min(1_000 * 2 ** failures, 30_000));
          return;
        }
        setState({ status: "error", me: null, error: e instanceof Error ? e.message : String(e) });
      }
    };
    void attempt();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [load]);
  return <MeContext.Provider value={state}>{children}</MeContext.Provider>;
}

/** Current principal state; `me` is null while loading or on error. */
export function useMe(): MeState {
  return useContext(MeContext);
}

// ---- shared list/loader helpers -------------------------------------------
// One canonical copy of the data plumbing the admin and data areas used to
// duplicate (Me/useMe/useLoader/listAll; folded in v0.3). useLoader is the
// generic "run an api read, keep the latest result" hook; listAll follows
// page cursors; loadMe/useMeLoaded serve the principal to pages rendered
// outside MeProvider (unit tests); get is the narrow typed GET escape for
// the not-yet-generated reads.

/** List `scope` filter shared by the workspace/data list endpoints. */
export type Scope = "mine" | "tenant";

export interface Loaded<T> {
  data: T | undefined;
  error: unknown;
  loading: boolean;
  reload: () => void;
}

// useLoader runs `load` on mount and whenever `key` changes; reload() re-runs
// it. A response that arrives after a newer request started is dropped, so a
// slow first page cannot overwrite a fresher reload.
export function useLoader<T>(load: (api: ApiClient) => Promise<T>, key: string): Loaded<T> {
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

// One /v1/me request per API client: the admin/data pages ask for the
// principal to decide whether the tenant-scope affordances show, and the
// answer only changes on re-login (a full page load).
const meCache = new WeakMap<ApiClient, Promise<Me>>();

export function loadMe(api: ApiClient): Promise<Me> {
  let p = meCache.get(api);
  if (!p) {
    p = get<Me>(api, "/v1/me");
    // A failed probe is not cached, so a later page can retry.
    p.catch(() => meCache.delete(api));
    meCache.set(api, p);
  }
  return p;
}

/** Principal as a Loaded value for views outside MeProvider. */
export function useMeLoaded(): Loaded<Me> {
  return useLoader(loadMe, "me");
}

// Lists stop following cursors at `maxPages` so a runaway server cannot pin
// the tab; the views show a "partial" hint when that happens.
export const MAX_PAGES = 20;
const PAGE_LIMIT = 200;

interface Page<T> {
  items: T[];
  nextPageToken?: string;
}

type Query = Record<string, string | number | undefined>;

// openapi-fetch's runtime accepts any path; only its typing is bound to the
// generated `paths`. This narrow view covers the not-yet-generated reads.
interface LooseGet {
  GET(
    path: string,
    init: { params?: { query?: Query } },
  ): Promise<{ data?: unknown; error?: unknown; response: Response }>;
}

export async function get<T>(api: ApiClient, path: string, query?: Query): Promise<T> {
  const loose = api as unknown as LooseGet;
  return unwrap(await loose.GET(path, query ? { params: { query } } : {})) as T;
}

export async function listAll<T>(
  api: ApiClient,
  path: string,
  query: Query,
): Promise<{ items: T[]; truncated: boolean }> {
  const items: T[] = [];
  let pageToken: string | undefined;
  for (let i = 0; i < MAX_PAGES; i++) {
    const page = await get<Page<T>>(api, path, { ...query, limit: PAGE_LIMIT, pageToken });
    items.push(...page.items);
    if (!page.nextPageToken) return { items, truncated: false };
    pageToken = page.nextPageToken;
  }
  return { items, truncated: true };
}
