import { execFileSync } from "node:child_process";
import { expect, test, type Page } from "@playwright/test";

// DEV-ONLY credentials of the throwaway Keycloak realm inside the kind
// cluster (hack/quickstart/realm-export.json, printed by up.sh).
const USER = process.env.TCDI_QS_USER ?? "demo";
const PASSWORD = process.env.TCDI_QS_PASSWORD ?? "tcdi-demo-dev-only";
const SESSION_DOMAIN =
  process.env.TCDI_QS_SESSION_DOMAIN ??
  `session.${process.env.TCDI_QS_DOMAIN ?? "tcdi.localtest.me"}`;

// The tenant namespace holding the runtime pods (hack/quickstart/values.yaml)
// and the kubeconfig up.sh printed (KUBECONFIG, inherited by kubectl).
const TENANT_NAMESPACE = process.env.TCDI_QS_TENANT_NAMESPACE ?? "tinycdi-tenant-a";
// Runtime home mount of the Linux runtime (internal/runtime/linux homeDir).
const HOME_DIR = "/home/workspace";

// Anonymous visit -> portal login -> dev Keycloak -> back on the portal.
async function login(page: Page) {
  await page.goto("/");
  await expect(page.locator("#username")).toBeVisible();
  await expect(page).toHaveURL(/^https:\/\/keycloak\./);
  await page.locator("#username").fill(USER);
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
  await row.getByRole("button", { name: "Attach" }).click();
  await page.getByRole("button", { name: "Attach disk" }).click();
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
