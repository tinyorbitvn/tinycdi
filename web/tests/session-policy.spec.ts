import { test, expect, type Page, type APIRequestContext } from "@playwright/test";

// Session policy page against the contract mock (tests/mock-api): the
// tenant clipboard ceiling of ADR 0010 — a ceiling narrows every
// template's own clipboard policy, never widens it. The page shows the
// live effective-policy preview (template policy ∧ selected ceiling).

const MOCK = process.env.PW_MOCK_API ?? "http://127.0.0.1:4310";

async function login(page: Page, returnTo = "/admin/session-policy") {
  await page.goto(`/v1/login?returnTo=${encodeURIComponent(returnTo)}`);
  await page.getByRole("button", { name: "Log in with SSO" }).click();
  await page.waitForURL(new RegExp(`${returnTo.replace(/\//g, "\\/")}$`));
}

async function makeAdmin(request: APIRequestContext) {
  const me = await request.post(`${MOCK}/_control/admin/me`, {
    data: { roles: ["user", "tenant-admin"] },
  });
  expect(me.ok()).toBeTruthy();
}

test.beforeEach(async ({ request }) => {
  const res = await request.post(`${MOCK}/_control/reset`);
  expect(res.ok()).toBeTruthy();
});

test("admin sets a clipboard ceiling and the effective preview updates", async ({
  page,
  request,
}) => {
  await makeAdmin(request);
  await login(page);

  const clipboard = page.getByLabel("Clipboard", { exact: true });
  await expect(clipboard).toHaveValue("FollowTemplate");
  // No row yet: every channel reports FollowTemplate.
  await expect(page.getByText("No ceiling is set", { exact: false })).toBeVisible();
  await expect(page.getByLabel("File transfer", { exact: true })).toHaveValue("FollowTemplate");

  // linux-firefox-desktop's template policy is Bidirectional: the preview
  // shows the template's own policy while the ceiling follows it.
  const preview = page.getByRole("table", {
    name: "Effective clipboard policy by template",
  });
  const linuxRow = preview.getByRole("row", { name: /linux-firefox-desktop/ });
  await expect(linuxRow.getByRole("cell", { name: "Both directions" }).first()).toBeVisible();

  // The select carries the five ceiling values, FollowTemplate included.
  const optionValues = await clipboard.locator("option").evaluateAll((os) =>
    os.map((o) => (o as HTMLOptionElement).value),
  );
  expect(optionValues).toEqual([
    "FollowTemplate",
    "Disabled",
    "Send",
    "Receive",
    "Bidirectional",
  ]);

  // Picking a ceiling updates the live preview before anything is saved.
  await clipboard.selectOption("Disabled");
  await expect(linuxRow.getByRole("cell", { name: "Disabled" }).first()).toBeVisible();
  await page.getByRole("button", { name: "Save ceiling" }).click();
  await expect(page.getByText("A ceiling is set for this tenant.")).toBeVisible();
  await expect(clipboard).toHaveValue("Disabled");

  // Resetting to Follow template deletes the row server-side: the view
  // reports no ceiling again and the preview reverts to template policy.
  await clipboard.selectOption("FollowTemplate");
  await page.getByRole("button", { name: "Save ceiling" }).click();
  await expect(page.getByText("No ceiling is set", { exact: false })).toBeVisible();
  await expect(linuxRow.getByRole("cell", { name: "Both directions" }).first()).toBeVisible();
});

test("a stale ETag answers 412 and reloads the current ceiling", async ({
  page,
  request,
}) => {
  await makeAdmin(request);
  await request.post(`${MOCK}/_control/admin/session-policy`, {
    data: { clipboard: "Send" },
  });
  await login(page);

  const clipboard = page.getByLabel("Clipboard", { exact: true });
  await expect(clipboard).toHaveValue("Send");

  // Someone else writes the ceiling while this page holds the stale
  // ETag: the save must 412 and reload their value, not clobber it.
  await clipboard.selectOption("Disabled");
  await request.post(`${MOCK}/_control/admin/session-policy`, {
    data: { clipboard: "Receive" },
  });
  await page.getByRole("button", { name: "Save ceiling" }).click();
  await expect(
    page.getByText("Someone else changed these limits", { exact: false }),
  ).toBeVisible();
  await expect(clipboard).toHaveValue("Receive");
});

test("a regular user cannot see the session policy page", async ({ page }) => {
  await login(page, "/");
  await page.goto("/admin/session-policy");
  await page.waitForURL(/\/workspaces\/?$/);
  const res = await page.request.get(`/v1/admin/tenants/acme/session-policy`);
  expect(res.status()).toBe(403);
});

test("apply now lists looser workspaces and restarts them", async ({ page, request }) => {
  await makeAdmin(request);
  await request.post(`${MOCK}/_control/admin/session-policy`, { data: { clipboard: "Disabled" } });
  await login(page);

  await page.getByRole("button", { name: "Apply now" }).click();
  const dialog = page.getByRole("dialog", { name: "Apply the ceiling to running workspaces" });
  await expect(dialog.getByRole("row", { name: /research-desktop/ })).toBeVisible();
  await expect(dialog.getByRole("row", { name: /build-box/ }).getByText("Live session")).toBeVisible();

  await dialog.getByLabel("Also restart workspaces someone is using right now").check();
  await dialog.getByRole("button", { name: "Restart affected workspaces" }).click();
  await expect(dialog.getByRole("row", { name: /research-desktop/ }).getByText("Restarted")).toBeVisible();
  await expect(dialog.getByRole("row", { name: /build-box/ }).getByText("Restarted")).toBeVisible();
  await expect(dialog.getByText("2 restarted, 0 skipped.")).toBeVisible();
});

test("apply now has nothing to do when every workspace follows the ceiling", async ({ page, request }) => {
  await makeAdmin(request);
  await request.post(`${MOCK}/_control/admin/session-policy`, { data: { clipboard: "Bidirectional" } });
  await login(page);
  await page.getByRole("button", { name: "Apply now" }).click();
  await expect(
    page.getByRole("dialog").getByText("Every running workspace already follows the ceiling."),
  ).toBeVisible();
  await expect(page.getByRole("dialog").getByRole("button", { name: "Restart affected workspaces" })).toHaveCount(0);
});

test("admin sets an Upload-only file-transfer ceiling with a size bound", async ({ page, request }) => {
  await makeAdmin(request);
  await login(page);

  const ft = page.getByLabel("File transfer", { exact: true });
  await expect(ft).toHaveValue("FollowTemplate");
  await ft.selectOption("Upload");
  await page.getByLabel("Largest file (MiB)").fill("10");

  // linux-firefox-desktop's file-transfer policy is Bidirectional, so the
  // preview shows Bidirectional ∧ Upload = Upload before anything is saved.
  const preview = page.getByRole("table", { name: "Effective clipboard policy by template" });
  const linuxRow = preview.getByRole("row", { name: /linux-firefox-desktop/ });
  await expect(linuxRow.getByRole("cell", { name: "Upload only" }).first()).toBeVisible();

  await page.getByRole("button", { name: "Save ceiling" }).click();
  await expect(page.getByText("A ceiling is set for this tenant.")).toBeVisible();

  await page.reload();
  await expect(page.getByLabel("File transfer", { exact: true })).toHaveValue("Upload");
  await expect(page.getByLabel("Largest file (MiB)")).toHaveValue("10");
});
