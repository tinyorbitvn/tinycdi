import { describe, expect, it } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin, seedTenant } from "./helpers";
import {
  canConnect,
  canRestart,
  canStart,
  WorkspacesAdminPage,
} from "../../../src/admin/WorkspacesAdminPage";
import { adminArea, tenancy, SEED_WORKSPACE_IDS } from "../../mock-api/admin.ts";
import { workspacesArea } from "../../mock-api/workspaces.ts";
import { makeWorkspace, readyWorkspace } from "../../mock-api/fixtures.ts";
import type { ScopedWorkspace } from "../../../src/admin/api";

describe("workspaces: tenant scope", () => {
  it("requests scope=tenant and shows the owner column", async () => {
    // adminArea claims the scoped GET routes; workspacesArea seeds the
    // principal's own workspace and serves the mutations.
    const api = createMockApi({ areas: [adminArea, workspacesArea] });
    loginCookies();
    makeAdmin(api);
    seedTenant(api);
    renderWithApi(<WorkspacesAdminPage now={Date.parse("2026-10-02T12:00:00Z")} />, api);

    await screen.findByRole("heading", { name: "Tenant workspaces" });
    const table = await screen.findByRole("table", { name: "Tenant workspaces" });

    // The list was fetched with the tenant scope.
    expect(tenancy(api.ctx).scopesSeen).toContain("tenant");

    // Other users' workspaces are listed with their owner names.
    await within(table).findByText("grace-analysis");
    expect(within(table).getByRole("columnheader", { name: "Owner" })).toBeInTheDocument();
    expect(within(table).getAllByText("Grace Hopper").length).toBeGreaterThan(0);
    expect(within(table).getByText("Linus Pauling")).toBeInTheDocument();

    // And the principal's own workspace is in the same list.
    expect(within(table).getByText("research-desktop")).toBeInTheDocument();
    // Linked rows point at workspace detail.
    const link = within(table).getByRole("link", { name: "grace-analysis" });
    expect(link).toHaveAttribute("href", `/workspaces/${SEED_WORKSPACE_IDS.graceReady}`);
  }, 20000);
});

describe("workspaces: action gating", () => {
  const base = makeWorkspace() as ScopedWorkspace;

  it("canStart covers the two terminal phases", () => {
    expect(canStart({ ...base, phase: "Stopped" })).toBe(true);
    expect(canStart({ ...base, phase: "Failed" })).toBe(true);
    for (const phase of ["Pending", "Provisioning", "Ready", "Stopping", "Terminating"] as const) {
      expect(canStart({ ...base, phase }), phase).toBe(false);
    }
  });

  it("canRestart needs a runtime to bounce (running, not already terminal)", () => {
    expect(canRestart(readyWorkspace() as ScopedWorkspace)).toBe(true);
    expect(canRestart({ ...base, phase: "Provisioning", desiredState: "Running" })).toBe(true);
    expect(
      canRestart({ ...base, phase: "Failed", desiredState: "Running" }),
    ).toBe(false);
    expect(canRestart({ ...base, phase: "Stopped" })).toBe(false);
    expect(canRestart({ ...base, phase: "Stopping" })).toBe(false);
    expect(canRestart({ ...base, phase: "Terminating" })).toBe(false);
  });

  it("canConnect is owner-only and needs a connectable workspace", () => {
    const ready = readyWorkspace() as ScopedWorkspace;
    const owner = ready.owner.subject;
    expect(canConnect(ready, owner)).toBe(true);
    expect(canConnect(ready, "someone-else")).toBe(false);
    expect(canConnect(ready, undefined)).toBe(false);
    expect(canConnect(ready, "")).toBe(false);
    // Ready but ConnectionReady not yet True -> not connectable.
    const notReady = {
      ...ready,
      conditions: ready.conditions.filter((c) => c.type !== "ConnectionReady"),
    };
    expect(canConnect(notReady, owner)).toBe(false);
    expect(canConnect(makeWorkspace() as ScopedWorkspace, owner)).toBe(false);
  });
});

describe("workspaces: admin actions", () => {
  function setup() {
    const api = createMockApi({ areas: [adminArea, workspacesArea] });
    loginCookies();
    makeAdmin(api);
    seedTenant(api);
    return api;
  }

  it("two or fewer row actions render inline; more fold into the row menu", async () => {
    const api = setup();
    renderWithApi(<WorkspacesAdminPage now={Date.parse("2026-10-02T12:00:00Z")} />, api);
    const table = await screen.findByRole("table", { name: "Tenant workspaces" });

    // linus-desktop is Stopped and not ours: Start + Delete inline.
    const linusRow = within(table).getByRole("row", { name: /linus-desktop/ });
    expect(within(linusRow).getByRole("button", { name: "Start linus-desktop" })).toBeInTheDocument();
    expect(within(linusRow).getByRole("button", { name: "Delete linus-desktop" })).toBeInTheDocument();
    expect(
      within(linusRow).queryByRole("button", { name: "Actions for linus-desktop" }),
    ).not.toBeInTheDocument();

    // grace-analysis is Ready and not ours: Stop + Restart + Delete > 2,
    // so the row collapses to the IconDots menu — with no Connect item,
    // because the viewer is not the owner.
    const graceRow = within(table).getByRole("row", { name: /grace-analysis/ });
    expect(within(graceRow).queryByRole("button", { name: /^Stop/ })).not.toBeInTheDocument();
    fireEvent.click(within(graceRow).getByRole("button", { name: "Actions for grace-analysis" }));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getByRole("menuitem", { name: "Stop" })).toBeInTheDocument();
    expect(within(menu).getByRole("menuitem", { name: "Restart" })).toBeInTheDocument();
    expect(within(menu).getByRole("menuitem", { name: "Delete" })).toBeInTheDocument();
    expect(within(menu).queryByRole("menuitem", { name: "Connect" })).not.toBeInTheDocument();
  }, 20000);

  it("offers Connect in the row menu only for the viewer's own workspace", async () => {
    const api = setup();
    // A Ready workspace owned by the principal (owners map has no entry
    // -> the mock attributes it to the caller, like the real API does).
    api.state.workspaces.set(
      "ws_ownready000000001",
      readyWorkspace({ id: "ws_ownready000000001", name: "ada-desktop" }),
    );
    renderWithApi(<WorkspacesAdminPage now={Date.parse("2026-10-02T12:00:00Z")} />, api);
    const table = await screen.findByRole("table", { name: "Tenant workspaces" });

    const ownRow = within(table).getByRole("row", { name: /ada-desktop/ });
    fireEvent.click(within(ownRow).getByRole("button", { name: "Actions for ada-desktop" }));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getByRole("menuitem", { name: "Connect" })).toBeInTheDocument();
  }, 20000);

  it("starts another user's stopped workspace after the owner-context confirm", async () => {
    const api = setup();
    renderWithApi(<WorkspacesAdminPage now={Date.parse("2026-10-02T12:00:00Z")} />, api);
    const table = await screen.findByRole("table", { name: "Tenant workspaces" });

    const linusRow = within(table).getByRole("row", { name: /linus-desktop/ });
    fireEvent.click(within(linusRow).getByRole("button", { name: "Start linus-desktop" }));

    const dialog = await screen.findByRole("dialog", { name: "Start linus-desktop?" });
    // The advisor-required context: whose workspace this is, and whose
    // limits a start consumes.
    expect(dialog).toHaveTextContent("acting on Linus Pauling's workspace");
    expect(dialog).toHaveTextContent("running-workspace limit");
    fireEvent.click(within(dialog).getByRole("button", { name: "Start workspace" }));

    await screen.findByText("Start requested for linus-desktop (Linus Pauling).");
    expect(
      api.state.requests.some(
        (r) => r.method === "POST" && r.path === `/v1/workspaces/${SEED_WORKSPACE_IDS.linusStopped}/start`,
      ),
    ).toBe(true);
    expect(api.state.workspaces.get(SEED_WORKSPACE_IDS.linusStopped)?.desiredState).toBe("Running");
  }, 20000);

  it("stop and delete dialogs state who the workspace belongs to", async () => {
    const api = setup();
    renderWithApi(<WorkspacesAdminPage now={Date.parse("2026-10-02T12:00:00Z")} />, api);
    const table = await screen.findByRole("table", { name: "Tenant workspaces" });

    const graceRow = within(table).getByRole("row", { name: /grace-analysis/ });
    fireEvent.click(within(graceRow).getByRole("button", { name: "Actions for grace-analysis" }));
    fireEvent.click(within(await screen.findByRole("menu")).getByRole("menuitem", { name: "Delete" }));

    const dialog = await screen.findByRole("alertdialog", { name: "Delete grace-analysis?" });
    expect(dialog).toHaveTextContent("acting on Grace Hopper's workspace");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  }, 20000);
});
