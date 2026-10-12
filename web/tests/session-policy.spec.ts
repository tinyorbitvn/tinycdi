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
  // File transfer is the inert v1.3 channel: present, disabled.
  await expect(page.getByLabel("File transfer", { exact: true })).toBeDisabled();

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

test("a stale version answers 412 and reloads the current ceiling", async ({
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
  // version: the save must 412 and reload their value, not clobber it.
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
