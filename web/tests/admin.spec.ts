import { test, expect, type Page } from "@playwright/test";

// Admin screens against the contract mock (tests/mock-api): portal API on
// :4310 via the Vite /v1 proxy. The default principal is a regular user; the
// tenant-admin role is granted through the mock's /_control/admin routes.

const MOCK = process.env.PW_MOCK_API ?? "http://127.0.0.1:4310";

async function login(page: Page, returnTo = "/") {
  await page.goto(`/v1/login?returnTo=${encodeURIComponent(returnTo)}`);
  await page.getByRole("button", { name: "Log in with SSO" }).click();
  await page.waitForURL(new RegExp(`${returnTo.replace(/\//g, "\\/")}$`));
}

test.beforeEach(async ({ request }) => {
  const res = await request.post(`${MOCK}/_control/reset`);
  expect(res.ok()).toBeTruthy();
});

test("a tenant admin sees all four admin pages", async ({ page, request }) => {
  const me = await request.post(`${MOCK}/_control/admin/me`, {
    data: { roles: ["user", "tenant-admin"] },
  });
  expect(me.ok()).toBeTruthy();
  await request.post(`${MOCK}/_control/admin/seed`);

  await login(page, "/admin");

  // The admin nav appears in the shell and each section renders its page.
  await expect(page.getByRole("heading", { name: "Tenant overview" })).toBeVisible();

  const adminNav = page.getByRole("navigation", { name: "Admin sections" });
  await expect(adminNav.getByRole("link", { name: "Workspaces" })).toBeVisible();
  await expect(adminNav.getByRole("link", { name: "Quota" })).toBeVisible();
  await expect(adminNav.getByRole("link", { name: "Templates" })).toBeVisible();

  await adminNav.getByRole("link", { name: "Workspaces" }).click();
  await expect(page.getByRole("heading", { name: "Tenant workspaces" })).toBeVisible();
  await expect(page.getByRole("columnheader", { name: "Owner" })).toBeVisible();
  await expect(page.getByRole("link", { name: "grace-analysis" })).toBeVisible();

  await adminNav.getByRole("link", { name: "Quota" }).click();
  await expect(page.getByRole("heading", { name: "Quota" })).toBeVisible();
  await expect(page.getByRole("table", { name: "Usage by user" })).toBeVisible();

  await adminNav.getByRole("link", { name: "Templates" }).click();
  await expect(page.getByRole("heading", { name: "Template catalog" })).toBeVisible();
});

test("a regular user is redirected away from /admin", async ({ page }) => {
  await login(page);
  await expect(page.getByRole("link", { name: "Admin", exact: true })).toHaveCount(0);

  await page.goto("/admin");
  await page.waitForURL(/\/workspaces\/?$/);
});
