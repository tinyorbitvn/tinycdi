import { test, expect, type Page, type APIRequestContext } from "@playwright/test";

// Portal UI against the contract mock (tests/mock-api): portal API :4310 via
// the Vite /v1 proxy; session origin :4311. Fixture values mirror
// internal/api/openapi.yaml examples.

const MOCK = process.env.MOCK_PORTAL_ORIGIN ?? "http://127.0.0.1:4310";
const TEMPLATE_ID = "tpl_01J4ZB3N1RXD7P2V8W5K0H6Q4M";

const READY_CONDITIONS = [
  { type: "Admitted", status: "True", reason: "QuotaReserved", lastTransitionTime: "2026-09-30T10:00:00Z" },
  { type: "StorageReady", status: "True", reason: "VolumeBound", lastTransitionTime: "2026-09-30T10:01:00Z" },
  { type: "RuntimeReady", status: "True", reason: "RuntimeUp", lastTransitionTime: "2026-09-30T10:02:00Z" },
  { type: "ConnectionReady", status: "True", reason: "StreamEndpointUp", lastTransitionTime: "2026-09-30T10:02:30Z" },
];

async function login(page: Page) {
  await page.goto("/v1/login?returnTo=/");
  await page.getByRole("button", { name: "Log in with SSO" }).click();
  await page.waitForURL("/");
  await expect(page.getByRole("heading", { name: "Workspaces" })).toBeVisible();
}

async function patchWorkspace(request: APIRequestContext, id: string, patch: object) {
  const res = await request.post(`${MOCK}/_control/workspaces/${id}`, { data: patch });
  expect(res.ok()).toBeTruthy();
}

test.beforeEach(async ({ request }) => {
  const res = await request.post(`${MOCK}/_control/reset`);
  expect(res.ok()).toBeTruthy();
});

test("unauthenticated users are redirected to /v1/login", async ({ page }) => {
  await page.goto("/");
  await expect(page).toHaveURL(/\/v1\/login\?returnTo=/, { timeout: 10_000 });
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
});

test("create -> ready -> session route -> stop -> delete", async ({ page, request }) => {
  await login(page);

  // Template catalog is reachable from the list page and preselects a
  // template on the create form.
  await page.getByRole("link", { name: "New workspace" }).click();
  await page.waitForURL("/workspaces/new");
  await page.locator('input[name="name"]').fill("e2e-desktop");
  await page.locator('select[name="template"]').selectOption(TEMPLATE_ID);
  await page.getByRole("button", { name: "Create workspace" }).click();

  await page.waitForURL(/\/workspaces\/ws_/);
  const wsId = page.url().split("/").pop()!;
  // The lifecycle step panel takes over while the create runs (V3.27).
  await expect(page.getByRole("region", { name: "Creating e2e-desktop" })).toBeVisible({
    timeout: 15_000,
  });

  // The create carried CSRF + an Idempotency-Key.
  const reqs = await (await request.get(`${MOCK}/_control/requests`)).json();
  const createReq = reqs.requests.find(
    (r: { method: string; path: string }) => r.method === "POST" && r.path === "/v1/workspaces",
  );
  expect(createReq.headers["x-csrf-token"]).toBe("csrf-token-01J4ZD");
  expect(createReq.headers["idempotency-key"]).toBeTruthy();

  // Runtime finishes provisioning; the Connect link appears once
  // ConnectionReady reports True (phase alone is not enough).
  await patchWorkspace(request, wsId, {
    phase: "Ready",
    desiredState: "Running",
    conditions: READY_CONDITIONS,
  });
  const connect = page.getByRole("link", { name: "Connect" });
  await expect(connect).toBeVisible({ timeout: 15_000 });

  // Connect routes to the in-portal session view — the ticket POST into the
  // frame happens on the session page itself.
  await connect.click();
  await page.waitForURL(`/workspaces/${wsId}/session`);

  // Back to the detail page: stop then delete.
  await page.goto(`/workspaces/${wsId}`);
  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await expect(page.getByText("Stopping").first()).toBeVisible({ timeout: 15_000 });
  await patchWorkspace(request, wsId, { phase: "Stopped" });
  await expect(page.getByRole("button", { name: "Start" })).toBeVisible({ timeout: 15_000 });

  await page.getByRole("button", { name: "Delete" }).click();
  const dialog = page.getByRole("alertdialog");
  // The confirm dialog names the data consequence (Retain default here).
  await expect(dialog).toContainText("retained inventory");
  await dialog.getByRole("button", { name: "Confirm delete" }).click();

  // The page keeps watching the teardown until the API drops the row —
  // the delete panel replaces the connectable state (V3.27).
  await expect(page.getByRole("region", { name: "Deleting e2e-desktop" })).toBeVisible({
    timeout: 15_000,
  });
  await request.delete(`${MOCK}/_control/workspaces/${wsId}`);
  await page.waitForURL("/");
  await expect(page.getByText("'e2e-desktop' was deleted.")).toBeVisible({ timeout: 15_000 });
  await expect(
    page.getByRole("button", { name: "Your disk is in Retained data" }),
  ).toBeVisible();
});

test("stable codes surface: QUOTA_EXHAUSTED and INVALID_TEMPLATE", async ({ page, request }) => {
  await login(page);
  await page.getByRole("link", { name: "New workspace" }).click();
  await page.locator('input[name="name"]').fill("quota-test");
  await page.locator('select[name="template"]').selectOption(TEMPLATE_ID);

  await request.post(`${MOCK}/_control/quota`, { data: { exhausted: true } });
  await page.getByRole("button", { name: "Create workspace" }).click();
  await expect(page.getByRole("alert")).toContainText("QUOTA_EXHAUSTED");
  await expect(page.getByRole("alert")).toContainText("Quota exhausted");

  await request.post(`${MOCK}/_control/quota`, { data: { exhausted: false } });
  await request.post(`${MOCK}/_control/templates/invalidate`, {
    data: { id: TEMPLATE_ID },
  });
  await page.getByRole("button", { name: "Create workspace" }).click();
  await expect(page.getByRole("alert")).toContainText("INVALID_TEMPLATE");
});
