// Report assembly and schema validation for the soak harness. The report
// is validated against report.schema.json before it is written, in both
// real and dry runs — a harness that emits an unparseable report fails the
// run, not the reader.

import fs from "node:fs";
import path from "node:path";
import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";
import type { ErrorObject, ValidateFunction } from "ajv";
import {
  elsewhereTransitions,
  longestDisconnectedGapMs,
  percentile,
  RELOAD_RECONNECT_WINDOW_MS,
  reloadRecovery,
  stateSpans,
  type Observation,
} from "./metrics.ts";

const SCHEMA_PATH = path.resolve(import.meta.dirname, "report.schema.json");

let validator: ValidateFunction | undefined;

function schemaValidator(): ValidateFunction {
  if (!validator) {
    const ajv = new Ajv2020({ allErrors: true, strict: true });
    addFormats(ajv);
    validator = ajv.compile(JSON.parse(fs.readFileSync(SCHEMA_PATH, "utf8")));
  }
  return validator;
}

/** Throws with every schema violation when the report is not schema-valid. */
export function validateReport(report: unknown): void {
  const validate = schemaValidator();
  if (!validate(report)) {
    const detail = (validate.errors ?? [])
      .map((e: ErrorObject) => `${e.instancePath || "/"} ${e.message}`)
      .join("; ");
    throw new Error(`soak report failed schema validation: ${detail}`);
  }
}

export interface Thresholds {
  connectP95Ms: number | null;
  reconnectP95Ms: number | null;
  maxGapMs: number | null;
  /** Disconnect-span (post-connect gap) p95/p100 caps — the advisor's after-drill reconnect gates. */
  disconnectP95Ms?: number | null;
  disconnectP100Ms?: number | null;
  /** Cap on disconnect-to-first-input latency (harness-side usability). */
  inputResumeP100Ms?: number | null;
  maxManualActions: number;
  maxDroppedSessions: number;
}

export interface SessionResult {
  workspaceId: string;
  workspaceName?: string;
  /** Username of the soak lane that owns this session (multi-user runs). */
  owner?: string;
  observations: Observation[];
  /** Launch click timestamp (epoch ms), or null when launch never happened. */
  launchedAt: number | null;
  /** Mid-run reload timestamp (epoch ms), or null when never reloaded. */
  reloadedAt: number | null;
  manualActions: number;
  inputEvents: number;
  /** Dispatches that hit the per-input timeout (the page's ops were wedged). */
  inputTimeouts?: number;
  /** Wall-clock ms of each scripted input dispatch (harness-side latency). */
  inputMs?: number[];
  /** Wall-clock ms from disconnect start to the first successful input after reconnect. */
  inputResumeMs?: number[];
  dropped: boolean;
  runEndAt: number;
}

export interface RunInfo {
  dryRun: boolean;
  portalOrigin?: string;
  template?: string;
  /** Run start: login, workspace creation and readiness included. */
  startedAt: number;
  endedAt: number;
  /**
   * When the soak clock started: the moment the last session first
   * connected (or was given up on). null when that never happened.
   */
  soakStartedAt: number | null;
  /** The --duration the soak was asked to observe, counted from soakStartedAt. */
  requestedDurationMs: number;
  sessionsRequested: number;
  /** Sessions actually created; lower than sessionsRequested when a lane was skipped. */
  sessionsEffective?: number;
  /** Lanes that exhausted their login attempts and were skipped (their share of sessions dropped). */
  skippedLanes?: { user: string; error: string }[];
  /** Per-lane tallies: HTTP 429 responses and the skip flag. */
  lanes?: { user: string; rateLimited429: number; skipped: boolean }[];
  /** How many distinct users the sessions were spread over (>= 1). */
  users?: number;
  inputIntervalSeconds: number;
  pollIntervalSeconds: number;
}

function firstConnectedAt(obs: Observation[]): number | null {
  const hit = [...obs].sort((a, b) => a.at - b.at).find((o) => o.state === "connected");
  return hit ? hit.at : null;
}

/** Build and validate the report object; throws on schema violation. */
export function buildReport(
  run: RunInfo,
  thresholds: Thresholds,
  sessions: SessionResult[],
  extraFailures: string[] = [],
): Record<string, unknown> {
  const rows = sessions.map((s) => {
    const connectedAt = firstConnectedAt(s.observations);
    const spans = stateSpans(s.observations).filter((sp) => sp.state !== "connected");
    const reload = s.reloadedAt === null ? null : reloadRecovery(s.observations, s.reloadedAt);
    return {
      workspaceId: s.workspaceId,
      ...(s.workspaceName !== undefined ? { workspaceName: s.workspaceName } : {}),
      ...(s.owner !== undefined ? { owner: s.owner } : {}),
      connectMs:
        s.launchedAt !== null && connectedAt !== null ? connectedAt - s.launchedAt : null,
      reconnectMs: reload === null ? null : reload.reconnectMs,
      seamless: reload?.seamless ?? false,
      reloadEvidence: reload === null ? null : reload.evidence,
      longestGapMs: longestDisconnectedGapMs(s.observations, s.runEndAt),
      nonConnectedStates: spans,
      elsewhereTransitions: elsewhereTransitions(s.observations),
      manualActions: s.manualActions,
      inputEvents: s.inputEvents,
      inputTimeouts: s.inputTimeouts,
      dropped: s.dropped,
    };
  });

  const connects = rows.map((r) => r.connectMs).filter((v): v is number => v !== null);
  const reconnects = rows.map((r) => r.reconnectMs).filter((v): v is number => v !== null);
  const inputs = sessions.flatMap((s) => s.inputMs ?? []);
  const inputResumes = sessions.flatMap((s) => s.inputResumeMs ?? []);
  // Disconnect spans = non-connected stretches that START after the session's
  // first connect — the reconnect latency the user feels around drills.
  const disconnects = sessions.flatMap((s) => {
    const first = firstConnectedAt(s.observations);
    if (first === null) return [];
    return stateSpans(s.observations)
      .filter(
        (sp) =>
          sp.state !== "connected" &&
          new Date(sp.startedAt).getTime() >= first &&
          sp.durationMs !== null,
      )
      .map((sp) => sp.durationMs);
  });
  const disconnectP95 = percentile(disconnects, 95);
  const disconnectP100 = percentile(disconnects, 100);
  const inputResumeP100 = percentile(inputResumes, 100);
  const manualTotal = rows.reduce((n, r) => n + r.manualActions, 0);
  const droppedTotal = rows.filter((r) => r.dropped).length;
  const connectP95 = percentile(connects, 95);
  const reconnectP95 = percentile(reconnects, 95);
  const worstGap = Math.max(0, ...rows.map((r) => r.longestGapMs));
  // A reloaded session whose 60 s window held no observation proves nothing:
  // the reload check failed for lack of evidence, never a pass (backlog 4).
  const unprovenReloads = rows.filter((r) => r.reloadEvidence === "none").length;

  const failures: string[] = [...extraFailures];
  if (unprovenReloads > 0) {
    failures.push(
      `${unprovenReloads} session(s) reloaded with no observation inside the ` +
        `${RELOAD_RECONNECT_WINDOW_MS / 1000}s window: reload check unproven`,
    );
  }
  if (thresholds.connectP95Ms !== null && connectP95 !== null && connectP95 > thresholds.connectP95Ms) {
    failures.push(`connect p95 ${connectP95}ms exceeds ${thresholds.connectP95Ms}ms`);
  }
  if (
    thresholds.reconnectP95Ms !== null &&
    reconnectP95 !== null &&
    reconnectP95 > thresholds.reconnectP95Ms
  ) {
    failures.push(`reconnect p95 ${reconnectP95}ms exceeds ${thresholds.reconnectP95Ms}ms`);
  }
  if (thresholds.maxGapMs !== null && worstGap > thresholds.maxGapMs) {
    failures.push(`longest connected gap ${worstGap}ms exceeds ${thresholds.maxGapMs}ms`);
  }
  if (
    thresholds.disconnectP95Ms != null &&
    disconnectP95 !== null &&
    disconnectP95 > thresholds.disconnectP95Ms
  ) {
    failures.push(`disconnect-span p95 ${disconnectP95}ms exceeds ${thresholds.disconnectP95Ms}ms`);
  }
  if (
    thresholds.disconnectP100Ms != null &&
    disconnectP100 !== null &&
    disconnectP100 > thresholds.disconnectP100Ms
  ) {
    failures.push(`disconnect-span p100 ${disconnectP100}ms exceeds ${thresholds.disconnectP100Ms}ms`);
  }
  if (
    thresholds.inputResumeP100Ms != null &&
    inputResumeP100 !== null &&
    inputResumeP100 > thresholds.inputResumeP100Ms
  ) {
    failures.push(`input-resume p100 ${inputResumeP100}ms exceeds ${thresholds.inputResumeP100Ms}ms`);
  }
  if (manualTotal > thresholds.maxManualActions) {
    failures.push(`${manualTotal} manual actions exceed ${thresholds.maxManualActions}`);
  }
  if (droppedTotal > thresholds.maxDroppedSessions) {
    failures.push(`${droppedTotal} dropped sessions exceed ${thresholds.maxDroppedSessions}`);
  }

  if (run.soakStartedAt === null) {
    failures.push("soak clock never started: not every session reached connected");
  } else if (run.endedAt - run.soakStartedAt < run.requestedDurationMs) {
    failures.push(
      `run truncated: observed ${(run.endedAt - run.soakStartedAt) / 1000}s of the requested ` +
        `${run.requestedDurationMs / 1000}s soak`,
    );
  }

  const report = {
    version: 1,
    run: {
      tool: "tinycdi-soak",
      dryRun: run.dryRun,
      ...(run.portalOrigin !== undefined ? { portalOrigin: run.portalOrigin } : {}),
      ...(run.template !== undefined ? { template: run.template } : {}),
      startedAt: new Date(run.startedAt).toISOString(),
      endedAt: new Date(run.endedAt).toISOString(),
      durationSeconds: (run.endedAt - run.startedAt) / 1000,
      ...(run.soakStartedAt !== null
        ? { soakStartedAt: new Date(run.soakStartedAt).toISOString() }
        : {}),
      requestedDurationSeconds: run.requestedDurationMs / 1000,
      sessionsRequested: run.sessionsRequested,
      ...(run.sessionsEffective !== undefined
        ? { sessionsEffective: run.sessionsEffective }
        : {}),
      ...(run.skippedLanes !== undefined && run.skippedLanes.length > 0
        ? { skippedLanes: run.skippedLanes }
        : {}),
      ...(run.lanes !== undefined ? { lanes: run.lanes } : {}),
      ...(run.users !== undefined ? { users: run.users } : {}),
      inputIntervalSeconds: run.inputIntervalSeconds,
      pollIntervalSeconds: run.pollIntervalSeconds,
    },
    thresholds,
    sessions: rows,
    summary: {
      connectMs: { p50: percentile(connects, 50), p95: connectP95 },
      reconnectMs: { p50: percentile(reconnects, 50), p95: reconnectP95 },
      inputDispatchMs: { p50: percentile(inputs, 50), p95: percentile(inputs, 95) },
      inputTimeouts: rows.reduce((n, r) => n + (r.inputTimeouts ?? 0), 0),
      disconnectMs: {
        p50: percentile(disconnects, 50),
        p95: disconnectP95,
        p100: disconnectP100,
      },
      inputResumeMs: {
        p50: percentile(inputResumes, 50),
        p95: percentile(inputResumes, 95),
        p100: inputResumeP100,
      },
      sessionsWithManualActions: rows.filter((r) => r.manualActions > 0).length,
      droppedSessions: droppedTotal,
      falseElsewhereTransitions: rows.reduce((n, r) => n + r.elsewhereTransitions, 0),
      pass: failures.length === 0,
      failures,
    },
  };
  validateReport(report);
  return report;
}
