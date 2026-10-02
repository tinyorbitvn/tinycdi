import { describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { WorkspaceDetailPage } from "../../../src/workspaces/WorkspaceDetailPage";
import { createMockApi, renderWithApi, loginCookies } from "../helpers";
import { readyWorkspace } from "../../mock-api/fixtures.ts";
import type { WorkspaceEventFixture } from "../../mock-api/fixtures.ts";

const WS_ID = "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E";

function seedReady(api: ReturnType<typeof createMockApi>, patch: object = {}) {
  const ws = readyWorkspace({ id: WS_ID, ...patch });
  api.state.workspaces.set(WS_ID, ws);
  return ws;
}

function seedEvents(api: ReturnType<typeof createMockApi>, events: WorkspaceEventFixture[]) {
  api.state.events.set(WS_ID, events);
}

describe("WorkspaceDetailPage", () => {
  it("detail: renders events newest first, in API order", async () => {
    const api = createMockApi();
    seedReady(api);
    seedEvents(api, [
      { type: "Normal", reason: "RuntimeReady", message: "second event", lastTimestamp: "2026-09-30T10:02:00Z" },
      { type: "Warning", reason: "BackOff", message: "first event", lastTimestamp: "2026-09-30T10:01:00Z" },
      { type: "Normal", reason: "Admitted", message: "third event", lastTimestamp: "2026-09-30T10:00:00Z" },
    ]);
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    const table = await screen.findByRole("table", { name: "Events" });
    const rows = within(table).getAllByRole("row").slice(1); // skip header
    const reasons = rows.map((r) => within(r).getAllByRole("rowheader")[0]!.textContent);
    expect(reasons).toEqual(["RuntimeReady", "BackOff", "Admitted"]);
    expect(within(table).getByText("Warning")).toBeInTheDocument();
  }, 20000);

  it("detail: stale image badge shows with its explanation", async () => {
    const api = createMockApi();
    seedReady(api, { imageStale: true, imageBuiltAt: "2026-09-10T00:00:00Z" });
    seedEvents(api, []);
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    const alert = await screen.findByRole("alert");
    await waitFor(() => expect(alert).toHaveTextContent("Stale image"));
    expect(alert).toHaveTextContent("older than the freshness window");
  }, 20000);

  it("detail: no stale badge when imageStale is absent", async () => {
    const api = createMockApi();
    seedReady(api);
    seedEvents(api, []);
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    await screen.findByRole("table", { name: "Events" });
    expect(screen.queryByText("Stale image")).not.toBeInTheDocument();
  }, 20000);

  it("detail: shows owner, data policy and network profile", async () => {
    const api = createMockApi();
    seedReady(api);
    seedEvents(api, []);
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    await screen.findByRole("table", { name: "Events" });
    // Owner comes from the principal directory (mock principal: Ada Lovelace).
    expect(screen.getByText(/Ada Lovelace/)).toBeInTheDocument();
    // Data policy: label + plain-language consequence.
    expect(screen.getByText(/Retain —/)).toBeInTheDocument();
    // Network profile resolved through the workspace's template.
    expect(screen.getByText(/Internet only/)).toBeInTheDocument();
  }, 20000);

  it("detail: connect button routes to the in-portal session route", async () => {
    const api = createMockApi();
    seedReady(api);
    seedEvents(api, []);
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    const connect = await screen.findByRole("link", { name: "Connect" });
    expect(connect).toHaveAttribute("href", `/workspaces/${WS_ID}/session`);
  }, 20000);

  it("detail: connect is disabled with a reason while ConnectionReady is false", async () => {
    const api = createMockApi();
    const ws = seedReady(api);
    ws.conditions = ws.conditions.map((c) =>
      c.type === "ConnectionReady" ? { ...c, status: "False" as const, reason: "StreamDown" } : c,
    );
    seedEvents(api, []);
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    const connect = await screen.findByRole("button", { name: "Connect" });
    expect(connect).toBeDisabled();
    await screen.findByText(/no ConnectionReady|ConnectionReady=False/);
  }, 20000);
});