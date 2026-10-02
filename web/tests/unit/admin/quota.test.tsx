import { describe, expect, it } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin, seedTenant, control } from "./helpers";
import { QuotaPage } from "../../../src/admin/QuotaPage";
import { OverviewPage } from "../../../src/admin/OverviewPage";
import { adminArea } from "../../mock-api/admin.ts";

function setup() {
  const api = createMockApi({ areas: [adminArea] });
  loginCookies();
  makeAdmin(api);
  return api;
}

describe("quota: meters", () => {
  it("shows usage against limits for all five quota fields with numbers", async () => {
    const api = setup();
    control(api, "/_control/admin/quota", {
      limits: {
        workspaces: 20,
        runningWorkspaces: 8,
        cpuMillicores: 32000,
        memoryMib: 65536,
        storageGib: 500,
      },
    });
    renderWithApi(<QuotaPage />, api);

    await screen.findByRole("heading", { name: "Quota" });
    for (const label of ["Workspaces", "Running workspaces", "CPU", "Memory", "Storage"]) {
      const meterLabel = await screen.findByText(label, { selector: ".tc-meter__label" });
      const meter = meterLabel.closest(".tc-meter")!;
      // Each meter reports "X of Y (Z%)" — real numbers, not just a bar.
      expect(meter).toHaveTextContent(/of/);
      expect(meter).toHaveTextContent(/\d+/);
      expect(meter).toHaveTextContent(/%/);
    }
  });
});

const NOT_CONFIGURED =
  "No quota configured. New workspaces are refused until a tenant quota is set.";

describe("quota: not configured", () => {
  it("a tenant with no quota row says so and never reads as 'No limit'", async () => {
    const api = setup();
    control(api, "/_control/admin/quota", { limits: null });
    renderWithApi(<QuotaPage />, api);

    await screen.findByRole("heading", { name: "Quota" });
    expect(await screen.findByText(NOT_CONFIGURED)).toBeInTheDocument();
    expect(screen.queryByText(/No limit/)).not.toBeInTheDocument();
    expect(document.querySelector(".tc-meter")).toBeNull();
  });

  it("a workspace limit of 0 means no count limit; other limits still show their numbers", async () => {
    const api = setup();
    control(api, "/_control/admin/quota", { limits: { workspaces: 0, runningWorkspaces: 8 } });
    renderWithApi(<QuotaPage />, api);

    await screen.findByRole("heading", { name: "Quota" });
    const workspaces = (await screen.findByText("Workspaces", { selector: ".tc-meter__label" })).closest(
      ".tc-meter",
    )!;
    expect(workspaces).toHaveTextContent("No limit");
    expect(workspaces).not.toHaveTextContent(/of 0/);
    expect(workspaces).not.toHaveTextContent(/100%/);

    const running = screen.getByText("Running workspaces", { selector: ".tc-meter__label" }).closest(".tc-meter")!;
    expect(running).toHaveTextContent(/of 8 \(\d+%\)/);
  });
});

describe("quota: overview without a quota", () => {
  it("says no quota is configured instead of showing 'No limit' meters", async () => {
    const api = setup();
    control(api, "/_control/admin/quota", { limits: null });
    renderWithApi(<OverviewPage />, api);

    expect(await screen.findByText(NOT_CONFIGURED)).toBeInTheDocument();
    expect(screen.queryByText(/No limit/)).not.toBeInTheDocument();
    expect(document.querySelector(".tc-meter")).toBeNull();
  });

  it("'No limit' stays for a configured quota with workspaces: 0", async () => {
    const api = setup();
    control(api, "/_control/admin/quota", { limits: { workspaces: 0 } });
    renderWithApi(<OverviewPage />, api);

    const meterLabel = await screen.findByText("Workspaces", { selector: ".tc-meter__label" });
    expect(meterLabel.closest(".tc-meter")).toHaveTextContent("No limit");
    expect(screen.queryByText(NOT_CONFIGURED)).not.toBeInTheDocument();
  });
});

describe("quota: per-user table", () => {
  it("renders one row per UserUsage and sorts by every column", async () => {
    const api = setup();
    seedTenant(api); // adds Grace Hopper's and Linus Pauling's workspaces/disks
    renderWithApi(<QuotaPage />, api);

    await screen.findByRole("heading", { name: "Quota" });
    const table = await screen.findByRole("table", { name: "Usage by user" });
    const rowHeaders = () => within(table).getAllByRole("rowheader");
    await within(table).findByText("Grace Hopper");
    // Three principals: Ada (self), Grace, Linus.
    expect(rowHeaders()).toHaveLength(3);
    const firstUser = () => rowHeaders()[0].textContent;
    const lastUser = () => rowHeaders()[rowHeaders().length - 1].textContent;

    // Sort by name ascending.
    fireEvent.click(within(table).getByRole("button", { name: "User" }));
    expect(firstUser()).toContain("Ada Lovelace");
    expect(lastUser()).toContain("Linus Pauling");

    // Grace holds the most workspaces, running runtimes and storage, so
    // descending sort puts her first under every numeric column.
    for (const label of ["Workspaces", "Running workspaces", "CPU", "Memory", "Storage"]) {
      fireEvent.click(within(table).getByRole("button", { name: label }));
      expect(firstUser(), `sorting by ${label}`).toContain("Grace Hopper");
    }

    // Clicking the active column again flips the direction.
    fireEvent.click(within(table).getByRole("button", { name: "Storage" }));
    expect(lastUser()).toContain("Grace Hopper");
  });
});
