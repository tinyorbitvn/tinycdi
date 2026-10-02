import { describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { WorkspaceListPage } from "../../../src/workspaces/WorkspaceListPage";
import { PhasePill } from "../../../src/workspaces/StatusBits";
import { createMockApi, renderWithApi, loginCookies } from "../helpers";
import { makeWorkspace, readyWorkspace } from "../../mock-api/fixtures.ts";
import type { WorkspacePhase } from "../../../src/workspaces/helpers";

const ALL_PHASES: WorkspacePhase[] = [
  "Pending",
  "Provisioning",
  "Ready",
  "Stopping",
  "Stopped",
  "Failed",
  "Terminating",
];

describe("WorkspaceListPage", () => {
  it("list: empty state shows the catalog call to action", async () => {
    const api = createMockApi();
    api.state.workspaces.clear();
    loginCookies();
    renderWithApi(<WorkspaceListPage pollIntervalMs={60_000} />, api);

    // Orbit empty state: what happened, then the next step — the CTA routes
    // to the template catalog.
    const cta = await screen.findByRole("link", { name: "Browse the template catalog" });
    expect(cta).toHaveAttribute("href", "/templates");
    expect(screen.getByText("No workspaces yet.")).toBeVisible();
  }, 20000);

  it("list: each phase maps to a StatusPill with a text label", async () => {
    const api = createMockApi();
    api.state.workspaces.clear();
    for (const [i, phase] of ALL_PHASES.entries()) {
      const ws = makeWorkspace({
        id: `ws_phase_${i}`,
        name: `ws-${phase.toLowerCase()}`,
        phase,
        desiredState: phase === "Stopped" || phase === "Terminating" ? "Stopped" : "Running",
            });
      api.state.workspaces.set(ws.id, ws);
    }
    loginCookies();
    renderWithApi(<WorkspaceListPage pollIntervalMs={60_000} />, api);

    const table = await screen.findByRole("table", { name: "workspaces" });
    await waitFor(() => expect(within(table).getAllByRole("row").length).toBe(ALL_PHASES.length + 1));

    // Every phase renders a pill whose text label is announced, never colour
    // alone — the pill span carries both the label text and data-phase.
    for (const phase of ALL_PHASES) {
      const cell = within(table).getByText((_, el) => (el as HTMLElement | null)?.dataset.phase === phase);
      expect(cell.textContent).toBeTruthy();
    }
    // Spot-check the relabelled phases carry their friendly text.
    expect(within(table).getByText("Starting")).toBeInTheDocument();
    expect(within(table).getByText("Deleting")).toBeInTheDocument();
    expect(within(table).getByText("Ready")).toBeInTheDocument();
  }, 20000);

  it("list: renders name, template and data policy", async () => {
    const api = createMockApi();
    api.state.workspaces.clear();
    api.state.workspaces.set("ws_one", readyWorkspace({ id: "ws_one", name: "dev-box" }));
    loginCookies();
    renderWithApi(<WorkspaceListPage pollIntervalMs={60_000} />, api);

    const table = await screen.findByRole("table", { name: "workspaces" });
    expect(within(table).getByRole("link", { name: "dev-box" })).toHaveAttribute(
      "href",
      "/workspaces/ws_one",
    );
    expect(within(table).getByText("linux-firefox-desktop@7")).toBeInTheDocument();
    expect(within(table).getByText("Retain")).toBeInTheDocument();
  }, 20000);
});

describe("PhasePill", () => {
  it("has a visible text label for every phase", () => {
    const api = createMockApi();
    loginCookies();
    for (const phase of ALL_PHASES) {
      const { unmount } = renderWithApi(<PhasePill phase={phase} />, api);
      const pill = screen.getByText((_, el) => (el as HTMLElement | null)?.dataset.phase === phase);
      expect(pill.textContent!.length).toBeGreaterThan(0);
      unmount();
    }
  }, 20000);
});