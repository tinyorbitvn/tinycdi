import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { setCsrfToken } from "../api/client";

// Signed-in principal from GET /v1/me (D17): the bootstrap payload carries
// the session-bound CSRF token (P1) and the session domain launch URLs are
// built under. Until the backend ships the endpoint (404) the shell runs on
// a stub principal with no roles, so admin nav stays hidden.

export const TENANT_ADMIN_ROLE = "tenant-admin";

export interface Me {
  subject: string;
  displayName: string;
  email?: string;
  tenant: string;
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
