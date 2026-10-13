import { mkdirSync, writeFileSync } from "node:fs";
import { expect, test, type Page, type Response } from "@playwright/test";

// Cold first connect (F2): N fresh workspaces, each timed from the moment
// the portal offers Connect (the workspace is Ready) to the moment the
// session reports Connected. After every run it rewrites
// test-results/cold-connect.json, and it fails when p95 >= TCDI_QS_COLD_P95_MS.
const RUNS = Number(process.env.TCDI_QS_COLD_RUNS ?? "20");
const P95_MS = Number(process.env.TCDI_QS_COLD_P95_MS ?? "20000");
const USER = process.env.TCDI_QS_USER ?? "demo";
const PASSWORD = process.env.TCDI_QS_PASSWORD ?? "tcdi-demo-dev-only";
const SESSION_DOMAIN =
  process.env.TCDI_QS_SESSION_DOMAIN ??
  `session.${process.env.TCDI_QS_DOMAIN ?? "tcdi.localtest.me"}`;

type Run = {
  readyToConnectedMs: number; // Connect visible -> portal badge "Connected"
  mintMs: number; //            ... -> POST /v1/workspaces/{id}/connections answered
  launchStatus: number; //      status of the first https://<ws>.<session>/v1/launch response (303 = redeemed)
  launchMs: number; //          ... -> that response
  frameMs: number; //           ... -> the session frame is on the workspace origin
  canvasMs: number; //          ... -> the KasmVNC canvas is visible
  upstream5xx: number; //       responses >= 500 from the workspace origin before Connected (gateway 502 = dial/redeem gave up)
  connectionPosts: number; //   ticket mints during the run (> 1: the watch or a cold re-mint fired)
  wallMs: number; //            create -> delete accepted (CI budget input)
};

async function login(page: Page) {
  await page.goto("/");
  await expect(page.locator("#username")).toBeVisible();
  await page.locator("#username").fill(USER);
  await page.locator("#password").fill(PASSWORD);
  await page.locator("#kc-login").click();
  await page.getByRole("heading", { name: "Workspaces", exact: true }).toBeVisible();
}

// Creates an Ephemeral browser workspace (no retained disk piles up over
// the runs). A just-deleted workspace's running-quota slot is held until
// the backend's recovery pass proves the runtime gone, so the API answers a
// retryable 409 details.reason=release_pending: retry only that, as
// smoke.spec.ts createWorkspaceOnTemplate does.
async function createWorkspace(page: Page, name: string): Promise<string> {
  const deadline = Date.now() + 120_000;
  for (;;) {
    await page.goto("/workspaces/new");
    await page.locator('input[name="name"]').fill(name);
    const option = page.locator('select[name="template"] option', { hasText: /browser/i }).first();
    await expect(option).toBeAttached();
    await page.locator('select[name="template"]').selectOption((await option.getAttribute("value")) ?? "");
    await page.locator('select[name="dataPolicy"]').selectOption("Ephemeral");
    const [resp] = await Promise.all([
      page.waitForResponse((r) => r.url().endsWith("/v1/workspaces") && r.request().method() === "POST"),
      page.getByRole("button", { name: "Create workspace" }).click(),
    ]);
    if (resp.status() === 201) {
      await page.waitForURL(/\/workspaces\/ws_/);
      return decodeURIComponent(new URL(page.url()).pathname.split("/").pop() ?? "");
    }
    const body = (await resp.json().catch(() => ({}))) as { error?: { details?: { reason?: string } } };
    expect(
      body?.error?.details?.reason === "release_pending" && Date.now() < deadline,
      `workspace create refused (${resp.status()}): ${JSON.stringify(body)}`,
    ).toBeTruthy();
    await page.waitForTimeout(5_000);
  }
}

// Wait for the accepted DELETE before any navigation (a goto that beats the
// in-flight request aborts it). The next create waits out release_pending.
async function deleteWorkspace(page: Page, id: string) {
  await page.goto(`/workspaces/${encodeURIComponent(id)}`);
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  const [resp] = await Promise.all([
    page.waitForResponse((r) => r.url().endsWith(`/v1/workspaces/${id}`) && r.request().method() === "DELETE"),
    page.getByRole("button", { name: "Confirm delete" }).click(),
  ]);
  expect(resp.status(), "delete is accepted").toBe(202);
}

// Nearest-rank percentile: the ceil(0.95 n)-th smallest value.
function p95(xs: number[]): number {
  const s = [...xs].sort((a, b) => a - b);
  return s[Math.min(s.length - 1, Math.ceil(0.95 * s.length) - 1)];
}

function writeReport(runs: Run[]) {
  const report = { runs, p95Ms: p95(runs.map((r) => r.readyToConnectedMs)) };
  mkdirSync("test-results", { recursive: true });
  writeFileSync("test-results/cold-connect.json", JSON.stringify(report, null, 2));
  return report;
}

test("cold first connect p95", async ({ page }) => {
  await login(page);
  const runs: Run[] = [];
  for (let i = 0; i < RUNS; i++) {
    const created = Date.now();
    const id = await createWorkspace(page, `cold-${Date.now().toString(36)}-${i}`);
    const origin = `https://${id.replaceAll("_", "-").toLowerCase()}.${SESSION_DOMAIN}`;
    let upstream5xx = 0;
    let connectionPosts = 0;
    const onResponse = (r: Response) => {
      if (r.url().startsWith(`${origin}/`) && r.status() >= 500) upstream5xx++;
      if (r.request().method() === "POST" && r.url().endsWith(`/v1/workspaces/${id}/connections`)) connectionPosts++;
    };
    page.on("response", onResponse);

    const connect = page.getByRole("link", { name: "Connect" });
    await expect(connect).toBeVisible({ timeout: 10 * 60_000 });
    const t0 = Date.now(); // the portal offers Connect: the workspace is Ready
    const mint = page.waitForResponse((r) => r.url().endsWith(`/v1/workspaces/${id}/connections`) && r.request().method() === "POST");
    const launch = page.waitForResponse((r) => r.url().startsWith(`${origin}/v1/launch`), { timeout: 120_000 });
    await connect.click();
    await mint;
    const mintMs = Date.now() - t0;
    const launchResp = await launch;
    const launchMs = Date.now() - t0;
    const inSession = () => page.frames().find((f) => f.url().startsWith(`${origin}/`) && !f.url().includes("/v1/launch"));
    await expect.poll(() => inSession() !== undefined, { timeout: 120_000, intervals: [100] }).toBe(true);
    const frameMs = Date.now() - t0;
    await expect(inSession()!.locator("canvas:visible").first()).toBeVisible({ timeout: 120_000 });
    const canvasMs = Date.now() - t0;
    await expect(page.getByText("Connected", { exact: true })).toBeVisible({ timeout: 120_000 });
    const readyToConnectedMs = Date.now() - t0;
    page.off("response", onResponse);

    await deleteWorkspace(page, id);
    runs.push({
      readyToConnectedMs, mintMs, launchStatus: launchResp.status(), launchMs, frameMs, canvasMs,
      upstream5xx, connectionPosts, wallMs: Date.now() - created,
    });
    writeReport(runs); // after every run: a timeout still leaves the data
  }
  const report = writeReport(runs);
  console.log(`cold-connect p95=${report.p95Ms}ms over ${runs.length} runs (gate < ${P95_MS} ms)`);
  expect(report.p95Ms, "cold-connect p95 (F2 gate)").toBeLessThan(P95_MS);
});
