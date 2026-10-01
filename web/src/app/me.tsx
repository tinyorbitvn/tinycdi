import { createContext, useContext, useEffect, useState, type ReactNode } from "react";

// Signed-in principal from GET /v1/me. Until the backend ships the endpoint
// (404) the shell runs on a stub principal with no roles, so admin nav stays
// hidden. Swap the raw fetch for the generated client once /v1/me is in
// internal/api/openapi.yaml.

export const TENANT_ADMIN_ROLE = "tenant-admin";

export interface Me {
  subject: string;
  displayName: string;
  email?: string;
  tenant: string;
  roles: string[];
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
  return {
    subject,
    displayName: str(b.displayName) ?? str(b.name) ?? email ?? (subject || "Signed in"),
    ...(email ? { email } : {}),
    tenant,
    roles: Array.isArray(b.roles) ? b.roles.filter((r): r is string => typeof r === "string") : [],
  };
}

export async function fetchMe(fetchImpl: typeof fetch = fetch): Promise<Me> {
  const res = await fetchImpl("/v1/me", { credentials: "same-origin", headers: { Accept: "application/json" } });
  if (res.status === 404 || res.status === 501) return STUB_ME;
  if (!res.ok) throw new Error(`GET /v1/me failed: ${res.status}`);
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
    load().then(
      (me) => !cancelled && setState({ status: "ready", me }),
      (e: unknown) =>
        !cancelled && setState({ status: "error", me: null, error: e instanceof Error ? e.message : String(e) }),
    );
    return () => {
      cancelled = true;
    };
  }, [load]);
  return <MeContext.Provider value={state}>{children}</MeContext.Provider>;
}

/** Current principal state; `me` is null while loading or on error. */
export function useMe(): MeState {
  return useContext(MeContext);
}
