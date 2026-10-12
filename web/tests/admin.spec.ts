import { test, expect, type Page, type APIRequestContext } from "@playwright/test";

// Admin screens against the contract mock (tests/mock-api): portal API on
// :4310 via the Vite /v1 proxy. The default principal is a regular user; the
// tenant-admin role is granted through the mock's /_control/admin routes.

const MOCK = process.env.PW_MOCK_API ?? "http://127.0.0.1:4310";
const GRACE_READY = "ws_01J4ZGRACEREADY01";
const LINUS_STOPPED = "ws_01J4ZLINUSSTOP001";
const GRACE_DISK = "rd_01J4ZGRACEDISK001";
// A Ready workspace the control route attributes to the caller (Ada) —
// used to prove Connect is owner-gated, not admin-gated.
const OWN_READY = "ws_01J4ZOWNREADY0001";

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

test.beforeEach(async ({ request }) => {
  const res = await request.post(`${MOCK}/_control/reset`);
  expect(res.ok()).toBeTruthy();
});

test("a tenant admin sees all five admin pages", async ({ page, request }) => {
  await makeAdmin(request);

  await login(page, "/admin");

  // The admin nav appears in the shell and each section renders its page.
  await expect(page.getByRole("heading", { name: "Tenant overview" })).toBeVisible();

  const adminNav = page.getByRole("tablist", { name: "Admin sections" });
  await expect(adminNav.getByRole("tab", { name: "Workspaces" })).toBeVisible();
  await expect(adminNav.getByRole("tab", { name: "Quota" })).toBeVisible();
  await expect(adminNav.getByRole("tab", { name: "Templates" })).toBeVisible();
  await expect(adminNav.getByRole("tab", { name: "Data" })).toBeVisible();

  // The route owns the selection: Overview is the selected tab on /admin.
  await expect(adminNav.getByRole("tab", { name: "Overview" })).toHaveAttribute(
    "aria-selected",
    "true",
  );

  await adminNav.getByRole("tab", { name: "Workspaces" }).click();
  await expect(page.getByRole("heading", { name: "Tenant workspaces" })).toBeVisible();
  await expect(page.getByRole("columnheader", { name: "Owner" })).toBeVisible();
  await expect(page.getByRole("link", { name: "grace-analysis" })).toBeVisible();
  await expect(adminNav.getByRole("tab", { name: "Workspaces" })).toHaveAttribute(
    "aria-selected",
    "true",
  );

  // Arrow keys move+activate: from Workspaces, ArrowRight selects Quota.
  await adminNav.getByRole("tab", { name: "Workspaces" }).focus();
  await page.keyboard.press("ArrowRight");
  await expect(page.getByRole("heading", { name: "Quota" })).toBeVisible();
  await expect(page.getByRole("table", { name: "Usage by user" })).toBeVisible();
  await expect(adminNav.getByRole("tab", { name: "Quota" })).toBeFocused();

  await adminNav.getByRole("tab", { name: "Templates" }).click();
  await expect(page.getByRole("heading", { name: "Template catalog" })).toBeVisible();

  await adminNav.getByRole("tab", { name: "Data" }).click();
  await expect(page.getByRole("heading", { name: "Retained data" })).toBeVisible();
  await expect(
    page.getByRole("table", { name: "Retained data across the tenant" }),
  ).toBeVisible();
  await expect(adminNav.getByRole("tab", { name: "Data" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
});

test("a regular user is redirected away from /admin", async ({ page }) => {
  await login(page);
  await expect(page.getByRole("link", { name: "Admin", exact: true })).toHaveCount(0);

  await page.goto("/admin");
  await page.waitForURL(/\/workspaces\/?$/);
});

test("a regular user is redirected away from /admin/data", async ({ page }) => {
  await login(page);
  await page.goto("/admin/data");
  await page.waitForURL(/\/workspaces\/?$/);
  // The tenant-scope read is refused server-side as well (contract 403).
  const res = await page.request.get("/v1/data?scope=tenant");
  expect(res.status()).toBe(403);
});

test("a tenant admin starts another user's stopped workspace", async ({ page, request }) => {
  await makeAdmin(request);
  await login(page, "/admin/workspaces");
  await expect(page.getByRole("heading", { name: "Tenant workspaces" })).toBeVisible();

  const row = page.getByRole("row", { name: /linus-desktop/ });
  await row.getByRole("button", { name: "Start linus-desktop" }).click();

  const dialog = page.getByRole("dialog", { name: "Start linus-desktop?" });
  // The confirm says whose workspace this is and whose limits it consumes.
  await expect(dialog).toContainText("acting on Linus Pauling's workspace");
  await expect(dialog).toContainText("running-workspace limit");
  await dialog.getByRole("button", { name: "Start workspace" }).click();

  await expect(
    page.getByText("Start requested for linus-desktop (Linus Pauling)."),
  ).toBeVisible();

  const reqs = await (await request.get(`${MOCK}/_control/requests`)).json();
  expect(
    reqs.requests.some(
      (r: { method: string; path: string }) =>
        r.method === "POST" && r.path === `/v1/workspaces/${LINUS_STOPPED}/start`,
    ),
  ).toBeTruthy();
});

test("a tenant admin restarts another user's running workspace", async ({ page, request }) => {
  await makeAdmin(request);
  await login(page, "/admin/workspaces");
  await expect(page.getByRole("heading", { name: "Tenant workspaces" })).toBeVisible();

  const row = page.getByRole("row", { name: /grace-analysis/ });
  await row.getByRole("button", { name: "Actions for grace-analysis" }).click();
  await page.getByRole("menu").getByRole("menuitem", { name: "Restart" }).click();

  const dialog = page.getByRole("dialog", { name: "Restart grace-analysis?" });
  // The dialog is explicit that restart is two operations, not one call.
  await expect(dialog).toContainText("two operations");
  await expect(dialog).toContainText("acting on Grace Hopper's workspace");
  await dialog.getByRole("button", { name: "Restart workspace" }).click();

  // The mock holds the workspace at Stopping; the reconciler's answer
  // lands between the client's polls.
  await page.waitForTimeout(400);
  const patch = await request.post(`${MOCK}/_control/workspaces/${GRACE_READY}`, {
    data: { phase: "Stopped", desiredState: "Stopped" },
  });
  expect(patch.ok()).toBeTruthy();

  await expect(
    page.getByText("Restart requested for grace-analysis (Grace Hopper)."),
  ).toBeVisible({ timeout: 15_000 });

  // Stop was sent before start, and only once each.
  const reqs = await (await request.get(`${MOCK}/_control/requests`)).json();
  const paths = reqs.requests.map((r: { method: string; path: string }) => `${r.method} ${r.path}`);
  const stopIdx = paths.indexOf(`POST /v1/workspaces/${GRACE_READY}/stop`);
  const startIdx = paths.indexOf(`POST /v1/workspaces/${GRACE_READY}/start`);
  expect(stopIdx).toBeGreaterThanOrEqual(0);
  expect(startIdx).toBeGreaterThan(stopIdx);
});

test("Connect is hidden for a non-owner and shown to the owner", async ({ page, request }) => {
  await makeAdmin(request);
  const own = await request.put(`${MOCK}/_control/workspaces/${OWN_READY}`, {
    data: { name: "ada-ready" },
  });
  expect(own.ok()).toBeTruthy();

  await login(page, "/admin/workspaces");
  await expect(page.getByRole("heading", { name: "Tenant workspaces" })).toBeVisible();

  // Grace's Ready workspace: the row menu has lifecycle actions but no Connect.
  const graceRow = page.getByRole("row", { name: /grace-analysis/ });
  await graceRow.getByRole("button", { name: "Actions for grace-analysis" }).click();
  const graceMenu = page.getByRole("menu");
  await expect(graceMenu.getByRole("menuitem", { name: "Stop" })).toBeVisible();
  await expect(graceMenu.getByRole("menuitem", { name: "Connect" })).toHaveCount(0);
  await page.keyboard.press("Escape");

  // The admin's own Ready workspace does offer Connect.
  const ownRow = page.getByRole("row", { name: /ada-ready/ });
  await ownRow.getByRole("button", { name: "Actions for ada-ready" }).click();
  await expect(page.getByRole("menu").getByRole("menuitem", { name: "Connect" })).toBeVisible();
  await page.keyboard.press("Escape");

  // On the shared detail page the admin sees the acting-as context instead
  // of a Connect control for another user's workspace…
  await page.goto(`/workspaces/${GRACE_READY}`);
  await expect(page.getByRole("heading", { name: /grace-analysis/ })).toBeVisible();
  await expect(page.getByText(/belongs to Grace Hopper/)).toBeVisible();
  await expect(page.getByRole("link", { name: "Connect" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Connect" })).toHaveCount(0);

  // …and the normal Connect link on their own.
  await page.goto(`/workspaces/${OWN_READY}`);
  await expect(page.getByRole("heading", { name: /ada-ready/ })).toBeVisible();
  await expect(page.getByRole("link", { name: "Connect" })).toBeVisible();
});

test("a tenant admin purges another owner's retained disk", async ({ page, request }) => {
  await makeAdmin(request);
  await login(page, "/admin/data");
  await expect(page.getByRole("heading", { name: "Retained data" })).toBeVisible();

  // Grace's disk is listed with its owner; Linus's Attached disk is not purgeable.
  const graceRow = page.getByRole("row", { name: new RegExp(GRACE_DISK) });
  await expect(graceRow).toContainText("Grace Hopper");
  await graceRow.getByRole("button", { name: `Purge ${GRACE_DISK}` }).click();

  // The same nonce + typed-confirm flow as the user data page.
  const dialog = page.getByRole("alertdialog");
  const confirm = dialog.getByRole("button", { name: "Purge permanently" });
  await expect(confirm).toBeDisabled();
  await dialog.getByLabel(/confirm/i).fill(GRACE_DISK);
  await expect(confirm).toBeEnabled();
  await confirm.click();

  await expect(page.getByText("Purge scheduled.")).toBeVisible();
  await expect(graceRow).toContainText("Purging");
});
