// Tests for the soak orchestration core (driveSessions), the drivers and the
// option parsing, using a scripted fake Driver — no browser, no cluster.
// Real timers with millisecond-scale budgets keep each case under a few
// seconds.

import { spawn, spawnSync } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import assert from "node:assert/strict";
import {
  ApiDriver,
  BrowserDriver,
  chromiumLaunchOptions,
  driveSessions,
  elsewhereDialogVisible,
  loadProfile,
  loginSelectors,
  newSession,
  parseArgs,
  parseUsersFile,
  takeoverDialogVisible,
  type Driver,
  type SessionProbe,
} from "./soak.ts";
import { buildReport } from "./report.ts";

const HERE = import.meta.dirname;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

const FAST = {
  durationMs: 600,
  inputIntervalMs: 50,
  pollIntervalMs: 20,
  connectTimeoutMs: 400,
  verbose: false,
};

/** Scriptable driver: probe() answers come from a per-session function. */
class FakeDriver implements Driver {
  opened: { id: string; at: number }[] = [];
  closed: string[] = [];
  reloads: string[] = [];
  openDelayMs = 0;
  takeoverPrompted = false;
  stateFor: (id: string, now: number, driver: FakeDriver) => string = () => "connected";

  login(): Promise<void> {
    return Promise.resolve();
  }
  async openSession(id: string): Promise<void> {
    if (this.openDelayMs) await sleep(this.openDelayMs);
    this.opened.push({ id, at: Date.now() });
  }
  reloadSession(id: string): Promise<{ takeoverPrompted: boolean }> {
    this.reloads.push(id);
    return Promise.resolve({ takeoverPrompted: this.takeoverPrompted });
  }
  sendInput(): Promise<void> {
    return Promise.resolve();
  }
  probe(id: string): Promise<SessionProbe> {
    return Promise.resolve({ state: this.stateFor(id, Date.now(), this), source: "api" });
  }
  closeSession(id: string): Promise<void> {
    this.closed.push(id);
    return Promise.resolve();
  }
  dispose(): Promise<void> {
    return Promise.resolve();
  }
}

// ---- R5b: relaunch resets the connect reference --------------------------

test("R5b: one automatic relaunch that then connects is not counted as dropped", async () => {
  const driver = new FakeDriver();
  const s = newSession("ws_a", "soak-a");
  // Connected from the start; the connection is lost mid-run for longer than
  // the connect budget; the relaunch connects 150 ms after it was issued.
  const lostAt = Date.now() + 200;
  driver.stateFor = (_id, now, d) => {
    if (now < lostAt) return "connected";
    const relaunchedAt = d.opened[1]?.at;
    if (relaunchedAt !== undefined && now - relaunchedAt >= 150) return "connected";
    return "disconnected";
  };
  await driveSessions({ ...FAST, durationMs: 1_500, connectTimeoutMs: 400 }, [s], driver, () => false);
  assert.equal(driver.opened.length, 2, "exactly one relaunch (initial open + one relaunch)");
  assert.equal(s.relaunches, 1);
  assert.equal(s.dropped, false, "a relaunch that connected is not a drop");
  assert.equal(s.manualActions, 0);
});

test("R5b: a relaunch gets the full connect budget before the next one", async () => {
  const driver = new FakeDriver();
  const s = newSession("ws_b", "soak-b");
  const lostAt = Date.now() + 100;
  driver.stateFor = (_id, now) => (now < lostAt ? "connected" : "disconnected");
  await driveSessions({ ...FAST, durationMs: 1_000, connectTimeoutMs: 300 }, [s], driver, () => false);
  // Timeline: lost at ~100, relaunch #1 at ~400, #2 at >= 700, then dropped.
  const gaps = driver.opened.slice(1).map((o, i) => o.at - driver.opened[i].at);
  assert.ok(driver.opened.length >= 3, `expected two relaunches, got ${driver.opened.length - 1}`);
  for (const g of gaps.slice(1)) {
    assert.ok(g >= 280, `relaunch issued ${g}ms after the previous one; budget is 300ms`);
  }
});

test("R5b: BrowserDriver.openSession closes the previous tab of the same session", async () => {
  const closed: string[] = [];
  const makePage = (label: string) => ({
    goto: () => Promise.resolve(),
    close: () => {
      closed.push(label);
      return Promise.resolve();
    },
    locator: () => ({ first: () => ({ count: () => Promise.resolve(0) }) }),
  });
  let n = 0;
  const context = {
    request: {},
    newPage: () => Promise.resolve(makePage(`tab${++n}`)),
  };
  const driver = new BrowserDriver(context as never, "https://portal.example.test");
  await driver.openSession("ws_1");
  assert.deepEqual(closed, []);
  await driver.openSession("ws_1"); // automatic relaunch
  assert.deepEqual(closed, ["tab1"], "the stale tab must be closed on relaunch");
});

// ---- R5d: per-session pollers, soak clock, truncation ---------------------

test("R5d: each session's poller starts right after its own open", async () => {
  const driver = new FakeDriver();
  driver.openDelayMs = 200; // sequential opens: 200, 400, 600 ms
  const sessions = ["a", "b", "c"].map((x) => newSession(`ws_${x}`, `soak-${x}`));
  await driveSessions({ ...FAST, durationMs: 300 }, sessions, driver, () => false);
  for (const s of sessions) {
    const firstConnected = s.observations.find((o) => o.state === "connected");
    assert.ok(firstConnected, `${s.id} observed connected`);
    const connectMs = firstConnected.at - (s.launchedAt ?? 0);
    // Own open (200 ms) + one poll; NOT the opens of the sessions after it.
    assert.ok(connectMs < 380, `${s.id} connectMs ${connectMs} includes other sessions' opens`);
  }
});

test("R5d: the soak clock starts when the last session first connects", async () => {
  const driver = new FakeDriver();
  const sessions = ["a", "b"].map((x) => newSession(`ws_${x}`, `soak-${x}`));
  const t0 = Date.now();
  // Session b only connects 500 ms in.
  driver.stateFor = (id, now) => (id === "ws_b" && now - t0 < 500 ? "none" : "connected");
  const out = await driveSessions({ ...FAST, durationMs: 400 }, sessions, driver, () => false);
  const lastFirstConnected = Math.max(
    ...sessions.map((s) => s.observations.find((o) => o.state === "connected")!.at),
  );
  assert.ok(out.soakStartedAt !== null);
  assert.ok(out.soakStartedAt >= lastFirstConnected - 5, "clock must not start before the last connect");
  assert.ok(out.soakStartedAt - lastFirstConnected < 150, "clock starts promptly after it");
  assert.equal(out.completed, true);
  assert.ok(Date.now() - out.soakStartedAt >= 400, "the full duration is observed after the clock start");
});

test("R5d: a run stopped before the duration elapsed is reported as truncated", async () => {
  const driver = new FakeDriver();
  const s = newSession("ws_t", "soak-t");
  const t0 = Date.now();
  const out = await driveSessions(
    { ...FAST, durationMs: 5_000 },
    [s],
    driver,
    () => Date.now() - t0 > 300,
  );
  assert.equal(out.completed, false);
  const report = buildReport(
    {
      dryRun: true,
      startedAt: t0,
      endedAt: Date.now(),
      soakStartedAt: out.soakStartedAt,
      requestedDurationMs: 5_000,
      sessionsRequested: 1,
      inputIntervalSeconds: 1,
      pollIntervalSeconds: 1,
    },
    { connectP95Ms: null, reconnectP95Ms: null, maxGapMs: null, maxManualActions: 0, maxDroppedSessions: 0 },
    [{ ...s, runEndAt: Date.now() }],
  ) as { summary: { pass: boolean; failures: string[] } };
  assert.equal(report.summary.pass, false);
  assert.match(report.summary.failures.join(";"), /truncated/);
});

// ---- R5g: no take-over dialog after the mid-run reload --------------------

test("R5g: a take-over prompt after the mid-run reload fails the run", async () => {
  const driver = new FakeDriver();
  driver.takeoverPrompted = true;
  const s = newSession("ws_g", "soak-g");
  const out = await driveSessions({ ...FAST, durationMs: 500 }, [s], driver, () => false);
  assert.deepEqual(driver.reloads, ["ws_g"]);
  assert.match(out.failures.join(";"), /take-over/);
});

test("R5g: no prompt after the reload, no failure", async () => {
  const driver = new FakeDriver();
  const out = await driveSessions({ ...FAST, durationMs: 500 }, [newSession("ws_h", "h")], driver, () => false);
  assert.deepEqual(out.failures, []);
});

test("R5g: takeoverDialogVisible detects the portal's take-over button", async () => {
  const page = (n: number) => ({
    getByText: (text: string) => {
      assert.equal(text, "Take over session");
      return { count: () => Promise.resolve(n) };
    },
  });
  assert.equal(await takeoverDialogVisible(page(1) as never), true);
  assert.equal(await takeoverDialogVisible(page(0) as never), false);
});

test("R5g: ApiDriver.reloadSession reports a take-over prompt and never takes over", async () => {
  const posts: { url: string; data: unknown }[] = [];
  const res = (status: number, body: unknown = {}, headers: Record<string, string> = {}) => ({
    status: () => status,
    ok: () => status >= 200 && status < 300,
    json: () => Promise.resolve(body),
    text: () => Promise.resolve(JSON.stringify(body)),
    headers: () => headers,
    headersArray: () =>
      Object.entries(headers).map(([name, value]) => ({ name, value })),
  });
  const ctx = {
    get: (url: string) => {
      if (url.endsWith("/v1/me")) return Promise.resolve(res(200, { csrfToken: "t" }));
      return Promise.resolve(res(401)); // session cookie lost -> relaunch
    },
    post: (url: string, opts: { data?: unknown }) => {
      posts.push({ url, data: opts.data });
      return Promise.resolve(res(409, { error: "in-use" }));
    },
  };
  const driver = new ApiDriver(ctx as never, "http://portal.test", false);
  driver.sessionPage.set("ws_1", { origin: "http://s.test", path: "/desktop/", cookie: "c=1" });
  const out = await driver.reloadSession("ws_1");
  assert.equal(out.takeoverPrompted, true);
  assert.ok(
    posts.every((p) => (p.data as { takeover?: boolean }).takeover !== true),
    "the reload must not silently take the session over",
  );
});

// ---- R5a: signals ---------------------------------------------------------

test("R5a: chromium is launched without Playwright's own signal handlers", () => {
  const o = chromiumLaunchOptions(true);
  assert.equal(o.handleSIGINT, false);
  assert.equal(o.handleSIGTERM, false);
  assert.equal(o.handleSIGHUP, false);
  assert.equal(o.headless, true);
});

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

test(
  "R5a: SIGINT mid-run deletes every created workspace and still writes the report",
  { timeout: 90_000 },
  async () => {
    const repoRoot = path.resolve(HERE, "..", "..");
    const portalPort = await freePort();
    const sessionPort = await freePort();
    const mock = spawn(
      process.execPath,
      ["--disable-warning=ExperimentalWarning", path.join(repoRoot, "web/tests/mock-api/server.ts")],
      {
        env: {
          ...process.env,
          MOCK_PORTAL_PORT: String(portalPort),
          MOCK_SESSION_PORT: String(sessionPort),
        },
        stdio: "ignore",
      },
    );
    const portal = `http://127.0.0.1:${portalPort}`;
    try {
      for (let i = 0; ; i++) {
        try {
          if ((await fetch(`${portal}/_control/health`)).ok) break;
        } catch {
          /* not up yet */
        }
        assert.ok(i < 100, "mock did not come up");
        await sleep(200);
      }
      const reportPath = path.join(
        fs.mkdtempSync(path.join(os.tmpdir(), "tcdi-soak-sig-")),
        "report.json",
      );
      const soak = spawn(
        process.execPath,
        [
          "--disable-warning=ExperimentalWarning",
          path.join(HERE, "soak.ts"),
          "--dry-run",
          "--portal-url",
          portal,
          "--sessions",
          "5",
          "--duration",
          "60s",
          "--poll-interval",
          "1s",
          "--input-interval",
          "2s",
          "--report",
          reportPath,
        ],
        { stdio: ["ignore", "pipe", "pipe"] },
      );
      let out = "";
      soak.stdout.on("data", (b) => (out += b));
      soak.stderr.on("data", (b) => (out += b));
      const exited = new Promise<number | null>((resolve) => soak.on("exit", (c) => resolve(c)));
      for (let i = 0; !out.includes("opening sessions"); i++) {
        assert.ok(i < 300, `soak never reached "opening sessions": ${out.slice(-400)}`);
        await sleep(100);
      }
      await sleep(1_500); // let some sessions open
      soak.kill("SIGINT");
      const code = await exited;
      assert.equal(code, 1, `a truncated run must fail (output: ${out.slice(-600)})`);
      const report = JSON.parse(fs.readFileSync(reportPath, "utf8"));
      assert.equal(report.summary.pass, false);
      const reqs = (await (await fetch(`${portal}/_control/requests`)).json()) as {
        requests: { method: string; path: string }[];
      };
      const created = reqs.requests.filter((r) => r.method === "POST" && r.path === "/v1/workspaces");
      const deleted = reqs.requests.filter(
        (r) => r.method === "DELETE" && /^\/v1\/workspaces\/[^/]+$/.test(r.path),
      );
      assert.ok(created.length > 0, "workspaces were created");
      assert.equal(deleted.length, created.length, "every created workspace is deleted");
    } finally {
      mock.kill("SIGKILL");
    }
  },
);

// ---- R5f: option validation, selectors ------------------------------------

test("R5f: numeric flags are validated", () => {
  const bad: string[][] = [
    ["--max-gap-ms", "abc"],
    ["--connect-p95-ms", "NaN"],
    ["--reconnect-p95-ms", "-5"],
    ["--max-manual-actions", "1.5"],
    ["--max-dropped", "x"],
    ["--sessions", "0"],
    ["--sessions", "two"],
  ];
  for (const argv of bad) {
    assert.throws(() => parseArgs(argv), /must be|invalid/i, argv.join(" "));
  }
  const ok = parseArgs(["--max-gap-ms", "5000", "--connect-p95-ms", "0", "--max-dropped", "2"]);
  assert.equal(ok.thresholds.maxGapMs, 5000);
  assert.equal(ok.thresholds.connectP95Ms, 0);
  assert.equal(ok.thresholds.maxDroppedSessions, 2);
});

test("R5f: numeric environment values are validated too", () => {
  const prev = process.env.SOAK_SESSIONS;
  process.env.SOAK_SESSIONS = "lots";
  try {
    assert.throws(() => parseArgs([]), /SOAK_SESSIONS|--sessions/);
  } finally {
    if (prev === undefined) delete process.env.SOAK_SESSIONS;
    else process.env.SOAK_SESSIONS = prev;
  }
});

test("R5f: SOAK_PASSWORD_SELECTOR is honoured before the generic password field", () => {
  const sel = loginSelectors({ SOAK_PASSWORD_SELECTOR: "#pw", SOAK_USER_SELECTOR: "#u", SOAK_SUBMIT_SELECTOR: "#go" });
  assert.equal(sel.password[0], "#pw");
  assert.ok(sel.password.includes('input[type="password"]'));
  assert.equal(sel.user[0], "#u");
  assert.equal(sel.submit, "#go");
  const dflt = loginSelectors({});
  assert.deepEqual(dflt.password, ['input[type="password"]']);
  assert.equal(dflt.submit, 'button[type="submit"]');
});

test("R5f: --help prints usage without script internals", () => {
  const r = spawnSync(process.execPath, ["--disable-warning=ExperimentalWarning", path.join(HERE, "soak.ts"), "--help"], {
    encoding: "utf8",
  });
  assert.equal(r.status, 0);
  assert.match(r.stdout, /Usage:/);
  assert.match(r.stdout, /SOAK_PASSWORD_SELECTOR/);
});

// ---- V3.10: lanes, users file, profiles, elsewhere count ----------------

test("V3.10: each session drives through its own lane", async () => {
  const dA = new FakeDriver();
  const dB = new FakeDriver();
  const lane = (d: FakeDriver) => ({
    api: {} as never,
    driver: d,
    rateLimited429: 0,
    dispose: () => Promise.resolve(),
  });
  const a = newSession("ws_a", "a");
  a.lane = lane(dA);
  const b = newSession("ws_b", "b");
  b.lane = lane(dB);
  await driveSessions({ ...FAST, durationMs: 400 }, [a, b], dA, () => false);
  assert.deepEqual(dA.opened.map((o) => o.id), ["ws_a"]);
  assert.deepEqual(dB.opened.map((o) => o.id), ["ws_b"]);
  // The mid-run reload is routed per lane too.
  assert.deepEqual(dA.reloads, ["ws_a"]);
  assert.deepEqual(dB.reloads, ["ws_b"]);
  assert.ok(a.observations.length > 0 && b.observations.length > 0);
});

test("V3.10: an elsewhere probe is recorded on the observation", async () => {
  const driver = new FakeDriver();
  let flip = false;
  driver.probe = (id) => {
    flip = !flip;
    return Promise.resolve({ state: "connected", source: "api", ...(flip ? { elsewhere: true } : {}) });
  };
  const s = newSession("ws_e", "e");
  await driveSessions({ ...FAST, durationMs: 300 }, [s], driver, () => false);
  const flagged = s.observations.filter((o) => o.elsewhere === true).length;
  assert.ok(flagged > 0, "elsewhere observations recorded");
});

test("V3.10: elsewhereDialogVisible detects the 'open in another tab' view", async () => {
  const page = (n: number) => ({
    getByText: (text: string) => {
      assert.equal(text, "This session is open in another tab");
      return { count: () => Promise.resolve(n) };
    },
  });
  assert.equal(await elsewhereDialogVisible(page(1) as never), true);
  assert.equal(await elsewhereDialogVisible(page(0) as never), false);
});

test("V3.10: parseUsersFile reads user,password lanes", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "tcdi-soak-users-"));
  const file = path.join(dir, "users.csv");
  fs.writeFileSync(file, "# comment\nsoak01,pw-1\n\nsoak02,p,w,2\n");
  assert.deepEqual(parseUsersFile(file), [
    { name: "soak01", password: "pw-1" },
    { name: "soak02", password: "p,w,2" },
  ]);
  fs.writeFileSync(file, "soak01,a\nsoak01,b\n");
  assert.throws(() => parseUsersFile(file), /duplicate/);
  fs.writeFileSync(file, "not-a-pair\n");
  assert.throws(() => parseUsersFile(file), /user,password/);
  fs.writeFileSync(file, "# only comments\n");
  assert.throws(() => parseUsersFile(file), /no users/);
  assert.throws(() => parseUsersFile(path.join(dir, "missing.csv")), /cannot read/);
});

test("V3.10: --users-file / SOAK_USERS_FILE populate the lane users", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "tcdi-soak-users-"));
  const file = path.join(dir, "users.csv");
  fs.writeFileSync(file, "u1,p1\nu2,p2\nu3,p3\n");
  const o = parseArgs(["--users-file", file]);
  assert.equal(o.users.length, 3);
  assert.equal(o.users[2].name, "u3");
  const prev = process.env.SOAK_USERS_FILE;
  process.env.SOAK_USERS_FILE = file;
  try {
    assert.equal(parseArgs([]).users.length, 3);
  } finally {
    if (prev === undefined) delete process.env.SOAK_USERS_FILE;
    else process.env.SOAK_USERS_FILE = prev;
  }
});

test("V3.10: the run aborts once dropped sessions exceed the abort share", async () => {
  const driver = new FakeDriver();
  // Two of three sessions never connect -> both drop past the relaunch
  // bound; 2/3 = 66% > the 1% advisor rule, so the soak aborts at once.
  driver.stateFor = (id) => (id === "ws_ok" ? "connected" : "none");
  const sessions = ["ws_ok", "ws_bad1", "ws_bad2"].map((id) => newSession(id, id));
  const out = await driveSessions(
    {
      ...FAST,
      durationMs: 5_000,
      connectTimeoutMs: 200,
      abortDroppedPct: 1,
    },
    sessions,
    driver,
    () => false,
  );
  assert.equal(out.completed, false);
  assert.match(out.failures.join(";"), /dropped sessions exceed 1%/);
});

test("V3.10: a slow ramp-up aborts on the connect-p95 rule", async () => {
  const driver = new FakeDriver();
  driver.openDelayMs = 120; // every connect takes ~120 ms
  const sessions = ["a", "b"].map((id) => newSession(`ws_${id}`, id));
  const out = await driveSessions(
    { ...FAST, durationMs: 1_000, abortConnectP95Ms: 50 },
    sessions,
    driver,
    () => false,
  );
  assert.equal(out.completed, false);
  assert.equal(out.soakStartedAt, null);
  assert.match(out.failures.join(";"), /connect p95 .* exceeds 50ms/);
});

test("V3.10: profiles provide defaults that flags override", () => {
  assert.equal(loadProfile("soak-100").sessions, 100);
  assert.equal(loadProfile("soak-200").sessions, 200);
  assert.throws(() => loadProfile("../secrets"), /bad profile name/);
  assert.throws(() => loadProfile("does-not-exist"), /cannot load profile/);
  const o = parseArgs(["--profile", "soak-100"]);
  assert.equal(o.sessions, 100);
  assert.equal(o.template, "soak-small");
  assert.equal(o.durationMs, 3_600_000);
  assert.equal(o.thresholds.reconnectP95Ms, 15_000);
  const over = parseArgs(["--profile", "soak-100", "--sessions", "7"]);
  assert.equal(over.sessions, 7, "a flag beats the profile");
  const prev = process.env.SOAK_SESSIONS;
  process.env.SOAK_SESSIONS = "9";
  try {
    assert.equal(parseArgs(["--profile", "soak-100"]).sessions, 9, "env beats the profile");
  } finally {
    if (prev === undefined) delete process.env.SOAK_SESSIONS;
    else process.env.SOAK_SESSIONS = prev;
  }
});
