// Workspace area of the contract mock: /v1/workspaces (+ start, stop,
// connections, events) and /v1/templates, the matching /_control routes,
// and the demo-mode controller that walks transitional phases forward.

import { randomBytes } from "node:crypto";
import {
  byString,
  err,
  ok,
  paginate,
  sessionHostLabel,
  sessionLaunchUrl,
  WORKSPACE_SORTS,
  type MockArea,
  type MockContext,
  type MockRequest,
  type MockResponse,
} from "./core.ts";
import {
  READY_CONDITIONS,
  RETAINED_DISK,
  SEED_EVENTS,
  TEMPLATE_BROWSER,
  TEMPLATE_LINUX,
  makeWorkspace,
  readyWorkspace,
  type ConditionFixture,
  type WorkspaceEventFixture,
  type WorkspaceFixture,
} from "./fixtures.ts";
import { DEMO_EVENTS, DEMO_TEMPLATES, DEMO_TRANSITIONAL, DEMO_WORKSPACES } from "./demo.ts";
import { tenancy } from "./admin.ts";

// meetClipboard mirrors the server-side lattice (ADR 0010 §1): the meet
// of a template's clipboard policy and the tenant ceiling.
function meetClipboard(template: string, ceiling: string): string {
  switch (ceiling) {
    case "Disabled":
      return "Disabled";
    case "Send":
    case "Receive":
      return template === ceiling || template === "Bidirectional" ? ceiling : "Disabled";
    default:
      return template;
  }
}

// Demo-mode controller delays per phase (ms since the last update).
const DEMO_STEP_MS: Partial<Record<WorkspaceFixture["phase"], number>> = {
  Pending: 2500,
  Provisioning: 6000,
  Stopping: 4000,
  Terminating: 4000,
};

function condition(
  ctx: MockContext,
  type: ConditionFixture["type"],
  status: ConditionFixture["status"],
  reason: string,
  message?: string,
  params?: Record<string, string>,
): ConditionFixture {
  return {
    type,
    status,
    reason,
    ...(message ? { message } : {}),
    ...(params ? { params } : {}),
    lastTransitionTime: ctx.nowIso(),
  };
}

function setCondition(ws: WorkspaceFixture, c: ConditionFixture): void {
  const i = ws.conditions.findIndex((x) => x.type === c.type);
  if (i >= 0) {
    if (ws.conditions[i].status === c.status && ws.conditions[i].reason === c.reason) return;
    ws.conditions[i] = c;
  } else {
    ws.conditions.push(c);
  }
}

// The API gives every event a stable id (unique within the list); fixtures
// that omit one get "<reason>" with a numeric suffix for repeats.
function withEventIds(events: WorkspaceEventFixture[]): (WorkspaceEventFixture & { id: string })[] {
  const seen = new Map<string, number>();
  return events.map((ev) => {
    if (ev.id) return { ...ev, id: ev.id };
    const n = seen.get(ev.reason) ?? 0;
    seen.set(ev.reason, n + 1);
    return { ...ev, id: n === 0 ? ev.reason : `${ev.reason}.${n}` };
  });
}

export function workspacesArea(ctx: MockContext): MockArea {
  const { state } = ctx;
  const fileTransfer = new Map<string, { policy: string; maxFileBytes: number }>();

  function reset(): void {
    fileTransfer.clear();
    state.templates = [structuredClone(TEMPLATE_LINUX), structuredClone(TEMPLATE_BROWSER)];
    const seed = makeWorkspace();
    state.workspaces.set(seed.id, seed);
    state.events.set(seed.id, structuredClone(SEED_EVENTS));
    if (ctx.demo) {
      state.templates.push(...structuredClone(DEMO_TEMPLATES));
      for (const ws of structuredClone(DEMO_WORKSPACES)) {
        if (DEMO_TRANSITIONAL.has(ws.id)) ws.updatedAt = ctx.nowIso();
        state.workspaces.set(ws.id, ws);
      }
      for (const [id, evs] of Object.entries(DEMO_EVENTS)) state.events.set(id, structuredClone(evs));
    }
  }

  function api(req: MockRequest): MockResponse | undefined {
    const { method, path } = req;
    if (path === "/v1/workspaces" && method === "GET") return listWorkspaces(req);
    if (path === "/v1/workspaces" && method === "POST") return createWorkspace(req);
    if (path === "/v1/templates" && method === "GET") return listTemplates(req);
    const m = path.match(/^\/v1\/workspaces\/([^/:]+)(:start|:stop|\/connections|\/events|\/file-transfers)?$/);
    if (!m) return undefined;
    const ws = state.workspaces.get(m[1]);
    if (!ws) return err(404, "NOT_FOUND", "workspace not found", false);
    const sub = m[2] ?? "";
    if (!sub && method === "GET") return ok(200, ws);
    if (!sub && method === "DELETE") return deleteWorkspace(ws);
    if (sub === ":start" && method === "POST") return startWorkspace(req, ws);
    if (sub === ":stop" && method === "POST") return stopWorkspace(ws);
    if (sub === "/connections" && method === "POST") return createConnection(req, ws);
    if (sub === "/events" && method === "GET") return listEvents(req, ws);
    if (sub === "/file-transfers" && method === "POST") return mintTransfer(req, ws);
    return undefined;
  }

  function listWorkspaces(req: MockRequest): MockResponse {
    const phase = req.query.get("phase");
    let items = [...state.workspaces.values()];
    if (phase) items = items.filter((w) => w.phase === phase);
    return paginate(req, items, WORKSPACE_SORTS, "-createdAt");
  }

  // Events page in one fixed order: `sort` accepts only `-time`.
  function listEvents(req: MockRequest, ws: WorkspaceFixture): MockResponse {
    const sort = req.query.get("sort");
    if (sort !== null && sort !== "-time") return err(400, "INVALID_REQUEST", "sort must be -time", false);
    return ok(200, { items: withEventIds(state.events.get(ws.id) ?? []) });
  }

  function createWorkspace(req: MockRequest): MockResponse {
    const idem = ctx.checkIdempotency(req);
    if ("error" in idem) return idem.error;
    if ("replay" in idem) return ok(idem.replay.status, idem.replay.body);
    if (state.quotaExhausted) {
      return err(409, "QUOTA_EXHAUSTED", "tenant quota has no headroom", false);
    }
    const { name, templateRef, desiredState, dataPolicy, retainedDataRef } = req.body ?? {};
    const tpl = state.templates.find((t) => t.id === templateRef);
    if (!tpl || state.invalidTemplates.has(String(templateRef))) {
      return err(422, "INVALID_TEMPLATE", "templateRef is unknown, unpublished or disallowed", false);
    }
    if (retainedDataRef !== undefined && !state.retained.has(String(retainedDataRef))) {
      return err(400, "INVALID_REQUEST", "retainedDataRef not found", false);
    }
    const running = desiredState === "Running";
    // One stamp for the whole insert: the UI keys "create" vs "start" on
    // createdAt === updatedAt, and two reads can straddle a millisecond.
    const ts = ctx.nowIso();
    const ws = makeWorkspace({
      id: `${ctx.nextId("ws_")}X8KQ2M9X`,
      name: String(name ?? ""),
      template: {
        id: tpl.id,
        name: tpl.name,
        family: tpl.family,
        revision: tpl.revision,
        runtime: tpl.runtime,
        experience: tpl.experience,
      },
      phase: running ? "Provisioning" : "Pending",
      desiredState: running ? "Running" : "Stopped",
      dataPolicy: (typeof dataPolicy === "string"
        ? dataPolicy
        : tpl.dataPolicyDefault) as WorkspaceFixture["dataPolicy"],
      conditions: [
        condition(ctx, "Admitted", "True", "QuotaReserved", undefined, {
          cpuMillicores: String(tpl.resources.cpuMillicores),
          memoryMiB: String(tpl.resources.memoryMib),
          storageGiB: String(tpl.resources.storageGib),
        }),
      ],
      ...(typeof retainedDataRef === "string" ? { retainedDataRef } : {}),
      createdAt: ts,
      updatedAt: ts,
    });
    state.workspaces.set(ws.id, ws);
    ctx.recordEvent(ws.id, {
      type: "Normal",
      reason: "QuotaReserved",
      message: `Quota reserved for template ${tpl.name}@${tpl.revision}`,
      params: {
        cpuMillicores: String(tpl.resources.cpuMillicores),
        memoryMiB: String(tpl.resources.memoryMib),
        storageGiB: String(tpl.resources.storageGib),
      },
    });
    idem.record(201, ws);
    return ok(201, ws);
  }

  function deleteWorkspace(ws: WorkspaceFixture): MockResponse {
    ws.phase = "Terminating";
    ws.updatedAt = ctx.nowIso();
    state.leases.delete(ws.id);
    ctx.recordEvent(ws.id, { type: "Normal", reason: "Deleting", message: "Workspace deletion requested" });
    return ok(202, ws);
  }

  function startWorkspace(req: MockRequest, ws: WorkspaceFixture): MockResponse {
    const idem = ctx.checkIdempotency(req);
    if ("error" in idem) return idem.error;
    if ("replay" in idem) return ok(idem.replay.status, idem.replay.body);
    if (ws.desiredState === "Running" && ws.phase !== "Terminating") return ok(202, ws);
    if (ws.phase !== "Stopped" && ws.phase !== "Failed") {
      return err(409, "INVALID_STATE", `cannot start while ${ws.phase}`, true);
    }
    ws.phase = "Provisioning";
    ws.desiredState = "Running";
    delete ws.failureReason;
    ws.updatedAt = ctx.nowIso();
    ctx.recordEvent(ws.id, { type: "Normal", reason: "Starting", message: "Start requested" });
    idem.record(202, ws);
    return ok(202, ws);
  }

  function stopWorkspace(ws: WorkspaceFixture): MockResponse {
    if (ws.phase !== "Stopped" && ws.phase !== "Stopping") {
      ws.phase = "Stopping";
      ctx.recordEvent(ws.id, { type: "Normal", reason: "Stopping", message: "Stop requested" });
    }
    ws.desiredState = "Stopped";
    ws.updatedAt = ctx.nowIso();
    state.leases.delete(ws.id);
    return ok(202, ws);
  }

  function createConnection(req: MockRequest, ws: WorkspaceFixture): MockResponse {
    if (!(ws.phase === "Ready" && ws.desiredState === "Running")) {
      return err(409, "INVALID_STATE", "workspace is not connectable", true);
    }
    const takeover = req.body?.takeover === true;
    if (state.leases.has(ws.id) && !takeover) {
      return err(
        409,
        "CONNECTION_IN_USE",
        "a live interactive lease exists; pass takeover to replace it",
        false,
      );
    }
    const ticket = `tkt_${randomBytes(24).toString("base64url")}`;
    state.tickets.set(ticket, {
      workspaceId: ws.id,
      expiresAt: ctx.now() + 60_000,
      used: false,
    });
    state.leases.set(ws.id, ctx.nextId("lease_"));
    ctx.recordEvent(ws.id, {
      type: "Normal",
      reason: takeover ? "SessionTakenOver" : "SessionStarted",
      message: takeover ? "Interactive session taken over" : "Interactive session started",
    });
    const tpl = state.templates.find((tp) => tp.id === ws.template.id);
    return ok(201, {
      workspaceId: ws.id,
      ticket,
      launchUrl: sessionLaunchUrl(ctx, ws.id),
      expiresAt: new Date(ctx.now() + 60_000).toISOString(),
      // The resolved vector the ticket records (ADR 0010): template
      // policy ∧ tenant ceiling.
      sessionPolicy: {
        clipboard: meetClipboard(
          tpl?.clipboardPolicy ?? "Bidirectional",
          tenancy(ctx).sessionPolicy?.clipboard ?? "FollowTemplate",
        ),
        fileTransfer: fileTransfer.get(ws.id)?.policy ?? "Disabled",
        maxFileBytes: fileTransfer.get(ws.id)?.maxFileBytes ?? 0,
      },
    });
  }

  function mintTransfer(req: MockRequest, ws: WorkspaceFixture): MockResponse {
    const b = (req.body ?? {}) as { direction?: string; path?: string; bytes?: number };
    const ft = fileTransfer.get(ws.id) ?? { policy: "Disabled", maxFileBytes: 0 };
    if (!b.path || b.path === "." || b.path === ".." || /[/\\\u0000-\u001f]/.test(b.path)) {
      return err(400, "INVALID_REQUEST", "path must be a single file name", false);
    }
    if (ws.phase !== "Ready") return err(409, "INVALID_STATE", "the workspace must be running to transfer files", false);
    const allowed =
      ft.policy === "Bidirectional" || (ft.policy === "Upload" && b.direction === "upload") || (ft.policy === "Download" && b.direction === "download");
    if (!allowed) {
      const denied = err(403, "FORBIDDEN", `file ${b.direction} is not allowed for this workspace`, false);
      (denied.body as { error: { details?: unknown } }).error.details = { reason: "transfer_denied" };
      return denied;
    }
    if (b.direction === "upload" && (b.bytes === undefined || b.bytes < 0 || b.bytes > ft.maxFileBytes)) {
      return err(400, "INVALID_REQUEST", "bytes is over the transfer limit", false);
    }
    const alphabet = "abcdefghijklmnopqrstuvwxyz234567";
    const id = "xf_" + Array.from({ length: 26 }, () => alphabet[Math.floor(Math.random() * 32)]).join("");
    return ok(201, {
      id,
      direction: b.direction,
      path: b.path,
      declaredBytes: b.direction === "upload" ? b.bytes : 0,
      maxBytes: ft.maxFileBytes,
      expiresAt: new Date(ctx.now() + 120_000).toISOString(),
      contentUrl: `${ctx.sessionOriginUrl.protocol}//${sessionHostLabel(ws.id)}.${ctx.sessionDomain}/v1/transfers/${id}/content`,
    });
  }

  function listTemplates(req: MockRequest): MockResponse {
    const runtime = req.query.get("runtime");
    let items = state.templates;
    if (runtime) items = items.filter((t) => t.runtime === runtime);
    return paginate(req, items, { name: byString((t) => t.name) }, "name");
  }

  function control(req: MockRequest): MockResponse | undefined {
    const p = req.path;
    if (p === "/_control/quota" && req.method === "POST") {
      state.quotaExhausted = req.body?.exhausted === true;
      return ok(200, { quotaExhausted: state.quotaExhausted });
    }
    if (p === "/_control/templates/invalidate" && req.method === "POST") {
      state.invalidTemplates.add(String(req.body?.id));
      return ok(200, { invalid: [...state.invalidTemplates] });
    }
    // POST /_control/workspaces/fileTransfer {workspaceId, policy, maxFileBytes}
    // — the resolved file-transfer vector the next ticket records.
    if (p === "/_control/workspaces/fileTransfer" && req.method === "POST") {
      const b = (req.body ?? {}) as { workspaceId: string; policy: string; maxFileBytes: number };
      fileTransfer.set(b.workspaceId, { policy: b.policy, maxFileBytes: b.maxFileBytes });
      return ok(200, { workspaceId: b.workspaceId, ...fileTransfer.get(b.workspaceId) });
    }
    const wsm = p.match(/^\/_control\/workspaces\/([^/]+)(\/events)?$/);
    if (wsm && req.method === "PUT" && !wsm[2]) {
      // Upsert a fixture under a caller-chosen id — the real-binary e2e
      // seeds workspaces whose ids must satisfy sessionhost.Label, which
      // the generated ids (upper-case suffix) would not.
      const ws = readyWorkspace({
        ...(req.body as Partial<WorkspaceFixture> | undefined),
        id: wsm[1],
      });
      state.workspaces.set(ws.id, ws);
      return ok(200, ws);
    }
    if (wsm && req.method === "DELETE" && !wsm[2]) {
      // Simulate teardown completion: the API hides a finalised row (FX-R19).
      const ws = state.workspaces.get(wsm[1]);
      if (!ws) return err(404, "NOT_FOUND", "no such workspace", false);
      state.workspaces.delete(ws.id);
      state.events.delete(ws.id);
      return ok(200, { deleted: ws.id });
    }
    if (wsm && req.method === "POST") {
      const ws = state.workspaces.get(wsm[1]);
      if (!ws) return err(404, "NOT_FOUND", "no such workspace", false);
      if (wsm[2]) {
        ctx.recordEvent(ws.id, {
          type: req.body?.type === "Warning" ? "Warning" : "Normal",
          reason: String(req.body?.reason ?? "Test"),
          message: String(req.body?.message ?? ""),
        });
        return ok(200, { items: state.events.get(ws.id) });
      }
      Object.assign(ws, req.body ?? {});
      return ok(200, ws);
    }
    if (p === "/_control/lease" && req.method === "POST") {
      const workspaceId = String(req.body?.workspaceId ?? "");
      if (req.body?.active) state.leases.set(workspaceId, ctx.nextId("lease_"));
      else state.leases.delete(workspaceId);
      return ok(200, { leases: [...state.leases.keys()] });
    }
    return undefined;
  }

  // ---- demo controller: a toy reconciler so the dev server feels alive ----

  function advance(ws: WorkspaceFixture): void {
    ws.updatedAt = ctx.nowIso();
    switch (ws.phase) {
      case "Pending":
        ws.phase = ws.desiredState === "Running" ? "Provisioning" : "Stopped";
        if (ws.phase === "Stopped") {
          const storage = state.templates.find((x) => x.id === ws.template.id)?.resources.storageGib;
          setCondition(ws, condition(ctx, "StorageReady", "True", "VolumeBound", undefined,
            storage !== undefined ? { sizeGiB: String(storage) } : undefined));
          ctx.recordEvent(ws.id, { type: "Normal", reason: "Stopped", message: "Created stopped; volume bound" });
        }
        return;
      case "Provisioning":
        ws.phase = "Ready";
        for (const c of READY_CONDITIONS) setCondition(ws, { ...c, lastTransitionTime: ctx.nowIso() });
        ctx.recordEvent(ws.id, { type: "Normal", reason: "RuntimeReady", message: "Runtime is up; stream endpoint ready" });
        return;
      case "Stopping":
        ws.phase = "Stopped";
        setCondition(ws, condition(ctx, "RuntimeReady", "False", "RuntimeStopped", "runtime is not running"));
        setCondition(ws, condition(ctx, "ConnectionReady", "False", "RuntimeStopped", "runtime is not running"));
        ctx.recordEvent(ws.id, {
          type: "Normal",
          reason: "Stopped",
          message: ws.dataPolicy === "Retain" ? "Runtime stopped; disk retained" : "Runtime stopped; ephemeral data discarded",
        });
        return;
      case "Terminating":
        state.workspaces.delete(ws.id);
        state.events.delete(ws.id);
        if (ws.dataPolicy === "Retain") {
          const id = ctx.nextId("rd_demo");
          state.retained.set(id, {
            ...structuredClone(RETAINED_DISK),
            id,
            sizeGib: 20,
            runtime: ws.template.runtime,
            sourceWorkspaceName: ws.name,
            retainedAt: ctx.nowIso(),
          });
        }
        return;
      default:
        return;
    }
  }

  function tick(now: number): void {
    for (const ws of [...state.workspaces.values()]) {
      const step = DEMO_STEP_MS[ws.phase];
      if (step !== undefined && now - Date.parse(ws.updatedAt) >= step) advance(ws);
    }
  }

  return { name: "workspaces", reset, api, control, tick };
}
