import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { control, makeAdmin, seedTenant } from "./helpers";
import {
  deriveUserRows,
  matchesUserFilter,
  UserDetailPage,
  UsersAdminPage,
} from "../../../src/admin/UsersAdminPage";
import {
  adminArea,
  tenancy,
  OTHER_USERS,
  SEED_DATA_IDS,
  SEED_WORKSPACE_IDS,
  TENANT,
} from "../../mock-api/admin.ts";
import { dataArea } from "../../mock-api/data.ts";
import { workspacesArea } from "../../mock-api/workspaces.ts";
import type {
  AdminUserLimitEntry,
  QuotaAmounts,
  ScopedRetainedData,
  UserUsage,
} from "../../../src/admin/api";

function usage(overrides: Partial<QuotaAmounts> = {}): QuotaAmounts {
  return {
    workspaces: 0,
    runningWorkspaces: 0,
    cpuMillicores: 0,
    memoryMib: 0,
    storageGib: 0,
    ...overrides,
  };
}

function qUser(overrides: Partial<UserUsage> = {}): UserUsage {
  return {
    subject: "u1",
    displayName: "User One",
    usage: usage(),
    ...overrides,
  } as UserUsage;
}

function lRow(overrides: Partial<AdminUserLimitEntry> = {}): AdminUserLimitEntry {
  return {
    ownerRef: "iss|u1",
    subject: "u1",
    displayName: "User One",
    limit: null,
    effective: null,
    running: 0,
    ...overrides,
  } as AdminUserLimitEntry;
}

function disk(overrides: Partial<ScopedRetainedData> = {}): ScopedRetainedData {
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

describe("deriveUserRows", () => {
  it("unions quota users and limits-only principals, sorted by display name", () => {
    const rows = deriveUserRows(
      [qUser({ subject: "u1", displayName: "Zed", usage: usage({ runningWorkspaces: 2 }) })],
      [lRow({ ownerRef: "iss|u2", subject: "u2", displayName: "Ann", running: 0 })],
      [],
    );
    expect(rows.map((r) => r.subject)).toEqual(["u2", "u1"]);
    // The limits-only principal reports a zeroed usage vector.
    const ann = rows[0]!;
    expect(ann.usage).toEqual(usage());
    expect(ann.running).toBe(0);
    expect(ann.limitsEntry?.ownerRef).toBe("iss|u2");
  });

  it("falls back to the subject when a display name is empty, from either source", () => {
    const rows = deriveUserRows(
      [qUser({ subject: "u1", displayName: "" })],
      [lRow({ ownerRef: "iss|u2", subject: "u2", displayName: "" })],
      [],
    );
    expect(rows.find((r) => r.subject === "u1")?.displayName).toBe("u1");
    expect(rows.find((r) => r.subject === "u2")?.displayName).toBe("u2");
  });

  it("resolves override and effective from the limits row, else the quota row", () => {
    const rows = deriveUserRows(
      [qUser({ subject: "u1", limit: 4 }), qUser({ subject: "u2", displayName: "Two" })],
      [lRow({ subject: "u1", limit: 7, effective: 7 })],
      [],
    );
    const u1 = rows.find((r) => r.subject === "u1")!;
    expect(u1.override).toBe(7);
    expect(u1.effective).toBe(7);
    // No limits row: override stays unknown, effective comes from the quota
    // user's own `limit` (absent = unlimited).
    const u2 = rows.find((r) => r.subject === "u2")!;
    expect(u2.override).toBeUndefined();
    expect(u2.limitsEntry).toBeUndefined();
    expect(u2.effective).toBeUndefined();
  });

  it("counts retained disks per owner and excludes Purged records", () => {
    const rows = deriveUserRows(
      [qUser({ subject: "u1" }), qUser({ subject: "u2", displayName: "Two" })],
      [],
      [
        disk({ owner: { subject: "u1", displayName: "User One" } }),
        disk({ id: "rd_y", owner: { subject: "u1", displayName: "User One" } }),
        disk({ id: "rd_z", state: "Purged", owner: { subject: "u1", displayName: "User One" } }),
        disk({ id: "rd_w", owner: { subject: "u2", displayName: "Two" } }),
      ],
    );
    expect(rows.find((r) => r.subject === "u1")?.retainedDisks).toBe(2);
    expect(rows.find((r) => r.subject === "u2")?.retainedDisks).toBe(1);
  });

  it("matches the filter on display name or subject, case-insensitively", () => {
    const [row] = deriveUserRows([qUser({ subject: "u1", displayName: "Grace Hopper" })], [], []);
    expect(matchesUserFilter(row!, "grace")).toBe(true);
    expect(matchesUserFilter(row!, "U1")).toBe(true);
    expect(matchesUserFilter(row!, "linus")).toBe(false);
    expect(matchesUserFilter(row!, "  ")).toBe(true);
  });
});

describe("UsersAdminPage", () => {
  function setup() {
    const api = createMockApi({ areas: [adminArea, workspacesArea, dataArea] });
    loginCookies();
    makeAdmin(api);
    seedTenant(api);
    return api;
  }

  async function usersTable() {
    return screen.findByRole("table", { name: "Tenant users" });
  }

  it("lists the union of quota and limits principals with usage, limits and disk counts", async () => {
    const api = setup();
    renderWithApi(<UsersAdminPage />, api);

    await screen.findByRole("heading", { name: "Users" });
    const table = await usersTable();
    for (const name of ["Ada Lovelace", "Grace Hopper", "Linus Pauling"]) {
      expect(within(table).getByText(name)).toBeInTheDocument();
    }
    // Every row links to its per-user detail page.
    expect(within(table).getByRole("link", { name: "Grace Hopper" })).toHaveAttribute(
      "href",
      `/admin/users/${OTHER_USERS[0]!.subject}`,
    );
    // Grace has one running-holding workspace pair and one retained disk.
    const grace = within(table).getByText("Grace Hopper").closest("tr")!;
    expect(within(grace).getAllByRole("cell").at(-1)).toHaveTextContent("1");
    // The page notes it is not a tenant directory.
    expect(screen.getByText(/not a full tenant directory/)).toBeInTheDocument();
  });

  it("filters rows by name or subject", async () => {
    const api = setup();
    renderWithApi(<UsersAdminPage />, api);
    const table = await usersTable();

    fireEvent.change(screen.getByLabelText("Filter"), { target: { value: "linus" } });
    expect(within(table).queryByText("Grace Hopper")).not.toBeInTheDocument();
    expect(within(table).getByText("Linus Pauling")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Filter"), { target: { value: "nobody" } });
    expect(screen.getByText("No users match the filter.")).toBeInTheDocument();
  });

  it("includes limits-only principals with a zeroed usage vector", async () => {
    const api = setup();
    control(api, "/_control/admin/userlimits", {
      overrides: { "https://idp.invalid|user-ghost": 2 },
    });
    renderWithApi(<UsersAdminPage />, api);

    const table = await usersTable();
    const ghost = (await within(table).findByRole("link", { name: "user-ghost" })).closest("tr")!;
    // No quota row: zero usage, but the override is visible and editable.
    expect(ghost).toHaveTextContent("0");
    expect(ghost).toHaveTextContent("2");
  });
});

describe("UserDetailPage", () => {
  const GRACE = OTHER_USERS[0]!.subject;
  const LINUS = OTHER_USERS[1]!.subject;

  function setup() {
    const api = createMockApi({ areas: [adminArea, workspacesArea, dataArea] });
    loginCookies();
    makeAdmin(api);
    seedTenant(api);
    return api;
  }

  it("shows only the user's workspaces and retained disks", async () => {
    const api = setup();
    renderWithApi(<UserDetailPage subject={GRACE} />, api);

    await screen.findByRole("heading", { name: "Grace Hopper" });
    // Detail reads the tenant scope — the same scope gate as the list pages.
    expect(tenancy(api.ctx).scopesSeen).toContain("tenant");

    const wsTable = await screen.findByRole("table", { name: "The user's workspaces" });
    expect(
      within(wsTable).getByRole("link", { name: "grace-analysis" }),
    ).toHaveAttribute("href", `/workspaces/${SEED_WORKSPACE_IDS.graceReady}`);
    expect(within(wsTable).getByText("grace-browser")).toBeInTheDocument();
    expect(within(wsTable).queryByText("linus-desktop")).not.toBeInTheDocument();

    const diskTable = await screen.findByRole("table", { name: "The user's retained disks" });
    expect(
      within(diskTable).getByRole("link", { name: SEED_DATA_IDS.grace }),
    ).toHaveAttribute("href", `/data/${SEED_DATA_IDS.grace}`);
    expect(within(diskTable).queryByText(SEED_DATA_IDS.linus)).not.toBeInTheDocument();
    // The section links through to the retained-data admin.
    expect(screen.getByRole("link", { name: "Open retained data" })).toHaveAttribute(
      "href",
      "/admin/data",
    );
  });

  it("edits the user's running limit through the shared dialog and reflects it", async () => {
    const api = setup();
    renderWithApi(<UserDetailPage subject={GRACE} />, api);
    await screen.findByRole("heading", { name: "Grace Hopper" });

    fireEvent.click(await screen.findByRole("button", { name: "Set limit" }));
    const dialog = await screen.findByRole("dialog", { name: "Running limit — Grace Hopper" });
    fireEvent.change(within(dialog).getByLabelText("Running workspaces"), {
      target: { value: "3" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save limit" }));

    await waitFor(() =>
      expect(
        api.state.requests.filter(
          (r) => r.method === "PUT" && r.path === `/v1/admin/tenants/${TENANT}/user-limits`,
        ),
      ).toHaveLength(1),
    );
    // The DescriptionList's Effective row re-renders from the reload.
    await waitFor(() =>
      expect(
        screen.getByText("Effective", { selector: "dt" }).closest("div"),
      ).toHaveTextContent("3"),
    );
  });

  it("renders a not-found state for an unknown subject", async () => {
    const api = setup();
    renderWithApi(<UserDetailPage subject="user-nobody" />, api);
    expect(await screen.findByText("User not found")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Back to users" })).toHaveAttribute(
      "href",
      "/admin/users",
    );
  });

  it("keeps Linus's limit untouched when editing Grace's", async () => {
    // Guards the per-row edit target: the dialog PUTs Grace's ownerRef.
    const api = setup();
    renderWithApi(<UserDetailPage subject={LINUS} />, api);
    await screen.findByRole("heading", { name: "Linus Pauling" });
    fireEvent.click(await screen.findByRole("button", { name: "Set limit" }));
    const dialog = await screen.findByRole("dialog", { name: "Running limit — Linus Pauling" });
    fireEvent.change(within(dialog).getByLabelText("Running workspaces"), {
      target: { value: "1" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save limit" }));
    await waitFor(() =>
      expect(
        api.state.requests.filter(
          (r) => r.method === "PUT" && r.path === `/v1/admin/tenants/${TENANT}/user-limits`,
        ),
      ).toHaveLength(1),
    );
    const put = api.state.requests.find(
      (r) => r.method === "PUT" && r.path === `/v1/admin/tenants/${TENANT}/user-limits`,
    )!;
    expect(JSON.parse(put.rawBody)).toEqual({
      ownerRef: `https://idp.invalid|${LINUS}`,
      limit: 1,
    });
  });
});
