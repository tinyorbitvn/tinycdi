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
// Usage:  npm --prefix web ci && node web/scripts/screenshots.mjs [--only portal|runtime] [--list]
//   --only portal   portal/admin pass only (no Docker needed)
//   --only runtime  runtime-image pass only
//   --list          print the files this run would write, then exit
// Portal/admin views are captured twice (colorScheme light + dark) at
// 1440×900, device scale 2, English UI, demo data (MOCK_DEMO=1).
// Env knobs: SHOTS_DIR, DESKTOP_IMAGE, BROWSER_IMAGE, KEEP=1 (skip cleanup).

import { chromium } from "@playwright/test";
import { spawn, spawnSync } from "node:child_process";
import { chmodSync, existsSync, mkdtempSync, mkdirSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import http from "node:http";
import https from "node:https";
import { randomBytes } from "node:crypto";

const WEB_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const REPO_ROOT = path.resolve(WEB_DIR, "..");
const OUT_DIR = path.resolve(process.env.SHOTS_DIR ?? path.join(REPO_ROOT, "docs/images/screenshots"));
const DESKTOP_IMAGE = process.env.DESKTOP_IMAGE ?? "ghcr.io/tinyorbitvn/tinycdi-linux-desktop:1.0.0";
const BROWSER_IMAGE = process.env.BROWSER_IMAGE ?? "ghcr.io/tinyorbitvn/tinycdi-browser:1.0.0";
const KEEP = process.env.KEEP === "1";
const RUN_PREFIX = "tcdi-w19-";

const PORTAL_ORIGIN = "https://localhost:4174";
const MOCK_API = "http://127.0.0.1:4320";
const VIEWPORT = { width: 1440, height: 900 };

// Demo identities are deliberately generic — no real names, hostnames or
// emails in shipped screenshots.
const DEMO_PASSWORD = randomBytes(18).toString("base64url");

const PORTAL_STEMS = [
  "portal-workspaces",
  "portal-create",
  "portal-detail",
  "admin-overview",
  "admin-session-policy",
  "admin-audit",
];
const RUNTIME_STEMS = ["session-desktop", "session-browser"];
const SCHEMES = ["light", "dark"];

function parseArgs(argv) {
  const out = { only: null, list: false };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--list") out.list = true;
    else if (a === "--only") out.only = argv[++i] ?? "";
    else if (a.startsWith("--only=")) out.only = a.slice("--only=".length);
    else throw new Error(`unknown argument ${a} (use --only portal|runtime, --list)`);
  }
  if (out.only !== null && out.only !== "portal" && out.only !== "runtime") {
    throw new Error(`--only must be portal or runtime, got "${out.only}"`);
  }
  return out;
}

function outputFiles(only) {
  const portal = PORTAL_STEMS.flatMap((s) => SCHEMES.map((c) => `${s}-${c}.png`));
  const runtime = RUNTIME_STEMS.map((s) => `${s}.png`);
  return only === "portal" ? portal : only === "runtime" ? runtime : [...portal, ...runtime];
}

const ARGS = parseArgs(process.argv.slice(2));
if (ARGS.list) {
  // Before any process/cleanup hooks: --list never starts the harness.
  for (const f of outputFiles(ARGS.only)) console.log(f);
  process.exit(0);
}

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
      cookie: "tcdi_session=session-01J4ZD",
      "x-csrf-token": "csrf-token-01J4ZD",
      ...(method === "POST" ? { "idempotency-key": `shots-${randomBytes(8).toString("hex")}` } : {}),
    },
    ...(body ? { body: JSON.stringify(body) } : {}),
  });
  const text = await res.text();
  if (res.status >= 400) throw new Error(`${method} ${p} -> ${res.status}: ${text}`);
  return text ? JSON.parse(text) : {};
}

async function seedPortalData() {
  // MOCK_DEMO=1: reset re-seeds the demo tenant (mixed phases, events,
  // retained data); the admin role + peer users fill the admin views.
  await api("POST", "/_control/reset");
  await api("POST", "/_control/admin/me", { roles: ["user", "tenant-admin"] });
  await api("POST", "/_control/admin/seed");
  const { items = [] } = await api("GET", "/v1/workspaces");
  // Prefer the demo workspace that carries events, so the detail shot
  // shows a populated Events table.
  const ready =
    items.find((w) => w.phase === "Ready" && w.name === "design-review") ??
    items.find((w) => w.phase === "Ready");
  if (!ready) throw new Error("demo tenant has no Ready workspace to open");
  return { readyId: ready.id, readyName: ready.name };
}

async function capturePortal(browser, scheme, ids) {
  const ctx = await browser.newContext({
    viewport: VIEWPORT,
    deviceScaleFactor: 2,
    colorScheme: scheme,
    locale: "en-US",
    reducedMotion: "reduce",
    ignoreHTTPSErrors: true,
  });
  const page = await ctx.newPage();
  const shoot = async (stem) => {
    await page.waitForTimeout(500); // let fonts and the first poll settle
    const file = `${stem}-${scheme}.png`;
    await page.screenshot({ path: path.join(OUT_DIR, file) });
    console.log(`[portal] captured ${file}`);
  };

  // Log in through the mock's dev SSO form (real redirect path).
  await page.goto(`${PORTAL_ORIGIN}/`, { waitUntil: "domcontentloaded" });
  await page.getByRole("button", { name: "Log in with SSO" }).click();
  await page.getByRole("table", { name: "workspaces" }).waitFor();
  await shoot("portal-workspaces");

  await page.goto(`${PORTAL_ORIGIN}/workspaces/new`);
  await page.getByRole("heading", { name: "New workspace" }).waitFor();
  await page.getByLabel(/^Name\b/).fill("team-browser");
  await page.locator("select[name='template']").selectOption({ index: 1 });
  await shoot("portal-create");

  await page.goto(`${PORTAL_ORIGIN}/workspaces/${ids.readyId}`);
  await page.locator("h1", { hasText: ids.readyName }).waitFor();
  await shoot("portal-detail");

  await page.goto(`${PORTAL_ORIGIN}/admin`);
  await page.getByRole("heading", { name: "Tenant overview" }).waitFor();
  await shoot("admin-overview");

  await page.goto(`${PORTAL_ORIGIN}/admin/session-policy`);
  await page.getByRole("heading", { name: "Session policy", level: 1 }).waitFor();
  await shoot("admin-session-policy");

  await page.goto(`${PORTAL_ORIGIN}/admin/audit`);
  await page.getByRole("heading", { name: "Audit log", level: 1 }).waitFor();
  await shoot("admin-audit");

  await ctx.close();
}

async function shotPortal() {
  console.log("[portal] starting tests-portal harness (builds SPA + portal binary)…");
  const serve = spawn("node", ["--disable-warning=ExperimentalWarning", "tests-portal/serve.ts"], {
    cwd: WEB_DIR,
    stdio: ["ignore", "pipe", "inherit"],
    env: { ...process.env, MOCK_DEMO: "1" },
  });
  procs.push(serve);
  serve.stdout.on("data", (d) => process.stdout.write(`[harness] ${d}`));
  await waitHttp(`${MOCK_API}/_control/health`, "mock api");
  await waitHttp(`${PORTAL_ORIGIN}/healthz`, "portal");

  const ids = await seedPortalData();
  const browser = await chromium.launch();
  try {
    for (const scheme of SCHEMES) await capturePortal(browser, scheme, ids);
  } finally {
    await browser.close();
  }
}

// Lossy-then-lossless PNG optimisation when the tools exist; warn above the
// 400 KB README budget instead of failing (manual tool).
function optimise(files) {
  const has = (bin) => spawnSync(bin, ["--version"], { stdio: "ignore" }).status === 0;
  const quant = has("pngquant");
  const oxi = has("oxipng");
  if (!quant && !oxi) console.warn("[optimise] neither pngquant nor oxipng found; PNGs left as captured");
  for (const f of files) {
    const p = path.join(OUT_DIR, f);
    if (!existsSync(p)) continue;
    if (quant) spawnSync("pngquant", ["--force", "--skip-if-larger", "--quality=70-95", "--output", p, p], { stdio: "inherit" });
    if (oxi) spawnSync("oxipng", ["-o", "3", "--strip", "safe", "-q", p], { stdio: "inherit" });
    const kb = Math.round(statSync(p).size / 1024);
    if (kb > 400) console.warn(`[optimise] ${f} is ${kb} KB (target ≤ 400 KB)`);
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
  // tcdi-xferd owns :8443 and refuses to start without its credential.
  writeFileSync(path.join(dir, "xfer-token"), randomBytes(32).toString("base64url") + "\n", { mode: 0o600 });
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
      deviceScaleFactor: 2,
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
  if (ARGS.only !== "runtime") await shotPortal();
  if (ARGS.only !== "portal") await shotRuntimes();
  optimise(outputFiles(ARGS.only));
} finally {
  cleanup();
}
console.log(`done: screenshots in ${OUT_DIR}`);
