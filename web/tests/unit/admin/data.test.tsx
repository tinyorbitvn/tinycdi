import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin, seedTenant } from "./helpers";
import { DataAdminPage } from "../../../src/admin/DataAdminPage";
import { adminArea, tenancy, SEED_DATA_IDS, SEED_WORKSPACE_IDS } from "../../mock-api/admin.ts";
import { dataArea } from "../../mock-api/data.ts";
import { workspacesArea } from "../../mock-api/workspaces.ts";

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

  it("pages on the server: newest first, header clicks re-query, Load more follows the cursor", async () => {
    const api = setup();
    // 60 more Retained disks: two server pages at the default limit (50).
    for (let i = 0; i < 60; i++) {
      const id = `rd_01J4ZDBULK${String(i).padStart(2, "0")}X`;
      api.state.retained.set(id, {
        id,
        state: "Retained",
        owner: { subject: "user-01J4ZDGRC", displayName: "Grace Hopper" },
        sizeGib: 1,
        runtime: "LinuxContainer",
        sourceWorkspaceName: `bulk-${String(i).padStart(2, "0")}`,
        retainedAt: `2026-08-${String((i % 28) + 1).padStart(2, "0")}T00:00:00Z`,
        purgeConfirmationNonce: "n",
      });
    }
    renderWithApi(
      <DataAdminPage now={Date.parse("2026-10-02T12:00:00Z")} pollIntervalMs={60_000} />,
      api,
    );
    const table = await screen.findByRole("table", { name: "Retained data across the tenant" });
    await within(table).findByRole("row", { name: new RegExp(SEED_DATA_IDS.grace) });
    const lists = () =>
      api.state.requests.filter((r) => r.method === "GET" && r.path === "/v1/data").map((r) => new URLSearchParams(r.query));
    expect(lists()[0].get("sort")).toBe("-retainedAt");
    expect(lists()[0].get("scope")).toBe("tenant");
    expect(within(table).getAllByRole("row")).toHaveLength(1 + 50);

    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    await waitFor(() => expect(within(table).getAllByRole("row").length).toBeGreaterThan(51));
    expect(lists().at(-1)?.get("cursor")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();

    fireEvent.click(within(table).getByRole("button", { name: /From workspace/ }));
    await waitFor(() => expect(lists().at(-1)?.get("sort")).toBe("name"));
    expect(lists().at(-1)?.get("cursor")).toBeNull();
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
