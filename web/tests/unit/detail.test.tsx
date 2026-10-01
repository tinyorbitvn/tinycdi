import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import type { ReactElement } from "react";
import { WorkspaceDetailPage } from "../../src/workspaces/WorkspaceDetailPage";
import { MeProvider, type Me } from "../../src/app/me";
import { createMockApi, renderWithApi, loginCookies, CSRF_TOKEN_VALUE } from "./helpers";
import { readyWorkspace, READY_CONDITIONS } from "../mock-api/fixtures.ts";

// Connect needs the session domain /v1/me publishes (D9) — render the page
// under the principal provider like the app shell does.
const ME: Me = {
  subject: "user-01J4ZDADA",
  displayName: "Ada Lovelace",
  tenant: "acme",
  roles: ["user"],
  csrfToken: CSRF_TOKEN_VALUE,
  sessionDomain: "session.example.com",
};

function renderPage(ui: ReactElement, api: ReturnType<typeof createMockApi>) {
  return renderWithApi(<MeProvider load={async () => ME}>{ui}</MeProvider>, api);
}

describe("WorkspaceDetailPage readiness", () => {
  it("does not offer Connect when phase=Ready but ConnectionReady is False", async () => {
    const ws = readyWorkspace({
      conditions: READY_CONDITIONS.map((c) =>
        c.type === "ConnectionReady"
          ? { ...c, status: "False", reason: "StreamEndpointBooting" }
          : c,
      ),
    });
    const api = createMockApi();
    api.state.workspaces.set(ws.id, ws);
    loginCookies();
    renderPage(<WorkspaceDetailPage workspaceId={ws.id} pollIntervalMs={60000} />, api);

    await screen.findByRole("heading", { name: /research-desktop/ });
    const connect = screen.getByRole("button", { name: "Connect" });
    expect(connect).toBeDisabled();
    expect(screen.getByLabelText("connect status")).toHaveTextContent(
      "ConnectionReady=False",
    );
    // The conditions table shows the real state — phase alone is a summary.
    expect(screen.getByRole("table", { name: "conditions" })).toHaveTextContent(
      "StreamEndpointBooting",
    );
  });

  it("enables Connect only when Ready AND ConnectionReady=True", async () => {
    const ws = readyWorkspace();
    const api = createMockApi();
    api.state.workspaces.set(ws.id, ws);
    loginCookies();
    renderPage(<WorkspaceDetailPage workspaceId={ws.id} pollIntervalMs={60000} />, api);

    await screen.findByRole("heading", { name: /research-desktop/ });
    expect(screen.getByRole("button", { name: "Connect" })).toBeEnabled();
  });
});
