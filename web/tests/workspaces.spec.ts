import { test, expect, type Page, type APIRequestContext } from "@playwright/test";

// Portal UI against the contract mock (tests/mock-api): portal API :4310 via
// the Vite /v1 proxy; session origin :4311. Fixture values mirror
// internal/api/openapi.yaml examples.

const MOCK = "http://127.0.0.1:4310";
const SEED_WS = "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E";
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

test("create -> progress -> connect POSTs the ticket to the session origin", async ({
  page,
  request,
  context,
}) => {
  await login(page);
  await page.getByRole("link", { name: "New workspace" }).click();
  await page.locator('input[name="name"]').fill("e2e-desktop");
  await page.locator('select[name="template"]').selectOption(TEMPLATE_ID);
  await page.getByRole("button", { name: "Create workspace" }).click();

  await page.waitForURL(/\/workspaces\/ws_/);
  const wsId = page.url().split("/").pop()!;
  await expect(page.locator(".phase")).toHaveText("Provisioning");

  // Mutations carried the CSRF header + an Idempotency-Key.
  const reqs = await (await request.get(`${MOCK}/_control/requests`)).json();
  const createReq = reqs.requests.find(
    (r: { method: string; path: string }) => r.method === "POST" && r.path === "/v1/workspaces",
  );
  expect(createReq.headers["x-csrf-token"]).toBe("csrf-token-01J4ZD");
  expect(createReq.headers["idempotency-key"]).toBeTruthy();

  // Runtime finishes provisioning.
  await patchWorkspace(request, wsId, {
    phase: "Ready",
    desiredState: "Running",
    conditions: READY_CONDITIONS,
  });
  await expect(page.getByRole("button", { name: "Connect" })).toBeEnabled({
    timeout: 15_000,
  });

  const [popup] = await Promise.all([
    context.waitForEvent("page"),
    page.getByRole("button", { name: "Connect" }).click(),
  ]);
  await popup.waitForLoadState("load");

  // Landed on the session origin at a clean URL — the ticket rode in the
  // POST body only.
  expect(popup.url()).toContain(":4311/desktop/");
  expect(popup.url()).not.toContain("tkt_");
  const launches = await (await request.get(`${MOCK}/_control/launchRequests`)).json();
  expect(launches.requests).toHaveLength(1);
  const ticket = new URLSearchParams(launches.requests[0].rawBody).get("ticket");
  expect(ticket).toBeTruthy();
  const ticketValue = ticket!;

  // Ticket never reached any URL or web storage.
  expect(popup.url()).not.toContain(ticketValue);
  expect(page.url()).not.toContain(ticketValue);
  for (const p of [page, popup]) {
    const leaked = await p.evaluate(
      (t) => JSON.stringify(localStorage).includes(t) || JSON.stringify(sessionStorage).includes(t),
      ticketValue,
    );
    expect(leaked).toBe(false);
  }
});

test("CONNECTION_IN_USE prompts for explicit takeover", async ({ page, request, context }) => {
  await patchWorkspace(request, SEED_WS, {
    phase: "Ready",
    desiredState: "Running",
    conditions: READY_CONDITIONS,
  });
  await request.post(`${MOCK}/_control/lease`, {
    data: { workspaceId: SEED_WS, active: true },
  });
  await login(page);
  await page.goto(`/workspaces/${SEED_WS}`);

  await page.getByRole("button", { name: "Connect" }).click();
  await expect(page.getByRole("dialog")).toContainText("already connected");
  const [popup] = await Promise.all([
    context.waitForEvent("page"),
    page.getByRole("button", { name: "Take over session" }).click(),
  ]);
  await popup.waitForLoadState("load");
  expect(popup.url()).toContain(":4311/desktop/");
});

test("stop and restart from the detail page", async ({ page, request }) => {
  await patchWorkspace(request, SEED_WS, {
    phase: "Ready",
    desiredState: "Running",
    conditions: READY_CONDITIONS,
  });
  await login(page);
  await page.goto(`/workspaces/${SEED_WS}`);
  await expect(page.locator(".phase")).toHaveText("Ready", { timeout: 15_000 });

  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await expect(page.locator(".phase")).toHaveText("Stopping");
  await patchWorkspace(request, SEED_WS, { phase: "Stopped" });
  await expect(page.getByRole("button", { name: "Start" })).toBeVisible({ timeout: 15_000 });

  await page.getByRole("button", { name: "Start" }).click();
  await expect(page.locator(".phase")).toHaveText("Provisioning");
  await patchWorkspace(request, SEED_WS, {
    phase: "Ready",
    desiredState: "Running",
    conditions: READY_CONDITIONS,
  });
  await expect(page.getByRole("button", { name: "Connect" })).toBeEnabled({ timeout: 15_000 });
});

test("Delete workspace vs Purge retained disk", async ({ page }) => {
  await login(page);
  await page.goto(`/workspaces/${SEED_WS}`);
  await page.getByRole("button", { name: "Delete" }).click();
  const dialog = page.getByRole("dialog", { name: "delete workspace" });
  await expect(dialog).toContainText("moves to the retained inventory");
  await dialog.getByRole("button", { name: "Confirm delete" }).click();
  await page.waitForURL("/");
  await expect(page.locator(`tr:has-text("research-desktop") .phase`)).toHaveText("Terminating");

  // Purge is a separate, typed-confirmation flow on the retained-data page.
  await page.getByRole("link", { name: "Retained data" }).click();
  await page.getByRole("button", { name: "Purge" }).click();
  const purgeDialog = page.getByRole("dialog", { name: "purge retained disk" });
  const confirm = purgeDialog.getByRole("button", { name: "Purge permanently" });
  await purgeDialog.locator('input[name="purge-confirm"]').fill("wrong-name");
  await expect(confirm).toBeDisabled();
  await purgeDialog.locator('input[name="purge-confirm"]').fill("old-desktop");
  await expect(confirm).toBeEnabled();
  await confirm.click();
  await expect(page.getByRole("table", { name: "retained data" })).toContainText("Purging");
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
    data: { id: "tpl_01J4ZB3N1RXD7P2V8W5K0H6Q4M" },
  });
  await page.getByRole("button", { name: "Create workspace" }).click();
  await expect(page.getByRole("alert")).toContainText("INVALID_TEMPLATE");
});
