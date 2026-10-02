// TinyCDI soak harness — drives N concurrent workspace sessions through the
// portal for a fixed duration while recording connection quality, then
// writes soak-report.json (report.schema.json).
//
// Modes:
//   real run : `node soak.ts` with SOAK_PORTAL_URL / SOAK_USER /
//              SOAK_PASSWORD — Playwright Chromium logs in through the real
//              OIDC flow and drives the in-portal session pages.
//   dry run  : `node soak.ts --dry-run` — spawns the contract mock API
//              (web/tests/mock-api, read-only) and drives the identical API
//              flow at request level; no browser and no cluster needed.
//
// Credentials come only from the environment and are never logged or
// written to the report.
//
// Erasable-syntax TypeScript only: run with
//   node --disable-warning=ExperimentalWarning soak.ts ...

import { spawn, type ChildProcess } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import {
  chromium,
  request,
  type APIRequestContext,
  type BrowserContext,
  type Page,
} from "playwright";
import { parseDurationMs, type Observation } from "./metrics.ts";
import { buildReport, type SessionResult, type Thresholds } from "./report.ts";

// ---- pinned API shapes (PLAN.md T2.3 / T2.5; internal/api/openapi.yaml) ----

interface LaunchTicket {
  workspaceId: string;
  ticket: string;
  launchUrl: string;
  expiresAt: string;
}

interface ConnectionStatus {
  state: "none" | "connected" | "disconnected" | "stale";
  leaseActive: boolean;
  lastRenewedAt?: string;
}

interface WorkspaceView {
  id: string;
  name: string;
  phase: string;
  desiredState: string;
}

const CSRF_HEADER = "X-CSRF-Token";
const MAX_AUTO_RELAUNCH = 2; // per 5 minutes, mirrors web/src/session/useConnectionWatch.ts

// ---- configuration ----

export interface Options {
  dryRun: boolean;
  portalUrl: string | undefined;
  sessions: number;
  template: string | undefined;
  durationMs: number;
  inputIntervalMs: number;
  pollIntervalMs: number;
  connectTimeoutMs: number;
  readyTimeoutMs: number;
  reportPath: string;
  thresholds: Thresholds;
  verbose: boolean;
}

function env(name: string): string | undefined {
  const v = process.env[name];
  return v === undefined || v === "" ? undefined : v;
}

function usage(): string {
  return `Usage: node soak.ts [--dry-run] [flags]

Environment:
  SOAK_PORTAL_URL        portal base URL, e.g. https://portal.example.com
  SOAK_USER              OIDC username (real run only; never logged)
  SOAK_PASSWORD          OIDC password (real run only; never logged)
  SOAK_SESSIONS          concurrent workspaces (default 25)
  SOAK_TEMPLATE          template id or name (default: first listed)
  SOAK_DURATION          run length, e.g. 60m (default 60m)
  SOAK_INPUT_INTERVAL    input cadence per session (default 10s)
  SOAK_POLL_INTERVAL     connection-status poll cadence (default 5s)
  SOAK_CONNECT_TIMEOUT   launch to connected budget (default 120s)
  SOAK_READY_TIMEOUT     workspace Ready budget (default 5m)
  SOAK_MOCK_PORTAL_PORT  dry-run mock portal port (default: a free port)
  SOAK_MOCK_SESSION_PORT dry-run mock session port (default: a free port)
  SOAK_IGNORE_TLS_ERRORS set to 1 for self-signed dev certs
  SOAK_USER_SELECTOR / SOAK_PASSWORD_SELECTOR / SOAK_SUBMIT_SELECTOR
                         override the generic OIDC form selectors

Flags:
  --dry-run              run against the spawned contract mock, no browser
  --portal-url URL       overrides SOAK_PORTAL_URL (in dry-run: attach to an
                         already-running mock instead of spawning one)
  --sessions N --duration D --input-interval D --poll-interval D
  --template REF --report PATH (default ./soak-report.json)
  --connect-p95-ms N --reconnect-p95-ms N --max-gap-ms N   pass/fail thresholds
  --max-manual-actions N (default 0) --max-dropped N (default 0)
  --verbose`;
}

function nonNegativeNumber(flag: string, raw: string): number {
  const n = raw.trim() === "" ? NaN : Number(raw);
  if (!Number.isFinite(n) || n < 0) {
    throw new Error(`${flag} must be a non-negative number (got "${raw}")`);
  }
  return n;
}

function nonNegativeInteger(flag: string, raw: string): number {
  const n = nonNegativeNumber(flag, raw);
  if (!Number.isInteger(n)) throw new Error(`${flag} must be a non-negative integer (got "${raw}")`);
  return n;
}

export function parseArgs(argv: string[]): Options {
  const o: Options = {
    dryRun: false,
    portalUrl: env("SOAK_PORTAL_URL"),
    sessions: Number(env("SOAK_SESSIONS") ?? "25"),
    template: env("SOAK_TEMPLATE"),
    durationMs: parseDurationMs(env("SOAK_DURATION") ?? "60m"),
    inputIntervalMs: parseDurationMs(env("SOAK_INPUT_INTERVAL") ?? "10s"),
    pollIntervalMs: parseDurationMs(env("SOAK_POLL_INTERVAL") ?? "5s"),
    connectTimeoutMs: parseDurationMs(env("SOAK_CONNECT_TIMEOUT") ?? "120s"),
    readyTimeoutMs: parseDurationMs(env("SOAK_READY_TIMEOUT") ?? "5m"),
    reportPath: "soak-report.json",
    thresholds: {
      connectP95Ms: null,
      reconnectP95Ms: null,
      maxGapMs: null,
      maxManualActions: 0,
      maxDroppedSessions: 0,
    },
    verbose: false,
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const next = () => {
      const v = argv[++i];
      if (v === undefined) throw new Error(`${a} needs a value`);
      return v;
    };
    switch (a) {
      case "--dry-run":
        o.dryRun = true;
        break;
      case "--portal-url":
        o.portalUrl = next();
        break;
      case "--sessions":
        o.sessions = Number(next());
        break;
      case "--template":
        o.template = next();
        break;
      case "--duration":
        o.durationMs = parseDurationMs(next());
        break;
      case "--input-interval":
        o.inputIntervalMs = parseDurationMs(next());
        break;
      case "--poll-interval":
        o.pollIntervalMs = parseDurationMs(next());
        break;
      case "--connect-timeout":
        o.connectTimeoutMs = parseDurationMs(next());
        break;
      case "--ready-timeout":
        o.readyTimeoutMs = parseDurationMs(next());
        break;
      case "--report":
        o.reportPath = next();
        break;
      case "--connect-p95-ms":
        o.thresholds.connectP95Ms = nonNegativeNumber(a, next());
        break;
      case "--reconnect-p95-ms":
        o.thresholds.reconnectP95Ms = nonNegativeNumber(a, next());
        break;
      case "--max-gap-ms":
        o.thresholds.maxGapMs = nonNegativeNumber(a, next());
        break;
      case "--max-manual-actions":
        o.thresholds.maxManualActions = nonNegativeInteger(a, next());
        break;
      case "--max-dropped":
        o.thresholds.maxDroppedSessions = nonNegativeInteger(a, next());
        break;
      case "--verbose":
        o.verbose = true;
        break;
      case "-h":
      case "--help":
        console.log(usage());
        process.exit(0);
      default:
        throw new Error(`unknown flag ${a}\n${usage()}`);
    }
  }
  if (!Number.isInteger(o.sessions) || o.sessions < 1) {
    throw new Error("--sessions (or SOAK_SESSIONS) must be a positive integer");
  }
  if (o.portalUrl) o.portalUrl = o.portalUrl.replace(/\/+$/, "");
  return o;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function stamp(msg: string): void {
  console.log(`${new Date().toISOString()} ${msg}`);
}

// ---- small testable helpers ----

/** Candidate selectors for the OIDC login form; env overrides come first. */
export function loginSelectors(e: Record<string, string | undefined>): {
  user: string[];
  password: string[];
  submit: string;
} {
  const get = (name: string) => (e[name] === undefined || e[name] === "" ? undefined : e[name]);
  return {
    user: [
      get("SOAK_USER_SELECTOR"),
      'input[name="username"]',
      'input[name="login"]',
      'input[name="email"]',
      'input[type="email"]',
      "input#username",
      'input[autocomplete="username"]',
    ].filter((x): x is string => x !== undefined),
    password: [get("SOAK_PASSWORD_SELECTOR"), 'input[type="password"]'].filter(
      (x): x is string => x !== undefined,
    ),
    submit: get("SOAK_SUBMIT_SELECTOR") ?? 'button[type="submit"]',
  };
}

/**
 * Playwright installs its own SIGINT/SIGTERM/SIGHUP handlers that close the
 * browser and exit before ours can delete the workspaces and write the
 * report; turn them off so main()'s handlers own the shutdown.
 */
export function chromiumLaunchOptions(headless: boolean) {
  return { headless, handleSIGINT: false, handleSIGTERM: false, handleSIGHUP: false };
}

/** True when the session view is asking the user to take the session over. */
export async function takeoverDialogVisible(page: {
  getByText(text: string): { count(): Promise<number> };
}): Promise<boolean> {
  return (await page.getByText("Take over session").count()) > 0;
}

// ---- portal API client (shared by both drivers) ----

export class PortalApi {
  ctx: APIRequestContext;
  portal: string;
  constructor(ctx: APIRequestContext, portal: string) {
    this.ctx = ctx;
    this.portal = portal;
  }

  async csrfToken(): Promise<string> {
    // v0.2 (T2.3, D17): GET /v1/me returns csrfToken. The pre-T2.3 mock
    // still carries the legacy readable tcdi_csrf cookie; accept either so
    // the harness works against both.
    const me = await this.ctx.get(`${this.portal}/v1/me`);
    if (me.ok()) {
      const body = (await me.json()) as { csrfToken?: string };
      if (body.csrfToken) return body.csrfToken;
    }
    const state = await this.ctx.storageState();
    const legacy = state.cookies.find((c) => /^(?:__Host-)?tcdi_csrf$/.test(c.name));
    if (legacy?.value) return legacy.value;
    throw new Error("no CSRF token: /v1/me has none and no tcdi_csrf cookie");
  }

  async listTemplates(): Promise<{ id: string; name: string }[]> {
    const r = await this.ctx.get(`${this.portal}/v1/templates`);
    if (!r.ok()) throw new Error(`GET /v1/templates -> ${r.status()}`);
    const body = (await r.json()) as { items?: { id: string; name: string }[] };
    return body.items ?? [];
  }

  async createWorkspace(name: string, templateRef: string): Promise<string> {
    const r = await this.ctx.post(`${this.portal}/v1/workspaces`, {
      data: { name, templateRef, desiredState: "Running" },
      headers: {
        [CSRF_HEADER]: await this.csrfToken(),
        "Idempotency-Key": randomUUID(),
      },
    });
    if (!r.ok()) throw new Error(`POST /v1/workspaces -> ${r.status()}: ${await r.text()}`);
    return ((await r.json()) as WorkspaceView).id;
  }

  async workspace(id: string): Promise<WorkspaceView> {
    const r = await this.ctx.get(`${this.portal}/v1/workspaces/${id}`);
    if (!r.ok()) throw new Error(`GET workspace ${id} -> ${r.status()}`);
    return (await r.json()) as WorkspaceView;
  }

  async deleteWorkspace(id: string): Promise<void> {
    const r = await this.ctx.delete(`${this.portal}/v1/workspaces/${id}`, {
      headers: { [CSRF_HEADER]: await this.csrfToken() },
    });
    if (!r.ok() && r.status() !== 404) {
      throw new Error(`DELETE workspace ${id} -> ${r.status()}`);
    }
  }

  async createConnection(id: string, takeover: boolean): Promise<LaunchTicket> {
    const r = await this.ctx.post(`${this.portal}/v1/workspaces/${id}/connections`, {
      data: { takeover },
      headers: { [CSRF_HEADER]: await this.csrfToken() },
    });
    if (!r.ok()) {
      const e = new Error(`POST connections ${id} -> ${r.status()}: ${await r.text()}`);
      (e as Error & { status?: number }).status = r.status();
      throw e;
    }
    return (await r.json()) as LaunchTicket;
  }

  /**
   * ConnectionStatus from GET /v1/workspaces/{id}/connection (T2.3 shape).
   * Returns null when the endpoint is absent (404) so the caller can fall
   * back to a session-origin probe.
   */
  async connectionStatus(id: string): Promise<(ConnectionStatus & { status: number }) | null> {
    const r = await this.ctx.get(`${this.portal}/v1/workspaces/${id}/connection`);
    if (r.status() === 404) return null;
    if (!r.ok()) return { status: r.status(), state: "none", leaseActive: false };
    return { ...((await r.json()) as ConnectionStatus), status: 200 };
  }
}

// ---- driver interface ----

export interface SessionProbe {
  state: Observation["state"];
  source: "api" | "probe";
}

export interface Driver {
  login(): Promise<void>;
  openSession(id: string): Promise<void>;
  /**
   * Reload the session page. After FX-R3c the reload resumes without a new
   * ticket; takeoverPrompted reports that the portal asked the user to take
   * the session over instead (a human would have had to click).
   */
  reloadSession(id: string): Promise<{ takeoverPrompted: boolean }>;
  sendInput(id: string): Promise<void>;
  probe(id: string): Promise<SessionProbe>;
  closeSession(id: string): Promise<void>;
  dispose(): Promise<void>;
}

/**
 * Request-level driver: used by --dry-run against the contract mock. The
 * mock predates T2.5's scriptable /connection endpoint, so state falls back
 * to probing the session origin's /desktop page (200 = connected). Input is
 * synthetic: the mock has no input channel, so ticks are counted only.
 */
export class ApiDriver implements Driver {
  ctx: APIRequestContext;
  portal: string;
  api: PortalApi;
  useControl: boolean;
  sessionPage = new Map<string, { origin: string; path: string; cookie: string }>();

  constructor(ctx: APIRequestContext, portal: string, useControl: boolean) {
    this.ctx = ctx;
    this.portal = portal;
    this.useControl = useControl;
    this.api = new PortalApi(ctx, portal);
  }

  async login(): Promise<void> {
    // Dev/test login: POST /v1/login mints the session cookie (mock API and
    // the backend's dev login alike). Real OIDC needs BrowserDriver.
    const r = await this.ctx.post(`${this.portal}/v1/login`, { maxRedirects: 0 });
    if (r.status() !== 302 && !r.ok()) {
      throw new Error(`POST /v1/login -> ${r.status()}`);
    }
    const check = await this.ctx.get(`${this.portal}/v1/workspaces`);
    if (!check.ok()) throw new Error(`login did not yield a session (${check.status()})`);
  }

  /** Fast-forward a workspace to Ready through the mock's /_control API. */
  async forceReady(id: string): Promise<void> {
    if (!this.useControl) return;
    const r = await this.ctx.post(`${this.portal}/_control/workspaces/${id}`, {
      data: { phase: "Ready" },
    });
    if (!r.ok()) throw new Error(`control ready ${id} -> ${r.status()}`);
  }

  async openSession(id: string, allowTakeover = true): Promise<void> {
    let ticket: LaunchTicket;
    try {
      ticket = await this.api.createConnection(id, false);
    } catch (e) {
      if (allowTakeover && (e as Error & { status?: number }).status === 409) {
        ticket = await this.api.createConnection(id, true); // takeover relaunch
      } else {
        throw e;
      }
    }
    const sessionOrigin = new URL(ticket.launchUrl).origin;
    // Ticket is POSTed as a form field, never in a URL (ADR 0001/0002).
    // Redirects are followed by hand: the mock's tcdi_desktop cookie carries
    // Secure, which the request jar refuses to replay over plain http, so
    // the cookie is captured here and sent explicitly on session requests.
    const r = await this.ctx.post(ticket.launchUrl, {
      form: { ticket: ticket.ticket },
      maxRedirects: 0,
    });
    if (r.status() !== 302 && r.status() !== 303) {
      throw new Error(`launch ${id} -> ${r.status()}`);
    }
    const setCookie = r
      .headersArray()
      .find((h) => h.name.toLowerCase() === "set-cookie" && h.value.startsWith("tcdi_desktop="));
    const location = r.headers()["location"];
    if (!setCookie || !location) throw new Error(`launch ${id}: no cookie/redirect`);
    const page = {
      origin: sessionOrigin,
      path: new URL(location, sessionOrigin).pathname,
      cookie: setCookie.value.split(";")[0],
    };
    this.sessionPage.set(id, page);
    const landed = await this.ctx.get(`${page.origin}${page.path}`, {
      headers: { cookie: page.cookie },
    });
    if (!landed.ok()) throw new Error(`desktop ${id} -> ${landed.status()}`);
  }

  async reloadSession(id: string): Promise<{ takeoverPrompted: boolean }> {
    const page = this.sessionPage.get(id);
    if (!page) throw new Error(`reload before launch: ${id}`);
    const r = await this.ctx.get(`${page.origin}${page.path}`, {
      headers: { cookie: page.cookie },
    });
    if (r.status() === 401 || r.status() === 403) {
      // Session cookie lost: re-launch, but never take the session over — a
      // 409 here is the request-level equivalent of the take-over dialog.
      try {
        await this.openSession(id, false);
      } catch (e) {
        if ((e as Error & { status?: number }).status === 409) return { takeoverPrompted: true };
        throw e;
      }
    }
    return { takeoverPrompted: false };
  }

  sendInput(_id: string): Promise<void> {
    // No input channel in the mock: the tick is counted by the caller.
    return Promise.resolve();
  }

  async probe(id: string): Promise<SessionProbe> {
    const st = await this.api.connectionStatus(id);
    if (st !== null) {
      return st.status === 200
        ? { state: st.state, source: "api" }
        : { state: "error", source: "api" };
    }
    // Endpoint absent (pre-T2.5 mock): probe the session origin instead.
    const page = this.sessionPage.get(id);
    if (!page) return { state: "none", source: "probe" };
    const r = await this.ctx.get(`${page.origin}${page.path}`, {
      headers: { cookie: page.cookie },
    });
    if (r.ok()) return { state: "connected", source: "probe" };
    if (r.status() === 401 || r.status() === 403 || r.status() === 404) {
      return { state: "disconnected", source: "probe" };
    }
    return { state: "error", source: "probe" };
  }

  closeSession(id: string): Promise<void> {
    this.sessionPage.delete(id);
    return Promise.resolve();
  }

  dispose(): Promise<void> {
    return Promise.resolve();
  }
}

/**
 * Browser driver for real runs: one Chromium context per soak run, one tab
 * per session showing the in-portal session view (/workspaces/{id}/session).
 * Input is real mouse movement and key presses into the session frame.
 */
export class BrowserDriver implements Driver {
  context: BrowserContext;
  portal: string;
  api: PortalApi;
  pages = new Map<string, Page>();

  constructor(context: BrowserContext, portal: string) {
    this.context = context;
    this.portal = portal;
    this.api = new PortalApi(context.request, portal);
  }

  /** First visible locator among the candidates, waiting up to timeoutMs. */
  private async firstMatch(page: Page, selectors: string[], timeoutMs: number) {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      for (const selector of selectors) {
        const loc = page.locator(selector).first();
        if ((await loc.count()) > 0 && (await loc.isVisible())) return loc;
      }
      if (Date.now() > deadline) {
        throw new Error(`no OIDC password field matched: ${selectors.join(", ")}`);
      }
      await sleep(250);
    }
  }

  private async fillFirst(page: Page, selectors: string[], value: string): Promise<void> {
    for (const sel of selectors) {
      const loc = page.locator(sel).first();
      if ((await loc.count()) > 0) {
        await loc.fill(value);
        return;
      }
    }
    throw new Error(`no OIDC field matched: ${selectors.join(", ")}`);
  }

  async login(): Promise<void> {
    const user = env("SOAK_USER");
    const password = env("SOAK_PASSWORD");
    if (!user || !password) throw new Error("SOAK_USER and SOAK_PASSWORD are required");
    const sel = loginSelectors(process.env);
    const page = await this.context.newPage();
    try {
      await page.goto(this.portal, { waitUntil: "domcontentloaded" });
      // The portal bounces to the IdP; wait for its password field.
      const pw = await this.firstMatch(page, sel.password, 30_000);
      await pw.waitFor({ state: "visible", timeout: 30_000 });
      await this.fillFirst(page, sel.user, user);
      await pw.fill(password);
      const submit = page.locator(sel.submit).first();
      if ((await submit.count()) > 0) await submit.click();
      else await pw.press("Enter");
      await page.waitForURL(`${this.portal}/**`, { timeout: 60_000 });
      const check = await this.context.request.get(`${this.portal}/v1/workspaces`);
      if (!check.ok()) throw new Error(`login did not yield a session (${check.status()})`);
    } finally {
      await page.close();
    }
  }

  async openSession(id: string): Promise<void> {
    // A relaunch replaces the session's tab: close the stale one first or
    // every automatic relaunch leaks a tab (and a live frame) for the run.
    await this.pages.get(id)?.close();
    this.pages.delete(id);
    const page = await this.context.newPage();
    this.pages.set(id, page);
    await page.goto(`${this.portal}/workspaces/${id}/session`, {
      waitUntil: "domcontentloaded",
    });
    // Focus the session frame once so later keyboard input lands in it.
    const frame = page.locator("iframe").first();
    if ((await frame.count()) > 0) {
      const box = await frame.boundingBox();
      if (box) await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
    }
  }

  async reloadSession(id: string): Promise<{ takeoverPrompted: boolean }> {
    const page = this.pages.get(id);
    if (!page) throw new Error(`reload before launch: ${id}`);
    await page.reload({ waitUntil: "domcontentloaded" });
    // The SPA renders the dialog after its connection request answers; give
    // it a few seconds to appear before declaring the reload seamless.
    for (let waited = 0; waited < 3_000; waited += 250) {
      if (await takeoverDialogVisible(page)) return { takeoverPrompted: true };
      await sleep(250);
    }
    return { takeoverPrompted: await takeoverDialogVisible(page) };
  }

  async sendInput(id: string): Promise<void> {
    const page = this.pages.get(id);
    if (!page) return;
    const frame = page.locator("iframe").first();
    const box = await frame.boundingBox();
    if (box) {
      const x = box.x + box.width * (0.2 + 0.6 * Math.random());
      const y = box.y + box.height * (0.2 + 0.6 * Math.random());
      await page.mouse.move(x, y, { steps: 4 });
    }
    const keys = ["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown", "a", "e", "Tab"];
    await page.keyboard.press(keys[Math.floor(Math.random() * keys.length)]);
  }

  async probe(id: string): Promise<SessionProbe> {
    const st = await this.api.connectionStatus(id);
    if (st !== null && st.status === 200) return { state: st.state, source: "api" };
    return { state: "error", source: "api" };
  }

  async closeSession(id: string): Promise<void> {
    await this.pages.get(id)?.close();
    this.pages.delete(id);
  }

  async dispose(): Promise<void> {
    await this.context.close();
  }
}

// ---- dry-run mock server ----

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.once("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const port = (srv.address() as net.AddressInfo).port;
      srv.close(() => resolve(port));
    });
  });
}

async function spawnMock(): Promise<{ child: ChildProcess; portal: string }> {
  const repoRoot = path.resolve(import.meta.dirname, "..", "..");
  const serverTs = path.join(repoRoot, "web", "tests", "mock-api", "server.ts");
  if (!fs.existsSync(serverTs)) throw new Error(`mock API not found at ${serverTs}`);
  // Honour configured ports, else pick free ones to avoid colliding with
  // the playwright suites' fixed 4310/4311.
  const portalPort = env("SOAK_MOCK_PORTAL_PORT")
    ? Number(env("SOAK_MOCK_PORTAL_PORT"))
    : await freePort();
  const sessionPort = env("SOAK_MOCK_SESSION_PORT")
    ? Number(env("SOAK_MOCK_SESSION_PORT"))
    : await freePort();
  const child = spawn(
    process.execPath,
    ["--disable-warning=ExperimentalWarning", serverTs],
    {
      env: {
        ...process.env,
        MOCK_PORTAL_PORT: String(portalPort),
        MOCK_SESSION_PORT: String(sessionPort),
      },
      stdio: ["ignore", "ignore", "inherit"],
    },
  );
  const portal = `http://127.0.0.1:${portalPort}`;
  const deadline = Date.now() + 30_000;
  for (;;) {
    try {
      const r = await fetch(`${portal}/_control/health`);
      if (r.ok) break;
    } catch {
      /* not up yet */
    }
    if (Date.now() > deadline) {
      child.kill("SIGKILL");
      throw new Error("mock API did not become healthy in 30s");
    }
    await sleep(200);
  }
  return { child, portal };
}

// ---- orchestration ----

export interface SessionCtx extends SessionResult {
  id: string;
  relaunches: number;
  recentRelaunches: number[];
  lastInputAt: number;
  lastConnectedAt: number | null;
  /** When the latest automatic relaunch was issued; starts a fresh connect budget. */
  relaunchedAt: number | null;
}

export function newSession(id: string, name: string): SessionCtx {
  return {
    id,
    workspaceId: id,
    workspaceName: name,
    observations: [],
    launchedAt: null,
    reloadedAt: null,
    manualActions: 0,
    inputEvents: 0,
    dropped: false,
    runEndAt: 0,
    relaunches: 0,
    recentRelaunches: [],
    lastInputAt: 0,
    lastConnectedAt: null,
    relaunchedAt: null,
  };
}

export interface DriveOptions {
  durationMs: number;
  inputIntervalMs: number;
  pollIntervalMs: number;
  connectTimeoutMs: number;
  verbose: boolean;
}

export interface DriveOutcome {
  /** When every session had first connected (or been given up on); null if never. */
  soakStartedAt: number | null;
  /** The soak ran for its full duration (not stopped early). */
  completed: boolean;
  /** Harness-level assertion failures to add to the report. */
  failures: string[];
}

/**
 * Open every session, supervise it, run the soak clock and the mid-run
 * reload. Pure orchestration over a Driver so tests can script the driver.
 *
 * Each session's poller starts right after its own open, so connectMs is
 * per session and not inflated by the sessions opened after it. The soak
 * clock starts when the last session first connects — workspace creation and
 * readiness do not eat into the requested duration.
 *
 * Reconnect supervision: a session that never connects, or stays
 * non-connected longer than the connect budget, is re-launched automatically,
 * at most MAX_AUTO_RELAUNCH per 5 minutes; beyond that it counts as dropped
 * plus one manual action. Every relaunch starts a fresh connect budget.
 */
export async function driveSessions(
  opts: DriveOptions,
  sessions: SessionCtx[],
  driver: Driver,
  shouldStop: () => boolean,
): Promise<DriveOutcome> {
  const failures: string[] = [];
  const stopPoll = { v: false };
  const pollers: Promise<void>[] = [];

  const watch = async (s: SessionCtx): Promise<void> => {
    while (!stopPoll.v) {
      const t0 = Date.now();
      try {
        const p = await driver.probe(s.id);
        const at = Date.now();
        s.observations.push({ at, state: p.state, source: p.source });
        if (p.state === "connected") s.lastConnectedAt = at;
      } catch {
        s.observations.push({ at: Date.now(), state: "error", source: "api" });
      }
      const now = Date.now();
      // The connect budget runs from the latest of: the launch, the last
      // time the session was connected, the last automatic relaunch.
      const reference = Math.max(s.launchedAt ?? t0, s.lastConnectedAt ?? 0, s.relaunchedAt ?? 0);
      if (now - reference > opts.connectTimeoutMs) {
        s.recentRelaunches = s.recentRelaunches.filter((t) => now - t < 5 * 60_000);
        if (s.recentRelaunches.length < MAX_AUTO_RELAUNCH) {
          s.recentRelaunches.push(now);
          s.relaunches++;
          s.relaunchedAt = now;
          try {
            await driver.openSession(s.id);
          } catch (e) {
            console.error(`relaunch ${s.id}: ${(e as Error).message}`);
          }
        } else if (!s.dropped) {
          s.dropped = true;
          s.manualActions++;
          console.error(`${s.id} dropped after ${MAX_AUTO_RELAUNCH} auto-relaunches`);
        }
      }
      await sleep(Math.max(10, opts.pollIntervalMs - (Date.now() - t0)));
    }
  };

  try {
    for (const s of sessions) {
      if (shouldStop()) break;
      s.launchedAt = Date.now();
      await driver.openSession(s.id);
      pollers.push(watch(s));
    }

    // Soak clock: every session connected at least once (or given up on).
    while (
      !shouldStop() &&
      !sessions.every((s) => s.lastConnectedAt !== null || s.dropped)
    ) {
      await sleep(Math.min(100, opts.pollIntervalMs));
    }
    if (shouldStop()) return { soakStartedAt: null, completed: false, failures };
    const soakStartedAt = Date.now();
    const deadline = soakStartedAt + opts.durationMs;
    const reloadAt = soakStartedAt + opts.durationMs / 2;
    let reloaded = false;

    while (Date.now() < deadline && !shouldStop()) {
      if (!reloaded && Date.now() >= reloadAt) {
        reloaded = true;
        stamp("mid-run reload of every session");
        for (const s of sessions) {
          s.reloadedAt = Date.now();
          try {
            const r = await driver.reloadSession(s.id);
            if (r.takeoverPrompted) {
              // FX-R3c: a reload resumes without a ticket. A take-over prompt
              // means a human would have had to click: always a failure.
              s.manualActions++;
              failures.push(`take-over prompt after the mid-run reload of ${s.id}`);
            }
          } catch (e) {
            console.error(`reload ${s.id}: ${(e as Error).message}`);
          }
        }
      }
      for (const s of sessions) {
        if (Date.now() - s.lastInputAt < opts.inputIntervalMs) continue;
        s.lastInputAt = Date.now();
        try {
          await driver.sendInput(s.id);
          s.inputEvents++;
        } catch (e) {
          if (opts.verbose) console.error(`input ${s.id}: ${(e as Error).message}`);
        }
      }
      await sleep(Math.min(1_000, opts.inputIntervalMs));
    }
    return { soakStartedAt, completed: Date.now() >= deadline, failures };
  } finally {
    stopPoll.v = true;
    await Promise.allSettled(pollers);
  }
}

async function waitForReady(api: PortalApi, id: string, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const ws = await api.workspace(id);
    if (ws.phase === "Ready" && ws.desiredState === "Running") return;
    if (ws.phase === "Failed") throw new Error(`workspace ${id} failed`);
    if (Date.now() > deadline) throw new Error(`workspace ${id} not Ready in ${timeoutMs}ms`);
    await sleep(2_000);
  }
}

async function run(opts: Options, shouldStop: () => boolean): Promise<number> {
  let mock: { child: ChildProcess; portal: string } | undefined;
  if (opts.dryRun && !opts.portalUrl) {
    mock = await spawnMock();
    opts.portalUrl = mock.portal;
    stamp(`mock api up at ${mock.portal}`);
  }
  const portalUrl = opts.portalUrl;
  if (!portalUrl) throw new Error("set SOAK_PORTAL_URL or --portal-url (or use --dry-run)");

  const sessions: SessionCtx[] = [];
  const startedAt = Date.now();
  let runError: Error | undefined;
  let outcome: DriveOutcome | undefined;

  // The API client must share the browser context's cookie jar in real
  // mode, so it is created per driver below.
  let api: PortalApi;
  let driver: Driver;
  let ownCtx: APIRequestContext | undefined;
  if (opts.dryRun) {
    ownCtx = await request.newContext({
      ignoreHTTPSErrors: env("SOAK_IGNORE_TLS_ERRORS") === "1",
    });
    api = new PortalApi(ownCtx, portalUrl);
    driver = new ApiDriver(ownCtx, portalUrl, true);
  } else {
    const browser = await chromium.launch(chromiumLaunchOptions(env("SOAK_HEADFUL") !== "1"));
    const context = await browser.newContext({
      ignoreHTTPSErrors: env("SOAK_IGNORE_TLS_ERRORS") === "1",
    });
    api = new PortalApi(context.request, portalUrl);
    driver = new BrowserDriver(context, portalUrl);
  }

  const cleanup = async () => {
    for (const s of sessions) {
      try {
        await driver.closeSession(s.id);
        await api.deleteWorkspace(s.id);
      } catch (e) {
        console.error(`cleanup ${s.id}: ${(e as Error).message}`);
      }
    }
    try {
      await driver.dispose();
      await ownCtx?.dispose();
    } catch {
      /* teardown */
    }
    mock?.child.kill("SIGTERM");
  };

  try {
    stamp(`login (${opts.dryRun ? "dev login" : "OIDC"})`);
    await driver.login();

    const templates = await api.listTemplates();
    const tpl = opts.template
      ? (templates.find((t) => t.id === opts.template || t.name === opts.template)?.id ??
        opts.template)
      : templates[0]?.id;
    if (!tpl) throw new Error("no templates listed");

    stamp(`creating ${opts.sessions} workspaces on template ${tpl}`);
    for (let i = 0; i < opts.sessions && !shouldStop(); i++) {
      const name = `soak-${startedAt.toString(36)}-${String(i).padStart(3, "0")}`;
      const id = await api.createWorkspace(name, tpl);
      sessions.push(newSession(id, name));
      if (driver instanceof ApiDriver) await driver.forceReady(id);
    }
    if (!(driver instanceof ApiDriver)) {
      await Promise.all(sessions.map((s) => waitForReady(api, s.id, opts.readyTimeoutMs)));
    }

    stamp("opening sessions");
    outcome = await driveSessions(opts, sessions, driver, shouldStop);
  } catch (e) {
    runError = e as Error;
    console.error(`run aborted: ${runError.message}`);
  } finally {
    const endedAt = Date.now();
    for (const s of sessions) s.runEndAt = endedAt;
    await cleanup();

    const report = buildReport(
      {
        dryRun: opts.dryRun,
        portalOrigin: new URL(portalUrl).origin,
        ...(opts.template !== undefined ? { template: opts.template } : {}),
        startedAt,
        endedAt,
        soakStartedAt: outcome?.soakStartedAt ?? null,
        requestedDurationMs: opts.durationMs,
        sessionsRequested: opts.sessions,
        inputIntervalSeconds: opts.inputIntervalMs / 1000,
        pollIntervalSeconds: opts.pollIntervalMs / 1000,
      },
      opts.thresholds,
      sessions,
      [
        ...(outcome?.failures ?? []),
        ...(runError ? [`run aborted: ${runError.message}`] : []),
      ],
    );
    fs.writeFileSync(opts.reportPath, JSON.stringify(report, null, 2) + "\n");
    const summary = report.summary as { pass: boolean; failures: string[] };
    stamp(
      `report at ${opts.reportPath} — ${summary.pass ? "PASS" : "FAIL"}` +
        (summary.failures.length ? `: ${summary.failures.join("; ")}` : ""),
    );
    if (runError || !summary.pass) return 1;
  }
  return 0;
}

async function main(): Promise<number> {
  const opts = parseArgs(process.argv.slice(2));
  let hits = 0;
  let stopped = false;
  const onSig = () => {
    hits++;
    if (hits === 1) {
      console.error("signal received — cleaning up workspaces and writing the report");
      stopped = true;
    } else {
      process.exit(130);
    }
  };
  process.on("SIGINT", onSig);
  process.on("SIGTERM", onSig);
  process.on("SIGHUP", onSig);
  return run(opts, () => stopped);
}

if (import.meta.main) {
  main()
    .then((code) => process.exit(code))
    .catch((e) => {
      console.error(`soak failed: ${(e as Error).message}`);
      process.exit(1);
    });
}
