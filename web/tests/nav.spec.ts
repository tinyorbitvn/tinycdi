import { test, expect, type Page } from "@playwright/test";

// Mobile navigation (UX-2): below 48rem the sidebar collapses behind a
// hamburger that opens a focus-trapped Drawer. Covers open → navigate →
// focus restore, plus Escape dismissal, at a 390×844 viewport against the
// contract mock (tests/mock-api) on :4310.

const MOCK = process.env.PW_MOCK_API ?? "http://127.0.0.1:4310";

async function login(page: Page, returnTo = "/") {
  await page.goto(`/v1/login?returnTo=${encodeURIComponent(returnTo)}`);
  await page.getByRole("button", { name: "Log in with SSO" }).click();
  await page.waitForURL(new RegExp(`${returnTo.replace(/\//g, "\\/")}$`));
}

test.use({ viewport: { width: 390, height: 844 } });

test.beforeEach(async ({ request }) => {
  const res = await request.post(`${MOCK}/_control/reset`);
  expect(res.ok()).toBeTruthy();
});

test("mobile: the hamburger opens a nav drawer; picking a section navigates and restores focus", async ({
  page,
}) => {
  await login(page);
  await expect(page.getByRole("heading", { name: "Workspaces" })).toBeVisible();

  // The desktop sidebar is out of the tree; the hamburger is the way in.
  await expect(page.locator("nav.tc-sidebar")).toBeHidden();
  const toggle = page.getByRole("button", { name: "Menu" });
  await expect(toggle).toBeVisible();

  await toggle.click();
  const drawer = page.getByRole("dialog", { name: "Sections" });
  await expect(drawer).toBeVisible();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  // Focus is trapped inside the drawer: it lands on a drawer control.
  const nav = drawer.getByRole("navigation", { name: "Sections" });
  await expect(nav.getByRole("link", { name: "Workspaces" })).toBeVisible();

  await nav.getByRole("link", { name: "Template catalog" }).click();
  await page.waitForURL(/\/templates$/);
  await expect(drawer).toBeHidden();
  await expect(page.getByRole("heading", { name: "Template catalog" })).toBeVisible();
  // The focus trap restores focus to the trigger that opened the drawer.
  await expect(toggle).toBeFocused();
});

test("mobile: Escape closes the drawer and restores focus", async ({ page }) => {
  await login(page);
  const toggle = page.getByRole("button", { name: "Menu" });
  await toggle.click();
  const drawer = page.getByRole("dialog", { name: "Sections" });
  await expect(drawer).toBeVisible();

  // Tab stays inside the drawer (focus trap), Escape dismisses it.
  await page.keyboard.press("Escape");
  await expect(drawer).toBeHidden();
  await expect(toggle).toBeFocused();
});

test("mobile: the drawer trap keeps Tab cycling inside the dialog", async ({ page }) => {
  await login(page);
  const toggle = page.getByRole("button", { name: "Menu" });
  await toggle.click();
  const drawer = page.getByRole("dialog", { name: "Sections" });
  await expect(drawer).toBeVisible();

  // Hammer Tab past the last item: focus must stay inside the drawer.
  for (let i = 0; i < 12; i++) await page.keyboard.press("Tab");
  const inside = await page.evaluate(
    () => document.querySelector(".tc-drawer")?.contains(document.activeElement) ?? false,
  );
  expect(inside).toBeTruthy();
});
