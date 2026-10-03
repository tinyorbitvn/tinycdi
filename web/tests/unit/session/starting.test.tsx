import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createApi, setCsrfToken } from "../../../src/api/client";
import { ApiProvider } from "../../../src/api/context";
import { MeProvider, type Me } from "../../../src/app/me";
import { SessionPage } from "../../../src/session/SessionPage";
import { createMockApi, CSRF_TOKEN_VALUE } from "../../mock-api/handler.ts";
import { loginCookies, stubFetch } from "../helpers";
import { makeWorkspace, readyWorkspace } from "../../mock-api/fixtures.ts";
import type { ConditionFixture, WorkspaceFixture } from "../../mock-api/fixtures.ts";

const SESSION_DOMAIN = "session.example.com";

const ME: Me = {
  subject: "user-01J4ZDADA",
  displayName: "Ada Lovelace",
  tenant: "acme",
  roles: ["user"],
  csrfToken: CSRF_TOKEN_VALUE,
  sessionDomain: SESSION_DOMAIN,
};

function cond(
  type: ConditionFixture["type"],
  status: ConditionFixture["status"],
  reason: string,
): ConditionFixture {
  return { type, status, reason, lastTransitionTime: new Date().toISOString() };
}

// A workspace mid start: operator picked it up, pod not ready yet.
function startingWorkspace(over: Partial<WorkspaceFixture> = {}): WorkspaceFixture {
  return makeWorkspace({
    phase: "Provisioning",
    desiredState: "Running",
    conditions: [
      cond("Admitted", "True", "QuotaReserved"),
      cond("StorageReady", "True", "Ready"),
      cond("RuntimeReady", "False", "PreparingPod"),
    ],
    updatedAt: new Date().toISOString(),
    ...over,
  });
}

function watchFormSubmits() {
  const submitted: HTMLFormElement[] = [];
  vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(function (
    this: HTMLFormElement,
  ) {
    submitted.push(this);
  });
  return submitted;
}

// The starting poll counts every GET /v1/workspaces/{id} through control;
// control.workspace401 forces UNAUTHENTICATED on those GETs.
function setup(workspace: WorkspaceFixture) {
  const api = createMockApi({ sessionOrigin: `https://${SESSION_DOMAIN}` });
  api.state.workspaces.set(workspace.id, workspace);
  loginCookies();
  const control = { workspaceGets: 0, workspace401: false };
  const base = stubFetch(api);
  const wrapped = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const req = input instanceof Request ? input : new Request(String(input), init);
    const path = new URL(req.url).pathname;
    if (req.method === "GET" && path === `/v1/workspaces/${workspace.id}`) {
      control.workspaceGets += 1;
      if (control.workspace401) {
        return new Response(
          JSON.stringify({ code: "UNAUTHENTICATED", message: "expired", retryable: false }),
          { status: 401, headers: { "content-type": "application/json" } },
        );
      }
    }
    return base(input as RequestInfo, init);
  }) as typeof fetch;
  const submitted = watchFormSubmits();
  const utils = render(
    <ApiProvider client={createApi(wrapped)}>
      <MeProvider load={async () => ME}>
        <SessionPage workspaceId={workspace.id} />
      </MeProvider>
    </ApiProvider>,
  );
  return { api, control, submitted, ...utils };
}

describe("session page — starting state (V3.27)", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    setCsrfToken(CSRF_TOKEN_VALUE);
  });

  it("shows the step panel while starting and launches once on connectable", async () => {
    const ws = startingWorkspace();
    const { api, submitted } = setup(ws);

    await screen.findByRole("region", { name: /Starting/ });
    expect(await screen.findByText("Preparing the machine")).toBeInTheDocument();

    api.state.workspaces.set(ws.id, readyWorkspace({ id: ws.id, name: ws.name }));
    await waitFor(() => expect(submitted).toHaveLength(1), { timeout: 20_000 });
  }, 25_000);

  it("stops polling the workspace once the launch path takes over", async () => {
    const ws = startingWorkspace();
    const { api, control, submitted } = setup(ws);

    await screen.findByRole("region", { name: /Starting/ });
    api.state.workspaces.set(ws.id, readyWorkspace({ id: ws.id, name: ws.name }));
    await waitFor(() => expect(submitted).toHaveLength(1), { timeout: 20_000 });
    const atConnect = control.workspaceGets;
    await new Promise((r) => setTimeout(r, 4_000));
    expect(control.workspaceGets).toBe(atConnect);
  }, 25_000);

  it("keeps polling through the Ready→ConnectionReady gap, then launches once (R-V3b M1)", async () => {
    // The phase flipped to Ready but the stream endpoint has not registered:
    // still "starting", still polling — not a static not-ready dead end.
    const ws = readyWorkspace({
      updatedAt: new Date().toISOString(),
      conditions: [
        cond("Admitted", "True", "QuotaReserved"),
        cond("StorageReady", "True", "VolumeBound"),
        cond("RuntimeReady", "True", "Ready"),
        cond("ConnectionReady", "False", "StreamDown"),
      ],
    });
    const { api, control, submitted } = setup(ws);

    await screen.findByRole("region", { name: /Starting|Creating/ });
    await screen.findByText("Ready to connect");
    const gets = control.workspaceGets;
    await new Promise((r) => setTimeout(r, 3_000));
    expect(control.workspaceGets).toBeGreaterThan(gets);
    expect(submitted).toHaveLength(0);

    api.state.workspaces.set(ws.id, readyWorkspace({ id: ws.id, name: ws.name }));
    await waitFor(() => expect(submitted).toHaveLength(1), { timeout: 20_000 });
  }, 25_000);

  it("offers Start on the stopped overlay and moves to starting", async () => {
    const ws = makeWorkspace({ phase: "Stopped", desiredState: "Stopped" });
    const { api } = setup(ws);

    const start = await screen.findByRole("button", { name: "Start" });
    fireEvent.click(start);
    await waitFor(() => {
      expect(api.state.workspaces.get(ws.id)!.desiredState).toBe("Running");
    });
    expect(
      await screen.findByRole("region", { name: /Starting/ }, { timeout: 20_000 }),
    ).toBeInTheDocument();
  }, 25_000);

  it("a 401 during the starting poll lands on signed-out, not on the error overlay", async () => {
    const ws = startingWorkspace();
    const { control } = setup(ws);
    await screen.findByRole("region", { name: /Starting/ });

    control.workspace401 = true;
    expect(await screen.findByText("Signed out", {}, { timeout: 20_000 })).toBeInTheDocument();
  }, 25_000);
});
