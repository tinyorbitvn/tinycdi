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

const GRACE_REF = "https://idp.invalid|user-01J4ZDGRC";
const GRACE_PATH = `/v1/admin/tenants/${TENANT}/user-limits/${encodeURIComponent(GRACE_REF)}`;

function writes(api: ReturnType<typeof createMockApi>, method: string, path: string) {
  return api.state.requests.filter((r) => r.method === method && r.path === path);
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

    // No override row yet: the PUT creates it with If-Match: *.
    await waitFor(() => expect(writes(api, "PUT", GRACE_PATH)).toHaveLength(1));
    const put = writes(api, "PUT", GRACE_PATH)[0]!;
    expect(JSON.parse(put.rawBody)).toEqual({ limit: 4 });
    expect(put.headers["if-match"]).toBe("*");
    await waitFor(() => expect(grace()).toHaveTextContent("4"));

    // Clearing deletes the override under the row's ETag; the row goes
    // back to inherit.
    fireEvent.click(within(grace()).getByRole("button", { name: "Set limit" }));
    const dialog2 = await screen.findByRole("dialog", { name: "Running limit — Grace Hopper" });
    fireEvent.change(within(dialog2).getByLabelText("Running workspaces"), { target: { value: "" } });
    fireEvent.click(within(dialog2).getByRole("button", { name: "Save limit" }));
    await waitFor(() => expect(writes(api, "DELETE", GRACE_PATH)).toHaveLength(1));
    expect(writes(api, "DELETE", GRACE_PATH)[0]!.headers["if-match"]).toMatch(/^"ul-\d+"$/);
    await waitFor(() => expect(within(grace()).getByText("Default")).toBeInTheDocument());
  });

  it("sets and clears the tenant default through the dialog", async () => {
    const api = setup();
    seedTenant(api);
    renderWithApi(<QuotaPage />, api);

    await userLimitsTable();
    await waitFor(() => expect(defaultLine()).toHaveTextContent("Unlimited"));
    fireEvent.click(screen.getByRole("button", { name: "Set default" }));
    const dialog = await screen.findByRole("dialog", { name: "Default per-user limit" });
    fireEvent.change(within(dialog).getByLabelText("Running workspaces"), { target: { value: "3" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save limit" }));

    const DEFAULT_PATH = `/v1/admin/tenants/${TENANT}/user-limits/default`;
    await waitFor(() => expect(writes(api, "PUT", DEFAULT_PATH)).toHaveLength(1));
    const put = writes(api, "PUT", DEFAULT_PATH)[0]!;
    expect(JSON.parse(put.rawBody)).toEqual({ limit: 3 });
    // The If-Match is the ETag the default's GET returned.
    expect(put.headers["if-match"]).toBe('"uld-1"');
    await waitFor(() => expect(defaultLine()).toHaveTextContent("3"));
  });

  it("a stale default ETag answers 412: the dialog explains and shows the current value", async () => {
    const api = setup();
    renderWithApi(<QuotaPage />, api);
    await userLimitsTable();
    await waitFor(() => expect(defaultLine()).toHaveTextContent("Unlimited"));

    fireEvent.click(screen.getByRole("button", { name: "Set default" }));
    const dialog = await screen.findByRole("dialog", { name: "Default per-user limit" });
    // Another admin writes the default after this page read it.
    control(api, "/_control/admin/userlimits", { default: 6 });
    fireEvent.change(within(dialog).getByLabelText("Running workspaces"), { target: { value: "3" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save limit" }));

    await within(dialog).findByText(/Someone else changed these limits/);
    await waitFor(() => expect(within(dialog).getByLabelText("Running workspaces")).toHaveValue(6));
    await waitFor(() => expect(defaultLine()).toHaveTextContent("6"));
  });
});
