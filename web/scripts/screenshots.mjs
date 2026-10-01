// Regenerates the README screenshots in docs/images/screenshots/.
//
// Two capture passes, both against REAL product surfaces (no mockups):
//
//   1. Portal — the built SPA (web/dist) served by the real Go portal
//      binary with the contract mock (tests/mock-api) behind it, i.e. the
//      same stack as tests-portal/serve.ts. Demo data is seeded through
//      the mock's public + _control endpoints.
//   2. Runtime — the published runtime images
//      (ghcr.io/tinyorbitvn/tinycdi-linux-desktop, tinycdi-browser) run
//      locally under Docker exactly like tests/integration does: mounted
//      TLS material + credentials, seccomp profile, dropped caps. The
//      KasmVNC web client is screenshotted over HTTPS.
//
// Manual tool — NOT part of CI. See docs/development.md.
//
// Usage:  npm --prefix web ci && node web/scripts/screenshots.mjs
// Env knobs: SHOTS_DIR, DESKTOP_IMAGE, BROWSER_IMAGE, KEEP=1 (skip cleanup).

import { chromium } from "@playwright/test";
import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync, chmodSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import http from "node:http";
import https from "node:https";
import { randomBytes } from "node:crypto";

const WEB_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const REPO_ROOT = path.resolve(WEB_DIR, "..");
const OUT_DIR = path.resolve(process.env.SHOTS_DIR ?? path.join(REPO_ROOT, "docs/images/screenshots"));
const DESKTOP_IMAGE = process.env.DESKTOP_IMAGE ?? "ghcr.io/tinyorbitvn/tinycdi-linux-desktop:0.1.0";
const BROWSER_IMAGE = process.env.BROWSER_IMAGE ?? "ghcr.io/tinyorbitvn/tinycdi-browser:0.1.0";
const KEEP = process.env.KEEP === "1";
const RUN_PREFIX = "tcdi-w19-";

const PORTAL_ORIGIN = "https://localhost:4174";
const MOCK_API = "http://127.0.0.1:4320";
const VIEWPORT = { width: 1440, height: 900 };

// Demo identities are deliberately generic — no real names, hostnames or
// emails in shipped screenshots.
const TEMPLATE_LINUX = "tpl_01J4ZB3N1RXD7P2V8W5K0H6Q4M"; // linux-firefox-desktop (mock fixture)
const TEMPLATE_BROWSER = "tpl_01J4ZC7M2QXW8P3V9H5K1N6R4T"; // linux-chromium-browser
const DEMO_PASSWORD = randomBytes(18).toString("base64url");

const procs = [];
function cleanup() {
  for (const p of procs) {
    try {
      p.kill("SIGTERM");
    } catch {}
  }
  if (!KEEP) {
    for (const name of ["desktop", "browser"]) {
      spawnSync("docker", ["rm", "-f", `${RUN_PREFIX}${name}`], { stdio: "ignore" });
    }
  }
}
process.on("exit", cleanup);
process.on("SIGINT", () => process.exit(130));

function sh(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { encoding: "utf8", ...opts });
  if (r.status !== 0) {
    throw new Error(`${cmd} ${args.join(" ")} failed (${r.status}): ${r.stderr || r.stdout}`);
  }
  return (r.stdout ?? "").trim();
}

function waitHttp(url, what, timeoutMs = 120_000) {
  const get = url.startsWith("https:") ? https.get : http.get;
  const deadline = Date.now() + timeoutMs;
  return new Promise((resolve, reject) => {
    const tick = () => {
      const req = get.call(null, url, { rejectUnauthorized: false }, (res) => {
        res.resume();
        if (res.statusCode === 200) return resolve();
        retry();
      });
      req.on("error", retry);
      req.setTimeout(2000, () => req.destroy());
      function retry() {
        if (Date.now() > deadline) return reject(new Error(`timeout waiting for ${what} (${url})`));
        setTimeout(tick, 300);
      }
    };
    tick();
  });
}

// Call the contract mock directly (no auth on _control; session/CSRF
// cookies are presence-checked only, so fixed dev values work).
async function api(method, p, body) {
  const res = await fetch(`${MOCK_API}${p}`, {
    method,
    headers: {
      "content-type": "application/json",
      cookie: "tcdi_session=session-01J4ZD; tcdi_csrf=csrf-token-01J4ZD",
      "x-csrf-token": "csrf-token-01J4ZD",
      ...(method === "POST" ? { "idempotency-key": `shots-${randomBytes(8).toString("hex")}` } : {}),
    },
    ...(body ? { body: JSON.stringify(body) } : {}),
  });
  const text = await res.text();
  if (res.status >= 400) throw new Error(`${method} ${p} -> ${res.status}: ${text}`);
  return text ? JSON.parse(text) : {};
}

const READY_CONDITIONS = [
  { type: "Admitted", status: "True", reason: "QuotaReserved", lastTransitionTime: "2026-09-30T10:00:00Z" },
  { type: "StorageReady", status: "True", reason: "VolumeBound", lastTransitionTime: "2026-09-30T10:01:00Z" },
  { type: "RuntimeReady", status: "True", reason: "RuntimeUp", lastTransitionTime: "2026-09-30T10:02:00Z" },
  { type: "ConnectionReady", status: "True", reason: "StreamEndpointUp", lastTransitionTime: "2026-09-30T10:02:30Z" },
];

async function seedPortalData() {
  await api("POST", "/_control/reset");

  // A ready Linux desktop — the one the detail screenshot opens.
  const alice = await api("POST", "/v1/workspaces", {
    name: "alice-desktop",
    templateRef: TEMPLATE_LINUX,
    desiredState: "Running",
  });
  await api("POST", `/_control/workspaces/${alice.id}`, {
    phase: "Ready",
    conditions: READY_CONDITIONS,
  });

  // A ready browser workspace.
  const browser = await api("POST", "/v1/workspaces", {
    name: "scratch-browser",
    templateRef: TEMPLATE_BROWSER,
    desiredState: "Running",
  });
  await api("POST", `/_control/workspaces/${browser.id}`, {
    phase: "Ready",
    conditions: READY_CONDITIONS,
  });

  // One mid-lifecycle workspace for variety in the list.
  await api("POST", "/v1/workspaces", {
    name: "render-job",
    templateRef: TEMPLATE_LINUX,
    desiredState: "Running",
  }); // stays Provisioning

  return { aliceId: alice.id };
}

async function shotPortal() {
  console.log("[portal] starting tests-portal harness (builds SPA + portal binary)…");
  const serve = spawn(
    "node",
    ["--disable-warning=ExperimentalWarning", "tests-portal/serve.ts"],
    { cwd: WEB_DIR, stdio: ["ignore", "pipe", "inherit"] },
  );
  procs.push(serve);
  serve.stdout.on("data", (d) => process.stdout.write(`[harness] ${d}`));
  await waitHttp(`${MOCK_API}/_control/health`, "mock api");
  await waitHttp(`${PORTAL_ORIGIN}/healthz`, "portal");

  const { aliceId } = await seedPortalData();

  const browser = await chromium.launch();
  try {
    const ctx = await browser.newContext({
      viewport: VIEWPORT,
      deviceScaleFactor: 1,
      ignoreHTTPSErrors: true,
    });
    const page = await ctx.newPage();

    // Log in through the mock's dev SSO form (real redirect path).
    await page.goto(`${PORTAL_ORIGIN}/`, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Log in with SSO" }).click();
    await page.getByRole("heading", { name: "Workspaces" }).waitFor();
    await page.waitForSelector("table[aria-label='workspaces']");
    await page.waitForTimeout(400);
    await page.screenshot({ path: path.join(OUT_DIR, "portal-workspaces.png") });
    console.log("[portal] captured portal-workspaces.png");

    // Create-workspace form, filled in.
    await page.getByRole("link", { name: "New workspace" }).click();
    await page.getByRole("heading", { name: "New workspace" }).waitFor();
    await page.getByLabel("Name").fill("team-browser");
    await page.locator("select[name='template']").selectOption(TEMPLATE_BROWSER);
    await page.waitForTimeout(300);
    await page.screenshot({ path: path.join(OUT_DIR, "portal-create.png") });
    console.log("[portal] captured portal-create.png");

    // Workspace detail of the ready desktop (conditions + connect).
    await page.goto(`${PORTAL_ORIGIN}/workspaces/${aliceId}`);
    await page.getByRole("heading", { name: /alice-desktop/ }).waitFor();
    await page.waitForTimeout(400);
    await page.screenshot({ path: path.join(OUT_DIR, "portal-detail.png") });
    console.log("[portal] captured portal-detail.png");

    // Retained-data inventory.
    await page.goto(`${PORTAL_ORIGIN}/data`);
    await page.getByRole("heading", { name: /retained data/i }).waitFor();
    await page.waitForTimeout(400);
    await page.screenshot({ path: path.join(OUT_DIR, "portal-data.png") });
    console.log("[portal] captured portal-data.png");

    await ctx.close();
  } finally {
    await browser.close();
  }
}

function stageSecret() {
  const dir = mkdtempSync(path.join(tmpdir(), "tcdi-w19-secret-"));
  sh("openssl", [
    "req", "-x509", "-newkey", "rsa:2048", "-nodes",
    "-keyout", path.join(dir, "tls.key"), "-out", path.join(dir, "tls.crt"),
    "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1,DNS:localhost",
    "-days", "1",
  ]);
  writeFileSync(path.join(dir, "password"), DEMO_PASSWORD + "\n", { mode: 0o600 });
  writeFileSync(path.join(dir, "username"), "kasm_user\n", { mode: 0o600 });
  return dir;
}

function runRuntime(name, image, secretDir, seccomp, extraArgs = []) {
  spawnSync("docker", ["rm", "-f", `${RUN_PREFIX}${name}`], { stdio: "ignore" });
  const args = [
    "run", "-d", "--name", `${RUN_PREFIX}${name}`,
    "-p", "127.0.0.1:0:8443",
    "-v", `${secretDir}:/run/secrets/tcdi:ro`,
    "--tmpfs", "/run/tcdi:rw,exec,uid=1000,gid=1000,mode=700",
    "--shm-size", "256m",
    "--cap-drop", "ALL",
    "--security-opt", "no-new-privileges",
    "--security-opt", `seccomp=${seccomp}`,
    ...extraArgs,
    image,
  ];
  sh("docker", args);
  const deadline = Date.now() + 180_000;
  for (;;) {
    const st = sh("docker", [
      "inspect", "-f",
      "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}",
      `${RUN_PREFIX}${name}`,
    ]);
    if (st.includes("healthy")) break;
    if (st.startsWith("exited") || st.startsWith("dead")) {
      throw new Error(`${name} died:\n${sh("docker", ["logs", "--tail", "40", `${RUN_PREFIX}${name}`])}`);
    }
    if (Date.now() > deadline) {
      throw new Error(`${name} not healthy:\n${sh("docker", ["logs", "--tail", "40", `${RUN_PREFIX}${name}`])}`);
    }
    spawnSync("sleep", ["2"]);
  }
  const port = sh("docker", [
    "inspect", "-f",
    "{{(index (index .NetworkSettings.Ports \"8443/tcp\") 0).HostPort}}",
    `${RUN_PREFIX}${name}`,
  ]);
  console.log(`[runtime] ${name} healthy on https://127.0.0.1:${port}`);
  return port;
}

async function shotRuntime(name, port, file, settleMs, interact) {
  const browser = await chromium.launch();
  try {
    const ctx = await browser.newContext({
      viewport: VIEWPORT,
      deviceScaleFactor: 1,
      ignoreHTTPSErrors: true,
      httpCredentials: { username: "kasm_user", password: DEMO_PASSWORD },
    });
    const page = await ctx.newPage();
    await page.goto(`https://127.0.0.1:${port}/`, { waitUntil: "domcontentloaded", timeout: 30_000 });
    // KasmVNC web client: wait for the session canvas (the multi-monitor
    // widget canvas stays hidden — match the display canvas explicitly),
    // then let the framebuffer draw before capturing.
    await page.waitForSelector("canvas#noVNC_canvas, canvas:not(#noVNC_multiMonitorWidget)", {
      state: "attached",
      timeout: 30_000,
    });
    await page.waitForTimeout(settleMs);
    if (interact) await interact(page);
    await page.screenshot({ path: path.join(OUT_DIR, file) });
    console.log(`[runtime] captured ${file}`);
    await ctx.close();
  } finally {
    await browser.close();
  }
}

async function shotRuntimes() {
  const secret = stageSecret();
  const testSeccomp = path.join(REPO_ROOT, "tests/integration/testdata/seccomp-runtime.json");
  const nodeSeccomp = path.join(REPO_ROOT, "deploy/node-profiles/seccomp/chromium-userns.json");

  // Browser session: keep the shipped sandbox on; only the start URL and
  // UI language are customized (the image boots Chromium on about:blank)
  // via an xstartup bind-mount — documented deviation for screenshots only.
  const browserXstartup = path.join(path.dirname(secret), "xstartup-browser.sh");
  writeFileSync(
    browserXstartup,
    "#!/bin/bash\nopenbox &\nsleep 1\nexec chromium --no-first-run --no-default-browser-check --lang=en-US --start-maximized https://example.com\n",
  );
  chmodSync(browserXstartup, 0o755);

  const deskPort = runRuntime("desktop", DESKTOP_IMAGE, secret, testSeccomp);
  const browPort = runRuntime("browser", BROWSER_IMAGE, secret, nodeSeccomp, [
    "-v", `${browserXstartup}:/opt/tcdi/xstartup.sh:ro`,
  ]);

  // Desktop session: the shipped xstartup leaves a focused xterm; type a
  // few environment-identifying commands through the VNC keyboard path.
  await shotRuntime("desktop", deskPort, "session-desktop.png", 9_000, async (page) => {
    await page.mouse.click(VIEWPORT.width / 2, VIEWPORT.height / 2);
    await page.waitForTimeout(500);
    await page.keyboard.type("cat /etc/os-release | head -1; id; ls -la ~");
    await page.keyboard.press("Enter");
    await page.waitForTimeout(2000);
  });
  await shotRuntime("browser", browPort, "session-browser.png", 15_000);
}

mkdirSync(OUT_DIR, { recursive: true });
try {
  await shotPortal();
  await shotRuntimes();
} finally {
  cleanup();
}
console.log(`done — screenshots in ${OUT_DIR}`);
