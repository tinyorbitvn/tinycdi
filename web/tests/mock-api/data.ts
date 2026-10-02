// Retained-data area of the contract mock: /v1/data (list, attach, purge)
// and /_control/data/{id}. Ownership comes from the admin area's Tenancy:
// GET /v1/data honours `?scope=mine|tenant`, pages with an opaque cursor and
// carries `owner`; other users' records are 404 for non-admins.

import { randomBytes } from "node:crypto";
import { err, ok, type MockArea, type MockContext, type MockRequest, type MockResponse } from "./core.ts";
import { RETAINED_DISK, makeWorkspace, type RetainedFixture } from "./fixtures.ts";
import { DEMO_RETAINED } from "./demo.ts";
import { tenancy } from "./admin.ts";

export function dataArea(ctx: MockContext): MockArea {
  const { state } = ctx;
  const t = tenancy(ctx);

  function reset(): void {
    state.retained.set(RETAINED_DISK.id, structuredClone(RETAINED_DISK));
    if (ctx.demo) {
      for (const r of structuredClone(DEMO_RETAINED)) state.retained.set(r.id, r);
    }
  }

  function freshNonce(dataId: string): string {
    const nonce = `nonce-${randomBytes(8).toString("hex")}`;
    state.nonces.set(dataId, nonce);
    return nonce;
  }

  function api(req: MockRequest): MockResponse | undefined {
    if (req.path === "/v1/data" && req.method === "GET") return t.takeFailure(req) ?? listRetained(req);
    const m = req.path.match(/^\/v1\/data\/([^/]+)(\/attach|\/purge)?$/);
    if (!m) return undefined;
    const rec = state.retained.get(m[1]);
    if (!rec || !t.visible(rec.id)) return err(404, "NOT_FOUND", "retained data record not found", false);
    if (!m[2] && req.method === "GET") {
      return ok(200, { ...rec, owner: t.ownerOf(rec.id), purgeConfirmationNonce: freshNonce(rec.id) });
    }
    if (m[2] === "/attach" && req.method === "POST") return attachData(req, rec);
    if (m[2] === "/purge" && req.method === "POST") return purgeData(req, rec);
    return undefined;
  }

  function listRetained(req: MockRequest): MockResponse {
    const scope = t.resolveScope(req);
    if ("status" in scope) return scope;
    const items = [...state.retained.values()]
      .filter((r) => !scope.mine || t.ownerOf(r.id).subject === t.me.subject)
      .map((r) => ({
        ...r,
        owner: t.ownerOf(r.id),
        purgeConfirmationNonce: freshNonce(r.id),
      }));
    return t.paginate(req, items);
  }

  function attachData(req: MockRequest, rec: RetainedFixture): MockResponse {
    const idem = ctx.checkIdempotency(req);
    if ("error" in idem) return idem.error;
    if ("replay" in idem) return ok(idem.replay.status, idem.replay.body);
    if (rec.state !== "Retained") {
      return err(409, "INVALID_STATE", `disk is ${rec.state}`, true);
    }
    const tpl = state.templates.find((t) => t.id === req.body?.templateRef);
    if (!tpl || tpl.runtime !== rec.runtime) {
      return err(
        422,
        "INVALID_TEMPLATE",
        "template is unknown or incompatible with the disk runtime",
        false,
      );
    }
    rec.state = "Attaching";
    const ws = makeWorkspace({
      id: `${ctx.nextId("ws_")}ATTACH9X`,
      name: String(req.body?.name ?? "restored-desktop"),
      template: {
        id: tpl.id,
        name: tpl.name,
        family: tpl.family,
        revision: tpl.revision,
        runtime: tpl.runtime,
        experience: tpl.experience,
      },
      phase: req.body?.desiredState === "Running" ? "Provisioning" : "Pending",
      desiredState: req.body?.desiredState === "Running" ? "Running" : "Stopped",
      dataPolicy: "Retain",
      retainedDataRef: rec.id,
      conditions: [],
      createdAt: ctx.nowIso(),
      updatedAt: ctx.nowIso(),
    });
    rec.consumingWorkspaceId = ws.id;
    state.workspaces.set(ws.id, ws);
    ctx.recordEvent(ws.id, {
      type: "Normal",
      reason: "DataAttached",
      message: `Retained disk ${rec.id} from ${rec.sourceWorkspaceName} attached`,
    });
    idem.record(201, ws);
    return ok(201, ws);
  }

  function purgeData(req: MockRequest, rec: RetainedFixture): MockResponse {
    const nonce = req.body?.confirmationNonce;
    if (!nonce || nonce !== state.nonces.get(rec.id)) {
      return err(400, "INVALID_REQUEST", "missing or stale confirmation nonce", false);
    }
    if (rec.state !== "Retained") {
      return err(409, "INVALID_STATE", `disk is ${rec.state}; only Retained records can be purged`, true);
    }
    rec.state = "Purging";
    const purged = { ...rec, purgeConfirmationNonce: freshNonce(rec.id) };
    return ok(202, purged);
  }

  function control(req: MockRequest): MockResponse | undefined {
    const m = req.path.match(/^\/_control\/data\/([^/]+)$/);
    if (!m || req.method !== "POST") return undefined;
    const rec = state.retained.get(m[1]);
    if (!rec) return err(404, "NOT_FOUND", "no such record", false);
    Object.assign(rec, req.body ?? {});
    return ok(200, rec);
  }

  return { name: "data", reset, api, control };
}
