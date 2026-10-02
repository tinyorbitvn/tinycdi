// Unit + dry-run tests for the soak harness. Run: npm test.
// Uses node:test and node:assert — no test-runner dependency.

import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import assert from "node:assert/strict";
import {
  longestDisconnectedGapMs,
  parseDurationMs,
  percentile,
  reloadRecovery,
  stateSpans,
  type Observation,
} from "./metrics.ts";
import { buildReport, validateReport } from "./report.ts";

const HERE = import.meta.dirname;

test("percentile: nearest-rank", () => {
  const values = Array.from({ length: 100 }, (_, i) => i + 1); // 1..100
  assert.equal(percentile(values, 50), 50);
  assert.equal(percentile(values, 95), 95);
  assert.equal(percentile(values, 100), 100);
  assert.equal(percentile(values, 1), 1);
  assert.equal(percentile([7], 95), 7);
  assert.equal(percentile([], 95), null);
  assert.equal(percentile([3, 1, 2], 50), 2);
  assert.throws(() => percentile([1], 0), RangeError);
  assert.throws(() => percentile([1], 101), RangeError);
});

test("parseDurationMs", () => {
  assert.equal(parseDurationMs("30s"), 30_000);
  assert.equal(parseDurationMs("60m"), 3_600_000);
  assert.equal(parseDurationMs("500ms"), 500);
  assert.equal(parseDurationMs("1h"), 3_600_000);
  assert.equal(parseDurationMs("45"), 45_000);
  assert.throws(() => parseDurationMs("abc"));
  assert.throws(() => parseDurationMs("10x"));
});

const obs = (atMs: number, state: string, source = "api"): Observation => ({
  at: atMs,
  state,
  source,
});

test("stateSpans: collapses runs and closes spans at the next state", () => {
  const t0 = Date.parse("2026-10-02T00:00:00Z");
  const spans = stateSpans([
    obs(t0, "none"),
    obs(t0 + 5_000, "connected"),
    obs(t0 + 10_000, "connected"),
    obs(t0 + 15_000, "disconnected"),
    obs(t0 + 20_000, "disconnected"),
    obs(t0 + 25_000, "connected"),
  ]);
  assert.equal(spans.length, 4);
  assert.equal(spans[0].state, "none");
  assert.equal(spans[0].durationMs, 5_000);
  assert.equal(spans[1].state, "connected");
  assert.equal(spans[1].durationMs, 10_000);
  assert.equal(spans[2].state, "disconnected");
  assert.equal(spans[2].durationMs, 10_000);
  assert.equal(spans[2].source, "api");
  assert.equal(spans[3].state, "connected");
  assert.equal(spans[3].endedAt, undefined);
});

test("longestDisconnectedGapMs: longest non-connected stretch after first connect", () => {
  const t0 = Date.parse("2026-10-02T00:00:00Z");
  const observations = [
    obs(t0, "none"), // excluded: before first connected (counted in connectMs)
    obs(t0 + 2_000, "connected"),
    obs(t0 + 10_000, "stale"),
    obs(t0 + 20_000, "disconnected"),
    obs(t0 + 30_000, "connected"), // gap = 30s - 10s = 20s
    obs(t0 + 40_000, "stale"),
    obs(t0 + 45_000, "connected"), // gap = 5s
  ];
  assert.equal(longestDisconnectedGapMs(observations, t0 + 50_000), 20_000);
});

test("longestDisconnectedGapMs: open gap counts to run end", () => {
  const t0 = Date.parse("2026-10-02T00:00:00Z");
  const observations = [obs(t0, "connected"), obs(t0 + 10_000, "disconnected")];
  assert.equal(longestDisconnectedGapMs(observations, t0 + 40_000), 30_000);
});

test("longestDisconnectedGapMs: never connected -> 0", () => {
  const t0 = Date.parse("2026-10-02T00:00:00Z");
  assert.equal(
    longestDisconnectedGapMs([obs(t0, "none"), obs(t0 + 5_000, "none")], t0 + 10_000),
    0,
  );
});

test("buildReport: schema-valid report with pass/fail evaluation", () => {
  const t0 = Date.parse("2026-10-02T01:00:00Z");
  const report = buildReport(
    {
      dryRun: true,
      portalOrigin: "http://127.0.0.1:4810",
      template: "tpl_test",
      startedAt: t0,
      endedAt: t0 + 60_000,
      soakStartedAt: t0 + 2_000,
      requestedDurationMs: 58_000,
      sessionsRequested: 1,
      inputIntervalSeconds: 10,
      pollIntervalSeconds: 5,
    },
    {
      connectP95Ms: 60_000,
      reconnectP95Ms: 30_000,
      maxGapMs: 60_000,
      maxManualActions: 0,
      maxDroppedSessions: 0,
    },
    [
      {
        workspaceId: "ws_01J4Z8KQ2M9XNBV3T7YH0R6D5E",
        workspaceName: "soak-test-000",
        observations: [
          obs(t0 + 1_000, "none"),
          obs(t0 + 2_000, "connected"),
          obs(t0 + 30_000, "connected"),
          obs(t0 + 31_000, "disconnected"), // observed after the reload at +30.5s
          obs(t0 + 35_000, "connected"),
        ],
        launchedAt: t0 + 500,
        reloadedAt: t0 + 30_500,
        manualActions: 0,
        inputEvents: 6,
        dropped: false,
        runEndAt: t0 + 60_000,
      },
    ],
  ) as {
    sessions: {
      connectMs: number;
      reconnectMs: number;
      nonConnectedStates: { state: string }[];
    }[];
    summary: { pass: boolean; connectMs: { p50: number | null } };
  };
  assert.equal(report.sessions[0].connectMs, 1_500);
  // reconnectMs: from the observed non-connected state (+31s) to the next
  // connected one (+35s), not from the reload click.
  assert.equal(report.sessions[0].reconnectMs, 4_000);
  assert.equal(report.summary.pass, true);
  assert.equal(report.summary.connectMs.p50, 1_500);
  // The pre-connect "none" span is still listed as a non-connected state.
  assert.equal(report.sessions[0].nonConnectedStates[0].state, "none");
  validateReport(report); // double-check: must not throw
});

test("buildReport: threshold breach fails the run", () => {
  const t0 = Date.parse("2026-10-02T01:00:00Z");
  const report = buildReport(
    {
      dryRun: true,
      startedAt: t0,
      endedAt: t0 + 60_000,
      soakStartedAt: t0 + 5_000,
      requestedDurationMs: 55_000,
      sessionsRequested: 1,
      inputIntervalSeconds: 10,
      pollIntervalSeconds: 5,
    },
    {
      connectP95Ms: 1_000,
      reconnectP95Ms: null,
      maxGapMs: null,
      maxManualActions: 0,
      maxDroppedSessions: 0,
    },
    [
      {
        workspaceId: "ws_x",
        observations: [obs(t0 + 5_000, "connected")],
        launchedAt: t0,
        reloadedAt: null,
        manualActions: 0,
        inputEvents: 0,
        dropped: false,
        runEndAt: t0 + 60_000,
      },
    ],
  ) as { summary: { pass: boolean; failures: string[] } };
  assert.equal(report.summary.pass, false);
  assert.match(report.summary.failures[0], /connect p95/);
});

const RUN = (t0: number, over: Record<string, unknown> = {}) => ({
  dryRun: true,
  startedAt: t0,
  endedAt: t0 + 60_000,
  soakStartedAt: t0 + 2_000 as number | null,
  requestedDurationMs: 58_000,
  sessionsRequested: 1,
  inputIntervalSeconds: 10,
  pollIntervalSeconds: 5,
  ...over,
});
const LAX = {
  connectP95Ms: null,
  reconnectP95Ms: null,
  maxGapMs: null,
  maxManualActions: 0,
  maxDroppedSessions: 0,
};
const SESSION = (t0: number, observations: Observation[], reloadedAt: number | null) => ({
  workspaceId: "ws_r",
  observations,
  launchedAt: t0,
  reloadedAt,
  manualActions: 0,
  inputEvents: 0,
  dropped: false,
  runEndAt: t0 + 60_000,
});

test("R5d/R10b: a seamless reload records reconnectMs 0 and seamless true", () => {
  const t0 = Date.parse("2026-10-02T01:00:00Z");
  const report = buildReport(
    RUN(t0),
    LAX,
    [
      SESSION(
        t0,
        [
          obs(t0 + 1_000, "connected"),
          obs(t0 + 29_000, "connected"),
          obs(t0 + 31_000, "connected"), // reload at +30s resumed seamlessly
          obs(t0 + 36_000, "connected"),
        ],
        t0 + 30_000,
      ),
    ],
  ) as {
    sessions: { reconnectMs: number | null; seamless: boolean }[];
    summary: { reconnectMs: { p95: number | null } };
  };
  assert.equal(report.sessions[0].reconnectMs, 0);
  assert.equal(report.sessions[0].seamless, true);
  assert.equal(report.summary.reconnectMs.p95, 0);
});

test("R5d: a non-connected state seen before the reload does not count as the reconnect", () => {
  const t0 = Date.parse("2026-10-02T01:00:00Z");
  const report = buildReport(RUN(t0), LAX, [
    SESSION(
      t0,
      [
        obs(t0 + 1_000, "none"),
        obs(t0 + 2_000, "connected"),
        obs(t0 + 20_000, "stale"), // before the reload
        obs(t0 + 24_000, "connected"),
        obs(t0 + 31_000, "connected"),
      ],
      t0 + 30_000,
    ),
  ]) as { sessions: { reconnectMs: number | null; seamless: boolean }[] };
  assert.equal(report.sessions[0].reconnectMs, 0);
  assert.equal(report.sessions[0].seamless, true);
});

test("R10b: a drill gap 20 minutes after a seamless reload is not the reload's reconnect", () => {
  const t0 = Date.parse("2026-10-02T01:00:00Z");
  const min = 60_000;
  const observations = [
    obs(t0 + 1_000, "connected"),
    obs(t0 + 30_000, "connected"),
    obs(t0 + 31_000, "connected"), // reload at +30s, seamless
    obs(t0 + 20 * min, "connected"),
    obs(t0 + 20 * min + 5_000, "disconnected"), // drill: gateway restart
    obs(t0 + 20 * min + 25_000, "connected"),
  ];
  assert.deepEqual(reloadRecovery(observations, t0 + 30_000), { reconnectMs: 0, seamless: true });
  const report = buildReport(RUN(t0), LAX, [
    { ...SESSION(t0, observations, t0 + 30_000), runEndAt: t0 + 25 * min },
  ]) as {
    sessions: { reconnectMs: number | null; seamless: boolean; longestGapMs: number }[];
    summary: { reconnectMs: { p95: number | null } };
  };
  assert.equal(report.sessions[0].reconnectMs, 0);
  assert.equal(report.sessions[0].seamless, true);
  assert.equal(report.summary.reconnectMs.p95, 0);
  // The drill gap is still reported, as the gap it is.
  assert.equal(report.sessions[0].longestGapMs, 20_000);
});

test("R10b: only a non-connected observation within 60 s after the reload counts", () => {
  const reloadedAt = Date.parse("2026-10-02T01:00:30Z");
  const at = (ms: number, state: string) => obs(reloadedAt + ms, state);
  // Exactly at the 60 s boundary still counts as the reload's.
  assert.deepEqual(
    reloadRecovery([at(1_000, "connected"), at(60_000, "disconnected"), at(64_000, "connected")], reloadedAt),
    { reconnectMs: 4_000, seamless: false },
  );
  // One millisecond later belongs to something else.
  assert.deepEqual(
    reloadRecovery([at(1_000, "connected"), at(60_001, "disconnected"), at(64_000, "connected")], reloadedAt),
    { reconnectMs: 0, seamless: true },
  );
  // Lost within the window and never back: no reconnect time, not seamless.
  assert.deepEqual(reloadRecovery([at(2_000, "stale")], reloadedAt), {
    reconnectMs: null,
    seamless: false,
  });
  // A non-connected observation just before the reload never counts.
  assert.deepEqual(
    reloadRecovery([at(-3_000, "stale"), at(-1_000, "connected"), at(5_000, "connected")], reloadedAt),
    { reconnectMs: 0, seamless: true },
  );
});

test("R10b: a session that was not reloaded has no reconnect and is not seamless", () => {
  const t0 = Date.parse("2026-10-02T01:00:00Z");
  const report = buildReport(RUN(t0), LAX, [
    SESSION(t0, [obs(t0 + 1_000, "connected"), obs(t0 + 20_000, "connected")], null),
  ]) as { sessions: { reconnectMs: number | null; seamless: boolean }[] };
  assert.equal(report.sessions[0].reconnectMs, null);
  assert.equal(report.sessions[0].seamless, false);
});

test("R5d: a run shorter than the requested soak duration fails as truncated", () => {
  const t0 = Date.parse("2026-10-02T01:00:00Z");
  const report = buildReport(
    RUN(t0, { soakStartedAt: t0 + 10_000, endedAt: t0 + 40_000, requestedDurationMs: 60_000 }),
    LAX,
    [SESSION(t0, [obs(t0 + 1_000, "connected")], null)],
  ) as { summary: { pass: boolean; failures: string[] } };
  assert.equal(report.summary.pass, false);
  assert.match(report.summary.failures.join(";"), /truncated/);
});

test("R5d: a run whose soak clock never started fails", () => {
  const t0 = Date.parse("2026-10-02T01:00:00Z");
  const report = buildReport(RUN(t0, { soakStartedAt: null }), LAX, [
    SESSION(t0, [obs(t0 + 1_000, "none")], null),
  ]) as { summary: { pass: boolean; failures: string[] } };
  assert.equal(report.summary.pass, false);
  assert.match(report.summary.failures.join(";"), /never started|not every session/);
});

test("R5d: the full soak duration passes the truncation check", () => {
  const t0 = Date.parse("2026-10-02T01:00:00Z");
  const report = buildReport(
    RUN(t0, { soakStartedAt: t0 + 2_000, endedAt: t0 + 60_000, requestedDurationMs: 58_000 }),
    LAX,
    [SESSION(t0, [obs(t0 + 1_000, "connected")], null)],
  ) as { summary: { pass: boolean } };
  assert.equal(report.summary.pass, true);
});

test("validateReport rejects malformed reports", () => {
  assert.throws(() => validateReport({ version: 1 }), /schema validation/);
});

// e2e: `soak.ts --dry-run` spawns the contract mock API
// (web/tests/mock-api), runs 3 sessions for ~25 s and must write a
// schema-valid report well inside two minutes.
test(
  "dry-run against the mock API writes a schema-valid report",
  { timeout: 150_000 },
  () => {
    const reportPath = path.join(
      fs.mkdtempSync(path.join(os.tmpdir(), "tcdi-soak-")),
      "soak-report.json",
    );
    const t0 = Date.now();
    const r = spawnSync(
      process.execPath,
      [
        "--disable-warning=ExperimentalWarning",
        path.join(HERE, "soak.ts"),
        "--dry-run",
        "--sessions",
        "3",
        "--duration",
        "25s",
        "--poll-interval",
        "1s",
        "--input-interval",
        "2s",
        "--report",
        reportPath,
      ],
      { encoding: "utf8", timeout: 140_000 },
    );
    assert.equal(
      r.status,
      0,
      `soak --dry-run exited ${r.status} (${Math.round((Date.now() - t0) / 1000)}s)\n` +
        `stdout: ${r.stdout?.slice(-2000)}\nstderr: ${r.stderr?.slice(-2000)}`,
    );
    assert.ok(Date.now() - t0 < 120_000, "dry run exceeded 2 minutes");
    const report = JSON.parse(fs.readFileSync(reportPath, "utf8"));
    validateReport(report);
    assert.equal(report.sessions.length, 3);
    assert.equal(report.summary.pass, true);
    assert.ok(report.run.soakStartedAt, "the soak clock started");
    assert.equal(report.run.requestedDurationSeconds, 25);
    assert.ok(
      report.sessions.every((s: { connectMs: number | null }) => s.connectMs !== null),
      "every session connected",
    );
  },
);
