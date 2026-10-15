import { test, expect, type Page, type APIRequestContext } from "@playwright/test";

// Retained-data flows against the contract mock (tests/mock-api): portal API
// :4310 via the Vite /v1 proxy. Fixture values mirror
// internal/api/openapi.yaml examples.

const MOCK = process.env.PW_MOCK_API ?? "http://127.0.0.1:4310";
const RETAINED_ID = "rd_01J4Z9W2PFK8G4TQ3M7H1R5N0A";

async function login(page: Page) {
  await page.goto("/v1/login?returnTo=/data");
  await page.getByRole("button", { name: "Log in with SSO" }).click();
  await page.waitForURL("/data");
  await expect(page.getByRole("heading", { name: "Retained data" })).toBeVisible();
}

async function patchData(request: APIRequestContext, id: string, patch: object) {
  const res = await request.post(`${MOCK}/_control/data/${id}`, { data: patch });
  expect(res.ok()).toBeTruthy();
}

test.beforeEach(async ({ request }) => {
  const res = await request.post(`${MOCK}/_control/reset`);
  expect(res.ok()).toBeTruthy();
});

test("list -> attach -> new workspace", async ({ page, request }) => {
  await login(page);

  const row = page.getByRole("row", { name: new RegExp(RETAINED_ID) });
  await expect(row).toContainText("Retained");
  await row.getByRole("button", { name: "Attach" }).click();

  const dialog = page.getByRole("dialog", { name: /attach/i });
  await dialog.getByLabel(/workspace name/i).fill("restored-desktop");
  await dialog.getByRole("button", { name: "Attach disk" }).click();

  // Attach creates a new workspace and sends the user to it.
  await page.waitForURL(/\/workspaces\/ws_/);
  const wsId = page.url().split("/").pop()!;

  const reqs = await (await request.get(`${MOCK}/_control/requests`)).json();
  const attachReq = reqs.requests.find(
    (r: { method: string; path: string }) =>
      r.method === "POST" && r.path === `/v1/data/${RETAINED_ID}:attach`,
  );
  expect(attachReq).toBeTruthy();
  expect(attachReq.headers["idempotency-key"]).toBeTruthy();
  expect(attachReq.headers["x-csrf-token"]).toBe("csrf-token-01J4ZD");

  // The record is claimed by the new workspace.
  await page.goto("/data");
  await expect(
    page.getByRole("row", { name: new RegExp(RETAINED_ID) }),
  ).toContainText("Attaching");
  expect(wsId).toMatch(/^ws_/);
});

test("list -> purge -> row gone", async ({ page, request }) => {
  await login(page);

  const row = page.getByRole("row", { name: new RegExp(RETAINED_ID) });
  await row.getByRole("button", { name: "Purge" }).click();

  const dialog = page.getByRole("alertdialog");
  const confirm = dialog.getByRole("button", { name: "Purge permanently" });
  await expect(confirm).toBeDisabled();
  await dialog.getByLabel(/confirm/i).fill("not-the-id");
  await expect(confirm).toBeDisabled();
  await dialog.getByLabel(/confirm/i).fill(RETAINED_ID);
  await expect(confirm).toBeEnabled();
  await confirm.click();

  // Purge accepted: the record reports Purging, then leaves the inventory
  // once the operator completes the destroy.
  await expect(
    page.getByRole("row", { name: new RegExp(RETAINED_ID) }),
  ).toContainText("Purging");

  await patchData(request, RETAINED_ID, { state: "Purged" });
  await page.reload();
  await expect(
    page.getByRole("row", { name: new RegExp(RETAINED_ID) }),
  ).toHaveCount(0);
});

// Visual check (plan Task 14): the admin Scope select is a labelled field,
// so in the header's actions slot it made the title row as tall as label +
// control and pushed the description far below the title.
test("admin scope select keeps the description under the title", async ({ page, request }) => {
  const me = await request.post(`${MOCK}/_control/admin/me`, {
    data: { roles: ["user", "tenant-admin"] },
  });
  expect(me.ok()).toBeTruthy();
  await login(page);
  await expect(page.getByLabel("Scope")).toBeVisible();
  const title = (await page.locator(".tc-page__title").boundingBox())!;
  const description = (await page.locator(".tc-page__description").boundingBox())!;
  expect(description.y - (title.y + title.height)).toBeLessThanOrEqual(16);
});

// Visual check (plan Task 14): a visually hidden column header is
// position:absolute; unless the table wrapper contains it, it escapes the
// wrapper's own scroll area and the whole page scrolls sideways at 390px.
test.describe("at 390px", () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test("the retained-data page does not scroll sideways", async ({ page }) => {
    await login(page);
    await expect(page.getByRole("row", { name: new RegExp(RETAINED_ID) })).toBeVisible();
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow).toBeLessThanOrEqual(0);
  });
});
