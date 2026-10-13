import { test, expect, type Page, type APIRequestContext } from "@playwright/test";
import { AxeBuilder } from "@axe-core/playwright";

// Server-side table paging + sorting against the contract mock: 60
// workspaces upserted through /_control/workspaces/{id} push the admin
// table past one server page (limit 50). "Load more" follows the cursor,
// a header click re-queries the server under the new sort, plus axe on
// both themes and vi.
const MOCK = process.env.PW_MOCK_API ?? "http://127.0.0.1:4310";
const BULK = 60;
// 1 base + 3 admin-seed + 60 bulk workspaces.
const TOTAL = 1 + 3 + BULK;

async function login(page: Page, returnTo = "/") {
  await page.goto(`/v1/login?returnTo=${encodeURIComponent(returnTo)}`);
  await page.getByRole("button", { name: "Log in with SSO" }).click();
  await page.waitForURL(new RegExp(`${returnTo.replace(/\//g, "\\/")}$`));
}

async function makeAdmin(request: APIRequestContext) {
  const me = await request.post(`${MOCK}/_control/admin/me`, {
    data: { roles: ["user", "tenant-admin"] },
  });
  expect(me.ok()).toBeTruthy();
  const seed = await request.post(`${MOCK}/_control/admin/seed`);
  expect(seed.ok()).toBeTruthy();
}

// Upserts Ready workspaces page-ws-01..N owned by the caller.
async function seedWorkspaces(request: APIRequestContext, count = BULK) {
  await Promise.all(
    Array.from({ length: count }, (_, i) => {
      const n = String(i + 1).padStart(2, "0");
      return request.put(`${MOCK}/_control/workspaces/ws_bulk${n}`, {
        data: { name: `page-ws-${n}` },
      });
    }),
  ).then((res) => res.forEach((r) => expect(r.ok()).toBeTruthy()));
}

test.beforeEach(async ({ request }) => {
  const res = await request.post(`${MOCK}/_control/reset`);
  expect(res.ok()).toBeTruthy();
});

test("loads 60+ tenant workspaces page by page: Load more, filter over loaded rows", async ({
  page,
  request,
}) => {
  await makeAdmin(request);
  await seedWorkspaces(request);
  const lists: URL[] = [];
  page.on("request", (r) => {
    const u = new URL(r.url());
    if (r.method() === "GET" && u.pathname === "/v1/workspaces") lists.push(u);
  });
  await login(page, "/admin/workspaces");
  await expect(page.getByRole("heading", { name: "Tenant workspaces" })).toBeVisible();

  const table = page.getByRole("table", { name: "Tenant workspaces" });
  const nav = page.getByRole("navigation", { name: "Tenant workspaces pages" });
  await expect(nav.getByText("Showing the first 50")).toBeVisible();
  await expect(table.getByRole("row")).toHaveCount(1 + 50);
  expect(lists[0]?.searchParams.get("sort")).toBe("-createdAt");
  expect(lists[0]?.searchParams.get("scope")).toBe("tenant");

  // Load more (a real button: Enter activates it) appends the next server
  // page; the last page drops the control.
  await nav.getByRole("button", { name: "Load more" }).focus();
  await page.keyboard.press("Enter");
  await expect(nav.getByText(`Showing all ${TOTAL}`)).toBeVisible();
  await expect(table.getByRole("row")).toHaveCount(1 + TOTAL);
  await expect(nav.getByRole("button", { name: "Load more" })).toHaveCount(0);
  expect(lists.at(-1)?.searchParams.get("cursor")).toBeTruthy();

  // The search narrows the rows loaded so far.
  await page.getByRole("searchbox", { name: "Filter" }).fill("grace");
  await expect(page.getByRole("row", { name: /grace-analysis/ })).toBeVisible();
  await expect(table.getByRole("row")).toHaveCount(1 + 2);
});

test("sorts the workspace column in both directions on the server", async ({ page, request }) => {
  await makeAdmin(request);
  await seedWorkspaces(request);
  const sorts: (string | null)[] = [];
  page.on("request", (r) => {
    const u = new URL(r.url());
    if (r.method() === "GET" && u.pathname === "/v1/workspaces") sorts.push(u.searchParams.get("sort"));
  });
  await login(page, "/admin/workspaces");
  const table = page.getByRole("table", { name: "Tenant workspaces" });
  await expect(table.getByRole("row").nth(1)).toBeVisible();

  const nameHeader = table.getByRole("columnheader", { name: "Workspace" });
  const firstRowName = () =>
    table.getByRole("row").nth(1).getByRole("rowheader").textContent();

  // Ascending puts grace-analysis first (g < l < p < r in this seed).
  await nameHeader.getByRole("button").click();
  await expect(nameHeader).toHaveAttribute("aria-sort", "ascending");
  await expect.poll(firstRowName).toContain("grace-analysis");
  expect(sorts.at(-1)).toBe("name");

  // Same column again flips to descending: research-desktop leads.
  await nameHeader.getByRole("button").click();
  await expect(nameHeader).toHaveAttribute("aria-sort", "descending");
  await expect.poll(firstRowName).toContain("research-desktop");
  expect(sorts.at(-1)).toBe("-name");

  // The order holds across the page boundary: the next page continues it.
  await page.getByRole("navigation", { name: "Tenant workspaces pages" })
    .getByRole("button", { name: "Load more" })
    .click();
  await expect(table.getByRole("row")).toHaveCount(1 + TOTAL);
  await expect.poll(() => table.getByRole("row").last().getByRole("rowheader").textContent()).toContain(
    "grace-analysis",
  );
});

for (const theme of ["light", "dark"] as const) {
  test(`axe [${theme}]: server-paged admin workspaces has no serious/critical violations`, async ({
    page,
    request,
    context,
  }) => {
    await page.emulateMedia({ colorScheme: theme });
    await context.addInitScript((t) => window.localStorage.setItem("tcdi.theme", t), theme);
    await makeAdmin(request);
    await seedWorkspaces(request);
    await login(page, "/admin/workspaces");
    await expect(page.getByText("Showing the first 50")).toBeVisible();
    const results = await new AxeBuilder({ page }).analyze();
    const bad = results.violations.filter(
      (v) => v.impact === "serious" || v.impact === "critical",
    );
    expect(bad, JSON.stringify(bad.map((v) => ({ id: v.id, nodes: v.nodes.length })))).toEqual(
      [],
    );
  });
}

test.describe("locale: vi", () => {
  test.use({ locale: "vi-VN" });

  test("pagination renders Vietnamese strings and stays axe-clean", async ({
    page,
    request,
    context,
  }) => {
    await context.addInitScript(() => window.localStorage.setItem("tcdi.theme", "light"));
    await makeAdmin(request);
    await seedWorkspaces(request);
    await login(page, "/admin/workspaces");
    await expect(page.locator("html")).toHaveAttribute("lang", "vi");

    const nav = page.getByRole("navigation", { name: "Phân trang Workspace của tenant" });
    await expect(nav.getByText("Đang hiển thị 50 mục đầu tiên")).toBeVisible();
    await nav.getByRole("button", { name: "Tải thêm" }).click();
    await expect(nav.getByText(`Đang hiển thị tất cả ${TOTAL} mục`)).toBeVisible();

    const results = await new AxeBuilder({ page }).analyze();
    const bad = results.violations.filter(
      (v) => v.impact === "serious" || v.impact === "critical",
    );
    expect(bad, JSON.stringify(bad.map((v) => ({ id: v.id, nodes: v.nodes.length })))).toEqual(
      [],
    );
  });
});
