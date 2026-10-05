import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin, seedTenant, control } from "./helpers";
import { QuotaPage } from "../../../src/admin/QuotaPage";
import { adminArea, TENANT } from "../../mock-api/admin.ts";

function setup() {
  const api = createMockApi({ areas: [adminArea] });
  loginCookies();
  makeAdmin(api);
  return api;
}

async function userLimitsTable() {
  return screen.findByRole("table", { name: "Per-user running limits" });
}

function defaultLine() {
  return screen.getByText("Default per-user limit:").closest("p")!;
}

function putRequests(api: ReturnType<typeof createMockApi>, suffix = "") {
  return api.state.requests.filter(
    (r) => r.method === "PUT" && r.path === `/v1/admin/tenants/${TENANT}/user-limits${suffix}`,
  );
}

describe("quota: per-user limits", () => {
  it("renders the tenant default and each user's override and effective limit", async () => {
    const api = setup();
    seedTenant(api); // Grace + Linus usage rows
    control(api, "/_control/admin/userlimits", {
      default: 2,
      overrides: { "https://idp.invalid|user-01J4ZDGRC": 5 },
    });
    renderWithApi(<QuotaPage />, api);

    await screen.findByRole("heading", { name: "Quota" });
    expect(await screen.findByText("Per-user limits")).toBeInTheDocument();
    await waitFor(() => expect(defaultLine()).toHaveTextContent("2"));

    const table = await userLimitsTable();
    const grace = (await within(table).findByText("Grace Hopper")).closest("tr")!;
    // Grace has a stored override of 5: override column shows it, effective resolves to it.
    expect(grace).toHaveTextContent("5");
    const linus = (await within(table).findByText("Linus Pauling")).closest("tr")!;
    // Linus inherits the tenant default.
    expect(within(linus).getByText("Default")).toBeInTheDocument();
    expect(within(linus).getByText("2")).toBeInTheDocument();
  });

  it("sets and clears a user's override through the dialog", async () => {
    const api = setup();
    seedTenant(api);
    renderWithApi(<QuotaPage />, api);

    const grace = () =>
      within(screen.getByRole("table", { name: "Per-user running limits" }))
        .getByText("Grace Hopper")
        .closest("tr")!;
    await userLimitsTable();
    fireEvent.click(within(grace()).getByRole("button", { name: "Set limit" }));

    const dialog = await screen.findByRole("dialog", { name: "Running limit — Grace Hopper" });
    fireEvent.change(within(dialog).getByLabelText("Running workspaces"), { target: { value: "4" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save limit" }));

    // The mock applied the write; the refreshed row shows override 4.
    await waitFor(() => expect(putRequests(api)).toHaveLength(1));
    expect(JSON.parse(putRequests(api)[0]!.rawBody)).toEqual({
      ownerRef: "https://idp.invalid|user-01J4ZDGRC",
      limit: 4,
    });
    await waitFor(() => expect(grace()).toHaveTextContent("4"));

    // Clearing writes limit:null and the row goes back to inherit.
    fireEvent.click(within(grace()).getByRole("button", { name: "Set limit" }));
    const dialog2 = await screen.findByRole("dialog", { name: "Running limit — Grace Hopper" });
    fireEvent.change(within(dialog2).getByLabelText("Running workspaces"), { target: { value: "" } });
    fireEvent.click(within(dialog2).getByRole("button", { name: "Save limit" }));
    await waitFor(() => expect(putRequests(api)).toHaveLength(2));
    expect(JSON.parse(putRequests(api)[1]!.rawBody)).toEqual({
      ownerRef: "https://idp.invalid|user-01J4ZDGRC",
      limit: null,
    });
    await waitFor(() => expect(within(grace()).getByText("Default")).toBeInTheDocument());
  });

  it("sets and clears the tenant default through the dialog", async () => {
    const api = setup();
    seedTenant(api);
    renderWithApi(<QuotaPage />, api);

    await userLimitsTable();
    fireEvent.click(screen.getByRole("button", { name: "Set default" }));
    const dialog = await screen.findByRole("dialog", { name: "Default per-user limit" });
    fireEvent.change(within(dialog).getByLabelText("Running workspaces"), { target: { value: "3" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save limit" }));

    await waitFor(() => expect(putRequests(api, "/default")).toHaveLength(1));
    expect(JSON.parse(putRequests(api, "/default")[0]!.rawBody)).toEqual({ limit: 3 });
    await waitFor(() => expect(defaultLine()).toHaveTextContent("3"));
  });
});
