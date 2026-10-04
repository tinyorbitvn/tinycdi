import { execFileSync, spawn } from "node:child_process";
import { readdirSync, readFileSync, existsSync } from "node:fs";
import * as path from "node:path";
import { expect, test, type Page, type Response } from "@playwright/test";

// N-1 -> N upgrade assertions (hack/quickstart/upgrade-test.sh). The cluster
// this runs against was installed from the PREVIOUS release's chart and
// signed images; the test seeds state, invokes `upgrade-test.sh --apply`
// while a desktop session stays open on a second page, then verifies the
// state survived the upgrade to the working-tree chart and images.
//
// Env (exported by upgrade-test.sh / the CI job): the TCDI_QS_* set of
// smoke.spec.ts plus TCDI_UPGRADE_STATE_DIR and TCDI_UPGRADE_APPLY.
const USER = process.env.TCDI_QS_USER ?? "demo";
const PASSWORD = process.env.TCDI_QS_PASSWORD ?? "tcdi-demo-dev-only";
const TENANT_NAMESPACE = process.env.TCDI_QS_TENANT_NAMESPACE ?? "tinycdi-tenant-a";
const DEPS_NAMESPACE = process.env.TCDI_QS_DEPS_NAMESPACE ?? "tcdi-qs-deps";
const STATE_DIR = process.env.TCDI_UPGRADE_STATE_DIR ?? "";
const APPLY_SCRIPT = process.env.TCDI_UPGRADE_APPLY ?? "";
const REPO_ROOT = path.resolve(__dirname, "../../..");
const HOME_DIR = "/home/workspace";

// ---- helpers (kept in step with smoke.spec.ts) -------------------------------

async function login(page: Page, user = USER) {
  await page.goto("/");
  await expect(page.locator("#username")).toBeVisible();
  await expect(page).toHaveURL(/^https:\/\/keycloak\./);
  await page.locator("#username").fill(user);
  await page.locator("#password").fill(PASSWORD);
  await page.locator("#kc-login").click();
  await expect(page.getByRole("heading", { name: "Workspaces", exact: true })).toBeVisible();
}

async function createBrowserWorkspace(
  page: Page,
  name: string,
  dataPolicy?: "Retain" | "Ephemeral",
): Promise<string> {
  // The released portal only offers "New workspace" from the list page —
  // the detail page (where the previous create left us) has none.
  await page.goto("/workspaces/new");
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
  return decodeURIComponent(new URL(page.url()).pathname.split("/").pop() ?? "");
}

async function waitForConnect(page: Page) {
  const connect = page.getByRole("link", { name: "Connect" });
  await expect(connect).toBeVisible({ timeout: 10 * 60_000 });
  return connect;
}

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

async function workspacePhase(page: Page, id: string): Promise<string> {
  return page.evaluate(async (wid) => {
    const r = await fetch(`/v1/workspaces/${encodeURIComponent(wid)}`);
    return r.ok ? ((await r.json()) as { phase?: string }).phase ?? "?" : `http-${r.status}`;
  }, id);
}

async function waitPhase(page: Page, id: string, phase: string, timeoutMs = 5 * 60_000) {
  await expect
    .poll(() => workspacePhase(page, id), { timeout: timeoutMs, intervals: [3_000], message: `${id} -> ${phase}` })
    .toBe(phase);
}

// Navigates to `url` with a bounded retry on 5xx and navigation errors
// (<= 60 s): right after a rollout the edge can briefly keep a stale
// route to a terminated frontend pod and answer Gateway Timeout — an
// availability hiccup, not a lost portal session (FX-R35). Returns the
// last response, null when navigation itself kept failing.
async function gotoRetryOn5xx(page: Page, url: string): Promise<Response | null> {
  const deadline = Date.now() + 60_000;
  let resp: Response | null = null;
  do {
    resp = await page.goto(url).catch(() => null);
    if (resp && resp.status() < 500) return resp;
    await page.waitForTimeout(1_000);
  } while (Date.now() < deadline);
  return resp;
}

async function getQuota(page: Page): Promise<{
  configured?: boolean;
  limits?: unknown;
  usage?: { runningWorkspaces?: number };
}> {
  return page.evaluate(async () => {
    const r = await fetch("/v1/quota");
    return r.ok
      ? ((await r.json()) as {
          configured?: boolean;
          limits?: unknown;
          usage?: { runningWorkspaces?: number };
        })
      : { configured: undefined };
  });
}

// A one-shot quota read that survives a transient in-page fetch failure.
// The polls above already retry a thrown evaluate, but a bare getQuota
// call does not: a navigation that races it (the detail page's own
// "workspace gone -> /" redirect after a delete, or a connection dropped
// by the post-upgrade rollout) aborts the fetch and would kill the test
// — the same race class as the aborted retained-disk DELETE. Poll until
// the read returns a real body instead of throwing once.
async function quotaNow(page: Page) {
  let quota: Awaited<ReturnType<typeof getQuota>> = {};
  await expect
    .poll(
      async () =>
        ((quota = await getQuota(page)).configured === undefined ? null : quota.configured),
      { timeout: 60_000, message: "quota read" },
    )
    .not.toBeNull();
  return quota;
}

// psql against the quickstart Postgres: local socket first, TCP+password
// (up.sh's generated one) as fallback — the pod serves TLS on TCP only.
function psql(sql: string): string {
  const base = ["-n", DEPS_NAMESPACE, "exec", "postgres-0", "--"];
  try {
    return execFileSync("kubectl", [...base, "psql", "-U", "tinycdi", "-d", "tinycdi", "-tAc", sql], {
      encoding: "utf8",
      timeout: 30_000,
    }).trim();
  } catch {
    const pw = existsSync(`${STATE_DIR}/db-password`)
      ? readFileSync(`${STATE_DIR}/db-password`, "utf8").trim()
      : "";
    return execFileSync(
      "kubectl",
      [...base, "env", `PGPASSWORD=${pw}`, "PGSSLMODE=require", "psql", "-h", "127.0.0.1", "-U", "tinycdi", "-d", "tinycdi", "-tAc", sql],
      { encoding: "utf8", timeout: 30_000 },
    ).trim();
  }
}

function latestMigration(): number {
  return Math.max(
    ...readdirSync(path.join(REPO_ROOT, "internal/store/migrations"))
      .map((f) => Number.parseInt(f.split("_")[0] ?? "", 10))
      .filter((n) => Number.isFinite(n)),
  );
}

// ---- the test ----------------------------------------------------------------

test("v0.2.0 -> working tree: state and live session survive the upgrade", async ({ page, browser, baseURL }) => {
  test.setTimeout(40 * 60_000);
  const stamp = Date.now().toString(36);
  const keepName = `upg-keep-${stamp}`;
  const stoppedName = `upg-stop-${stamp}`;
  const rtName = `upg-rt-${stamp}`;
  const keepToken = `keep-${stamp}`;
  const stopToken = `stop-${stamp}`;
  const rtToken = `rt-${stamp}`;
  const file = (t: string) => `${HOME_DIR}/${t}.txt`;

  // ---------- seed state on the previous release ----------
  await login(page);
  const quotaBefore = await quotaNow(page);
  expect(quotaBefore.configured, "tenant quota row before upgrade").toBe(true);

  // A Retain workspace whose disk must carry a file across the upgrade. Its
  // session page stays open in a second tab through the helm upgrade — the
  // live-stream part of the check (B3.13 / V3.19).
  const keepId = await createBrowserWorkspace(page, keepName, "Retain");
  await waitForConnect(page);
  inHome(keepId, 'printf %s "$1" > "$2" && sync', keepToken, file(keepToken));
  expect(inHome(keepId, 'cat "$1"', file(keepToken))).toBe(keepToken);

  const sessionPage = await page.context().newPage();
  await sessionPage.goto(`/workspaces/${encodeURIComponent(keepId)}/session`);
  const status = sessionPage.locator(".tc-session__status");
  await expect(status).toHaveText("Connected", { timeout: 4 * 60_000 });
  // Launch tickets minted after the session is up — must stay empty across
  // the upgrade: reconnects inside the live lease never mint one.
  const tickets: string[] = [];
  sessionPage.on("request", (r) => {
    if (r.method() === "POST" && /\/v1\/workspaces\/[^/]+\/connections$/.test(r.url())) {
      tickets.push(r.url());
    }
  });

  // A second workspace, stopped before the upgrade: the B3.1 case — a
  // stopped desktop with retained home must start afterwards with its files.
  const stoppedId = await createBrowserWorkspace(page, stoppedName, "Retain");
  await waitForConnect(page);
  inHome(stoppedId, 'printf %s "$1" > "$2" && sync', stopToken, file(stopToken));
  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await waitPhase(page, stoppedId, "Stopped");
  // The released backend frees the compute slots asynchronously — the
  // workspace reports Stopped before convertToDiskOnly runs, and the next
  // create races it. Wait until the running-slot usage drops.
  await expect
    .poll(async () => (await getQuota(page)).usage?.runningWorkspaces ?? -1, {
      timeout: 90_000,
      intervals: [2_000],
      message: "stopped workspace frees its running slot",
    })
    .toBe(1);

  // A Retain workspace deleted before the upgrade: its disk sits in the
  // retained inventory and must stay attachable.
  const rtId = await createBrowserWorkspace(page, rtName, "Retain");
  await waitForConnect(page);
  inHome(rtId, 'printf %s "$1" > "$2" && sync', rtToken, file(rtToken));
  await page.getByRole("button", { name: "Delete", exact: true }).click();
  await page.getByRole("button", { name: "Confirm delete" }).click();
  const rtRowVisible = () =>
    page.evaluate(async (name) => {
      const r = await fetch("/v1/data");
      if (!r.ok) return false;
      const body = (await r.json()) as { items?: { sourceWorkspaceName?: string; state?: string }[] };
      return (body.items ?? []).some((i) => i.sourceWorkspaceName === name);
    }, rtName);
  await expect
    .poll(rtRowVisible, { timeout: 5 * 60_000, intervals: [5_000], message: "retained-data row created" })
    .toBe(true);
  const quotaSeeded = await quotaNow(page);
  expect(quotaSeeded.limits, "quota limits must not move while seeding").toEqual(quotaBefore.limits);

  // ---------- upgrade: CRDs, then helm, while the session stays open ----------
  expect(APPLY_SCRIPT, "TCDI_UPGRADE_APPLY is set by upgrade-test.sh").toBeTruthy();
  let applyOut = "";
  const proc = spawn("bash", [APPLY_SCRIPT, "--apply"], { stdio: ["ignore", "pipe", "pipe"] });
  proc.stdout.on("data", (d) => (applyOut += String(d)));
  proc.stderr.on("data", (d) => (applyOut += String(d)));
  const applyExit = new Promise<number>((res) => proc.on("exit", (c) => res(c ?? 1)));
  let applyRunning = true;
  void applyExit.then(() => (applyRunning = false));

  const terminal = new Set([
    "Ended",
    "In another tab",
    "Open in another tab",
    "In use elsewhere",
    "Signed out",
    "Blocked",
  ]);
  const deadline = Date.now() + 20 * 60_000;
  while (applyRunning && Date.now() < deadline) {
    const badge = await status.innerText().catch(() => "?");
    expect(terminal.has(badge), `session reached a terminal state mid-upgrade: ${badge}`).toBe(false);
    await sessionPage.waitForTimeout(3_000);
  }
  expect(await applyExit, `upgrade-test.sh --apply failed:\n${applyOut}`).toBe(0);

  // The apply leg must wait for ALL rollouts — backend, operator AND
  // frontend — before returning, so the portal navigation below never
  // races an unfinished rollout (FX-R35). kubectl prints
  // `deployment "<name>" successfully rolled out` per wait.
  for (const d of ["backend", "operator", "frontend"]) {
    expect(applyOut, `--apply waited for deployment/${d} to roll out`).toContain(
      `deployment "${d}" successfully rolled out`,
    );
  }

  // The live session reconnects to a new backend replica inside its lease —
  // no second launch ticket (docs/runbooks/upgrade.md "safe to upgrade").
  await expect(status).toHaveText("Connected", { timeout: 5 * 60_000 });
  expect(tickets, "launch tickets minted during the upgrade").toEqual([]);
  await sessionPage.screenshot({ path: "test-results/session-after-upgrade.png" });

  // ---------- post-upgrade assertions ----------
  // Portal session survives: the same browser context still reaches the
  // app. The frontend roll just ended — retry the navigation on 5xx for a
  // bounded window rather than failing on the first edge hiccup (FX-R35).
  const resp = await gotoRetryOn5xx(page, "/");
  expect(
    resp !== null && resp.status() < 500,
    `portal unavailable after the upgrade: last GET / -> ${resp ? `HTTP ${resp.status()}` : "navigation failed"}`,
  ).toBe(true);
  await expect(
    page.getByRole("heading", { name: "Workspaces", exact: true }),
    "portal session lost across the upgrade",
  ).toBeVisible({ timeout: 60_000 });

  const quotaAfter = await quotaNow(page);
  expect(quotaAfter.configured, "tenant quota row after upgrade").toBe(true);
  expect(quotaAfter.limits, "quota limits unchanged").toEqual(quotaBefore.limits);

  // Migrations ran to the newest file in internal/store/migrations.
  const wantMigration = latestMigration();
  const gotMigration = psql("SELECT max(version) FROM schema_migrations");
  expect(Number(gotMigration), "schema_migrations max(version)").toBe(wantMigration);

  // The persistent workspace kept running; its file is still there.
  expect(inHome(keepId, 'cat "$1"', file(keepToken)), "file on the running Retain workspace").toBe(
    keepToken,
  );

  // The stopped workspace starts on the upgraded platform, with its disk.
  await page.goto(`/workspaces/${encodeURIComponent(stoppedId)}`);
  await page.getByRole("button", { name: "Start", exact: true }).click();
  await waitPhase(page, stoppedId, "Ready");
  expect(inHome(stoppedId, 'cat "$1"', file(stopToken)), "file on the restarted workspace").toBe(
    stopToken,
  );
  // Free the quota slot again before the attach below (tenant quota is 2)
  // — same async free as before the upgrade.
  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await waitPhase(page, stoppedId, "Stopped");
  await expect
    .poll(async () => (await getQuota(page)).usage?.runningWorkspaces ?? -1, {
      timeout: 90_000,
      intervals: [2_000],
      message: "stopped workspace frees its running slot (post-upgrade)",
    })
    .toBe(1);

  // The retained-data row survived and is still attachable.
  await page.goto("/data");
  const row = page.getByRole("row", { name: new RegExp(rtName) });
  await expect(row, "retained-data row after upgrade").toBeVisible({ timeout: 60_000 });
  await row.getByRole("button", { name: "Attach" }).click();
  const attachButton = page.getByRole("button", { name: "Attach disk" });
  const attachDeadline = Date.now() + 60_000;
  for (;;) {
    const [resp] = await Promise.all([
      page.waitForResponse((r) => r.url().includes("/attach") && r.request().method() === "POST", {
        timeout: 60_000,
      }),
      attachButton.click(),
    ]);
    if (resp.ok()) break;
    const body = (await resp.json().catch(() => null)) as { details?: { reason?: string } } | null;
    if (resp.status() !== 409 || body?.details?.reason !== "release_pending") {
      throw new Error(`attach failed: HTTP ${resp.status()} ${JSON.stringify(body)}`);
    }
    if (Date.now() > attachDeadline) throw new Error("attach still refused with release_pending");
  }
  await page.waitForURL(/\/workspaces\/ws_/);
  const attachedId = decodeURIComponent(new URL(page.url()).pathname.split("/").pop() ?? "");
  expect(attachedId).not.toBe(rtId);
  await waitForConnect(page);
  expect(inHome(attachedId, 'cat "$1"', file(rtToken)), "file on the attached retained disk").toBe(
    rtToken,
  );

  // The persistent workspace also survives a stop/start on the new platform.
  await page.goto(`/workspaces/${encodeURIComponent(keepId)}`);
  await page.getByRole("button", { name: "Stop", exact: true }).click();
  await waitPhase(page, keepId, "Stopped");
  await expect
    .poll(async () => (await getQuota(page)).usage?.runningWorkspaces ?? -1, {
      timeout: 90_000,
      intervals: [2_000],
      message: "keep workspace frees its running slot before restart",
    })
    .toBe(1);
  await page.getByRole("button", { name: "Start", exact: true }).click();
  await waitPhase(page, keepId, "Ready");
  expect(inHome(keepId, 'cat "$1"', file(keepToken)), "file after post-upgrade restart").toBe(
    keepToken,
  );

  // No crash loops: every pod Running/Completed, ready, no bad wait reasons.
  const pods = JSON.parse(
    execFileSync("kubectl", ["get", "pods", "-A", "-o", "json"], { encoding: "utf8", timeout: 60_000 }),
  ) as {
    items: {
      metadata: { name: string; namespace: string };
      status: {
        phase: string;
        containerStatuses?: { ready: boolean; restartCount: number; state?: { waiting?: { reason?: string } } }[];
      };
    }[];
  };
  const ours = pods.items.filter((p) =>
    ["tinycdi-system", "tinycdi-tenant-a", "tinycdi-tenant-noquota", "tcdi-qs-deps", "tcdi-qs-ingress"].includes(
      p.metadata.namespace,
    ),
  );
  for (const p of ours) {
    const id = `${p.metadata.namespace}/${p.metadata.name}`;
    expect(["Running", "Succeeded"], `${id} phase ${p.status.phase}`).toContain(p.status.phase);
    if (p.status.phase !== "Running") continue;
    for (const cs of p.status.containerStatuses ?? []) {
      const reason = cs.state?.waiting?.reason ?? "";
      expect(
        ["CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError"].includes(reason),
        `${id} container waiting: ${reason}`,
      ).toBe(false);
      expect(cs.ready, `${id} container not ready`).toBe(true);
      expect(cs.restartCount, `${id} restartCount ${cs.restartCount}`).toBeLessThanOrEqual(3);
    }
  }

  // A fresh login works against the upgraded portal.
  const fresh = await browser.newContext({ baseURL, ignoreHTTPSErrors: true });
  try {
    const freshPage = await fresh.newPage();
    await login(freshPage);
  } finally {
    await fresh.close();
  }
});
