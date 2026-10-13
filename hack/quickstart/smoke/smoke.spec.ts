import { execFileSync } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { expect, test, type Frame, type Page } from "@playwright/test";

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

// ---------------------------------------------------------------------------
// Clipboard e2e: the template clipboardPolicy is enforced by the
// session server — the operator renders it into the pod env and the image
// entrypoint renders the KasmVNC DLP block from it. The oracle for "the
// workspace clipboard" is xclip inside the runtime pod on DISPLAY :1.
// ---------------------------------------------------------------------------

// A bookworm-built xclip: the runtime images carry its deps (libX11/libXmu)
// but not the tool itself, so the spec extracts the .deb out of a throwaway
// debian:bookworm-slim container and kubectl-cp's it into the pod's /tmp.
// Building from the same release the image is based on keeps the dynamic
// link (glibc 2.36) satisfiable.
let xclipHostPath: string | null = null;
function ensureXclip(): string {
  if (xclipHostPath) return xclipHostPath;
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "tcdi-xclip-"));
  execFileSync(
    "docker",
    [
      "run", "--rm", "-v", `${dir}:/out`, "debian:bookworm-slim",
      "sh", "-c",
      "apt-get update -qq && " +
        "DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --download-only xclip >/dev/null && " +
        "dpkg -x /var/cache/apt/archives/xclip_*.deb /out",
    ],
    { encoding: "utf8", timeout: 300_000 },
  );
  const bin = path.join(dir, "usr", "bin", "xclip");
  expect(fs.existsSync(bin), `xclip extracted to ${bin}`).toBe(true);
  xclipHostPath = bin;
  return bin;
}

// Copies xclip into the workspace pod once, then returns exec helpers.
function podClipboard(workspaceId: string) {
  const pod = runtimePod(workspaceId);
  kubectl("cp", ensureXclip(), `${pod}:/tmp/xclip`, "-c", "desktop");
  inHome(workspaceId, "chmod +x /tmp/xclip");
  const X = "DISPLAY=:1 XAUTHORITY=/home/workspace/.Xauthority";
  return {
    get(): string {
      try {
        return inHome(workspaceId, `${X} /tmp/xclip -o -selection clipboard 2>/dev/null`);
      } catch {
        return ""; // no CLIPBOARD owner yet — xclip exits non-zero
      }
    },
    set(text: string) {
      // xclip -in stays alive to SERVE the selection (X selections are
      // owned by a process). Run it in the background with the exec's
      // streams detached — foreground it never returns and kubectl exec
      // hits its 60 s timeout with the selection already set.
      inHome(
        workspaceId,
        `${X} printf %s "$1" | ${X} /tmp/xclip -in -selection clipboard >/dev/null 2>&1 &`,
        text,
      );
    },
  };
}

// Clones the seeded Browser template with a different clipboardPolicy so
// the non-Disabled case can run on a dedicated catalog entry (the seeded
// templates are all Disabled). The clone keeps the quickstart-safe spec —
// Firefox + no Localhost node profiles — verbatim.
function cloneBrowserTemplate(name: string, display: string, policy: string) {
  const list = JSON.parse(
    kubectl(
      "get", "workspacetemplates",
      "-l", "workspaces.cdi.tinyorbit.vn/catalog-name=browser",
      "-o", "json",
    ),
  ) as { items: Array<Record<string, any>> };
  const src = list.items[0];
  expect(src, "seeded browser template").toBeTruthy();
  const clone = {
    apiVersion: src.apiVersion,
    kind: src.kind,
    metadata: {
      name,
      namespace: TENANT_NAMESPACE,
      labels: { "workspaces.cdi.tinyorbit.vn/catalog-name": name },
      annotations: {
        ...(src.metadata.annotations ?? {}),
        "workspaces.cdi.tinyorbit.vn/display-name": display,
      },
    },
    spec: { ...src.spec, revision: `clip-${name}`, clipboardPolicy: policy },
  };
  const f = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "tcdi-tpl-")), "tpl.json");
  fs.writeFileSync(f, JSON.stringify(clone));
  kubectl("apply", "-f", f);
}

// Creates a workspace on the template whose option label matches
// `label`, retrying the API's retryable release_pending quota refusal —
// a just-deleted workspace's compute holds the running slot until the
// backend's 30 s recovery pass proves the runtime gone (the same
// refusal the retained-attach smoke retries through).
async function createWorkspaceOnTemplate(
  page: Page,
  name: string,
  label: RegExp,
): Promise<string> {
  const deadline = Date.now() + 120_000;
  for (;;) {
    await page.goto("/workspaces/new");
    await page.locator('input[name="name"]').fill(name);
    const option = page.locator('select[name="template"] option', { hasText: label }).first();
    await expect(option).toBeAttached();
    await page
      .locator('select[name="template"]')
      .selectOption((await option.getAttribute("value")) ?? "");
    const [resp] = await Promise.all([
      page.waitForResponse(
        (r) => r.url().endsWith("/v1/workspaces") && r.request().method() === "POST",
      ),
      page.getByRole("button", { name: "Create workspace" }).click(),
    ]);
    const body = (await resp.json().catch(() => ({}))) as Record<string, any>;
    if (resp.status() === 201) {
      await page.waitForURL(/\/workspaces\/ws_/);
      return workspaceIdFromUrl(page);
    }
    expect(
      body?.error?.details?.reason === "release_pending" && Date.now() < deadline,
      `workspace create refused (${resp.status()}): ${JSON.stringify(body)}`,
    ).toBeTruthy();
    await page.waitForTimeout(5_000);
  }
}

async function workspacePhase(page: Page, id: string): Promise<string> {
  return page.evaluate(async (wid) => {
    const r = await fetch(`/v1/workspaces/${encodeURIComponent(wid)}`);
    return r.ok ? ((await r.json()) as { phase?: string }).phase ?? "?" : `http-${r.status}`;
  }, id);
}

async function waitPhase(page: Page, id: string, phase: string, timeoutMs = 5 * 60_000) {
  await expect
    .poll(() => workspacePhase(page, id), {
      timeout: timeoutMs,
      intervals: [3_000],
      message: `${id} -> ${phase}`,
    })
    .toBe(phase);
}

// Stops the workspace through the portal UI — a stopped workspace frees
// its running-quota slot once the backend's recovery pass proves the
// runtime gone (the next create retries through release_pending).
async function stopWorkspace(page: Page, workspaceId: string) {
  await page.goto(`/workspaces/${encodeURIComponent(workspaceId)}`);
  const stop = page.getByRole("button", { name: "Stop", exact: true });
  await expect(stop).toBeVisible();
  await stop.click();
  await waitPhase(page, workspaceId, "Stopped");
}

// Connects to the workspace and returns the KasmVNC session frame once the
// display canvas is up (same wait chain as the session smoke test).
async function connectSessionFrame(page: Page, workspaceId: string): Promise<Frame> {
  await page.goto(`/workspaces/${encodeURIComponent(workspaceId)}`);
  const connect = await waitForConnect(page);
  await connect.click();
  await page.waitForURL(`**/workspaces/${encodeURIComponent(workspaceId)}/session`);
  // A leftover workspace an earlier smoke connected to can still carry its
  // live interactive lease — the sliding 30 s lease outlives the tab that
  // owned it (resumable sessions, broker LeaseTTL), so the ticket request
  // can answer 409 CONNECTION_IN_USE and the portal shows the "In use
  // elsewhere" dialog. A takeover is the product's designed answer for
  // exactly this case: the previous session's page is long gone.
  const takeover = page.getByRole("button", { name: "Take over session" });
  if (
    await takeover
      .waitFor({ state: "visible", timeout: 10_000 })
      .then(() => true)
      .catch(() => false)
  ) {
    await takeover.click();
  }
  const origin = `https://${workspaceId.replaceAll("_", "-").toLowerCase()}.${SESSION_DOMAIN}`;
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
  return frame!;
}

// Types into the KasmVNC client's clipboard textarea and fires the change
// event the client listens on — the same path a user's paste into the
// client's clipboard panel takes (UI.clipboardSend -> clipboardPasteFrom).
async function sendClipboard(frame: Frame, text: string) {
  await frame.locator("#noVNC_clipboard_text").evaluate((el, t) => {
    (el as HTMLTextAreaElement).value = t;
    el.dispatchEvent(new Event("change", { bubbles: true }));
  }, text);
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
    const body = (await resp.json().catch(() => null)) as { error?: { code?: string } } | null;
    expect(body?.error?.code).toBe("QUOTA_NOT_CONFIGURED");
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

  // 2. Delete it: Retain moves the disk to the retained inventory. Wait for
  //    the accepted delete BEFORE any navigation: a page.goto that beats the
  //    in-flight DELETE aborts the fetch — the intent is never recorded, the
  //    workspace stays Running and the poll below times out for no product
  //    reason (observed in CI: status -1 on the DELETE, CR still Ready).
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  const [delResp] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().endsWith(`/v1/workspaces/${sourceId}`) && r.request().method() === "DELETE",
    ),
    page.getByRole("button", { name: "Confirm delete" }).click(),
  ]);
  expect(delResp.status(), "delete is accepted").toBe(202);

  // 3. The disk shows up under Retained data (the inventory sync runs on an
  //    interval and the runtime teardown takes a moment). The table fills
  //    from an async /v1/data fetch after goto resolves, so the predicate
  //    must wait for the row to render before counting — an immediate
  //    count() reads the pre-render DOM and misses a record the API is
  //    already serving (run 37219121799 attempt 1: the record was in every
  //    /v1/data response from +10 s while count() saw 0 for 5 min).
  const row = page.getByRole("row", { name: new RegExp(name) });
  await expect
    .poll(
      async () => {
        await page.goto("/data");
        await row
          .first()
          .waitFor({ state: "attached", timeout: 5_000 })
          .catch(() => {});
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
        (r) => r.url().includes(":attach") && r.request().method() === "POST",
        { timeout: 60_000 },
      ),
      attachButton.click(),
    ]);
    if (resp.ok()) break;
    const body = (await resp.json().catch(() => null)) as {
      error?: { details?: { reason?: string } };
    } | null;
    if (resp.status() !== 409 || body?.error?.details?.reason !== "release_pending") {
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

// The desktop container's env as name->value (spec.containers[0]).
function podEnv(workspaceId: string): Record<string, string> {
  const out = kubectl(
    "get", "pod", runtimePod(workspaceId),
    "-o", "jsonpath={.spec.containers[0].env}",
  );
  const env = (out ? JSON.parse(out) : []) as Array<{ name: string; value?: string }>;
  return Object.fromEntries(env.map((e) => [e.name, e.value ?? ""]));
}

// The one allowed skip of the browser round-trip below: an explicit,
// named opt-out. A caller sets it when the install pinned a runtime
// train older than the entrypoint DLP render, so the round-trip cannot
// run there (up.sh writes it into the install's env file). On the
// default runtime pins
// (runtime-2026.10.12, hack/quickstart/common.sh) the round-trip is
// MANDATORY — a runtime image without the render fails the check below
// loudly instead of skipping silently.
const ROUNDTRIP_OPTOUT = process.env.TCDI_QS_CLIPBOARD_ROUNDTRIP_OPTOUT ?? "";

// True when the pod's runtime image carries the entrypoint DLP render:
// an image built before the operator->entrypoint wiring has no
// tcdi_clipboard_dlp in its entrypoint; one built after does. Used only
// for the failure message — the round-trip no longer gates on it.
function runtimeRendersClipboard(workspaceId: string): boolean {
  try {
    inHome(workspaceId, "grep -q tcdi_clipboard_dlp /opt/tcdi/entrypoint.sh");
    return true;
  } catch {
    return false;
  }
}

// The platform workspace id (ws_<hex>) of a Running pod carrying the
// rendered policy — found through the pod's workspace-name label and the
// CR's workspace-uid label. null when no running pod matches.
function runningWorkspaceWithPolicy(policy: string): string | null {
  const pods = JSON.parse(kubectl("get", "pods", "-o", "json")) as {
    items: Array<Record<string, any>>;
  };
  for (const p of pods.items ?? []) {
    if (p.status?.phase !== "Running") continue;
    const env = (p.spec?.containers?.[0]?.env ?? []) as Array<{ name: string; value?: string }>;
    if (!env.some((e) => e.name === "TCDI_CLIPBOARD_POLICY" && e.value === policy)) continue;
    const crName = p.metadata?.labels?.["workspaces.cdi.tinyorbit.vn/workspace-name"];
    if (!crName) continue;
    const cr = JSON.parse(kubectl("get", "workspace", crName, "-o", "json")) as Record<
      string,
      any
    >;
    const uid = cr.metadata?.labels?.["workspaces.cdi.tinyorbit.vn/workspace-uid"];
    if (typeof uid === "string" && uid) return uid;
  }
  return null;
}

// The rendered policy env of a workspace's pod, tolerating a pod that is
// not materialized yet (kubectl fails while provisioning is in flight).
function podPolicy(workspaceId: string): string | null {
  try {
    return podEnv(workspaceId)["TCDI_CLIPBOARD_POLICY"] ?? null;
  } catch {
    return null;
  }
}

// The template clipboardPolicy is enforced by the session
// server — buildPod renders it into the pod env (TCDI_CLIPBOARD_POLICY)
// and the image entrypoint renders the KasmVNC DLP block from it. A
// Bidirectional template round-trips both directions through the real
// KasmVNC client and the workspace X clipboard; the seeded Disabled
// template lets neither direction through.
test("clipboard follows the template policy (Bidirectional works, Disabled is blocked)", async ({ page }) => {
  // The only allowed skip — the explicit, named opt-out (see
  // ROUNDTRIP_OPTOUT). On the default runtime pins it never fires: the
  // round-trip below is MANDATORY and a runtime image without the
  // entrypoint render fails loudly instead of skipping.
  test.skip(
    ROUNDTRIP_OPTOUT !== "",
    `clipboard round-trip opted out: ${ROUNDTRIP_OPTOUT}`,
  );

  const stamp = Date.now().toString(36);

  // A dedicated Bidirectional template — the seeded catalog is all
  // Disabled, so the non-default direction needs its own catalog entry.
  const tplName = `clip-bidi-${stamp}`;
  cloneBrowserTemplate(tplName, `Clip Bidi ${stamp}`, "Bidirectional");

  await login(page);

  // runningWorkspaces=2 and the earlier smokes leave two workspaces
  // running — no free slot. Every seeded template is Disabled, so a
  // leftover covers the Disabled check; stopping it afterwards frees
  // the slot the Bidirectional workspace needs. The pod-env assertions
  // are the operator half; the browser round-trip is the server half
  // and runs unconditionally on the pinned runtime train.

  // --- Disabled (a leftover seeded-template workspace) -----------------
  const offId = runningWorkspaceWithPolicy("Disabled");
  expect(
    offId,
    "a running workspace carries TCDI_CLIPBOARD_POLICY=Disabled " +
      "(the earlier smokes leave one on the seeded browser template)",
  ).toBeTruthy();

  // Gate kept only for the message: a missing render is a FAILURE on the
  // pinned train (runtime-2026.10.12 carries it), never a skip — the skip
  // path is the named opt-out above and nothing else.
  const capable = runtimeRendersClipboard(offId!);
  expect(
    capable,
    "the running workspace's runtime image lacks the entrypoint clipboard " +
      "render (no tcdi_clipboard_dlp in /opt/tcdi/entrypoint.sh) — the " +
      "default quickstart train runtime-2026.10.12 carries it. On an " +
      "intentionally older runtime the only allowed skip is the named " +
      "opt-out TCDI_QS_CLIPBOARD_ROUNDTRIP_OPTOUT=<reason>.",
  ).toBe(true);

  {
    const offFrame = await connectSessionFrame(page, offId!);
    const offClip = podClipboard(offId!);

    // A cut inside the pod must never reach the client (SendCutText off).
    const s2cOff = `s2c-off-${stamp}`;
    offClip.set(s2cOff);
    await page.waitForTimeout(15_000);
    expect(
      await offFrame.locator("#noVNC_clipboard_text").inputValue(),
      "Disabled: remote cut must not reach the client",
    ).not.toBe(s2cOff);

    // A client-side paste must never reach the workspace (AcceptCutText
    // off): the remote selection keeps the value set above.
    const c2sOff = `c2s-off-${stamp}`;
    await sendClipboard(offFrame, c2sOff);
    await page.waitForTimeout(10_000);
    expect(offClip.get(), "Disabled: client cut must not reach the workspace").toBe(s2cOff);
    console.log(`clipboard Disabled negative ok: ${s2cOff} stayed server-side, ${c2sOff} stayed client-side`);
  }

  // Free the running slot for the Bidirectional workspace — a
  // predecessor smoke workspace is stopped, per review.
  await stopWorkspace(page, offId!);

  // --- Bidirectional: both directions through the real client ----------
  const bidiId = await createWorkspaceOnTemplate(
    // The option label is the catalog name ("<name> (Linux container)"),
    // not the display-name annotation — match the object name.
    page, `clipbi-${stamp}`, new RegExp(tplName, "i"),
  );
  // The pod object lands during provisioning — poll until it exists.
  await expect
    .poll(() => podPolicy(bidiId), { timeout: 120_000 })
    .toBe("Bidirectional");

  try {
    const bidiFrame = await connectSessionFrame(page, bidiId);
    const bidiClip = podClipboard(bidiId);

    // workspace -> client: a cut inside the pod reaches the client's
    // clipboard textarea (the client's own mirror of the remote
    // clipboard).
    const s2c = `s2c-${stamp}`;
    bidiClip.set(s2c);
    await expect
      .poll(() => bidiFrame.locator("#noVNC_clipboard_text").inputValue(), {
        timeout: 30_000,
      })
      .toBe(s2c);
    console.log(`clipboard workspace->client round-trip ok: ${s2c}`);

    // client -> workspace: pasting into the client's clipboard panel
    // lands on the workspace CLIPBOARD selection.
    const c2s = `c2s-${stamp}`;
    await sendClipboard(bidiFrame, c2s);
    await expect.poll(() => bidiClip.get(), { timeout: 30_000 }).toBe(c2s);
    console.log(`clipboard client->workspace round-trip ok: ${c2s}`);
  } finally {
    // Leave no running workspace of ours behind either way.
    await stopWorkspace(page, bidiId);
  }
});
