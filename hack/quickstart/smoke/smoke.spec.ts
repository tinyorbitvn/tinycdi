import { expect, test } from "@playwright/test";

// DEV-ONLY credentials of the throwaway Keycloak realm inside the kind
// cluster (hack/quickstart/realm-export.json, printed by up.sh).
const USER = process.env.TCDI_QS_USER ?? "demo";
const PASSWORD = process.env.TCDI_QS_PASSWORD ?? "tcdi-demo-dev-only";
const SESSION_DOMAIN =
  process.env.TCDI_QS_SESSION_DOMAIN ??
  `session.${process.env.TCDI_QS_DOMAIN ?? "tcdi.localtest.me"}`;

test("log in, create a browser workspace and see the session frame load", async ({ page }) => {
  // 1. Anonymous visit -> portal login -> dev Keycloak -> back on the portal.
  await page.goto("/");
  await expect(page.locator("#username")).toBeVisible();
  await expect(page).toHaveURL(/^https:\/\/keycloak\./);
  await page.locator("#username").fill(USER);
  await page.locator("#password").fill(PASSWORD);
  await page.locator("#kc-login").click();
  await expect(page.getByRole("heading", { name: "Workspaces", exact: true })).toBeVisible();

  // 2. Create a workspace from the seeded Browser template.
  await page.getByRole("link", { name: "New workspace" }).click();
  await page.waitForURL("**/workspaces/new");
  await page.locator('input[name="name"]').fill(`smoke-${Date.now().toString(36)}`);
  const browserTemplate = page
    .locator('select[name="template"] option', { hasText: /browser/i })
    .first();
  await expect(browserTemplate).toBeAttached();
  await page
    .locator('select[name="template"]')
    .selectOption((await browserTemplate.getAttribute("value")) ?? "");
  await page.getByRole("button", { name: "Create workspace" }).click();
  await page.waitForURL(/\/workspaces\/ws_/);
  const workspaceId = decodeURIComponent(new URL(page.url()).pathname.split("/").pop() ?? "");

  // 3. The Connect link appears once the runtime pod is up and the gateway
  //    reports the stream endpoint ready (cold start: image already pulled
  //    by up.sh, so this is the boot of the desktop itself).
  const connect = page.getByRole("link", { name: "Connect" });
  await expect(connect).toBeVisible({ timeout: 10 * 60_000 });
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
});
