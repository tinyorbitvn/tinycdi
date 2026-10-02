// Workspace area of the contract mock: /v1/workspaces (+ start, stop,
// connections, events) and /v1/templates, the matching /_control routes,
// and the demo-mode controller that walks transitional phases forward.

import { randomBytes } from "node:crypto";
import {
  err,
  ok,
  sessionLaunchUrl,
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
  type WorkspaceFixture,
} from "./fixtures.ts";
import { DEMO_EVENTS, DEMO_TEMPLATES, DEMO_TRANSITIONAL, DEMO_WORKSPACES } from "./demo.ts";

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
): ConditionFixture {
  return {
    type,
    status,
    reason,
    ...(message ? { message } : {}),
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

export function workspacesArea(ctx: MockContext): MockArea {
  const { state } = ctx;

  function reset(): void {
    state.templates = [structuredClone(TEMPLATE_LINUX), structuredClone(TEMPLATE_BROWSER)];
    const seed = makeWorkspace();
    state.workspaces.set(seed.id, seed);
    state.events.set(seed.id, structuredClone(SEED_EVENTS));
    if (ctx.demo) {
      state.templates.push(...structuredClone(DEMO_TEMPLATES).map((tpl) => ({ family: tpl.name, ...tpl })));
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
    const m = path.match(/^\/v1\/workspaces\/([^/]+)(\/start|\/stop|\/connections|\/events)?$/);
    if (!m) return undefined;
    const ws = state.workspaces.get(m[1]);
    if (!ws) return err(404, "NOT_FOUND", "workspace not found", false);
    const sub = m[2] ?? "";
    if (!sub && method === "GET") return ok(200, ws);
    if (!sub && method === "DELETE") return deleteWorkspace(ws);
    if (sub === "/start" && method === "POST") return startWorkspace(req, ws);
    if (sub === "/stop" && method === "POST") return stopWorkspace(ws);
    if (sub === "/connections" && method === "POST") return createConnection(req, ws);
    if (sub === "/events" && method === "GET") return ok(200, { items: state.events.get(ws.id) ?? [] });
    return undefined;
  }

  function listWorkspaces(req: MockRequest): MockResponse {
    const phase = req.query.get("phase");
    let items = [...state.workspaces.values()].sort((a, b) =>
      a.createdAt.localeCompare(b.createdAt),
    );
    if (phase) items = items.filter((w) => w.phase === phase);
    return ok(200, { items });
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
    const ws = makeWorkspace({
      id: `${ctx.nextId("ws_")}X8KQ2M9X`,
      name: String(name ?? ""),
      template: {
        id: tpl.id,
        name: tpl.name,
        family: tpl.family ?? tpl.name,
        revision: tpl.revision,
        runtime: tpl.runtime,
        experience: tpl.experience,
      },
      phase: running ? "Provisioning" : "Pending",
      desiredState: running ? "Running" : "Stopped",
      dataPolicy: (typeof dataPolicy === "string"
        ? dataPolicy
        : tpl.dataPolicyDefault) as WorkspaceFixture["dataPolicy"],
      conditions: [condition(ctx, "Admitted", "True", "QuotaReserved")],
      ...(typeof retainedDataRef === "string" ? { retainedDataRef } : {}),
      createdAt: ctx.nowIso(),
      updatedAt: ctx.nowIso(),
    });
    state.workspaces.set(ws.id, ws);
    ctx.recordEvent(ws.id, {
      type: "Normal",
      reason: "Admitted",
      message: `Quota reserved for template ${tpl.name}@${tpl.revision}`,
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
    if (ws.desiredState === "Running" && ws.phase !== "Terminating") return ok(200, ws);
    if (ws.phase !== "Stopped" && ws.phase !== "Failed") {
      return err(409, "INVALID_STATE", `cannot start while ${ws.phase}`, true);
    }
    ws.phase = "Provisioning";
    ws.desiredState = "Running";
    delete ws.failureReason;
    ws.updatedAt = ctx.nowIso();
    ctx.recordEvent(ws.id, { type: "Normal", reason: "Starting", message: "Start requested" });
    idem.record(200, ws);
    return ok(200, ws);
  }

  function stopWorkspace(ws: WorkspaceFixture): MockResponse {
    if (ws.phase !== "Stopped" && ws.phase !== "Stopping") {
      ws.phase = "Stopping";
      ctx.recordEvent(ws.id, { type: "Normal", reason: "Stopping", message: "Stop requested" });
    }
    ws.desiredState = "Stopped";
    ws.updatedAt = ctx.nowIso();
    state.leases.delete(ws.id);
    return ok(200, ws);
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
    return ok(201, {
      workspaceId: ws.id,
      ticket,
      launchUrl: sessionLaunchUrl(ctx, ws.id),
      expiresAt: new Date(ctx.now() + 60_000).toISOString(),
    });
  }

  function listTemplates(req: MockRequest): MockResponse {
    const runtime = req.query.get("runtime");
    let items = state.templates;
    if (runtime) items = items.filter((t) => t.runtime === runtime);
    return ok(200, { items });
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
          setCondition(ws, condition(ctx, "StorageReady", "True", "VolumeBound"));
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
