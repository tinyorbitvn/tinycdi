import { test, expect, type Page } from "@playwright/test";
import { AxeBuilder } from "@axe-core/playwright";

// FX-R21: sign-out. Account menu -> POST /v1/logout -> the identity
// provider's end-session URL (when the backend returns one) or the public
// signed-out page, which never starts a login by itself. Runs against the
// contract mock (tests/mock-api) on :4310.

const MOCK = "http://127.0.0.1:4310";
const IDP_LOGOUT = "https://idp.test.invalid/logout?client_id=tinycdi-portal";

async function login(page: Page) {
  await page.goto("/v1/login?returnTo=/");
  await page.getByRole("button", { name: "Log in with SSO" }).click();
  await page.waitForURL("/");
  await expect(page.getByRole("heading", { name: "Workspaces" })).toBeVisible();
}

test.beforeEach(async ({ request }) => {
  expect((await request.post(`${MOCK}/_control/reset`)).ok()).toBeTruthy();
  expect((await request.post(`${MOCK}/_control/requests/clear`)).ok()).toBeTruthy();
});

// The page URLs the browser requested from the mock's /v1 surface.
function recordApi(page: Page): string[] {
  const seen: string[] = [];
  page.on("request", (r) => {
    const u = new URL(r.url());
    if (u.pathname.startsWith("/v1/")) seen.push(`${r.method()} ${u.pathname}`);
  });
  return seen;
}

test("account menu: keyboard reachable, axe-clean, Escape returns focus", async ({ page }) => {
  await login(page);
  const trigger = page.getByRole("button", { name: "Ada Lovelace" });
  await trigger.focus();
  await expect(trigger).toBeFocused();
  await page.keyboard.press("ArrowDown");
  const signOut = page.getByRole("menuitem", { name: "Sign out", exact: true });
  await expect(signOut).toBeFocused();
  const results = await new AxeBuilder({ page }).analyze();
  expect(results.violations.filter((v) => v.impact === "serious" || v.impact === "critical")).toEqual([]);
  await page.keyboard.press("Escape");
  await expect(page.getByRole("menu")).toHaveCount(0);
  await expect(trigger).toBeFocused();
});

test("sign out without a provider end-session lands on the signed-out page and does not sign back in", async ({ page }) => {
  await login(page);
  const seen = recordApi(page);
  await page.getByRole("button", { name: "Ada Lovelace" }).click();
  await page.getByRole("menuitem", { name: "Sign out", exact: true }).click();

  await page.waitForURL("**/signed-out");
  await expect(page.getByRole("heading", { name: "You have signed out" })).toBeVisible();
  const link = page.getByRole("link", { name: "Sign in again" });
  await expect(link).toBeVisible();
  expect(seen).toContain("POST /v1/logout");

  // The page itself is quiet: no probe, no login, even after settling.
  const before = seen.length;
  await page.waitForLoadState("networkidle");
  await page.waitForTimeout(500);
  expect(seen.slice(before)).toEqual([]);
  expect(seen.filter((s) => s.includes("/v1/login"))).toEqual([]);

  // The session really is gone, and a reload of the signed-out page stays put.
  await page.reload();
  await expect(page.getByRole("heading", { name: "You have signed out" })).toBeVisible();
  expect(page.url()).toContain("/signed-out");

  const results = await new AxeBuilder({ page }).analyze();
  expect(results.violations.filter((v) => v.impact === "serious" || v.impact === "critical")).toEqual([]);

  // Only the user's own click starts a login.
  await link.click();
  await expect(page.getByRole("button", { name: "Log in with SSO" })).toBeVisible();
});

test("sign out continues at the identity provider when the backend returns an end-session URL", async ({ page, request }) => {
  expect((await request.post(`${MOCK}/_control/auth/endSession`, { data: { url: IDP_LOGOUT } })).ok()).toBeTruthy();
  await page.route("https://idp.test.invalid/**", (route) =>
    route.fulfill({ status: 200, contentType: "text/html", body: "<html><body><h1>Logged out at the provider</h1></body></html>" }),
  );
  await login(page);
  await page.getByRole("button", { name: "Ada Lovelace" }).click();
  await page.getByRole("menuitem", { name: "Sign out", exact: true }).click();

  await page.waitForURL(IDP_LOGOUT);
  await expect(page.getByRole("heading", { name: "Logged out at the provider" })).toBeVisible();
  // The portal session is gone: the probe says so.
  const probe = await page.request.get("/v1/session");
  expect(await probe.json()).toEqual({ authenticated: false });
});

// ADR 0007: "Sign out everywhere" confirms (naming the tenant scope), then
// POSTs /v1/me/sessions:revoke-all and lands on the same signed-out page.
test("sign out everywhere confirms the tenant scope, then ends all sessions", async ({ page }) => {
  await login(page);
  const seen = recordApi(page);
  await page.getByRole("button", { name: "Ada Lovelace" }).click();
  await page.getByRole("menuitem", { name: "Sign out everywhere" }).click();

  const dialog = page.getByRole("alertdialog");
  await expect(dialog).toBeVisible();
  await expect(dialog).toContainText("tenant acme");
  await expect(dialog).toContainText("including this one");
  expect(seen.filter((s) => s.includes("revoke-all"))).toEqual([]);

  await dialog.getByRole("button", { name: "Sign out everywhere" }).click();
  await page.waitForURL("**/signed-out");
  await expect(page.getByRole("heading", { name: "You have signed out" })).toBeVisible();
  expect(seen).toContain("POST /v1/me/sessions:revoke-all");
  const probe = await page.request.get("/v1/session");
  expect(await probe.json()).toEqual({ authenticated: false });
});
