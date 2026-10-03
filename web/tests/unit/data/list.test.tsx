import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { DataListPage } from "../../../src/data/DataListPage";
import {
  createMockApi,
  renderWithApi,
  loginCookies,
  SESSION_COOKIE,
  SESSION_PRINCIPAL,
  CSRF_TOKEN_VALUE,
} from "../helpers";
import { RETAINED_DISK, type RetainedFixture } from "../../mock-api/fixtures.ts";

function authHeaders() {
  return {
    cookie: `${SESSION_COOKIE}=${SESSION_PRINCIPAL}`,
    "x-csrf-token": CSRF_TOKEN_VALUE,
  };
}

function seedStates(api: ReturnType<typeof createMockApi>) {
  const states = ["Retained", "Attaching", "Attached", "Purging"] as const;
  const ids: string[] = [];
  for (const [i, state] of states.entries()) {
    const rec: RetainedFixture = {
      ...structuredClone(RETAINED_DISK),
      id: `rd_state0${i}AAAAAAAAAAAAAAAAAA`.slice(0, 30),
      state,
      sourceWorkspaceName: `ws-${state.toLowerCase()}`,
      ...(state === "Attached" || state === "Attaching"
        ? { consumingWorkspaceId: "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E" }
        : {}),
    };
    api.state.retained.set(rec.id, rec);
    ids.push(rec.id);
  }
  return ids;
}

describe("data list", () => {
  it("renders a labelled status for each retained state", async () => {
    const api = createMockApi();
    seedStates(api);
    loginCookies();
    renderWithApi(<DataListPage />, api);

    const table = await screen.findByRole("table", { name: /retained/i });
    for (const state of ["Retained", "Attaching", "Attached", "Purging"]) {
      const row = within(table)
        .getAllByRole("row")
        .find((r) => r.textContent?.includes(`ws-${state.toLowerCase()}`));
      expect(row, `row for ${state}`).toBeTruthy();
      expect(within(row!).getByText(state)).toBeInTheDocument();
    }
  });

  it("hides Attach and Purge unless the record is Retained", async () => {
    const api = createMockApi();
    seedStates(api);
    loginCookies();
    renderWithApi(<DataListPage />, api);

    const table = await screen.findByRole("table", { name: /retained/i });
    const rows = within(table).getAllByRole("row").slice(1); // skip header

    // RETAINED_DISK (the default seed) is also Retained; assert per record.
    const cases: [source: string, expectActions: boolean][] = [
      ["ws-retained", true],
      ["ws-attaching", false],
      ["ws-attached", false],
      ["ws-purging", false],
    ];
    for (const [source, expectActions] of cases) {
      const row = rows.find((r) => r.textContent?.includes(source));
      expect(row, `row for ${source}`).toBeTruthy();
      const attach = within(row!).queryByRole("button", { name: /attach/i });
      const purge = within(row!).queryByRole("button", { name: /purge/i });
      if (expectActions) {
        expect(attach).toBeInTheDocument();
        expect(purge).toBeInTheDocument();
      } else {
        expect(attach).not.toBeInTheDocument();
        expect(purge).not.toBeInTheDocument();
      }
    }
  });

  it("lets a tenant admin switch to scope=tenant and see the owner column", async () => {
    const api = createMockApi();
    // Promote the principal to tenant-admin and seed other users' records.
    api.handle({
      method: "POST",
      path: "/_control/admin/me",
      query: new URLSearchParams(),
      headers: authHeaders(),
      body: { roles: ["user", "tenant-admin"] },
    });
    api.handle({
      method: "POST",
      path: "/_control/admin/seed",
      query: new URLSearchParams(),
      headers: authHeaders(),
      body: {},
    });
    loginCookies();
    renderWithApi(<DataListPage />, api);

    const scope = await screen.findByLabelText(/scope/i);
    fireEvent.change(scope, { target: { value: "tenant" } });

    const table = await screen.findByRole("table", { name: /retained/i });
    await waitFor(() =>
      expect(within(table).getByText("Grace Hopper")).toBeInTheDocument(),
    );
    expect(within(table).getByText(/owner/i)).toBeInTheDocument();
    // scope=mine would only show the caller's own disk; a record owned by
    // another user proves the tenant scope was requested and honoured.
    expect(within(table).getByText("grace-old-lab")).toBeInTheDocument();
  });

  it("does not show the scope switcher to regular users", async () => {
    const api = createMockApi();
    loginCookies();
    renderWithApi(<DataListPage />, api);
    await screen.findByRole("table", { name: /retained/i });
    await waitFor(() =>
      expect(api.state.requests.some((r) => r.path === "/v1/me")).toBe(true),
    );
    expect(screen.queryByLabelText(/scope/i)).not.toBeInTheDocument();
  });
});
