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

import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import {
  chromium,
  request,
  type APIRequestContext,
  type APIResponse,
  type Browser,
  type BrowserContext,
  type Page,
} from "playwright";
import { parseDurationMs, percentile, type Observation } from "./metrics.ts";
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

/** One soak lane: a user whose share of the sessions it owns. */
export interface SoakUser {
  name: string;
  password: string;
}

export interface Options {
  dryRun: boolean;
  portalUrl: string | undefined;
  sessions: number;
  template: string | undefined;
  durationMs: number;
  inputIntervalMs: number;
  /** Per-dispatch cap on a scripted input; a wedged page op cannot starve the rest. */
  inputTimeoutMs: number;
  pollIntervalMs: number;
  connectTimeoutMs: number;
  readyTimeoutMs: number;
  reportPath: string;
  users: SoakUser[];
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
  SOAK_USERS_FILE        CSV "user,password" per line: sessions spread
                         round-robin over these users (multi-user runs)
  SOAK_PROFILE           profile name under tests/soak/profiles/ (e.g.
                         soak-100); flags and other env vars override it
  SOAK_ABORT_DROPPED_PCT abort the run once dropped sessions exceed this
                         share of the total, in percent (default 1)
  SOAK_ABORT_CONNECT_P95_MS
                         abort once the ramp-up connect p95 exceeds this
                         (unset: never; the scale runs use 15000)
  SOAK_LOGIN_CONCURRENCY OIDC logins at once during ramp-up (default 4)
  SOAK_LOGIN_ATTEMPTS    per-lane login tries before the lane is skipped
                         (all lanes skipped = abort) (default 3)
  SOAK_MOCK_PORTAL_PORT  dry-run mock portal port (default: a free port)
  SOAK_MOCK_SESSION_PORT dry-run mock session port (default: a free port)
  SOAK_IGNORE_TLS_ERRORS set to 1 for self-signed dev certs
  SOAK_USER_SELECTOR / SOAK_PASSWORD_SELECTOR / SOAK_SUBMIT_SELECTOR
                         override the generic OIDC form selectors

Flags:
  --dry-run              run against the spawned contract mock, no browser
  --portal-url URL       overrides SOAK_PORTAL_URL (in dry-run: attach to an
                         already-running mock instead of spawning one)
  --profile NAME         load tests/soak/profiles/NAME.json as defaults
  --users-file PATH      overrides SOAK_USERS_FILE
  --sessions N --duration D --input-interval D --input-timeout-ms N --poll-interval D
  --template REF --report PATH (default ./soak-report.json)
  --connect-p95-ms N --reconnect-p95-ms N --max-gap-ms N   pass/fail thresholds
  --disconnect-p95-ms N --disconnect-p100-ms N --input-resume-p100-ms N  drill gates
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

/**
 * A named default set under tests/soak/profiles/<name>.json. Fields use the
 * same names/units as the environment variables; thresholds use the
 * Thresholds names. Unknown keys are rejected so a typo cannot silently
 * change a soak's scale.
 */
interface Profile {
  sessions?: number;
  duration?: string;
  template?: string;
  inputInterval?: string;
  pollInterval?: string;
  connectTimeout?: string;
  readyTimeout?: string;
  thresholds?: Partial<Thresholds>;
}

const PROFILE_KEYS = new Set([
  "sessions",
  "duration",
  "template",
  "inputInterval",
  "pollInterval",
  "connectTimeout",
  "readyTimeout",
  "thresholds",
]);
const PROFILE_THRESHOLD_KEYS = new Set([
  "connectP95Ms",
  "reconnectP95Ms",
  "maxGapMs",
  "disconnectP95Ms",
  "disconnectP100Ms",
  "inputResumeP100Ms",
  "maxManualActions",
  "maxDroppedSessions",
]);

export function loadProfile(name: string): Profile {
  if (!/^[A-Za-z0-9._-]+$/.test(name)) throw new Error(`bad profile name "${name}"`);
  const file = path.resolve(import.meta.dirname, "profiles", `${name}.json`);
  let raw: unknown;
  try {
    raw = JSON.parse(fs.readFileSync(file, "utf8"));
  } catch (e) {
    throw new Error(`cannot load profile ${name} (${file}): ${(e as Error).message}`);
  }
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    throw new Error(`profile ${name} must be a JSON object`);
  }
  const p = raw as Record<string, unknown>;
  for (const k of Object.keys(p)) {
    if (!PROFILE_KEYS.has(k)) throw new Error(`profile ${name}: unknown key "${k}"`);
  }
  if (p.sessions !== undefined && !Number.isInteger(p.sessions)) {
    throw new Error(`profile ${name}: sessions must be an integer`);
  }
  for (const k of ["duration", "template", "inputInterval", "pollInterval", "connectTimeout", "readyTimeout"]) {
    if (p[k] !== undefined && typeof p[k] !== "string") {
      throw new Error(`profile ${name}: ${k} must be a string`);
    }
  }
  if (p.thresholds !== undefined) {
    if (typeof p.thresholds !== "object" || p.thresholds === null) {
      throw new Error(`profile ${name}: thresholds must be an object`);
    }
    for (const [k, v] of Object.entries(p.thresholds as Record<string, unknown>)) {
      if (!PROFILE_THRESHOLD_KEYS.has(k)) throw new Error(`profile ${name}: unknown threshold "${k}"`);
      if (v !== null && (typeof v !== "number" || !Number.isFinite(v) || v < 0)) {
        throw new Error(`profile ${name}: threshold ${k} must be a non-negative number or null`);
      }
    }
  }
  return p as Profile;
}

/**
 * CSV "user,password" lanes (comments/blank lines skipped). Passwords may
 * contain commas only when nothing after the first comma needs splitting —
 * the split is on the first comma so `u,p,a` means password "p,a". The
 * file is read once at option parsing; its path is never logged.
 */
export function parseUsersFile(file: string): SoakUser[] {
  let text: string;
  try {
    text = fs.readFileSync(file, "utf8");
  } catch (e) {
    throw new Error(`cannot read users file ${file}: ${(e as Error).message}`);
  }
  const users: SoakUser[] = [];
  const seen = new Set<string>();
  text.split(/\r?\n/).forEach((line, i) => {
    const t = line.trim();
    if (t === "" || t.startsWith("#")) return;
    const comma = t.indexOf(",");
    if (comma <= 0 || comma === t.length - 1) {
      throw new Error(`users file ${file}:${i + 1}: expected "user,password"`);
    }
    const name = t.slice(0, comma).trim();
    const password = t.slice(comma + 1).trim();
    if (seen.has(name)) throw new Error(`users file ${file}:${i + 1}: duplicate user "${name}"`);
    seen.add(name);
    users.push({ name, password });
  });
  if (users.length === 0) throw new Error(`users file ${file}: no users`);
  return users;
}

export function parseArgs(argv: string[]): Options {
  const o: Options = {
    dryRun: false,
    portalUrl: undefined,
    sessions: 25,
    template: undefined,
    durationMs: parseDurationMs("60m"),
    inputIntervalMs: parseDurationMs("10s"),
    inputTimeoutMs: 5_000,
    pollIntervalMs: parseDurationMs("5s"),
    connectTimeoutMs: parseDurationMs("120s"),
    readyTimeoutMs: parseDurationMs("5m"),
    reportPath: "soak-report.json",
    users: [],
    thresholds: {
      connectP95Ms: null,
      reconnectP95Ms: null,
      maxGapMs: null,
      maxManualActions: 0,
      maxDroppedSessions: 0,
    },
    verbose: false,
  };
  // Defaults < profile < environment < flags.
  const profileIdx = argv.indexOf("--profile");
  if (profileIdx !== -1 && argv[profileIdx + 1] === undefined) {
    throw new Error("--profile needs a value");
  }
  const profileName =
    profileIdx !== -1 ? argv[profileIdx + 1] : env("SOAK_PROFILE");
  if (profileName !== undefined) {
    const p = loadProfile(profileName);
    if (p.sessions !== undefined) o.sessions = p.sessions;
    if (p.template !== undefined) o.template = p.template;
    if (p.duration !== undefined) o.durationMs = parseDurationMs(p.duration);
    if (p.inputInterval !== undefined) o.inputIntervalMs = parseDurationMs(p.inputInterval);
    if (p.pollInterval !== undefined) o.pollIntervalMs = parseDurationMs(p.pollInterval);
    if (p.connectTimeout !== undefined) o.connectTimeoutMs = parseDurationMs(p.connectTimeout);
    if (p.readyTimeout !== undefined) o.readyTimeoutMs = parseDurationMs(p.readyTimeout);
    if (p.thresholds) Object.assign(o.thresholds, p.thresholds);
  }
  if (env("SOAK_PORTAL_URL") !== undefined) o.portalUrl = env("SOAK_PORTAL_URL");
  if (env("SOAK_SESSIONS") !== undefined) o.sessions = Number(env("SOAK_SESSIONS"));
  if (env("SOAK_TEMPLATE") !== undefined) o.template = env("SOAK_TEMPLATE");
  if (env("SOAK_DURATION") !== undefined) o.durationMs = parseDurationMs(env("SOAK_DURATION")!);
  if (env("SOAK_INPUT_INTERVAL") !== undefined) {
    o.inputIntervalMs = parseDurationMs(env("SOAK_INPUT_INTERVAL")!);
  }
  if (env("SOAK_INPUT_TIMEOUT_MS") !== undefined) {
    o.inputTimeoutMs = nonNegativeNumber("SOAK_INPUT_TIMEOUT_MS", env("SOAK_INPUT_TIMEOUT_MS")!);
  }
  if (env("SOAK_POLL_INTERVAL") !== undefined) {
    o.pollIntervalMs = parseDurationMs(env("SOAK_POLL_INTERVAL")!);
  }
  if (env("SOAK_CONNECT_TIMEOUT") !== undefined) {
    o.connectTimeoutMs = parseDurationMs(env("SOAK_CONNECT_TIMEOUT")!);
  }
  if (env("SOAK_READY_TIMEOUT") !== undefined) {
    o.readyTimeoutMs = parseDurationMs(env("SOAK_READY_TIMEOUT")!);
  }
  let usersFile = env("SOAK_USERS_FILE");
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
      case "--profile":
        next(); // already applied above; consume its value
        break;
      case "--users-file":
        usersFile = next();
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
      case "--input-timeout-ms":
        o.inputTimeoutMs = nonNegativeNumber(a, next());
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
      case "--disconnect-p95-ms":
        o.thresholds.disconnectP95Ms = nonNegativeNumber(a, next());
        break;
      case "--disconnect-p100-ms":
        o.thresholds.disconnectP100Ms = nonNegativeNumber(a, next());
        break;
      case "--input-resume-p100-ms":
        o.thresholds.inputResumeP100Ms = nonNegativeNumber(a, next());
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
  if (usersFile !== undefined) o.users = parseUsersFile(usersFile);
  if (o.portalUrl) o.portalUrl = o.portalUrl.replace(/\/+$/, "");
  return o;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function stamp(msg: string): void {
  console.log(`${new Date().toISOString()} ${msg}`);
}

/**
 * Counts HTTP 429s on an APIRequestContext. `context.request` calls do not
 * emit BrowserContext "response" events, so the methods are wrapped.
 */
function patchRateLimitCounter(
  reqCtx: APIRequestContext,
  lane: { rateLimited429: number },
): void {
  for (const m of ["get", "post", "delete", "put", "patch", "head", "fetch"] as const) {
    const orig = reqCtx[m].bind(reqCtx) as (...a: unknown[]) => Promise<APIResponse>;
    (reqCtx as unknown as Record<string, unknown>)[m] = async (...args: unknown[]) => {
      const r = await orig(...args);
      if (r.status() === 429) lane.rateLimited429++;
      return r;
    };
  }
}

/**
 * Per-node CPU-requests percentage and allocatable CPU (millicores) from
 * `kubectl describe node` (the advisor's worst-node guard): used to stop
 * adding soak workspaces once any guarded node would exceed the cap.
 * kubectl is a debug-time tool — when SOAK_NODE_GUARD is unset this never
 * runs.
 */
function nodeRequestStats(
  nodes: string[],
): Record<string, { pct: number; allocatableCpuM: number }> {
  const kubectl = env("SOAK_NODE_KUBECTL") ?? "kubectl";
  const kcArgs = env("SOAK_NODE_KUBECONFIG")
    ? ["--kubeconfig", env("SOAK_NODE_KUBECONFIG")!]
    : [];
  const out: Record<string, { pct: number; allocatableCpuM: number }> = {};
  for (const n of nodes) {
    const desc = execFileSync(kubectl, [...kcArgs, "describe", "node", n], {
      encoding: "utf8",
      timeout: 30_000,
    });
    const m = /^ {2}cpu\s+\S+m \((\d+)%\)/m.exec(desc);
    if (!m) throw new Error(`cannot parse cpu requests of node ${n}`);
    const allocBlock = /^Allocatable:\n((?: {2}.+\n)+)/m.exec(desc);
    const a = allocBlock && /^ {2}cpu:\s+(\d+)(m?)\s*$/m.exec(allocBlock[1]);
    if (!a) throw new Error(`cannot parse cpu allocatable of node ${n}`);
    out[n] = {
      pct: Number(m[1]),
      allocatableCpuM: a[2] === "m" ? Number(a[1]) : Number(a[1]) * 1000,
    };
  }
  return out;
}

/**
 * Guard nodes whose projected CPU-request share would reach the cap once
 * the next create wave lands: `describe node` only counts bound pods, so
 * a wave of still-Pending pods is invisible to a current-only check
 * (PR-71 review). The wave is projected spread over the guarded nodes —
 * the scheduler's expected shape — as
 * `current + wavePods * podCpuM / nodes / allocatable >= guardPct`.
 */
export function guardOverProjected(
  stats: Record<string, { pct: number; allocatableCpuM: number }>,
  wavePods: number,
  podCpuM: number,
  guardPct: number,
): string[] {
  const names = Object.keys(stats);
  if (names.length === 0) return [];
  const perNodeWaveM = (wavePods * podCpuM) / names.length;
  return names.filter(
    (n) => stats[n].pct + (perNodeWaveM / stats[n].allocatableCpuM) * 100 >= guardPct,
  );
}

/** Soak pods per node in the workspace namespace (placement record). */
function soakPodsPerNode(ns: string): Record<string, number> {
  const kubectl = env("SOAK_NODE_KUBECTL") ?? "kubectl";
  const kcArgs = env("SOAK_NODE_KUBECONFIG")
    ? ["--kubeconfig", env("SOAK_NODE_KUBECONFIG")!]
    : [];
  const json = execFileSync(
    kubectl,
    [...kcArgs, "-n", ns, "get", "pods", "-o", "jsonpath={range .items[*]}{.spec.nodeName}{\"\\n\"}{end}"],
    { encoding: "utf8", timeout: 30_000 },
  );
  const out: Record<string, number> = {};
  for (const n of json.split("\n").filter(Boolean)) out[n] = (out[n] ?? 0) + 1;
  return out;
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

/**
 * True when the session view shows its "open in another tab" state (the
 * V3.24 lease epoch logic decided a newer stream claimed our lease). The
 * harness opens exactly one tab per session, so this is always a false
 * positive to count (V3.10 watch item).
 */
export async function elsewhereDialogVisible(page: {
  getByText(text: string): { count(): Promise<number> };
}): Promise<boolean> {
  return (await page.getByText("This session is open in another tab").count()) > 0;
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
    // v0.2 (T2.3, D17): GET /v1/me returns the session-bound csrfToken.
    // The legacy readable tcdi_csrf cookie no longer exists.
    const me = await this.ctx.get(`${this.portal}/v1/me`);
    if (me.ok()) {
      const body = (await me.json()) as { csrfToken?: string };
      if (body.csrfToken) return body.csrfToken;
    }
    throw new Error(`no CSRF token: GET /v1/me -> ${me.status()}`);
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
  /** The session page was showing "open in another tab" at probe time. */
  elsewhere?: boolean;
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
  /** Lane credentials; falls back to SOAK_USER / SOAK_PASSWORD. */
  user: SoakUser | undefined;
  pages = new Map<string, Page>();

  constructor(context: BrowserContext, portal: string, user?: SoakUser) {
    this.context = context;
    this.portal = portal;
    this.user = user;
    this.api = new PortalApi(context.request, portal);
  }

  /** First visible locator among the candidates, or null. */
  private async tryMatch(page: Page, selectors: string[]) {
    for (const selector of selectors) {
      const loc = page.locator(selector).first();
      if ((await loc.count()) > 0 && (await loc.isVisible())) return loc;
    }
    return null;
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
    const user = this.user?.name ?? env("SOAK_USER");
    const password = this.user?.password ?? env("SOAK_PASSWORD");
    if (!user || !password) {
      throw new Error("SOAK_USER and SOAK_PASSWORD (or a users-file lane) are required");
    }
    const sel = loginSelectors(process.env);
    const page = await this.context.newPage();
    const hasSession = async () =>
      (await this.context.request.get(`${this.portal}/v1/workspaces`)).ok();
    try {
      // A previous attempt that died after the IdP set the SSO cookie but
      // before the session probe passed already has a login: skip the form.
      if (await hasSession()) return;
      await page.goto(this.portal, { waitUntil: "domcontentloaded" });
      // The portal bounces to the IdP; wait for its password field. If the
      // redirect chain bounces straight back through an existing SSO
      // session instead, hasSession() flips and we skip the form.
      const deadline = Date.now() + 30_000;
      for (;;) {
        const pw = await this.tryMatch(page, sel.password);
        if (pw) {
          await this.fillFirst(page, sel.user, user);
          await pw.fill(password);
          const submit = page.locator(sel.submit).first();
          if ((await submit.count()) > 0) await submit.click();
          else await pw.press("Enter");
          await page.waitForURL(`${this.portal}/**`, { timeout: 60_000 });
          break;
        }
        if (await hasSession()) return;
        if (Date.now() > deadline) {
          throw new Error(`no OIDC password field matched: ${sel.password.join(", ")}`);
        }
        await sleep(250);
      }
      // The backend session can lag the redirect: probe it for a few
      // seconds before declaring the login failed.
      const sessDeadline = Date.now() + 15_000;
      let status = 0;
      while (Date.now() <= sessDeadline) {
        const check = await this.context.request.get(`${this.portal}/v1/workspaces`);
        if (check.ok()) return;
        status = check.status();
        await sleep(1500);
      }
      throw new Error(`login did not yield a session (${status})`);
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
    const state = st !== null && st.status === 200 ? st.state : "error";
    // Watch item (V3.10): the page flipping to "open in another tab" while
    // we hold the only stream is a false elsewhere transition — count it.
    let elsewhere = false;
    const page = this.pages.get(id);
    if (page && !page.isClosed()) {
      try {
        elsewhere = await elsewhereDialogVisible(page);
      } catch {
        /* navigation in flight; unknown, not counted */
      }
    }
    return { state, source: "api", ...(elsewhere ? { elsewhere: true } : {}) };
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

/**
 * One user's share of a multi-user run: its own API context and driver, so
 * sessions stay under their owner's credentials and browser context.
 */
export interface Lane {
  user?: SoakUser;
  api: PortalApi;
  driver: Driver;
  /** Login attempts exhausted: the lane's share of sessions is dropped and
   *  the run continues without it (advisor skip-lane rule). */
  skipped?: boolean;
  /** First line of the final login error when the lane was skipped. */
  loginError?: string;
  /** HTTP 429 responses observed on the lane's page traffic and API calls. */
  rateLimited429: number;
  dispose(): Promise<void>;
}

export interface SessionCtx extends SessionResult {
  id: string;
  /** Owning lane (multi-user runs); the driveSessions driver is the fallback. */
  lane?: Lane;
  relaunches: number;
  recentRelaunches: number[];
  lastInputAt: number;
  lastConnectedAt: number | null;
  /** When the latest automatic relaunch was issued; starts a fresh connect budget. */
  relaunchedAt: number | null;
  /** Whether the latest observation showed "open in another tab". */
  elsewhere: boolean;
  /** Wall-clock ms of each scripted input dispatch (the harness-side latency). */
  inputMs: number[];
  /** Dispatches that hit the per-input timeout (the page's ops were wedged). */
  inputTimeouts: number;
  /** Start of the current disconnect span (set on the first non-connected obs). */
  disconnectStartedAt: number | null;
  /** A connected obs ended a disconnect span; cleared when the next input lands. */
  awaitingInputResume: boolean;
  /** Wall-clock ms from disconnect start to the first successful input after reconnect. */
  inputResumeMs: number[];
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
    inputTimeouts: 0,
    disconnectStartedAt: null,
    awaitingInputResume: false,
    inputResumeMs: [],
    dropped: false,
    runEndAt: 0,
    relaunches: 0,
    recentRelaunches: [],
    lastInputAt: 0,
    lastConnectedAt: null,
    relaunchedAt: null,
    elsewhere: false,
    inputMs: [],
  };
}

export interface DriveOptions {
  durationMs: number;
  inputIntervalMs: number;
  /** Per-dispatch cap on a scripted input; a wedged page op cannot starve the rest. */
  inputTimeoutMs: number;
  pollIntervalMs: number;
  connectTimeoutMs: number;
  verbose: boolean;
  /** Abort the soak once dropped sessions exceed this share of the total (%). */
  abortDroppedPct?: number;
  /** Abort once the ramp-up connect p95 exceeds this budget (ms). */
  abortConnectP95Ms?: number | null;
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
  const drv = (s: SessionCtx): Driver => s.lane?.driver ?? driver;
  // Advisor stop rules: the monitor handles the cluster-side ones; these
  // are the harness-side aborts (dropped share, ramp-up connect p95).
  let aborted: string | null = null;
  const droppedShareExceeded = (): boolean =>
    opts.abortDroppedPct !== undefined &&
    (sessions.filter((s) => s.dropped).length / sessions.length) * 100 >
      opts.abortDroppedPct;

  const watch = async (s: SessionCtx): Promise<void> => {
    while (!stopPoll.v) {
      const t0 = Date.now();
      try {
        const p = await drv(s).probe(s.id);
        const at = Date.now();
        s.observations.push({
          at,
          state: p.state,
          source: p.source,
          ...(p.elsewhere === true ? { elsewhere: true } : {}),
        });
        if (p.state === "connected") {
          s.lastConnectedAt = at;
          if (s.disconnectStartedAt !== null) s.awaitingInputResume = true;
        } else if (s.lastConnectedAt !== null && s.disconnectStartedAt === null) {
          s.disconnectStartedAt = at;
        }
        if (p.elsewhere === true && !s.elsewhere) {
          stamp(`${s.id}: page shows "open in another tab" while its stream is live (false elsewhere)`);
        }
        s.elsewhere = p.elsewhere === true;
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
            await drv(s).openSession(s.id);
          } catch (e) {
            console.error(`relaunch ${s.id}: ${(e as Error).message}`);
          }
        } else if (!s.dropped) {
          s.dropped = true;
          s.manualActions++;
          console.error(`${s.id} dropped after ${MAX_AUTO_RELAUNCH} auto-relaunches`);
          if (aborted === null && droppedShareExceeded()) {
            aborted = `dropped sessions exceed ${opts.abortDroppedPct}% of the fleet`;
            console.error(`aborting: ${aborted}`);
          }
        }
      }
      await sleep(Math.max(10, opts.pollIntervalMs - (Date.now() - t0)));
    }
  };

  try {
    // Sessions open in waves (advisor msg_7d6860d0d212): a bounded burst
    // keeps the per-IP launch limiter and the ramp healthy; a pause between
    // waves lets the tail settle before the next batch.
    const openWave = Math.max(1, Number(env("SOAK_OPEN_WAVE") ?? "10") || 10);
    const openWavePauseMs = Math.max(
      0,
      Number(env("SOAK_OPEN_WAVE_PAUSE_MS") ?? "15000") || 0,
    );
    for (let w = 0; w < sessions.length && !shouldStop(); w += openWave) {
      const wave = sessions.slice(w, w + openWave);
      await Promise.all(
        wave.map(async (s) => {
          s.launchedAt = Date.now();
          await drv(s).openSession(s.id);
          pollers.push(watch(s));
        }),
      );
      if (w + openWave < sessions.length) await sleep(openWavePauseMs);
    }

    // Soak clock: every session connected at least once (or given up on).
    while (
      !shouldStop() &&
      aborted === null &&
      !sessions.every((s) => s.lastConnectedAt !== null || s.dropped)
    ) {
      await sleep(Math.min(100, opts.pollIntervalMs));
    }
    if (shouldStop()) return { soakStartedAt: null, completed: false, failures };
    if (aborted !== null) {
      failures.push(`aborted: ${aborted}`);
      return { soakStartedAt: null, completed: false, failures };
    }
    // Ramp-up quality gate: if connecting the fleet was already too slow,
    // an hour of soaking adds nothing — abort now (advisor stop rule).
    if (opts.abortConnectP95Ms !== undefined && opts.abortConnectP95Ms !== null) {
      const connects = sessions
        .map((s) => {
          const first = s.observations.find((o) => o.state === "connected");
          return first !== undefined && s.launchedAt !== null ? first.at - s.launchedAt : null;
        })
        .filter((v): v is number => v !== null);
      const p95 = percentile(connects, 95);
      if (p95 !== null && p95 > opts.abortConnectP95Ms) {
        aborted = `ramp-up connect p95 ${p95}ms exceeds ${opts.abortConnectP95Ms}ms`;
        console.error(`aborting: ${aborted}`);
        failures.push(`aborted: ${aborted}`);
        return { soakStartedAt: null, completed: false, failures };
      }
    }
    const soakStartedAt = Date.now();
    const deadline = soakStartedAt + opts.durationMs;
    const reloadAt = soakStartedAt + opts.durationMs / 2;

    let reloadQueue: SessionCtx[] | null = null;
    while (Date.now() < deadline && !shouldStop() && aborted === null) {
      if (reloadQueue === null && Date.now() >= reloadAt) {
        stamp("mid-run reload of every session");
        reloadQueue = [...sessions];
      }
      // One reload per pass: a serialized reload burst starves the input
      // loop exactly like a wedged send would (rc.3 e2e: 60 sequential
      // reloads blocked every input heartbeat for the whole window).
      const rs = reloadQueue?.shift();
      if (rs !== undefined) {
        rs.reloadedAt = Date.now();
        try {
          const r = await drv(rs).reloadSession(rs.id);
          if (r.takeoverPrompted) {
            // FX-R3c: a reload resumes without a ticket. A take-over prompt
            // means a human would have had to click: always a failure.
            rs.manualActions++;
            failures.push(`take-over prompt after the mid-run reload of ${rs.id}`);
          }
        } catch (e) {
          console.error(`reload ${rs.id}: ${(e as Error).message}`);
        }
        if (reloadQueue!.length === 0) reloadQueue = [];
      }
      for (const s of sessions) {
        if (Date.now() - s.lastInputAt < opts.inputIntervalMs) continue;
        s.lastInputAt = Date.now();
        const t0 = Date.now();
        const send = drv(s).sendInput(s.id);
        const outcome = await Promise.race([
          send.then((): "ok" => "ok").catch((e): Error => e as Error),
          sleep(opts.inputTimeoutMs).then((): "timeout" => "timeout"),
        ]);
        const ms = Date.now() - t0;
        if (outcome === "ok") {
          s.inputMs.push(ms);
          s.inputEvents++;
          if (s.awaitingInputResume) {
            s.inputResumeMs.push(Date.now() - (s.disconnectStartedAt ?? Date.now()));
            s.awaitingInputResume = false;
            s.disconnectStartedAt = null;
          }
        } else {
          s.inputTimeouts++;
          s.inputMs.push(Math.min(ms, opts.inputTimeoutMs));
          if (opts.verbose)
            console.error(
              `input ${s.id}: ${outcome === "timeout" ? `exceeded ${opts.inputTimeoutMs}ms` : outcome.message}`,
            );
        }
      }
      await sleep(Math.min(1_000, opts.inputIntervalMs));
    }
    // Inputs that never resumed before the run ended count against the
    // resume window at their floor value — the real stall was longer.
    const endedAt = Date.now();
    for (const s of sessions) {
      if (s.awaitingInputResume && s.disconnectStartedAt !== null) {
        s.inputResumeMs.push(endedAt - s.disconnectStartedAt);
      }
    }
    if (aborted !== null) failures.push(`aborted: ${aborted}`);
    return {
      soakStartedAt,
      completed: Date.now() >= deadline && aborted === null,
      failures,
    };
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

  // One lane per soak user: its own API context (cookie jar) and driver.
  // Without a users file there is a single lane on SOAK_USER/SOAK_PASSWORD.
  const lanes: Lane[] = [];
  let browser: Browser | undefined;
  // More users than sessions is pointless: lane i only ever receives
  // session i mod laneCount, so cap the lanes at the session count.
  const laneUsers = (opts: Options): (SoakUser | undefined)[] =>
    opts.users.length ? opts.users.slice(0, opts.sessions) : [undefined];
  if (opts.dryRun) {
    const users = laneUsers(opts);
    for (const user of users) {
      const ctx = await request.newContext({
        ignoreHTTPSErrors: env("SOAK_IGNORE_TLS_ERRORS") === "1",
      });
      const lane: Lane = {
        ...(user !== undefined ? { user } : {}),
        api: new PortalApi(ctx, portalUrl),
        driver: new ApiDriver(ctx, portalUrl, true),
        rateLimited429: 0,
        dispose: () => ctx.dispose(),
      };
      lanes.push(lane);
      patchRateLimitCounter(ctx, lane);
    }
  } else {
    browser = await chromium.launch(chromiumLaunchOptions(env("SOAK_HEADFUL") !== "1"));
    const users = laneUsers(opts);
    for (const user of users) {
      const context = await browser.newContext({
        ignoreHTTPSErrors: env("SOAK_IGNORE_TLS_ERRORS") === "1",
      });
      const driver = new BrowserDriver(context, portalUrl, user);
      const lane: Lane = {
        ...(user !== undefined ? { user } : {}),
        api: driver.api,
        driver,
        rateLimited429: 0,
        dispose: () => driver.dispose(),
      };
      context.on("response", (r) => {
        if (r.status() === 429) lane.rateLimited429++;
      });
      patchRateLimitCounter(context.request, lane);
      lanes.push(lane);
    }
  }

  const cleanup = async () => {
    for (const s of sessions) {
      const lane = s.lane ?? lanes.find((l) => !l.skipped) ?? lanes[0];
      try {
        await lane.driver.closeSession(s.id);
        await lane.api.deleteWorkspace(s.id);
      } catch (e) {
        console.error(`cleanup ${s.id}: ${(e as Error).message}`);
      }
    }
    for (const lane of lanes) {
      try {
        await lane.dispose();
      } catch {
        /* teardown */
      }
    }
    try {
      await browser?.close();
    } catch {
      /* teardown */
    }
    mock?.child.kill("SIGTERM");
  };

  try {
    stamp(`login (${opts.dryRun ? "dev login" : "OIDC"}, ${lanes.length} lane${lanes.length === 1 ? "" : "s"})`);
    // Bounded parallelism: a big OIDC burst can push the IdP form past the
    // 30 s field deadline. A failed lane is retried twice before the run
    // gives up (the name goes into the error, never the password).
    const loginConcurrency = Math.max(
      1,
      Number(env("SOAK_LOGIN_CONCURRENCY") ?? "4") || 4,
    );
    const loginAttempts = Math.max(1, Number(env("SOAK_LOGIN_ATTEMPTS") ?? "3") || 3);
    for (let i = 0; i < lanes.length; i += loginConcurrency) {
      await Promise.all(
        lanes.slice(i, i + loginConcurrency).map(async (lane) => {
          const who = lane.user?.name ?? "default";
          let lastErr: Error | undefined;
          for (let attempt = 1; attempt <= loginAttempts; attempt++) {
            try {
              await lane.driver.login();
              lastErr = undefined;
              break;
            } catch (e) {
              lastErr = e as Error;
              if (attempt < loginAttempts) {
                stamp(`login lane ${who} failed attempt ${attempt} (${lastErr.message.split("\n")[0]}) — retrying`);
              }
            }
          }
          if (lastErr) {
            // Advisor skip-lane rule: a lane that exhausts its login attempts
            // is dropped (its share of sessions is never created) instead of
            // aborting the run; skipped lanes are reported below.
            lane.skipped = true;
            lane.loginError = lastErr.message.split("\n")[0];
            stamp(`login lane ${who} skipped after ${loginAttempts} attempts (${lane.loginError})`);
          }
        }),
      );
    }
    const activeLanes = lanes.filter((l) => !l.skipped);
    if (activeLanes.length !== lanes.length) {
      stamp(`${lanes.length - activeLanes.length} lane(s) skipped; continuing with ${activeLanes.length}`);
    }
    if (activeLanes.length === 0) {
      throw new Error(`every login lane failed (${lanes.length} skipped)`);
    }

    const templates = await activeLanes[0].api.listTemplates();
    const tpl = opts.template
      ? (templates.find((t) => t.id === opts.template || t.name === opts.template)?.id ??
        opts.template)
      : templates[0]?.id;
    if (!tpl) throw new Error("no templates listed");

    stamp(`creating ${opts.sessions} workspaces on template ${tpl}`);
    // Advisor per-node guard (msg_7d6860d0d212): workspaces are created in
    // waves; between waves the guarded nodes' CPU-request share plus the
    // next wave's projected share is checked and creation stops when any
    // node would reach the cap. Effective N is what actually got created
    // (sessionsEffective in the report).
    const guardNodes = (env("SOAK_NODE_GUARD") ?? "")
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
    const guardPct = Math.max(1, Number(env("SOAK_NODE_GUARD_PCT") ?? "80") || 80);
    const createWave = Math.max(1, Number(env("SOAK_CREATE_WAVE") ?? "10") || 10);
    for (let w = 0; w < opts.sessions && !shouldStop(); w += createWave) {
      if (guardNodes.length) {
        const podCpuM = Math.max(
          1,
          Number(env("SOAK_GUARD_POD_CPU_M") ?? "250") || 250,
        );
        const wavePods = Math.min(createWave, opts.sessions - w);
        const over = guardOverProjected(
          nodeRequestStats(guardNodes),
          wavePods,
          podCpuM,
          guardPct,
        );
        if (over.length) {
          stamp(
            `node-request guard: ${over.join(", ")} would reach ` +
              `>= ${guardPct}% projected CPU requests with the next wave ` +
              `— stopping creation at ${sessions.length} sessions`,
          );
          break;
        }
      }
      for (let i = w; i < Math.min(w + createWave, opts.sessions) && !shouldStop(); i++) {
        const lane = lanes[i % lanes.length];
        if (lane.skipped) continue; // skipped lane's share of sessions is dropped
        const name = `soak-${startedAt.toString(36)}-${String(i).padStart(3, "0")}`;
        const id = await lane.api.createWorkspace(name, tpl);
        const s = newSession(id, name);
        s.lane = lane;
        if (lane.user !== undefined) s.owner = lane.user.name;
        else if (!opts.dryRun && env("SOAK_USER") !== undefined) s.owner = env("SOAK_USER");
        sessions.push(s);
        if (lane.driver instanceof ApiDriver) await lane.driver.forceReady(id);
      }
    }
    if (guardNodes.length && sessions.length > 0 && env("SOAK_GUARD_NS")) {
      const placement = soakPodsPerNode(env("SOAK_GUARD_NS")!);
      stamp(
        `workspace placement by node: ${Object.entries(placement)
          .sort()
          .map(([n, c]) => `${n}=${c}`)
          .join(" ")}`,
      );
    }
    if (!opts.dryRun) {
      await Promise.all(
        sessions.map((s) => waitForReady(s.lane?.api ?? lanes[0].api, s.id, opts.readyTimeoutMs)),
      );
    }

    stamp("opening sessions");
    outcome = await driveSessions(
      {
        ...opts,
        abortDroppedPct:
          env("SOAK_ABORT_DROPPED_PCT") !== undefined
            ? nonNegativeNumber("SOAK_ABORT_DROPPED_PCT", env("SOAK_ABORT_DROPPED_PCT")!)
            : 1,
        abortConnectP95Ms:
          env("SOAK_ABORT_CONNECT_P95_MS") !== undefined
            ? nonNegativeNumber("SOAK_ABORT_CONNECT_P95_MS", env("SOAK_ABORT_CONNECT_P95_MS")!)
            : null,
      },
      sessions,
      activeLanes[0].driver,
      shouldStop,
    );
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
        sessionsEffective: sessions.length,
        users: lanes.length,
        ...(lanes.some((l) => l.skipped)
          ? {
              skippedLanes: lanes
                .filter((l) => l.skipped)
                .map((l) => ({ user: l.user?.name ?? "default", error: l.loginError ?? "" })),
            }
          : {}),
        lanes: lanes.map((l) => ({
          user: l.user?.name ?? "default",
          rateLimited429: l.rateLimited429,
          skipped: l.skipped === true,
        })),
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
