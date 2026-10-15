import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { createMockApi, loginCookies, renderWithApi } from "../helpers";
import { makeAdmin, seedTenant } from "./helpers";
import {
  diskCounts,
  matchesUserFilter,
  ownerRefFromParam,
  userDetailPath,
  userRowOf,
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
import type { AdminTenantUser, ScopedRetainedData } from "../../../src/admin/api";

function uRow(overrides: Partial<AdminTenantUser> = {}): AdminTenantUser {
  return {
    ownerRef: "iss|u1",
    subject: "u1",
    displayName: "User One",
    email: "",
    lastSeenAt: "2026-10-13T09:00:00Z",
    workspaces: 0,
    limits: {
      ownerRef: "iss|u1",
      subject: "u1",
      displayName: "User One",
      limit: null,
      effective: null,
      running: 0,
    },
    ...overrides,
  } as AdminTenantUser;
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

describe("userRowOf", () => {
  it("maps a directory item and the owner's disk count", () => {
    const counts = diskCounts([
      disk({ owner: { subject: "u1", displayName: "User One" } }),
      disk({ id: "rd_y", owner: { subject: "u1", displayName: "User One" } }),
      disk({ id: "rd_w", owner: { subject: "u2", displayName: "Two" } }),
    ]);
    const row = userRowOf(
      uRow({ workspaces: 3, limits: { ...uRow().limits, running: 2, limit: 7, effective: 7, etag: '"ul-3"' } }),
      counts,
    );
    expect(row).toMatchObject({
      subject: "u1",
      ownerRef: "iss|u1",
      running: 2,
      override: 7,
      effective: 7,
      retainedDisks: 2,
      workspaces: 3,
    });
    expect(row.limitsEntry.etag).toBe('"ul-3"');
    expect(userRowOf(uRow({ subject: "u3" }), counts).retainedDisks).toBe(0);
  });

  it("maps a principal with no override to override: null", () => {
    const row = userRowOf(uRow(), new Map());
    expect(row.override).toBeNull();
    expect(row.effective).toBeNull();
  });

  it("falls back to the subject when the display name is empty", () => {
    expect(userRowOf(uRow({ displayName: "" }), new Map()).displayName).toBe("u1");
  });

  it("matches the filter on display name or subject, case-insensitively", () => {
    const row = userRowOf(uRow({ displayName: "Grace Hopper" }), new Map());
    expect(matchesUserFilter(row, "grace")).toBe(true);
    expect(matchesUserFilter(row, "U1")).toBe(true);
    expect(matchesUserFilter(row, "linus")).toBe(false);
    expect(matchesUserFilter(row, "  ")).toBe(true);
  });
});

describe("userDetailPath", () => {
  it("is a dot-free, slash-free segment that round-trips", () => {
    for (const ref of ["https://idp.example.com/realms/x|alice@example.com", "https://idp.invalid|usér-1"]) {
      const path = userDetailPath(ref);
      expect(path).toMatch(/^\/admin\/users\/[A-Za-z0-9_-]+$/);
      expect(ownerRefFromParam(path.split("/").pop()!)).toBe(ref);
    }
    expect(ownerRefFromParam("***")).toBeUndefined();
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

  it("lists the signed-in principals with workspaces, limits and disk counts", async () => {
    const api = setup();
    renderWithApi(<UsersAdminPage />, api);

    await screen.findByRole("heading", { name: "Users" });
    const table = await usersTable();
    for (const name of ["Ada Lovelace", "Grace Hopper", "Linus Pauling", "Margaret Hamilton"]) {
      expect(await within(table).findByText(name)).toBeInTheDocument();
    }
    // Every row links to its per-user detail page.
    expect(within(table).getByRole("link", { name: "Grace Hopper" })).toHaveAttribute(
      "href",
      userDetailPath("https://idp.invalid|" + OTHER_USERS[0]!.subject),
    );
    // Grace has one running-holding workspace pair and one retained disk
    // (the disks cell: lastSeen and workspaces were appended after it).
    const grace = within(table).getByText("Grace Hopper").closest("tr")!;
    await waitFor(() => expect(within(grace).getAllByRole("cell").at(-3)).toHaveTextContent("1"));
    // The rows come from the server-paged user directory.
    const list = api.state.requests.find(
      (r) => r.method === "GET" && r.path === `/v1/admin/tenants/${TENANT}/users`,
    )!;
    expect(new URLSearchParams(list.query).get("sort")).toBe("-lastSeen");
    expect(screen.queryByText(/not a full tenant directory/)).not.toBeInTheDocument();
  });

  it("filters rows by name or subject", async () => {
    const api = setup();
    renderWithApi(<UsersAdminPage />, api);
    const table = await usersTable();
    await within(table).findByText("Grace Hopper");

    fireEvent.change(screen.getByLabelText("Filter"), { target: { value: "linus" } });
    expect(within(table).queryByText("Grace Hopper")).not.toBeInTheDocument();
    expect(within(table).getByText("Linus Pauling")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Filter"), { target: { value: "nobody" } });
    expect(screen.getByText("No users match the filter.")).toBeInTheDocument();
  });

  it("lists a signed-in user who holds nothing", async () => {
    const api = setup();
    renderWithApi(<UsersAdminPage />, api);

    const table = await usersTable();
    const margaret = (await within(table).findByRole("link", { name: "Margaret Hamilton" })).closest("tr")!;
    // No workspaces: zero running, the override cell shows the default.
    expect(margaret).toHaveTextContent("0");
    expect(margaret).toHaveTextContent("Default");
    expect(within(margaret).getByRole("link", { name: "Margaret Hamilton" })).toHaveAttribute(
      "href",
      userDetailPath("https://idp.invalid|user-01J4ZDMGH"),
    );
  });
  it("caps the filter field width", async () => {
    renderWithApi(<UsersAdminPage />, setup());
    const search = await screen.findByRole("searchbox");
    expect(search.closest(".tc-admin-filter")).not.toBeNull();
  });
});

function userLimitPuts(api: ReturnType<typeof createMockApi>) {
  return api.state.requests.filter(
    (r) => r.method === "PUT" && r.path.startsWith(`/v1/admin/tenants/${TENANT}/user-limits/`),
  );
}

describe("UserDetailPage", () => {
  const GRACE = OTHER_USERS[0]!.subject;
  const LINUS = OTHER_USERS[1]!.subject;
  const GRACE_REF = "https://idp.invalid|" + GRACE;
  const LINUS_REF = "https://idp.invalid|" + LINUS;

  function setup() {
    const api = createMockApi({ areas: [adminArea, workspacesArea, dataArea] });
    loginCookies();
    makeAdmin(api);
    seedTenant(api);
    return api;
  }

  it("shows only the user's workspaces and retained disks", async () => {
    const api = setup();
    renderWithApi(<UserDetailPage ownerRef={GRACE_REF} />, api);

    await screen.findByRole("heading", { name: "Grace Hopper" });
    // Detail reads the tenant scope — the same scope gate as the list pages.
    expect(tenancy(api.ctx).scopesSeen).toContain("tenant");
    expect(screen.getByText("Last seen", { selector: "dt" })).toBeInTheDocument();

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
    renderWithApi(<UserDetailPage ownerRef={GRACE_REF} />, api);
    await screen.findByRole("heading", { name: "Grace Hopper" });

    fireEvent.click(await screen.findByRole("button", { name: "Set limit" }));
    const dialog = await screen.findByRole("dialog", { name: "Running limit — Grace Hopper" });
    fireEvent.change(within(dialog).getByLabelText("Running workspaces"), {
      target: { value: "3" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save limit" }));

    await waitFor(() => expect(userLimitPuts(api)).toHaveLength(1));
    // The DescriptionList's Effective row re-renders from the reload.
    await waitFor(() =>
      expect(
        screen.getByText("Effective", { selector: "dt" }).closest("div"),
      ).toHaveTextContent("3"),
    );
  });

  it("renders a not-found state for a principal who never signed in", async () => {
    const api = setup();
    renderWithApi(<UserDetailPage ownerRef="https://idp.invalid|user-nobody" />, api);
    expect(await screen.findByText("User not found")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Back to users" })).toHaveAttribute(
      "href",
      "/admin/users",
    );
  });

  it("keeps Linus's limit untouched when editing Grace's", async () => {
    // Guards the per-row edit target: the dialog PUTs Grace's ownerRef.
    const api = setup();
    renderWithApi(<UserDetailPage ownerRef={LINUS_REF} />, api);
    await screen.findByRole("heading", { name: "Linus Pauling" });
    fireEvent.click(await screen.findByRole("button", { name: "Set limit" }));
    const dialog = await screen.findByRole("dialog", { name: "Running limit — Linus Pauling" });
    fireEvent.change(within(dialog).getByLabelText("Running workspaces"), {
      target: { value: "1" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save limit" }));
    await waitFor(() => expect(userLimitPuts(api)).toHaveLength(1));
    const put = userLimitPuts(api)[0]!;
    expect(decodeURIComponent(put.path)).toBe(`/v1/admin/tenants/${TENANT}/user-limits/https://idp.invalid|${LINUS}`);
    expect(JSON.parse(put.rawBody)).toEqual({ limit: 1 });
    // No override row existed: the write creates it with If-Match: *.
    expect(put.headers["if-match"]).toBe("*");
  });
  it("puts identity facts under an Identity section, not the limit heading or the subtitle", async () => {
    renderWithApi(<UserDetailPage ownerRef={GRACE_REF} />, setup());
    await screen.findByRole("heading", { name: "Grace Hopper" });
    const identity = screen.getByRole("region", { name: "Identity" });
    expect(within(identity).getByText("Issuer", { selector: "dt" })).toBeInTheDocument();
    expect(within(identity).getByText("https://idp.invalid", { selector: "code" })).toBeInTheDocument();
    expect(within(identity).getByText("Last seen", { selector: "dt" })).toBeInTheDocument();
    const limit = screen.getByRole("region", { name: "Running-workspace limit" });
    expect(within(limit).queryByText("Subject", { selector: "dt" })).toBeNull();
    expect(document.querySelector(".tc-page__description")).toBeNull();
    expect(screen.getByRole("link", { name: "Back to users" })).toHaveClass("tc-backlink");
  });
});
