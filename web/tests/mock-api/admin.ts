// Tenant-admin area of the contract mock: GET /v1/me, GET /v1/quota,
// GET/PUT /v1/admin/tenants/{tenant}/quota, the `owner` field on records and
// `?scope=mine|tenant` (+ cursor pagination) on GET /v1/workspaces. Record
// ownership lives in a per-context Tenancy that data.ts shares for GET
// /v1/data. Must be registered before the workspaces area in areas.ts: it
// claims GET /v1/workspaces and GET /v1/workspaces/{id} and hides other
// users' records from non-admins (contract: 404, never a leak); every other
// route passes through.
//
// The default principal is a regular user with no other users' records, so
// existing specs see the base contract unchanged. Demo mode makes the
// principal a tenant admin and seeds two more users.
//
// Test controls (not part of the contract):
//   POST /_control/admin/me     {roles?, displayName?}   patch the principal
//   POST /_control/admin/seed                            add other users' workspaces + disks
//   POST /_control/admin/quota  {limits?, userLimits?, managed?}   patch quota limits (null clears) / mark the row config-managed
//   POST /_control/admin/fail   {path, code?, status?}   fail the next GET of `path`
//
// Erasable-syntax TypeScript only (Node type-stripping runs it directly).

import { CSRF_TOKEN_VALUE, err, ok, type MockArea, type MockContext, type MockRequest, type MockResponse } from "./core.ts";
import { makeWorkspace, readyWorkspace, TEMPLATE_BROWSER, TEMPLATE_LINUX } from "./fixtures.ts";
import type { RetainedFixture, TemplateFixture, WorkspaceFixture } from "./fixtures.ts";

export interface Owner {
  subject: string;
  displayName: string;
}

export interface MockMe {
  subject: string;
  displayName: string;
  email?: string;
  tenant: string;
  roles: string[];
}

export interface QuotaAmounts {
  workspaces: number;
  runningWorkspaces: number;
  cpuMillicores: number;
  memoryMib: number;
  storageGib: number;
}

export const TENANT = "acme";
export const ME: Owner = { subject: "user-01J4ZDADA", displayName: "Ada Lovelace" };
export const OTHER_USERS: Owner[] = [
  { subject: "user-01J4ZDGRC", displayName: "Grace Hopper" },
  { subject: "user-01J4ZDLIN", displayName: "Linus Pauling" },
];

export const DEFAULT_LIMITS: QuotaAmounts = {
  workspaces: 20,
  runningWorkspaces: 8,
  cpuMillicores: 32000,
  memoryMib: 65536,
  storageGib: 500,
};
export const DEFAULT_USER_LIMITS: QuotaAmounts = {
  workspaces: 5,
  runningWorkspaces: 2,
  cpuMillicores: 8000,
  memoryMib: 16384,
  storageGib: 100,
};

// Other users' records added by /_control/admin/seed. IDs match the
// contract patterns; values are synthetic.
export const SEED_WORKSPACE_IDS = {
  graceReady: "ws_01J4ZGRACEREADY01",
  graceFailed: "ws_01J4ZGRACEFAIL001",
  linusStopped: "ws_01J4ZLINUSSTOP001",
} as const;
export const SEED_DATA_IDS = {
  grace: "rd_01J4ZGRACEDISK001",
  linus: "rd_01J4ZLINUSDISK001",
} as const;

function tplSummary(t: TemplateFixture) {
  return { id: t.id, name: t.name, family: t.family, revision: t.revision, runtime: t.runtime, experience: t.experience };
}

function seedWorkspaces(): { ws: WorkspaceFixture; owner: Owner }[] {
  const [grace, linus] = OTHER_USERS;
  return [
    {
      owner: grace,
      ws: readyWorkspace({
        id: SEED_WORKSPACE_IDS.graceReady,
        name: "grace-analysis",
        createdAt: "2026-09-28T08:00:00Z",
        updatedAt: "2026-09-30T08:00:00Z",
      }),
    },
    {
      owner: grace,
      ws: makeWorkspace({
        id: SEED_WORKSPACE_IDS.graceFailed,
        name: "grace-browser",
        template: tplSummary(TEMPLATE_BROWSER),
        phase: "Failed",
        desiredState: "Running",
        dataPolicy: "Ephemeral",
        failureReason: "BootDeadlineExceeded",
        createdAt: "2026-09-29T08:00:00Z",
        updatedAt: "2026-09-29T08:10:00Z",
      }),
    },
    {
      owner: linus,
      ws: makeWorkspace({
        id: SEED_WORKSPACE_IDS.linusStopped,
        name: "linus-desktop",
        createdAt: "2026-09-30T11:00:00Z",
        updatedAt: "2026-09-30T12:00:00Z",
      }),
    },
  ];
}

function seedRetained(): { rec: RetainedFixture; owner: Owner }[] {
  const [grace, linus] = OTHER_USERS;
  return [
    {
      owner: grace,
      rec: {
        id: SEED_DATA_IDS.grace,
        owner: grace,
        state: "Retained",
        sizeGib: 40,
        runtime: "LinuxContainer",
        sourceWorkspaceName: "grace-old-lab",
        retainedAt: "2026-09-20T09:00:00Z",
        purgeConfirmationNonce: "nonce-seed",
      },
    },
    {
      owner: linus,
      rec: {
        id: SEED_DATA_IDS.linus,
        owner: linus,
        state: "Attached",
        sizeGib: 20,
        runtime: "LinuxContainer",
        sourceWorkspaceName: "linus-thesis",
        consumingWorkspaceId: SEED_WORKSPACE_IDS.linusStopped,
        retainedAt: "2026-09-25T09:00:00Z",
        purgeConfirmationNonce: "nonce-seed",
      },
    },
  ];
}

export interface Tenancy {
  me: MockMe;
  // undefined = the tenant has no quota row (the API then omits `limits`).
  limits: QuotaAmounts | undefined;
  userLimits: QuotaAmounts | undefined;
  // Per-principal running limits (v0.5): the tenant default (null =
  // unlimited) and ownerRef ("iss|sub") -> max running workspaces.
  userLimitDefault: number | null;
  userLimitOverrides: Map<string, number>;
  // True = the tenant is declared in -tenant-quotas: source "config" and
  // PUT /v1/admin/tenants/{tenant}/quota answers 409 QUOTA_MANAGED_BY_CONFIG.
  managed: boolean;
  // Limits-row change token — bumps on every quota write; PUT echoes it via
  // If-Match (optimistic concurrency).
  version: number;
  // Record ID -> owner; records without an entry belong to the principal.
  owners: Map<string, Owner>;
  failNext: Map<string, { status: number; code: string }>;
  isAdmin(): boolean;
  ownerOf(id: string): Owner;
  visible(id: string): boolean;
  // Every `scope` query value seen by resolveScope ("" when absent) — lets
  // tests assert which scope a screen requested.
  scopesSeen: string[];
  // Resolves ?scope: { mine } or an error response (403 tenant for
  // non-admins, 400 unknown value). Omitted = admin sees the tenant.
  resolveScope(req: MockRequest): { mine: boolean } | MockResponse;
  paginate(req: MockRequest, items: unknown[]): MockResponse;
  takeFailure(req: MockRequest): MockResponse | undefined;
}

function freshMe(admin: boolean): MockMe {
  return {
    ...ME,
    email: "ada@example.invalid",
    tenant: TENANT,
    roles: admin ? ["user", "tenant-admin"] : ["user"],
  };
}

const tenancies = new WeakMap<MockContext, Tenancy>();

// tenancy returns the context's shared ownership state (created on first use
// by whichever area asks first).
export function tenancy(ctx: MockContext): Tenancy {
  let t = tenancies.get(ctx);
  if (t) return t;
  const self: Tenancy = {
    me: freshMe(ctx.demo),
    limits: { ...DEFAULT_LIMITS },
    userLimits: { ...DEFAULT_USER_LIMITS },
    userLimitDefault: null,
    userLimitOverrides: new Map(),
    managed: false,
    version: 1,
    owners: new Map(),
    failNext: new Map(),
    scopesSeen: [],
    isAdmin: () => self.me.roles.includes("tenant-admin"),
    ownerOf: (id) => self.owners.get(id) ?? { subject: self.me.subject, displayName: self.me.displayName },
    visible: (id) => self.isAdmin() || self.ownerOf(id).subject === self.me.subject,
    resolveScope(req) {
      const scope = req.query.get("scope");
      self.scopesSeen.push(scope ?? "");
      if (scope === "tenant") {
        return self.isAdmin() ? { mine: false } : err(403, "FORBIDDEN", "tenant scope requires tenant-admin", false);
      }
      if (scope === "mine") return { mine: true };
      if (scope) return err(400, "INVALID_REQUEST", `unknown scope ${scope}`, false);
      return { mine: !self.isAdmin() };
    },
    // Offset cursor; pageToken is opaque to clients.
    paginate(req, items) {
      const limit = Math.min(Math.max(Number(req.query.get("limit") ?? 50) || 50, 1), 200);
      const offset = Number(req.query.get("pageToken") ?? 0) || 0;
      const page = items.slice(offset, offset + limit);
      const next = offset + limit < items.length ? String(offset + limit) : undefined;
      return ok(200, next ? { items: page, nextPageToken: next } : { items: page });
    },
    takeFailure(req) {
      const f = self.failNext.get(req.path);
      if (!f || req.method !== "GET") return undefined;
      self.failNext.delete(req.path);
      return err(f.status, f.code, `injected ${f.code}`, f.status >= 500);
    },
  };
  tenancies.set(ctx, self);
  return self;
}

function zero(): QuotaAmounts {
  return { workspaces: 0, runningWorkspaces: 0, cpuMillicores: 0, memoryMib: 0, storageGib: 0 };
}

// ISS prefixes the "issuer|sub" owner references the contract API keys
// per-user limits on; the portal echoes an ownerRef back verbatim on PUT.
const ISS = "https://idp.invalid";
function ownerRefOf(subject: string): string {
  return `${ISS}|${subject}`;
}

export function adminArea(ctx: MockContext): MockArea {
  const { state } = ctx;
  const t = tenancy(ctx);

  function seed(): void {
    for (const { ws, owner } of seedWorkspaces()) {
      state.workspaces.set(ws.id, ws);
      t.owners.set(ws.id, owner);
    }
    for (const { rec, owner } of seedRetained()) {
      state.retained.set(rec.id, rec);
      t.owners.set(rec.id, owner);
    }
  }

  function reset(): void {
    t.me = freshMe(ctx.demo);
    t.limits = { ...DEFAULT_LIMITS };
    t.userLimits = { ...DEFAULT_USER_LIMITS };
    t.userLimitDefault = null;
    t.userLimitOverrides = new Map();
    t.managed = false;
    t.version = 1;
    t.owners = new Map();
    t.failNext = new Map();
    t.scopesSeen = [];
    if (ctx.demo) seed();
  }

  function templateOf(ws: WorkspaceFixture): TemplateFixture | undefined {
    return (
      state.templates.find((tpl) => tpl.id === ws.template.id) ??
      [TEMPLATE_LINUX, TEMPLATE_BROWSER].find((tpl) => tpl.id === ws.template.id)
    );
  }

  // Usage is derived from the shared state: running workspaces consume the
  // template's CPU/memory; Retain workspaces and un-attached retained disks
  // consume storage (Purging keeps its reservation until deletion).
  function quotaBody() {
    const perUser = new Map<string, { owner: Owner; usage: QuotaAmounts }>();
    const bucket = (owner: Owner) => {
      let b = perUser.get(owner.subject);
      if (!b) {
        b = { owner, usage: zero() };
        perUser.set(owner.subject, b);
      }
      return b.usage;
    };
    bucket(t.ownerOf(""));
    for (const ws of state.workspaces.values()) {
      if (ws.phase === "Terminating") continue;
      const u = bucket(t.ownerOf(ws.id));
      const tpl = templateOf(ws);
      u.workspaces += 1;
      if (ws.desiredState === "Running" && ws.phase !== "Stopped") {
        u.runningWorkspaces += 1;
        u.cpuMillicores += tpl?.resources.cpuMillicores ?? 0;
        u.memoryMib += tpl?.resources.memoryMib ?? 0;
      }
      if (ws.dataPolicy === "Retain") u.storageGib += tpl?.resources.storageGib ?? 0;
    }
    for (const rec of state.retained.values()) {
      if (rec.state === "Retained" || rec.state === "Purging") bucket(t.ownerOf(rec.id)).storageGib += rec.sizeGib;
    }
    const usage = zero();
    for (const b of perUser.values()) {
      for (const k of Object.keys(usage) as (keyof QuotaAmounts)[]) usage[k] += b.usage[k];
    }
    const users = [...perUser.values()]
      .filter((b) => t.isAdmin() || b.owner.subject === t.me.subject)
      .map((b) => {
        // users[].limit mirrors the contract: the owner's effective
        // per-principal running limit, absent when unlimited.
        const limit = t.userLimitOverrides.get(`${ISS}|${b.owner.subject}`) ?? t.userLimitDefault;
        return {
          subject: b.owner.subject,
          displayName: b.owner.displayName,
          usage: b.usage,
          ...(limit === null ? {} : { limit }),
        };
      });
    return {
      tenant: t.me.tenant,
      configured: t.limits !== undefined,
      ...(t.limits ? { limits: t.limits } : {}),
      usage,
      users,
    };
  }

  function quota(): MockResponse {
    return ok(200, {
      ...quotaBody(),
      ...(t.userLimits ? { userLimits: t.userLimits } : {}),
    });
  }

  // The admin quota view adds `source` — which layer owns the limits row —
  // and drops userLimits, which no store backs.
  function adminQuotaBody() {
    return {
      ...quotaBody(),
      source: t.managed ? "config" : t.limits !== undefined ? "api" : "none",
      ...(t.limits !== undefined ? { version: String(t.version) } : {}),
    };
  }

  // GET/PUT /v1/admin/tenants/{tenant}/quota — tenant-admin of the named
  // tenant only; config-managed rows reject writes with the 409. PUT is
  // optimistic-concurrency: If-Match names the reported version ("*" only
  // creates a missing row) or the write answers 412 PRECONDITION_FAILED.
  function adminQuota(req: MockRequest, tenant: string): MockResponse {
    if (!t.isAdmin() || tenant !== t.me.tenant) {
      return err(403, "FORBIDDEN", "tenant quota administration requires the tenant-admin role", false);
    }
    if (req.method === "GET") {
      const failed = t.takeFailure(req);
      if (failed) return failed;
      return ok(200, adminQuotaBody());
    }
    if (req.method === "PUT") {
      if (t.managed) {
        return err(409, "QUOTA_MANAGED_BY_CONFIG", "quota for this tenant is managed by configuration", false);
      }
      const ifMatch = req.headers["if-match"];
      if (!ifMatch) return err(400, "INVALID_REQUEST", "If-Match header required", false);
      const b = req.body as Record<string, unknown> | null;
      const keys = ["runningWorkspaces", "cpuMillicores", "memoryMib", "storageGib"];
      const valid =
        b !== null &&
        keys.every((k) => Number.isInteger(b[k]) && (b[k] as number) >= 0) &&
        Object.keys(b).every((k) => keys.includes(k));
      if (!valid) return err(400, "INVALID_REQUEST", "invalid limits", false);
      const match = t.limits === undefined ? ifMatch === "*" : ifMatch === String(t.version);
      if (!match) {
        return err(412, "PRECONDITION_FAILED", "the quota changed since it was read", false);
      }
      t.version += 1;
      t.limits = {
        workspaces: 0,
        runningWorkspaces: b!.runningWorkspaces as number,
        cpuMillicores: b!.cpuMillicores as number,
        memoryMib: b!.memoryMib as number,
        storageGib: b!.storageGib as number,
      };
      return ok(200, adminQuotaBody());
    }
    return err(405, "INVALID_REQUEST", "method not allowed", false);
  }

  // GET/PUT /v1/admin/tenants/{tenant}/user-limits[...] — tenant-admin of
  // the named tenant only. The view unions principals with usage and
  // stored overrides; PUT upserts (`limit: null` clears back to inherit).
  function userLimitsBody() {
    const rows = new Map<
      string,
      { ownerRef: string; subject: string; displayName: string; running: number }
    >();
    for (const u of quotaBody().users) {
      rows.set(ownerRefOf(u.subject), {
        ownerRef: ownerRefOf(u.subject),
        subject: u.subject,
        displayName: u.displayName,
        running: u.usage.runningWorkspaces,
      });
    }
    for (const ref of t.userLimitOverrides.keys()) {
      if (rows.has(ref)) continue;
      const sub = ref.split("|").slice(1).join("|");
      const known = [t.me, ...OTHER_USERS].find((o) => o.subject === sub);
      rows.set(ref, {
        ownerRef: ref,
        subject: sub,
        displayName: known?.displayName ?? sub,
        running: 0,
      });
    }
    return {
      tenant: t.me.tenant,
      default: t.userLimitDefault,
      users: [...rows.values()].map((u) => ({
        ...u,
        limit: t.userLimitOverrides.get(u.ownerRef) ?? null,
        effective: t.userLimitOverrides.get(u.ownerRef) ?? t.userLimitDefault,
      })),
    };
  }

  function adminUserLimits(req: MockRequest, tenant: string, isDefault: boolean): MockResponse {
    if (!t.isAdmin() || tenant !== t.me.tenant) {
      return err(403, "FORBIDDEN", "tenant administration requires the tenant-admin role", false);
    }
    if (req.method === "GET" && !isDefault) {
      const failed = t.takeFailure(req);
      if (failed) return failed;
      return ok(200, userLimitsBody());
    }
    if (req.method === "PUT") {
      const b = req.body as Record<string, unknown> | null;
      if (b === null) return err(400, "INVALID_REQUEST", "invalid request body", false);
      const limit = b.limit;
      const lv =
        limit === null || limit === undefined
          ? null
          : Number.isInteger(limit) && (limit as number) >= 0
            ? (limit as number)
            : undefined;
      if (lv === undefined) return err(400, "INVALID_REQUEST", "invalid limit", false);
      if (isDefault) {
        t.userLimitDefault = lv;
      } else {
        const ref = b.ownerRef;
        if (typeof ref !== "string" || !ref.includes("|") || ref.endsWith("|") || ref.startsWith("|")) {
          return err(400, "INVALID_REQUEST", "ownerRef must be an issuer|sub reference", false);
        }
        if (lv === null) t.userLimitOverrides.delete(ref);
        else t.userLimitOverrides.set(ref, lv);
      }
      return ok(200, userLimitsBody());
    }
    return err(405, "INVALID_REQUEST", "method not allowed", false);
  }

  function listWorkspaces(req: MockRequest): MockResponse {
    const scope = t.resolveScope(req);
    if ("status" in scope) return scope;
    const phase = req.query.get("phase");
    let items = [...state.workspaces.values()].sort((a, b) => a.createdAt.localeCompare(b.createdAt));
    if (scope.mine) items = items.filter((w) => t.ownerOf(w.id).subject === t.me.subject);
    if (phase) items = items.filter((w) => w.phase === phase);
    return t.paginate(
      req,
      items.map((w) => ({ ...w, owner: t.ownerOf(w.id) })),
    );
  }

  function api(req: MockRequest): MockResponse | undefined {
    const { method, path } = req;
    const mQuota = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/quota$/);
    if (mQuota) return adminQuota(req, mQuota[1]);
    const mUL = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/user-limits(\/default)?$/);
    if (mUL) return adminUserLimits(req, mUL[1], mUL[2] !== undefined);
    if (method === "GET" && (path === "/v1/me" || path === "/v1/quota" || path === "/v1/workspaces")) {
      const failed = t.takeFailure(req);
      if (failed) return failed;
      if (path === "/v1/me") {
        // v0.2 (P1/D17): /v1/me also publishes the session-bound CSRF token
        // and the session domain the portal builds launch URLs under.
        return ok(200, { ...t.me, csrfToken: CSRF_TOKEN_VALUE, sessionDomain: ctx.sessionDomain });
      }
      if (path === "/v1/quota") return quota();
      return listWorkspaces(req);
    }
    const m = path.match(/^\/v1\/workspaces\/([^/]+)(\/[a-z]+)?$/);
    if (!m) return undefined;
    const ws = state.workspaces.get(m[1]);
    if (!ws) return undefined;
    if (!t.visible(ws.id)) return err(404, "NOT_FOUND", "workspace not found", false);
    if (!m[2] && method === "GET") return ok(200, { ...ws, owner: t.ownerOf(ws.id) });
    return undefined;
  }

  function control(req: MockRequest): MockResponse | undefined {
    if (!req.path.startsWith("/_control/admin/")) return undefined;
    if (req.method !== "POST") return err(404, "NOT_FOUND", "no control route", false);
    switch (req.path) {
      case "/_control/admin/me":
        if (Array.isArray(req.body?.roles)) t.me.roles = (req.body.roles as unknown[]).map(String);
        if (typeof req.body?.displayName === "string") t.me.displayName = req.body.displayName;
        return ok(200, t.me);
      case "/_control/admin/seed":
        seed();
        return ok(200, { seeded: true });
      case "/_control/admin/quota":
        if (req.body?.limits === null) t.limits = undefined;
        else if (req.body?.limits) {
          t.limits = { ...(t.limits ?? DEFAULT_LIMITS), ...(req.body.limits as object) };
          t.version += 1;
        }
        if (req.body?.userLimits === null) t.userLimits = undefined;
        else if (req.body?.userLimits) t.userLimits = { ...DEFAULT_USER_LIMITS, ...(req.body.userLimits as object) };
        if (typeof req.body?.managed === "boolean") t.managed = req.body.managed;
        return ok(200, { limits: t.limits ?? null, userLimits: t.userLimits ?? null, managed: t.managed });
      case "/_control/admin/userlimits":
        if (req.body?.default !== undefined) {
          t.userLimitDefault = req.body.default === null ? null : Number(req.body.default);
        }
        if (req.body?.overrides) {
          for (const [ref, v] of Object.entries(req.body.overrides as Record<string, number | null>)) {
            if (v === null) t.userLimitOverrides.delete(ref);
            else t.userLimitOverrides.set(ref, v);
          }
        }
        return ok(200, userLimitsBody());
      case "/_control/admin/fail":
        t.failNext.set(String(req.body?.path), {
          status: Number(req.body?.status ?? 503),
          code: String(req.body?.code ?? "UNAVAILABLE"),
        });
        return ok(200, { armed: req.body?.path });
      default:
        return err(404, "NOT_FOUND", `no control route ${req.path}`, false);
    }
  }

  return { name: "admin", reset, api, control, state: t };
}
