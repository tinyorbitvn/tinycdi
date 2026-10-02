import { describe, expect, it } from "vitest";
import { screen, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin, seedTenant } from "./helpers";
import { WorkspacesAdminPage } from "../../../src/admin/WorkspacesAdminPage";
import { adminArea, tenancy, SEED_WORKSPACE_IDS } from "../../mock-api/admin.ts";
import { workspacesArea } from "../../mock-api/workspaces.ts";

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
  });
});
