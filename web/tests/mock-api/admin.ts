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

import {
  byString,
  checkIfMatch,
  CSRF_TOKEN_VALUE,
  err,
  etagOf,
  ok,
  paginate,
  WORKSPACE_SORTS,
  type MockArea, type MockContext, type MockRequest, type MockResponse } from "./core.ts";
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
]
// Principals who signed in but hold no workspace, disk or override — the
// user directory lists them, the user-limits collection does not.
export const SIGNED_IN_ONLY: Owner[] = [
  { subject: "user-01J4ZDMGH", displayName: "Margaret Hamilton" },
  { subject: "user-01J4ZDKJN", displayName: "Katherine Johnson" },
]

export interface AuditRow {
  id: string;
  createdAt: string;
  actor: string;
  action: string;
  targetKind: string;
  targetId: string;
  outcome: string;
  errorCode?: string | undefined;
  requestId: string;
  details: Record<string, string>;
}

export const AUDIT_ROWS = 60;

// Deterministic audit rows for the /admin/audit mock: newest first, row 0 a
// denied workspace.start; workspace.start rows total 12 (one per 5). The 60
// rows exceed the mock's default page (50), so the UI exercises Load more.
function auditRows(): AuditRow[] {
  const ACTIONS = ["workspace.start", "workspace.stop", "admin.quota.set", "admin.user_limit.get", "data.purge"];
  const rows: AuditRow[] = [];
  for (let i = 0; i < AUDIT_ROWS; i += 1) {
    const action = ACTIONS[i % 5];
    rows.push({
      id: String(AUDIT_ROWS - i),
      createdAt: new Date(Date.UTC(2026, 9, 13, 12, 0) - i * 60_000).toISOString(),
      actor: "oidc:0011223344556677",
      action,
      targetKind: action.startsWith("workspace.") ? "workspace" : "tenant",
      targetId: action.startsWith("workspace.") ? `ws_audit${i}` : TENANT,
      outcome: i % 7 === 0 ? "denied" : "success",
      errorCode: i % 7 === 0 ? "FORBIDDEN" : undefined,
      requestId: `req-${i}`,
      details: {},
    });
  }
  return rows;
};

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

interface MockLiveSession {
  leaseRef: string;
  workspaceId: string;
  workspaceName: string;
  ownerRef: string;
  owner: Owner;
  createdAt: string;
  lastRenewedAt?: string;
  expiresAt: string;
}

// Two live sessions for Grace, one for Linus (newest first by createdAt).
function seedLiveSessions(): MockLiveSession[] {
  const [grace, linus] = OTHER_USERS;
  const row = (leaseRef: string, workspaceId: string, workspaceName: string, o: Owner, createdAt: string): MockLiveSession => ({
    leaseRef, workspaceId, workspaceName, ownerRef: ownerRefOf(o.subject), owner: o,
    createdAt, lastRenewedAt: createdAt, expiresAt: "2099-01-01T00:00:00Z",
  });
  return [
    row("0123456789abcdef", SEED_WORKSPACE_IDS.graceReady, "grace-analysis", grace, "2026-10-13T08:00:00Z"),
    row("fedcba9876543210", "ws_01J4ZGRACEREADY02", "grace-notes", grace, "2026-10-13T08:05:00Z"),
    row("00112233445566aa", SEED_WORKSPACE_IDS.linusStopped, "linus-desktop", linus, "2026-10-13T08:10:00Z"),
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
  // ownerRef -> the override row's version (its ETag is "ul-<n>"); the
  // default's version is userLimitDefaultVersion ("uld-<n>").
  userLimitVersions: Map<string, number>;
  userLimitDefaultVersion: number;
  userLimitSeq: number;
  // Tenant session-policy ceiling (ADR 0010): undefined = no row —
  // every channel FollowTemplate. spVersion is its version; the ETag
  // "sp-<n>" travels as a header and PUT echoes it via If-Match.
  sessionPolicy: { clipboard: string } | undefined;
  spVersion: number;
  // True = the tenant is declared in -tenant-quotas: source "config" and
  // PUT /v1/admin/tenants/{tenant}/quota answers 409 QUOTA_MANAGED_BY_CONFIG.
  managed: boolean;
  // Limits-row version — bumps on every quota write; its ETag "q-<n>"
  // travels as a header and PUT echoes it via If-Match.
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
    userLimitVersions: new Map(),
    userLimitDefaultVersion: 1,
    userLimitSeq: 1,
    sessionPolicy: undefined,
    spVersion: 1,
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
  let liveSessions = seedLiveSessions();
  let overviewFresh = true;

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
    t.userLimitVersions = new Map();
    t.userLimitDefaultVersion = 1;
    t.userLimitSeq = 1;
    t.sessionPolicy = undefined;
    t.spVersion = 1;
    t.managed = false;
    t.version = 1;
    t.owners = new Map();
    t.failNext = new Map();
    t.scopesSeen = [];
    liveSessions = seedLiveSessions();
    overviewFresh = true;
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
    return {
      perUser,
      body: {
        tenant: t.me.tenant,
        configured: t.limits !== undefined,
        ...(t.limits ? { limits: t.limits } : {}),
        usage,
      },
    };
  }

  function quota(): MockResponse {
    return ok(200, {
      ...quotaBody().body,
      ...(t.userLimits ? { userLimits: t.userLimits } : {}),
    });
  }

  // The admin quota view adds `source` — which layer owns the limits row —
  // and drops userLimits, which no store backs.
  function adminQuotaBody() {
    return {
      ...quotaBody().body,
      source: t.managed ? "config" : t.limits !== undefined ? "api" : "none",
    };
  }

  // The quota row's strong ETag; undefined while the tenant has no row.
  function quotaTag(): string | undefined {
    return t.limits === undefined ? undefined : etagOf(`q-${t.version}`);
  }

  function withTag(status: number, body: unknown, tag: string | undefined): MockResponse {
    return ok(status, body, tag ? { etag: tag } : {});
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
      return withTag(200, adminQuotaBody(), quotaTag());
    }
    if (req.method === "PUT") {
      if (t.managed) {
        return err(409, "QUOTA_MANAGED_BY_CONFIG", "quota for this tenant is managed by configuration", false);
      }
      const pre = checkIfMatch(req, quotaTag());
      if (pre) return pre;
      const b = req.body as Record<string, unknown> | null;
      const keys = ["runningWorkspaces", "cpuMillicores", "memoryMib", "storageGib"];
      const valid =
        b !== null &&
        keys.every((k) => Number.isInteger(b[k]) && (b[k] as number) >= 0) &&
        Object.keys(b).every((k) => keys.includes(k));
      if (!valid) return err(400, "INVALID_REQUEST", "invalid limits", false);
      t.version += 1;
      t.limits = {
        workspaces: 0,
        runningWorkspaces: b!.runningWorkspaces as number,
        cpuMillicores: b!.cpuMillicores as number,
        memoryMib: b!.memoryMib as number,
        storageGib: b!.storageGib as number,
      };
      return withTag(200, adminQuotaBody(), quotaTag());
    }
    return err(405, "INVALID_REQUEST", "method not allowed", false);
  }

  // /v1/admin/tenants/{tenant}/user-limits — tenant-admin of the named
  // tenant only. GET pages one item per principal with usage or a stored
  // override (sort `ownerRef`); PUT/DELETE .../{ownerRef} write one
  // override under If-Match (its etag, or `*` to create); GET/PUT
  // .../default read and write the tenant fallback under its own ETag.
  function userLimitEntries() {
    const rows = new Map<
      string,
      { ownerRef: string; subject: string; displayName: string; running: number }
    >();
    for (const b of quotaBody().perUser.values()) {
      rows.set(ownerRefOf(b.owner.subject), {
        ownerRef: ownerRefOf(b.owner.subject),
        subject: b.owner.subject,
        displayName: b.owner.displayName,
        running: b.usage.runningWorkspaces,
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
    return [...rows.values()].map((u) => entryOf(u));
  }

  function overrideTag(ref: string): string | undefined {
    const v = t.userLimitVersions.get(ref);
    return t.userLimitOverrides.has(ref) && v !== undefined ? etagOf(`ul-${v}`) : undefined;
  }

  function entryOf(u: { ownerRef: string; subject: string; displayName: string; running: number }) {
    const tag = overrideTag(u.ownerRef);
    return {
      ...u,
      limit: t.userLimitOverrides.get(u.ownerRef) ?? null,
      effective: t.userLimitOverrides.get(u.ownerRef) ?? t.userLimitDefault,
      ...(tag ? { etag: tag } : {}),
    };
  }

  function defaultTag(): string {
    return etagOf(`uld-${t.userLimitDefaultVersion}`);
  }

  function setOverride(ref: string, limit: number | null): void {
    if (limit === null) {
      t.userLimitOverrides.delete(ref);
      t.userLimitVersions.delete(ref);
      return;
    }
    t.userLimitOverrides.set(ref, limit);
    t.userLimitVersions.set(ref, ++t.userLimitSeq);
  }

  function adminUserLimits(req: MockRequest, tenant: string, sub: string | undefined): MockResponse {
    if (!t.isAdmin() || tenant !== t.me.tenant) {
      return err(403, "FORBIDDEN", "tenant administration requires the tenant-admin role", false);
    }
    if (sub === undefined) {
      if (req.method !== "GET") return err(405, "INVALID_REQUEST", "method not allowed", false);
      const failed = t.takeFailure(req);
      if (failed) return failed;
      return paginate(req, userLimitEntries(), { ownerRef: byString((u) => u.ownerRef) }, "ownerRef", (u) => u.ownerRef);
    }
    if (sub === "default") {
      if (req.method === "GET") {
        return withTag(200, { tenant: t.me.tenant, limit: t.userLimitDefault }, defaultTag());
      }
      if (req.method !== "PUT") return err(405, "INVALID_REQUEST", "method not allowed", false);
      const pre = checkIfMatch(req, defaultTag());
      if (pre) return pre;
      const limit = req.body?.limit;
      const lv =
        limit === null || limit === undefined
          ? null
          : Number.isInteger(limit) && (limit as number) >= 0
            ? (limit as number)
            : undefined;
      if (lv === undefined) return err(400, "INVALID_REQUEST", "invalid limit", false);
      t.userLimitDefault = lv;
      t.userLimitDefaultVersion += 1;
      return withTag(200, { tenant: t.me.tenant, limit: t.userLimitDefault }, defaultTag());
    }
    let ref: string;
    try {
      ref = decodeURIComponent(sub);
    } catch {
      return err(400, "INVALID_REQUEST", "ownerRef must be an issuer|sub reference", false);
    }
    if (!ref.includes("|") || ref.endsWith("|") || ref.startsWith("|")) {
      return err(400, "INVALID_REQUEST", "ownerRef must be an issuer|sub reference", false);
    }
    if (req.method === "DELETE") {
      const current = overrideTag(ref);
      if (current === undefined) return err(404, "NOT_FOUND", "no override for this principal", false);
      const pre = checkIfMatch(req, current);
      if (pre) return pre;
      setOverride(ref, null);
      return { status: 204, headers: {}, body: undefined };
    }
    if (req.method === "PUT") {
      const pre = checkIfMatch(req, overrideTag(ref));
      if (pre) return pre;
      const limit = req.body?.limit;
      if (!Number.isInteger(limit) || (limit as number) < 0) {
        return err(400, "INVALID_REQUEST", "limit must be a whole number >= 0", false);
      }
      setOverride(ref, limit as number);
      const row = userLimitEntries().find((u) => u.ownerRef === ref)!;
      return withTag(200, row, overrideTag(ref));
    }
    return err(405, "INVALID_REQUEST", "method not allowed", false);
  }

  // GET /v1/admin/tenants/{tenant}/users[/{ownerRef}] — the tenant's
  // principal directory: everyone who signed in, whether or not they hold
  // a workspace, disk or limit override. lastSeenAt is a fixed instant per
  // principal (staggered by an hour) so the lastSeen order is stable.
  function directoryUsers() {
    const principals = [t.me, ...OTHER_USERS, ...SIGNED_IN_ONLY];
    const perUser = quotaBody().perUser;
    return principals.map((p, i) => {
      const ownerRef = ownerRefOf(p.subject);
      let workspaces = 0;
      for (const id of state.workspaces.keys()) {
        if (t.ownerOf(id).subject === p.subject) workspaces += 1;
      }
      return {
        ownerRef,
        subject: p.subject,
        displayName: p.displayName,
        email: p.subject === t.me.subject ? (t.me.email ?? "") : "",
        lastSeenAt: new Date(Date.UTC(2026, 9, 13, 9, 0) - i * 3_600_000).toISOString(),
        workspaces,
        limits: entryOf({
          ownerRef,
          subject: p.subject,
          displayName: p.displayName,
          running: perUser.get(p.subject)?.usage.runningWorkspaces ?? 0,
        }),
      };
    });
  }

  function adminUsers(req: MockRequest, tenant: string, ownerRef: string | undefined): MockResponse {
    if (!t.isAdmin() || tenant !== t.me.tenant) {
      return err(403, "FORBIDDEN", "tenant administration requires the tenant-admin role", false);
    }
    if (req.method !== "GET") return err(405, "INVALID_REQUEST", "method not allowed", false);
    const failed = t.takeFailure(req);
    if (failed) return failed;
    const rows = directoryUsers();
    if (ownerRef === undefined) {
      return paginate(
        req,
        rows,
        {
          ownerRef: byString((u) => u.ownerRef),
          displayName: byString((u) => u.displayName),
          lastSeen: byString((u) => u.lastSeenAt),
        },
        "ownerRef",
        (u) => u.ownerRef,
      );
    }
    const row = rows.find((u) => u.ownerRef === ownerRef);
    if (!row) return err(404, "NOT_FOUND", "user has not signed in to this tenant", false);
    return ok(200, row);
  }

  // GET/PUT /v1/admin/tenants/{tenant}/session-policy — tenant-admin of
  // the named tenant only. No row resolves FollowTemplate everywhere
  // (source FollowTemplate); a PUT whose every channel is
  // FollowTemplate deletes the row. Optimistic-concurrency If-Match
  // like quota: the reported version or "*" only on a missing row.
  function sessionPolicyBody() {
    return {
      tenant: t.me.tenant,
      clipboard: t.sessionPolicy?.clipboard ?? "FollowTemplate",
      fileTransfer: { mode: "FollowTemplate", maxFileBytes: null },
      source: t.sessionPolicy ? "TenantRow" : "FollowTemplate",
    };
  }

  // The policy row's strong ETag; undefined while the tenant has no row.
  function sessionPolicyTag(): string | undefined {
    return t.sessionPolicy ? etagOf(`sp-${t.spVersion}`) : undefined;
  }

  function adminSessionPolicy(req: MockRequest, tenant: string): MockResponse {
    if (!t.isAdmin() || tenant !== t.me.tenant) {
      return err(403, "FORBIDDEN", "session policy administration requires the tenant-admin role", false);
    }
    if (req.method === "GET") {
      const failed = t.takeFailure(req);
      if (failed) return failed;
      return withTag(200, sessionPolicyBody(), sessionPolicyTag());
    }
    if (req.method === "PUT") {
      const pre = checkIfMatch(req, sessionPolicyTag());
      if (pre) return pre;
      const b = req.body as Record<string, unknown> | null;
      const CEILINGS = ["FollowTemplate", "Disabled", "Send", "Receive", "Bidirectional"];
      const clip = b?.clipboard;
      if (b === null || typeof clip !== "string" || !CEILINGS.includes(clip)) {
        return err(400, "INVALID_REQUEST", "clipboard must be one of FollowTemplate, Disabled, Send, Receive, Bidirectional", false);
      }
      const ft = b.fileTransfer as { mode?: string; maxFileBytes?: number | null } | undefined;
      if (ft !== undefined && ((ft.mode ?? "FollowTemplate") !== "FollowTemplate" || (ft.maxFileBytes ?? null) !== null)) {
        return err(400, "INVALID_REQUEST", "fileTransfer is inert in this version; only FollowTemplate is accepted", false);
      }
      if (clip === "FollowTemplate") {
        t.sessionPolicy = undefined;
      } else {
        t.sessionPolicy = { clipboard: clip };
      }
      t.spVersion += 1;
      return withTag(200, sessionPolicyBody(), sessionPolicyTag());
    }
    return err(405, "INVALID_REQUEST", "method not allowed", false);
  }

  function adminAudit(req: MockRequest, tenant: string): MockResponse {
    if (!t.isAdmin() || tenant !== t.me.tenant) {
      return err(403, "FORBIDDEN", "tenant administration requires the tenant-admin role", false);
    }
    if (req.method !== "GET") return err(405, "INVALID_REQUEST", "method not allowed", false);
    const failed = t.takeFailure(req);
    if (failed) return failed;
    const sort = req.query.get("sort") || "-createdAt";
    if (sort !== "-createdAt") {
      return err(400, "INVALID_REQUEST", "audit lists newest first only", false);
    }
    const action = req.query.get("action");
    const actor = req.query.get("actor");
    const target = req.query.get("target");
    const since = req.query.get("since");
    const until = req.query.get("until");
    const rows = auditRows().filter((r) => {
      if (action !== null && r.action !== action) return false;
      if (actor !== null && r.actor !== actor) return false;
      if (target !== null && r.targetId !== target) return false;
      if (since !== null && Date.parse(r.createdAt) < Date.parse(since)) return false;
      if (until !== null && Date.parse(r.createdAt) >= Date.parse(until)) return false;
      return true;
    });
    return paginate(req, rows, { createdAt: byString((a) => a.createdAt) }, "-createdAt", (a) => a.id);
  }

  function listWorkspaces(req: MockRequest): MockResponse {
    const scope = t.resolveScope(req);
    if ("status" in scope) return scope;
    const phase = req.query.get("phase");
    let items = [...state.workspaces.values()];
    if (scope.mine) items = items.filter((w) => t.ownerOf(w.id).subject === t.me.subject);
    if (phase) items = items.filter((w) => w.phase === phase);
    return paginate(
      req,
      items.map((w) => ({ ...w, owner: t.ownerOf(w.id) })),
      WORKSPACE_SORTS,
      "-createdAt",
    );
  }

  function adminSessions(req: MockRequest, tenant: string): MockResponse {
    if (!t.isAdmin() || tenant !== t.me.tenant) {
      return err(403, "FORBIDDEN", "tenant administration requires the tenant-admin role", false);
    }
    if (req.method !== "GET") return err(405, "INVALID_REQUEST", "method not allowed", false);
    if ((req.query.get("sort") || "-createdAt") !== "-createdAt") {
      return err(400, "INVALID_REQUEST", "sessions list newest first only (sort=-createdAt)", false);
    }
    const failed = t.takeFailure(req);
    if (failed) return failed;
    const owner = req.query.get("ownerRef");
    const rows = liveSessions.filter((s) => !owner || s.ownerRef === owner);
    return paginate(req, rows, { createdAt: byString((s) => s.createdAt) }, "-createdAt", (s) => s.workspaceId);
  }

  function adminRevokeSessions(req: MockRequest, tenant: string, rawRef: string): MockResponse {
    if (!t.isAdmin() || tenant !== t.me.tenant) {
      return err(403, "FORBIDDEN", "tenant administration requires the tenant-admin role", false);
    }
    if (req.method !== "POST") return err(405, "INVALID_REQUEST", "method not allowed", false);
    let ref: string;
    try {
      ref = decodeURIComponent(rawRef);
    } catch {
      return err(400, "INVALID_REQUEST", "ownerRef must be an issuer|sub reference", false);
    }
    if (!ref.includes("|") || ref.endsWith("|") || ref.startsWith("|")) {
      return err(400, "INVALID_REQUEST", "ownerRef must be an issuer|sub reference", false);
    }
    const gone = liveSessions.filter((s) => s.ownerRef === ref);
    liveSessions = liveSessions.filter((s) => s.ownerRef !== ref);
    return ok(200, { sessions: gone.length > 0 ? 1 : 0, tickets: 0, leases: gone.length });
  }

  // GET /v1/admin/tenants/{tenant}/overview — fixed counts; /_control/admin/overview {fresh} flips the stale flag.
  function adminOverview(req: MockRequest, tenant: string): MockResponse {
    if (!t.isAdmin() || tenant !== t.me.tenant) {
      return err(403, "FORBIDDEN", "tenant administration requires the tenant-admin role", false);
    }
    if (req.method !== "GET") return err(405, "INVALID_REQUEST", "method not allowed", false);
    const failed = t.takeFailure(req);
    if (failed) return failed;
    return ok(200, {
      liveSessions: 2,
      runningByTemplate: overviewFresh
        ? [{ template: "chrome-r1", count: 2 }, { template: "linux-r3", count: 1 }]
        : [],
      phases: overviewFresh ? { Ready: 3, Stopped: 1 } : {},
      provisioning: { p95Seconds: 12.4, samples: 7 },
      stuckFinalizers: 1,
      fresh: overviewFresh,
      generatedAt: "2026-10-13T09:00:00Z",
    });
  }

  function api(req: MockRequest): MockResponse | undefined {
    const { method, path } = req;
    const mOvw = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/overview$/);
    if (mOvw) return adminOverview(req, mOvw[1]);
    const mSess = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/sessions$/);
    if (mSess) return adminSessions(req, mSess[1]);
    const mRevoke = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/users\/([^/]+)\/sessions:revoke-all$/);
    if (mRevoke) return adminRevokeSessions(req, mRevoke[1], mRevoke[2]);
    const mQuota = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/quota$/);
    if (mQuota) return adminQuota(req, mQuota[1]);
    const mUL = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/user-limits(?:\/([^/]+))?$/);
    if (mUL) return adminUserLimits(req, mUL[1], mUL[2]);
    const mU = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/users(?:\/([^/]+))?$/);
    if (mU) return adminUsers(req, mU[1], mU[2] === undefined ? undefined : decodeURIComponent(mU[2]));
    const mSP = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/session-policy$/);
    if (mSP) return adminSessionPolicy(req, mSP[1]);
    const mAU = path.match(/^\/v1\/admin\/tenants\/([^/]+)\/audit$/);
    if (mAU) return adminAudit(req, mAU[1]);
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
    const m = path.match(/^\/v1\/workspaces\/([^/:]+)([/:][a-z]+)?$/);
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
          t.userLimitDefaultVersion += 1;
        }
        if (req.body?.overrides) {
          for (const [ref, v] of Object.entries(req.body.overrides as Record<string, number | null>)) {
            setOverride(ref, v);
          }
        }
        return ok(200, { default: t.userLimitDefault, items: userLimitEntries() });
      case "/_control/admin/session-policy":
        if (req.body?.clipboard === null || req.body?.clipboard === "FollowTemplate") {
          t.sessionPolicy = undefined;
        } else if (typeof req.body?.clipboard === "string") {
          t.sessionPolicy = { clipboard: req.body.clipboard };
        }
        t.spVersion += 1;
        return ok(200, sessionPolicyBody());
      case "/_control/admin/overview":
        overviewFresh = req.body?.fresh !== false;
        return ok(200, { fresh: overviewFresh });
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
