import { test, expect, type Page } from "@playwright/test";

// Vietnamese UI (E11): the language switch sets <html lang> and the choice
// survives a reload via tcdi.lang; with no stored choice the portal follows
// navigator.language. Runs against the contract mock (tests/mock-api).

const MOCK = "http://127.0.0.1:4310";

async function login(page: Page, heading: string) {
  await page.goto("/v1/login?returnTo=/");
  await page.getByRole("button", { name: "Log in with SSO" }).click();
  await page.waitForURL("/");
  await expect(
    page.getByRole("heading", { name: heading, exact: true }),
  ).toBeVisible();
}

test.beforeEach(async ({ request }) => {
  const res = await request.post(`${MOCK}/_control/reset`);
  expect(res.ok()).toBeTruthy();
});

test("language switch sets <html lang> and survives a reload", async ({
  page,
}) => {
  await login(page, "Workspaces");
  await expect(page.locator("html")).toHaveAttribute("lang", "en");

  await page.getByRole("button", { name: "Language" }).click();
  await page.getByRole("menuitem", { name: "Tiếng Việt" }).click();

  await expect(page.locator("html")).toHaveAttribute("lang", "vi");
  await expect(
    page.getByRole("heading", { name: "Workspace", exact: true }),
  ).toBeVisible();
  await expect(
    page
      .getByRole("navigation", { name: "Mục" })
      .getByRole("link", { name: "Danh mục template" }),
  ).toBeVisible();

  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("lang", "vi");
  await expect(
    page.getByRole("heading", { name: "Workspace", exact: true }),
  ).toBeVisible();
});

test.describe("locale detection", () => {
  test.use({ locale: "vi-VN" });

  test("navigator.language vi lands on the Vietnamese UI", async ({
    page,
  }) => {
    await login(page, "Workspace");
    await expect(page.locator("html")).toHaveAttribute("lang", "vi");
  });

  test("a stored preference beats navigator.language", async ({
    page,
    context,
  }) => {
    await context.addInitScript(() =>
      window.localStorage.setItem("tcdi.lang", "en"),
    );
    await login(page, "Workspaces");
    await expect(page.locator("html")).toHaveAttribute("lang", "en");
  });
});
