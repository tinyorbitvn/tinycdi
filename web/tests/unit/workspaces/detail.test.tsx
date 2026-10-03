import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
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

  it("detail: events with the same reason and timestamp are keyed by their id", async () => {
    const api = createMockApi();
    seedReady(api);
    seedEvents(api, [
      { id: "Ready:RuntimeReady", type: "Normal", reason: "Retry", message: "first attempt", lastTimestamp: "2026-09-30T10:00:00Z" },
      { id: "Ready:RuntimeReadyAgain", type: "Normal", reason: "Retry", message: "second attempt", lastTimestamp: "2026-09-30T10:00:00Z" },
    ]);
    loginCookies();
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

      const table = await screen.findByRole("table", { name: "Events" });
      expect(within(table).getByText("first attempt")).toBeInTheDocument();
      expect(within(table).getByText("second attempt")).toBeInTheDocument();
      const duplicateKey = errors.mock.calls.some((c) => String(c[0]).includes("same key"));
      expect(duplicateKey).toBe(false);
    } finally {
      errors.mockRestore();
    }
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

  it("detail: resolves the template through its family after a revision bump", async () => {
    const api = createMockApi();
    // The workspace pins a superseded revision (its object is gone from the
    // catalog); the current revision of the family has a different id.
    seedReady(api, {
      template: {
        id: "tpl_SUPERSEDED_REV",
        name: "linux-firefox-desktop",
        family: "linux-firefox-desktop",
        revision: 6,
        runtime: "LinuxContainer",
        experience: "Desktop",
      },
    });
    seedEvents(api, []);
    loginCookies();
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    await screen.findByRole("table", { name: "Events" });
    expect(await screen.findByText(/Internet only/)).toBeInTheDocument();
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
    expect(connect).toHaveAttribute("aria-disabled", "true");
    await screen.findByText(/no ConnectionReady|ConnectionReady=False/);
  }, 20000);

  it("detail: Connect is aria-disabled with 'Starting…' while provisioning (V3.27)", async () => {
    const api = createMockApi();
    api.state.workspaces.set(
      WS_ID,
      readyWorkspace({ id: WS_ID, phase: "Provisioning", desiredState: "Running" }),
    );
    loginCookies();
    window.history.pushState({}, "", `/workspaces/${WS_ID}`);
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    const connect = await screen.findByRole("button", { name: "Connect" });
    expect(connect).toHaveAttribute("aria-disabled", "true");
    expect(connect).toHaveAttribute("title", "Starting…");
    fireEvent.click(connect);
    expect(window.location.pathname).toBe(`/workspaces/${WS_ID}`);
  }, 20000);

  it("detail: Connect is aria-disabled with 'Failed' on a failed workspace (V3.27)", async () => {
    const api = createMockApi();
    api.state.workspaces.set(
      WS_ID,
      readyWorkspace({ id: WS_ID, phase: "Failed", failureReason: "BootDeadlineExceeded" }),
    );
    loginCookies();
    window.history.pushState({}, "", `/workspaces/${WS_ID}`);
    renderWithApi(<WorkspaceDetailPage workspaceId={WS_ID} pollIntervalMs={60_000} />, api);

    const connect = await screen.findByRole("button", { name: "Connect" });
    expect(connect).toHaveAttribute("aria-disabled", "true");
    expect(connect).toHaveAttribute("title", "Failed");
    fireEvent.click(connect);
    expect(window.location.pathname).toBe(`/workspaces/${WS_ID}`);
  }, 20000);
});