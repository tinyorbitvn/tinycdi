import { describe, expect, it } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin, seedTenant } from "./helpers";
import { adminDataRows, DataAdminPage } from "../../../src/admin/DataAdminPage";
import { adminArea, tenancy, SEED_DATA_IDS, SEED_WORKSPACE_IDS } from "../../mock-api/admin.ts";
import { dataArea } from "../../mock-api/data.ts";
import { workspacesArea } from "../../mock-api/workspaces.ts";
import type { ScopedRetainedData } from "../../../src/admin/api";

function rec(overrides: Partial<ScopedRetainedData> = {}): ScopedRetainedData {
  return {
    id: "rd_x",
    state: "Retained",
    owner: { subject: "u1", displayName: "User One" },
    sizeGib: 10,
    runtime: "LinuxContainer",
    sourceWorkspaceName: "src",
    retainedAt: "2026-09-30T10:00:00Z",
    purgeConfirmationNonce: "n",
    ...overrides,
  } as ScopedRetainedData;
}

describe("adminDataRows", () => {
  it("drops Purged records and orders newest-retained first", () => {
    const rows = adminDataRows([
      rec({ id: "old", retainedAt: "2026-09-01T00:00:00Z" }),
      rec({ id: "gone", state: "Purged", retainedAt: "2026-10-01T00:00:00Z" }),
      rec({ id: "new", retainedAt: "2026-09-29T00:00:00Z" }),
      rec({ id: "mid", retainedAt: "2026-09-15T00:00:00Z" }),
    ]);
    expect(rows.map((r) => r.id)).toEqual(["new", "mid", "old"]);
  });
});

describe("DataAdminPage", () => {
  function setup() {
    const api = createMockApi({ areas: [adminArea, workspacesArea, dataArea] });
    loginCookies();
    makeAdmin(api);
    seedTenant(api);
    return api;
  }

  it("lists tenant-scope retained disks with owner, state and attach target", async () => {
    const api = setup();
    renderWithApi(
      <DataAdminPage now={Date.parse("2026-10-02T12:00:00Z")} pollIntervalMs={60_000} />,
      api,
    );

    await screen.findByRole("heading", { name: "Retained data" });
    const table = await screen.findByRole("table", { name: "Retained data across the tenant" });
    expect(tenancy(api.ctx).scopesSeen).toContain("tenant");

    // Grace's free disk: Retained, purgeable.
    const graceRow = await within(table).findByRole("row", {
      name: new RegExp(SEED_DATA_IDS.grace),
    });
    expect(graceRow).toHaveTextContent("Grace Hopper");
    expect(graceRow).toHaveTextContent("Retained");
    expect(graceRow).toHaveTextContent("grace-old-lab");
    expect(
      within(graceRow).getByRole("button", { name: `Purge ${SEED_DATA_IDS.grace}` }),
    ).toBeInTheDocument();

    // Linus's disk is Attached — no Purge, and the consumer links out.
    const linusRow = within(table).getByRole("row", { name: new RegExp(SEED_DATA_IDS.linus) });
    expect(linusRow).toHaveTextContent("Attached");
    expect(
      within(linusRow).queryByRole("button", { name: `Purge ${SEED_DATA_IDS.linus}` }),
    ).not.toBeInTheDocument();
    const consumer = within(linusRow).getByRole("link", {
      name: SEED_WORKSPACE_IDS.linusStopped,
    });
    expect(consumer).toHaveAttribute("href", `/workspaces/${SEED_WORKSPACE_IDS.linusStopped}`);
  }, 20000);

  it("purges another owner's disk through the nonce + typed-confirm dialog", async () => {
    const api = setup();
    renderWithApi(
      <DataAdminPage now={Date.parse("2026-10-02T12:00:00Z")} pollIntervalMs={60_000} />,
      api,
    );
    const table = await screen.findByRole("table", { name: "Retained data across the tenant" });
    const graceRow = await within(table).findByRole("row", {
      name: new RegExp(SEED_DATA_IDS.grace),
    });
    fireEvent.click(
      within(graceRow).getByRole("button", { name: `Purge ${SEED_DATA_IDS.grace}` }),
    );

    const dialog = await screen.findByRole("alertdialog");
    const confirm = within(dialog).getByRole("button", { name: "Purge permanently" });
    expect(confirm).toBeDisabled();
    // The dialog re-read the record for a fresh single-use nonce.
    fireEvent.change(within(dialog).getByLabelText(/Confirmation/), {
      target: { value: SEED_DATA_IDS.grace },
    });
    expect(confirm).toBeEnabled();
    fireEvent.click(confirm);

    await screen.findByText("Purge scheduled.");
    expect(api.state.retained.get(SEED_DATA_IDS.grace)?.state).toBe("Purging");
  }, 20000);

  it("renders the empty state when the tenant has no retained disks", async () => {
    const api = createMockApi({ areas: [adminArea, workspacesArea, dataArea] });
    loginCookies();
    makeAdmin(api);
    api.state.retained.clear();
    renderWithApi(
      <DataAdminPage now={Date.parse("2026-10-02T12:00:00Z")} pollIntervalMs={60_000} />,
      api,
    );
    await screen.findByRole("heading", { name: "Retained data" });
    expect(await screen.findByText("No retained disks")).toBeInTheDocument();
  }, 20000);
});
