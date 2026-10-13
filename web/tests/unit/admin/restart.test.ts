import { describe, expect, it } from "vitest";
import { createApi } from "../../../src/api/client";
import {
  restartWorkspace,
  RestartAbortedError,
  RestartTimeoutError,
} from "../../../src/admin/api";
import { getWorkspace } from "../../../src/workspaces/api";
import { createMockApi, loginCookies, stubFetch } from "../helpers";
import { makeAdmin, seedTenant } from "./helpers";
import { adminArea, SEED_WORKSPACE_IDS } from "../../mock-api/admin.ts";
import { workspacesArea } from "../../mock-api/workspaces.ts";
import { readyWorkspace } from "../../mock-api/fixtures.ts";
import type { ScopedWorkspace, WorkspacePhase } from "../../../src/admin/api";

const WS = SEED_WORKSPACE_IDS.graceReady;

function setup() {
  const api = createMockApi({ areas: [adminArea, workspacesArea] });
  loginCookies();
  makeAdmin(api);
  seedTenant(api);
  const client = createApi(stubFetch(api));
  return { api, client };
}

function view(phase: WorkspacePhase): ScopedWorkspace {
  return { ...readyWorkspace({ id: WS }), phase } as ScopedWorkspace;
}

function requests(api: ReturnType<typeof createMockApi>, method: string, suffix: string) {
  return api.state.requests.filter((r) => r.method === method && r.path.endsWith(suffix));
}

describe("restartWorkspace sequencing", () => {
  it("stops, waits for Stopped, then starts — in that order", async () => {
    const { api, client } = setup();
    const steps: string[] = [];
    const result = await restartWorkspace(client, WS, {
      fetch: (a, id) => getWorkspace(a, id),
      // The mock holds the phase at Stopping; the reconciler's answer to
      // the stop intent lands between polls.
      sleep: async () => {
        api.state.workspaces.get(WS)!.phase = "Stopped";
      },
      pollMs: 1,
      onStep: (s) => steps.push(s),
    });

    expect(result.desiredState).toBe("Running");
    expect(steps).toEqual(["stopping", "waiting", "starting"]);
    const stops = requests(api, "POST", `/v1/workspaces/${WS}:stop`);
    const starts = requests(api, "POST", `/v1/workspaces/${WS}:start`);
    expect(stops).toHaveLength(1);
    expect(starts).toHaveLength(1);
    const idx = api.state.requests.map((r) => `${r.method} ${r.path}`);
    expect(idx.indexOf(`POST /v1/workspaces/${WS}:stop`)).toBeLessThan(
      idx.indexOf(`POST /v1/workspaces/${WS}:start`),
    );
  });

  it("starts immediately when the workspace is already Stopped", async () => {
    const { api, client } = setup();
    api.state.workspaces.get(WS)!.phase = "Stopped";
    api.state.workspaces.get(WS)!.desiredState = "Stopped";
    let polls = 0;
    const result = await restartWorkspace(client, WS, {
      fetch: async () => {
        polls += 1;
        return view("Stopped");
      },
      sleep: async () => {},
    });
    expect(result.desiredState).toBe("Running");
    expect(polls).toBe(1);
    expect(requests(api, "POST", `/v1/workspaces/${WS}:start`)).toHaveLength(1);
  });

  it("times out without sending start when Stopped never arrives", async () => {
    const { api, client } = setup();
    let ticks = 0;
    await expect(
      restartWorkspace(client, WS, {
        fetch: async () => view("Stopping"),
        sleep: async () => {},
        now: () => (ticks += 60_000),
        timeoutMs: 120_000,
      }),
    ).rejects.toBeInstanceOf(RestartTimeoutError);
    expect(requests(api, "POST", `/v1/workspaces/${WS}:start`)).toHaveLength(0);
  });

  it("aborts when the workspace is deleted mid-wait", async () => {
    const { api, client } = setup();
    const phases: WorkspacePhase[] = ["Stopping", "Terminating"];
    await expect(
      restartWorkspace(client, WS, {
        fetch: async () => view(phases.shift() ?? "Terminating"),
        sleep: async () => {},
      }),
    ).rejects.toBeInstanceOf(RestartAbortedError);
    expect(requests(api, "POST", `/v1/workspaces/${WS}:start`)).toHaveLength(0);
  });

  it("propagates the API error when start is refused", async () => {
    const { api, client } = setup();
    // The injected read claims Stopped but the mock's real phase after the
    // stop POST is Stopping -> the contract 409 INVALID_STATE surfaces.
    await expect(
      restartWorkspace(client, WS, { fetch: async () => view("Stopped"), sleep: async () => {} }),
    ).rejects.toMatchObject({ code: "INVALID_STATE" });
    expect(requests(api, "POST", `/v1/workspaces/${WS}:start`)).toHaveLength(1);
  });
});
