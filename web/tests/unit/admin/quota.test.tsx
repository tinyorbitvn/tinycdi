import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin, seedTenant, control } from "./helpers";
import { QuotaPage } from "../../../src/admin/QuotaPage";
import { OverviewPage } from "../../../src/admin/OverviewPage";
import { adminArea } from "../../mock-api/admin.ts";
import { dataArea } from "../../mock-api/data.ts";

function setup() {
  // dataArea backs the overview's retained-data card (GET /v1/data).
  const api = createMockApi({ areas: [adminArea, dataArea] });
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

describe("quota: per-user section", () => {
  it("has no per-user usage table: per-user rows come from the paged user-limits collection", async () => {
    const api = setup();
    seedTenant(api);
    renderWithApi(<QuotaPage />, api);

    await screen.findByRole("heading", { name: "Quota" });
    const table = await screen.findByRole("table", { name: "Per-user running limits" });
    await within(table).findByText("Grace Hopper");
    expect(screen.queryByRole("table", { name: "Usage by user" })).not.toBeInTheDocument();
  });
});

describe("quota: admin edit", () => {
  it("shows the source and saves new limits through the admin API", async () => {
    const api = setup();
    renderWithApi(<QuotaPage />, api);

    await screen.findByRole("heading", { name: "Quota" });
    await screen.findByText("Admin API");

    fireEvent.click(screen.getByRole("button", { name: "Edit limits" }));
    const dialog = await screen.findByRole("dialog", { name: "Set tenant limits" });
    fireEvent.change(within(dialog).getByLabelText(/Running workspaces/), {
      target: { value: "12" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save limits" }));

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    const meter = (
      await screen.findByText("Running workspaces", { selector: ".tc-meter__label" })
    ).closest(".tc-meter")!;
    expect(meter).toHaveTextContent(/of 12/);
  });

  it("a config-managed tenant shows the platform source and no edit action", async () => {
    const api = setup();
    control(api, "/_control/admin/quota", { managed: true });
    renderWithApi(<QuotaPage />, api);

    await screen.findByRole("heading", { name: "Quota" });
    await screen.findByText("Platform configuration");
    await screen.findByText(/can only change there/);
    expect(screen.queryByRole("button", { name: "Edit limits" })).not.toBeInTheDocument();
  });

  it("a 409 QUOTA_MANAGED_BY_CONFIG inside the dialog shows guidance", async () => {
    const api = setup();
    renderWithApi(<QuotaPage />, api);
    await screen.findByRole("heading", { name: "Quota" });

    fireEvent.click(screen.getByRole("button", { name: "Edit limits" }));
    const dialog = await screen.findByRole("dialog", { name: "Set tenant limits" });
    // The row becomes config-managed between open and save (e.g. a deploy).
    control(api, "/_control/admin/quota", { managed: true });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save limits" }));

    await within(dialog).findByText(/managed by the platform configuration/);
  });
});

describe("quota: optimistic concurrency", () => {
  it("reopening the dialog shows freshly read values", async () => {
    const api = setup();
    renderWithApi(<QuotaPage />, api);
    await screen.findByRole("heading", { name: "Quota" });

    fireEvent.click(screen.getByRole("button", { name: "Edit limits" }));
    let dialog = await screen.findByRole("dialog", { name: "Set tenant limits" });
    expect(within(dialog).getByLabelText(/Running workspaces/)).toHaveValue(8);
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());

    // Another actor changed the limits while the dialog was closed.
    control(api, "/_control/admin/quota", { limits: { runningWorkspaces: 20 } });
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByText(/of 20/);
    fireEvent.click(screen.getByRole("button", { name: "Edit limits" }));
    dialog = await screen.findByRole("dialog", { name: "Set tenant limits" });
    expect(within(dialog).getByLabelText(/Running workspaces/)).toHaveValue(20);
  });

  it("a stale save answers 412: the dialog warns and reloads fresh values", async () => {
    const api = setup();
    renderWithApi(<QuotaPage />, api);
    await screen.findByRole("heading", { name: "Quota" });

    fireEvent.click(screen.getByRole("button", { name: "Edit limits" }));
    const dialog = await screen.findByRole("dialog", { name: "Set tenant limits" });
    // A concurrent write lands between our read and our save.
    control(api, "/_control/admin/quota", { limits: { runningWorkspaces: 20 } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save limits" }));

    await within(dialog).findByText(/changed these limits/);
    // The reload updated the read model — the dialog now shows fresh values.
    await waitFor(() =>
      expect(within(dialog).getByLabelText(/Running workspaces/)).toHaveValue(20),
    );

    // Saving again carries the fresh version and lands.
    fireEvent.click(within(dialog).getByRole("button", { name: "Save limits" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    const meter = (
      await screen.findByText("Running workspaces", { selector: ".tc-meter__label" })
    ).closest(".tc-meter")!;
    expect(meter).toHaveTextContent(/of 20/);
  });
});
