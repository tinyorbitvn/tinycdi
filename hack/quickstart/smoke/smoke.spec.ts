import { execFileSync } from "node:child_process";
import { expect, test, type Page } from "@playwright/test";

// DEV-ONLY credentials of the throwaway Keycloak realm inside the kind
// cluster (hack/quickstart/realm-export.json, printed by up.sh). The
// noquota user maps to a tenant with no quota row (B5.4).
const USER = process.env.TCDI_QS_USER ?? "demo";
const PASSWORD = process.env.TCDI_QS_PASSWORD ?? "tcdi-demo-dev-only";
const NOQUOTA_USER = process.env.TCDI_QS_NOQUOTA_USER ?? "demo-noquota";
const SESSION_DOMAIN =
  process.env.TCDI_QS_SESSION_DOMAIN ??
  `session.${process.env.TCDI_QS_DOMAIN ?? "tcdi.localtest.me"}`;

// The tenant namespace holding the runtime pods (hack/quickstart/values.yaml)
// and the kubeconfig up.sh printed (KUBECONFIG, inherited by kubectl).
const TENANT_NAMESPACE = process.env.TCDI_QS_TENANT_NAMESPACE ?? "tinycdi-tenant-a";
// Runtime home mount of the Linux runtime (internal/runtime/linux homeDir).
const HOME_DIR = "/home/workspace";

// Anonymous visit -> portal login -> dev Keycloak -> back on the portal.
async function login(page: Page, user = USER) {
  await page.goto("/");
  await expect(page.locator("#username")).toBeVisible();
  await expect(page).toHaveURL(/^https:\/\/keycloak\./);
  await page.locator("#username").fill(user);
  await page.locator("#password").fill(PASSWORD);
  await page.locator("#kc-login").click();
  await expect(page.getByRole("heading", { name: "Workspaces", exact: true })).toBeVisible();
}

// Create a workspace from the seeded Browser template; returns its id.
async function createBrowserWorkspace(
  page: Page,
  name: string,
  dataPolicy?: "Retain" | "Ephemeral",
): Promise<string> {
  await page.getByRole("link", { name: "New workspace" }).click();
  await page.waitForURL("**/workspaces/new");
  await page.locator('input[name="name"]').fill(name);
  const browserTemplate = page
    .locator('select[name="template"] option', { hasText: /browser/i })
    .first();
  await expect(browserTemplate).toBeAttached();
  await page
    .locator('select[name="template"]')
    .selectOption((await browserTemplate.getAttribute("value")) ?? "");
  if (dataPolicy) {
    await page.locator('select[name="dataPolicy"]').selectOption(dataPolicy);
  }
  await page.getByRole("button", { name: "Create workspace" }).click();
  await page.waitForURL(/\/workspaces\/ws_/);
  return workspaceIdFromUrl(page);
}

function workspaceIdFromUrl(page: Page): string {
  return decodeURIComponent(new URL(page.url()).pathname.split("/").pop() ?? "");
}

// The Connect link appears once the runtime pod is up and the gateway
// reports the stream endpoint ready.
async function waitForConnect(page: Page) {
  const connect = page.getByRole("link", { name: "Connect" });
  await expect(connect).toBeVisible({ timeout: 10 * 60_000 });
  return connect;
}

// The runtime pod of a platform workspace id (ws_<hex>): children carry the
// CR name (ws-<hex>) in the workspace-name label.
function runtimePod(workspaceId: string): string {
  const crName = workspaceId.replaceAll("_", "-").toLowerCase();
  const pod = kubectl(
    "get", "pods", "-l", `workspaces.cdi.tinyorbit.vn/workspace-name=${crName}`,
    "-o", "jsonpath={.items[0].metadata.name}",
  );
  expect(pod, `runtime pod of ${workspaceId}`).not.toBe("");
  return pod;
}

function kubectl(...args: string[]): string {
  return execFileSync("kubectl", ["-n", TENANT_NAMESPACE, ...args], {
    encoding: "utf8",
    timeout: 60_000,
  }).trim();
}

function inHome(workspaceId: string, script: string, ...args: string[]): string {
  return kubectl("exec", runtimePod(workspaceId), "-c", "desktop", "--", "sh", "-c", script, "_", ...args);
}

test("log in, create a browser workspace and see the session frame load", async ({ page }) => {
  // 1. Log in.
  await login(page);

  // 2. Create a workspace from the seeded Browser template.
  const workspaceId = await createBrowserWorkspace(page, `smoke-${Date.now().toString(36)}`);

  // 3. The Connect link appears once the runtime pod is up and the gateway
  //    reports the stream endpoint ready (cold start: image already pulled
  //    by up.sh, so this is the boot of the desktop itself).
  const connect = await waitForConnect(page);
  await connect.click();
  await page.waitForURL(`**/workspaces/${encodeURIComponent(workspaceId)}/session`);

  // 4. The portal embeds the desktop in an iframe on the workspace's own
  //    session host, ws-<hex>.<sessionDomain>; wait for that frame to load
  //    the streaming client (KasmVNC's noVNC display canvas) and the portal to report
  //    the stream as Connected.
  const label = workspaceId.replaceAll("_", "-").toLowerCase();
  const origin = `https://${label}.${SESSION_DOMAIN}`;
  await expect
    .poll(
      () =>
        page
          .frames()
          .some((f) => f.url().startsWith(`${origin}/`) && !f.url().includes("/v1/launch")),
      { timeout: 120_000, message: `session frame on ${origin}` },
    )
    .toBe(true);
  const frame = page
    .frames()
    .find((f) => f.url().startsWith(`${origin}/`) && !f.url().includes("/v1/launch"));
  expect(frame, "session frame").toBeDefined();
  await expect(frame!.locator("canvas:visible").first()).toBeVisible({ timeout: 120_000 });
  await expect(page.getByText("Connected", { exact: true })).toBeVisible({ timeout: 120_000 });
  await page.screenshot({ path: "test-results/session.png" });

  // B5.4 (permanent): the embedded session must hold for 120 seconds — no
  // second launch ticket and the same lease throughout; the FX-R18-era
  // churn showed extra tickets and lease flips inside this window. The
  // badge is sampled, not awaited: expect().toContainText would retry for
  // up to 30 s and mask a drop, so each sample reads innerText once and
  // fails on the first state that is not exactly "Connected" (the badge's
  // real states are Connecting/Connected/Disconnected/Ended/In another
  // tab — none may appear mid-hold).
  const tickets: string[] = [];
  page.on("request", (r) => {
    if (r.method() === "POST" && /\/v1\/workspaces\/[^/]+\/connections$/.test(r.url())) {
      tickets.push(r.url());
    }
  });
  const connectionState = () =>
    page.evaluate(async (id) => {
      const r = await fetch(`/v1/workspaces/${encodeURIComponent(id)}/connection`);
      return (r.ok ? ((await r.json()) as { leaseActive?: boolean; leaseRef?: string }) : null);
    }, workspaceId);
  const start = await connectionState();
  expect(start?.leaseActive, "live lease at the start of the hold").toBe(true);
  const holdDeadline = Date.now() + 120_000;
  const status = page.locator(".tc-session__status");
  while (Date.now() < holdDeadline) {
    const badge = await status.innerText();
    expect(badge, `badge inside the 120 s hold`).toBe("Connected");
    await page.waitForTimeout(5_000);
  }
  const end = await connectionState();
  expect(end?.leaseRef, "lease must not change during a steady session").toBe(start?.leaseRef);
  expect(end?.leaseActive, "lease still live at the end of the hold").toBe(true);
  await expect(status).toHaveText(/Connected/);
  expect(tickets, "no second launch ticket in 120 s").toEqual([]);
});

// B5.4 (permanent): a fresh install can create workspaces — the seeded
// tenant's quota covers the creates above — while a tenant WITHOUT a quota
// row is refused every create with 409 QUOTA_NOT_CONFIGURED and the
// administrator message. The dev realm's demo-noquota user maps to
// tenant-noquota, which has no row (realm-export.json).
test("a tenant without quota is refused with the administrator message", async ({
  browser,
  baseURL,
}) => {
  // A separate browser context: the quota outcome depends on which user
  // logs in, and the realm user attribute decides the tenant.
  const context = await browser.newContext({
    baseURL,
    ignoreHTTPSErrors: true,
  });
  const page = await context.newPage();
  try {
    await login(page, NOQUOTA_USER);

    await page.getByRole("link", { name: "New workspace" }).click();
    await page.waitForURL("**/workspaces/new");
    await page.locator('input[name="name"]').fill(`noquota-${Date.now().toString(36)}`);
    const browserTemplate = page
      .locator('select[name="template"] option', { hasText: /browser/i })
      .first();
    await expect(browserTemplate).toBeAttached();
    await page
      .locator('select[name="template"]')
      .selectOption((await browserTemplate.getAttribute("value")) ?? "");
    const [resp] = await Promise.all([
      page.waitForResponse(
        (r) => r.url().endsWith("/v1/workspaces") && r.request().method() === "POST",
      ),
      page.getByRole("button", { name: "Create workspace" }).click(),
    ]);
    expect(resp.status(), "create must be refused").toBe(409);
    const body = (await resp.json().catch(() => null)) as { code?: string } | null;
    expect(body?.code).toBe("QUOTA_NOT_CONFIGURED");
    // The portal surfaces the API's actionable message, not a generic
    // error (the page also warns about the template's data policy — scope
    // to the quota alert).
    await expect(
      page.getByRole("alert").filter({ hasText: "No quota is configured" }),
    ).toContainText(
      "No quota is configured for your tenant. Ask an administrator to set one.",
    );
  } finally {
    await context.close();
  }
});

// FX-R20: the retained-data promise end to end. Write a file into a Retain
// workspace's home, delete the workspace, attach the retained disk to a NEW
// workspace and read the file back from the new pod. A new EMPTY volume
// (the FX-R20 defect: the record said Attached while the pod mounted a fresh
// disk) fails the final read.
test("attach a retained disk: the file written before delete is there after", async ({ page }) => {
  const stamp = Date.now().toString(36);
  const name = `rt-${stamp}`;
  const token = `attach-roundtrip-${stamp}`;
  const file = `${HOME_DIR}/attach-roundtrip.txt`;

  await login(page);

  // 1. A Retain workspace; write a file into its home once it is up.
  const sourceId = await createBrowserWorkspace(page, name, "Retain");
  await waitForConnect(page);
  inHome(sourceId, 'printf %s "$1" > "$2" && sync', token, file);
  expect(inHome(sourceId, 'cat "$1"', file)).toBe(token);

  // 2. Delete it: Retain moves the disk to the retained inventory.
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  await page.getByRole("button", { name: "Confirm delete" }).click();

  // 3. The disk shows up under Retained data (the inventory sync runs on an
  //    interval and the runtime teardown takes a moment).
  const row = page.getByRole("row", { name: new RegExp(name) });
  await expect
    .poll(
      async () => {
        await page.goto("/data");
        return row.count();
      },
      { timeout: 5 * 60_000, intervals: [5_000], message: `retained disk of ${name}` },
    )
    .toBeGreaterThan(0);

  // 4. Attach it to a new workspace and wait for that workspace to run.
  //    The deleted source's compute quota stays held until the backend's
  //    recovery pass proves the runtime gone (30 s cadence). An attach that
  //    lands inside that window is refused with 409 QUOTA_EXHAUSTED and
  //    details.reason=release_pending — a retryable refusal that resolves
  //    on its own. Retry only that specific response within 60 s; any
  //    other failure aborts immediately.
  await row.getByRole("button", { name: "Attach" }).click();
  const attachButton = page.getByRole("button", { name: "Attach disk" });
  const attachDeadline = Date.now() + 60_000;
  for (;;) {
    const [resp] = await Promise.all([
      page.waitForResponse(
        (r) => r.url().includes("/attach") && r.request().method() === "POST",
        { timeout: 60_000 },
      ),
      attachButton.click(),
    ]);
    if (resp.ok()) break;
    const body = (await resp.json().catch(() => null)) as {
      details?: { reason?: string };
    } | null;
    if (resp.status() !== 409 || body?.details?.reason !== "release_pending") {
      throw new Error(`attach failed: HTTP ${resp.status()} ${JSON.stringify(body)}`);
    }
    if (Date.now() > attachDeadline) {
      throw new Error("attach still refused with release_pending after 60 s");
    }
  }
  await page.waitForURL(/\/workspaces\/ws_/);
  const attachedId = workspaceIdFromUrl(page);
  expect(attachedId).not.toBe(sourceId);
  await waitForConnect(page);

  // 5. The new workspace's home is the old disk: the file is there.
  expect(inHome(attachedId, 'cat "$1"', file), "file written before delete").toBe(token);
  // ...and no second, empty home volume was created for it.
  const claims = kubectl(
    "get", "pod", runtimePod(attachedId),
    "-o", "jsonpath={.spec.volumes[*].persistentVolumeClaim.claimName}",
  ).split(/\s+/).filter(Boolean);
  expect(claims, "claims mounted by the attached workspace").toHaveLength(1);
});
