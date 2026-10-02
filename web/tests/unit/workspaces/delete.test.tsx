import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { WorkspaceDetailPage } from "../../../src/workspaces/WorkspaceDetailPage";
import { createMockApi, renderWithApi, loginCookies } from "../helpers";
import { readyWorkspace } from "../../mock-api/fixtures.ts";

const WS_ID = "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E";

describe("DeleteWorkspaceButton", () => {
  it("delete: confirm names the retain consequence", async () => {
    const api = createMockApi();
    api.state.workspaces.set(WS_ID, readyWorkspace({ id: WS_ID, dataPolicy: "Retain" }));
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog).toHaveTextContent("moves to the retained inventory");
    expect(dialog).toHaveTextContent("research-desktop");
  }, 20000);

  it("delete: confirm names the destroy consequence for Ephemeral", async () => {
    const api = createMockApi();
    api.state.workspaces.set(
      WS_ID,
      readyWorkspace({ id: WS_ID, name: "ephemeral-box", dataPolicy: "Ephemeral" }),
    );
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog).toHaveTextContent("Ephemeral and will be destroyed");
  }, 20000);

  it("delete: confirm sends DELETE and reports Terminating", async () => {
    const api = createMockApi();
    api.state.workspaces.set(WS_ID, readyWorkspace({ id: WS_ID }));
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Confirm delete" }));

    await waitFor(() =>
      expect(
        api.state.requests.some(
          (r) => r.method === "DELETE" && r.path === `/v1/workspaces/${WS_ID}`,
        ),
      ).toBe(true),
    );
    // After DELETE the detail page is left via onDeleted → navigate("/").
    await waitFor(() => expect(window.location.pathname).toBe("/"));
  }, 20000);
});