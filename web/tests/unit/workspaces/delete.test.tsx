import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { WorkspaceDetailPage } from "../../../src/workspaces/WorkspaceDetailPage";
import { createMockApi, renderWithApi, loginCookies } from "../helpers";
import { readyWorkspace } from "../../mock-api/fixtures.ts";

const WS_ID = "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E";

describe("WorkspaceDetailPage delete flow (V3.27)", () => {
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

  it("delete: stays through teardown, toasts + navigates once the API drops the row", async () => {
    const api = createMockApi();
    api.state.workspaces.set(WS_ID, readyWorkspace({ id: WS_ID, dataPolicy: "Retain" }));
    loginCookies();
    // jsdom has no router: park a sentinel path so a premature navigate("/")
    // would be visible.
    window.history.pushState({}, "", `/workspaces/${WS_ID}`);
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={80} />, api);

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

    // The 202 only started teardown: the page stays and shows the steps.
    await screen.findByRole("region", { name: "Deleting research-desktop" });
    expect(window.location.pathname).toBe(`/workspaces/${WS_ID}`);

    // Once the GET 404s (row hidden after finalize), the toast fires and
    // the page returns to the list; Retain links to the retained inventory.
    api.state.workspaces.delete(WS_ID);
    await waitFor(() => expect(window.location.pathname).toBe("/"), { timeout: 10_000 });
    expect(await screen.findByText("'research-desktop' was deleted.")).toBeInTheDocument();
    expect(
      await screen.findByRole("button", { name: "Your disk is in Retained data" }),
    ).toBeInTheDocument();
  }, 30000);

  it("delete: Ephemeral toast carries no retained-data link", async () => {
    const api = createMockApi();
    api.state.workspaces.set(
      WS_ID,
      readyWorkspace({ id: WS_ID, name: "ephemeral-box", dataPolicy: "Ephemeral" }),
    );
    loginCookies();
    window.history.pushState({}, "", `/workspaces/${WS_ID}`);
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={80} />, api);

    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    fireEvent.click(
      within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Confirm delete" }),
    );
    await screen.findByRole("region", { name: "Deleting ephemeral-box" });
    expect(window.location.pathname).toBe(`/workspaces/${WS_ID}`);

    api.state.workspaces.delete(WS_ID);
    await waitFor(() => expect(window.location.pathname).toBe("/"), { timeout: 10_000 });
    expect(await screen.findByText("'ephemeral-box' was deleted.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Your disk is in Retained data" })).toBeNull();
  }, 30000);
});
