import { test, expect, type Page, type APIRequestContext } from "@playwright/test";
import { AxeBuilder } from "@axe-core/playwright";

// Client-side table pagination + sortable columns (T5) against the contract
// mock: 60 workspaces upserted through /_control/workspaces/{id} push the
// admin table past one page; page-size change, filter reset and a two-way
// sort are exercised end to end, plus axe on both themes and vi.
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

test("pages through 60+ tenant workspaces: next, number, page size, filter reset", async ({
  page,
  request,
}) => {
  await makeAdmin(request);
  await seedWorkspaces(request);
  await login(page, "/admin/workspaces");
  await expect(page.getByRole("heading", { name: "Tenant workspaces" })).toBeVisible();

  const nav = page.getByRole("navigation", { name: "Tenant workspaces pages" });
  const status = page.getByText(/^Showing \d+–\d+ of \d+$/);
  await expect(status).toHaveText(`Showing 1–50 of ${TOTAL}`);
  await expect(nav.getByRole("button", { name: "Page 1" })).toHaveAttribute(
    "aria-current",
    "page",
  );

  // Next advances; a numbered page returns.
  await nav.getByRole("button", { name: "Next" }).click();
  await expect(status).toHaveText(`Showing 51–${TOTAL} of ${TOTAL}`);
  await expect(nav.getByRole("button", { name: "Page 2" })).toHaveAttribute(
    "aria-current",
    "page",
  );
  await nav.getByRole("button", { name: "Page 1" }).click();
  await expect(status).toHaveText(`Showing 1–50 of ${TOTAL}`);

  // Previous on page 1 is disabled; the strip offers every page at this size.
  await expect(nav.getByRole("button", { name: "Previous" })).toBeDisabled();
  await page.getByRole("combobox", { name: "Rows per page" }).selectOption("25");
  await expect(status).toHaveText(`Showing 1–25 of ${TOTAL}`);
  await expect(nav.getByRole("button", { name: "Page 3" })).toBeVisible();

  // Keyboard: Enter on the focused Next button turns the page.
  await nav.getByRole("button", { name: "Next" }).focus();
  await page.keyboard.press("Enter");
  await expect(status).toHaveText(`Showing 26–50 of ${TOTAL}`);

  // A search filter narrows the list and brings the view back to page 1.
  await page.getByRole("searchbox", { name: "Filter" }).fill("grace");
  await expect(status).toHaveText("Showing 1–2 of 2");
  await expect(nav.getByRole("button", { name: "Page 2" })).toHaveCount(0);
  await expect(page.getByRole("row", { name: /grace-analysis/ })).toBeVisible();
});

test("sorts the workspace column in both directions", async ({ page, request }) => {
  await makeAdmin(request);
  await seedWorkspaces(request);
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

  // Same column again flips to descending: research-desktop leads.
  await nameHeader.getByRole("button").click();
  await expect(nameHeader).toHaveAttribute("aria-sort", "descending");
  await expect.poll(firstRowName).toContain("research-desktop");

  // The sorted order holds across the page boundary.
  await page.getByRole("navigation", { name: "Tenant workspaces pages" })
    .getByRole("button", { name: "Next" })
    .click();
  await expect.poll(firstRowName).not.toContain("research-desktop");
});

for (const theme of ["light", "dark"] as const) {
  test(`axe [${theme}]: paginated admin workspaces has no serious/critical violations`, async ({
    page,
    request,
    context,
  }) => {
    await page.emulateMedia({ colorScheme: theme });
    await context.addInitScript((t) => window.localStorage.setItem("tcdi.theme", t), theme);
    await makeAdmin(request);
    await seedWorkspaces(request);
    await login(page, "/admin/workspaces");
    await expect(page.getByText(`Showing 1–50 of ${TOTAL}`)).toBeVisible();
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
    await expect(page.getByText(`Hiển thị 1–50 trên ${TOTAL}`)).toBeVisible();
    await expect(nav.getByRole("button", { name: "Trang 2" })).toBeVisible();
    await nav.getByRole("button", { name: "Sau" }).click();
    await expect(page.getByText(`Hiển thị 51–${TOTAL} trên ${TOTAL}`)).toBeVisible();

    const results = await new AxeBuilder({ page }).analyze();
    const bad = results.violations.filter(
      (v) => v.impact === "serious" || v.impact === "critical",
    );
    expect(bad, JSON.stringify(bad.map((v) => ({ id: v.id, nodes: v.nodes.length })))).toEqual(
      [],
    );
  });
});
