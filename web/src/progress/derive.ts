// Lifecycle progress derivation (V3.27 PR-B): a pure function turns a
// WorkspaceView — the same object the list/detail polls already return —
// into an ordered step list for the progress UI. Polling only: nothing here
// issues requests. Step reasons are the operator tokens merged in V3.27
// PR-A (podReason, BackendError, WaitingForDisk, the finalizer's teardown
// marks); an unknown token is shown verbatim in the details line, never
// dropped.

import type { components } from "../api/generated/schema";
import type { MessageKey } from "../i18n";

export type WorkspaceView = components["schemas"]["WorkspaceView"];
type Condition = WorkspaceView["conditions"][number];

export type ProgressOp = "create" | "start" | "stop" | "delete";

export type StepState = "done" | "active" | "waiting" | "failed" | "skipped";

export type StepId =
  | "accepted"
  | "disk"
  | "machine"
  | "desktop"
  | "connect"
  | "shutdown"
  | "stopped"
  | "sessions"
  | "runtime"
  | "data"
  | "finishing";

export interface ProgressStep {
  id: StepId;
  label: MessageKey;
  state: StepState;
  /** Operator reason token driving the active/failed state, when known. */
  reason?: string;
  /** Detail-line copy (slow/failed guidance); resolved at derive time. */
  notice?: MessageKey;
  /** `reason` is not a known operator token — render it verbatim. */
  unknownReason?: boolean;
}

export interface ProgressModel {
  op: ProgressOp;
  title: MessageKey;
  steps: ProgressStep[];
  /** Index of the active or failed step; `steps.length` when all done. */
  current: number;
  /** Terminal outcome rendered inside the panel ("deleted" is the page's 404). */
  terminal: "failed" | null;
  /** Operation anchor: workspaces.updated_at — written by start/stop/delete intents. */
  startedAtMs: number;
  /** Operation age at derive time, clamped ≥ 0. */
  elapsedMs: number;
  /** Client-observed age of the current step (tracker only — a lower bound). */
  stepElapsedMs?: number;
  /** The current step is past its slow threshold. */
  slow: boolean;
  /** The operation ran past its stall threshold — show the "still waiting" note. */
  stalled: boolean;
  /** A StatusStale Degraded condition marks the view as delayed. */
  delayed: boolean;
}

// --- operator reason tokens -------------------------------------------------

// RuntimeReady=False reasons that mean "the pod exists and is being made
// ready" — everything before them is still scheduling/machine work.
const DESKTOP_REASONS = new Set([
  "PreparingPod",
  "PullingImage",
  "ContainerCreating",
  "PodInitializing",
  "NotReady",
  "ImagePullBackOff",
  "ErrImagePull",
  "CrashLoopBackOff",
  "CreateContainerConfigError",
  "PodFailed",
]);

// Finalizer step marks on RuntimeReady=False during delete (G5), in order.
const TEARDOWN_ORDER = [
  "BlockingConnects",
  "RevokingLeases",
  "DrainingStreams",
  "StoppingRuntime",
  "ApplyingRetention",
  "CleaningUp",
] as const;

// Every token the backend is known to emit on step conditions; anything
// else renders verbatim in the details line. Stale stop-era reasons
// (RuntimeStopped) are recognized but quiet: they linger on conditions left
// over from a previous run and carry no progress information.
const KNOWN_REASONS = new Set([
  ...DESKTOP_REASONS,
  ...TEARDOWN_ORDER,
  "Provisioning",
  "Unschedulable",
  "BackendError",
  "WaitingForDisk",
  "BootDeadlineExceeded",
  "QuotaReserved",
  "TemplateResolved",
  "IntentApplied",
  "VolumeBound",
  "Ready",
  "RuntimeUp",
  "StreamEndpointUp",
  "Stopped",
  "RuntimeStopped",
  "StopRequested",
  "IdleTimeout",
  "DisconnectTimeout",
  "MaxDuration",
  "Terminating",
  "StreamDraining",
  "DrainTimedOut",
  "RetentionPending",
  "CleanupRetry",
  "FailedCleanup",
  "MissingWorkspaceID",
  "StatusStale",
]);

// Reason tokens that always carry a detail line (informative on their own).
const REASON_NOTICE: Partial<Record<string, MessageKey>> = {
  Unschedulable: "progress.slow.machine",
  ImagePullBackOff: "progress.failed.imagePull",
  ErrImagePull: "progress.failed.imagePull",
  CrashLoopBackOff: "progress.notice.retry",
  CreateContainerConfigError: "progress.notice.retry",
  PodFailed: "progress.notice.retry",
  BackendError: "progress.notice.retry",
  StreamDraining: "progress.slow.drain",
  DrainTimedOut: "progress.slow.generic",
  RetentionPending: "progress.slow.generic",
  CleanupRetry: "progress.failed.cleanup",
  FailedCleanup: "progress.failed.cleanup",
};

// Active-step label overrides for desktop-phase reasons (one step slot;
// the label follows what the kubelet is actually doing).
const DESKTOP_LABEL: Partial<Record<string, MessageKey>> = {
  PullingImage: "progress.step.imagePull",
  ImagePullBackOff: "progress.step.imagePull",
  ErrImagePull: "progress.step.imagePull",
  PreparingPod: "progress.step.machinePrepare",
  CrashLoopBackOff: "progress.step.desktopStart",
  CreateContainerConfigError: "progress.step.desktopStart",
  PodFailed: "progress.step.desktopStart",
};

// Slow thresholds (design A3), per step id.
const SLOW_MS: Record<StepId, number> = {
  accepted: 15_000,
  disk: 60_000,
  machine: 60_000,
  desktop: 120_000,
  connect: 30_000,
  shutdown: 30_000,
  stopped: 30_000,
  sessions: 50_000,
  runtime: 60_000,
  data: 90_000,
  finishing: 90_000,
};

/** Past this age a start/create op shows the "still waiting" note. */
export const STALL_MS = 10 * 60_000;

// --- server clock ------------------------------------------------------------

// Elapsed times are anchored on server timestamps (updatedAt). One observed
// response Date header corrects client clock skew; none is fine too.
let skewMs = 0;

export function noteServerDateHeader(header: string | null): void {
  if (!header) return;
  const t = Date.parse(header);
  if (!Number.isNaN(t)) skewMs = t - Date.now();
}

export function serverNow(): number {
  return Date.now() + skewMs;
}

// --- tracker (replica-lag latch) ----------------------------------------------

// Per tab, in memory. Two API replicas serve slightly stale informer views
// and may disagree by a few seconds either way: once a step is reached, a
// later poll showing an earlier step is lag, not regression — it is ignored
// for up to LAG_MAX polls, unless phase or updatedAt moved (a real change).
export const LAG_MAX = 3;

export interface ProgressTracker {
  key: string;
  phase: string;
  /** The view object the latch last scored — a new poll makes a new object. */
  seen: WorkspaceView | null;
  stepIndex: number;
  lag: number;
  /** Client ms when the latched step was first observed. */
  stepSince: number;
}

export function createTracker(): ProgressTracker {
  return { key: "", phase: "", seen: null, stepIndex: -1, lag: 0, stepSince: 0 };
}

// --- derivation ---------------------------------------------------------------

function cond(ws: WorkspaceView, type: Condition["type"]): Condition | undefined {
  return ws.conditions.find((c) => c.type === type);
}

function isDelayed(ws: WorkspaceView): boolean {
  const d = cond(ws, "Degraded");
  return d?.status === "True" && d.reason === "StatusStale";
}

function opOf(ws: WorkspaceView): ProgressOp | null {
  // Order matters: FX-R19 sets desiredState=Stopped on delete, so
  // Terminating must be checked before the stop rule.
  if (ws.phase === "Terminating") return "delete";
  if (ws.phase === "Failed") return ws.createdAt === ws.updatedAt ? "create" : "start";
  if (
    ws.desiredState === "Stopped" &&
    (ws.phase === "Ready" ||
      ws.phase === "Pending" ||
      ws.phase === "Provisioning" ||
      ws.phase === "Stopping")
  ) {
    return "stop";
  }
  if (
    ws.desiredState === "Running" &&
    (ws.phase === "Stopped" || ws.phase === "Pending" || ws.phase === "Provisioning")
  ) {
    return ws.createdAt === ws.updatedAt ? "create" : "start";
  }
  return null;
}

const OP_TITLE: Record<ProgressOp, MessageKey> = {
  create: "progress.create.title",
  start: "progress.start.title",
  stop: "progress.stop.title",
  delete: "progress.delete.title",
};

interface StepDef {
  id: StepId;
  label: MessageKey;
  done: boolean;
  reason?: string;
}

// Which step a reason belongs to, for locating the failed step at Failed:
// the operator keeps the last pod reason on ConnectionReady while
// RuntimeReady carries the BootDeadlineExceeded overwrite.
function stepOfReason(reason: string | undefined): StepId | null {
  if (!reason) return null;
  if (reason === "WaitingForDisk") return "disk";
  if (reason === "Unschedulable") return "machine";
  if (DESKTOP_REASONS.has(reason) || reason === "BackendError") return "desktop";
  return null;
}

function startDefs(ws: WorkspaceView): StepDef[] {
  const storage = cond(ws, "StorageReady");
  const runtime = cond(ws, "RuntimeReady");
  const conn = cond(ws, "ConnectionReady");
  const runtimeReason = runtime?.status === "False" ? runtime.reason : undefined;
  const pickedUp = ws.phase === "Pending" || ws.phase === "Provisioning" || ws.phase === "Ready";
  const defs: StepDef[] = [
    {
      id: "accepted",
      label: pickedUp ? "progress.step.accepted" : "progress.step.queued",
      done: pickedUp,
    },
  ];
  if (ws.dataPolicy === "Retain") {
    defs.push({
      id: "disk",
      label: ws.retainedDataRef ? "progress.step.diskAttach" : "progress.step.disk",
      done: storage?.status === "True",
      ...(storage?.status === "False" ? { reason: storage.reason } : {}),
    });
  }
  defs.push(
    {
      id: "machine",
      label: "progress.step.machine",
      done: !!runtime && (runtime.status === "True" || DESKTOP_REASONS.has(runtime.reason)),
      ...(runtimeReason !== undefined && !DESKTOP_REASONS.has(runtimeReason)
        ? { reason: runtimeReason }
        : {}),
    },
    {
      id: "desktop",
      label: "progress.step.desktop",
      done: runtime?.status === "True",
      ...(runtimeReason !== undefined && DESKTOP_REASONS.has(runtimeReason)
        ? { reason: runtimeReason }
        : {}),
    },
    {
      id: "connect",
      label: "progress.step.connect",
      done: ws.phase === "Ready" && conn?.status === "True",
      ...(runtime?.status === "True" && conn?.status === "False" ? { reason: conn.reason } : {}),
    },
  );
  return defs;
}

function stopDefs(ws: WorkspaceView): StepDef[] {
  const shuttingDown = ws.phase === "Stopping" || ws.phase === "Stopped";
  return [
    {
      id: "accepted",
      label: shuttingDown ? "progress.step.accepted" : "progress.step.queued",
      done: shuttingDown,
    },
    { id: "shutdown", label: "progress.step.shutdown", done: ws.phase === "Stopped" },
    { id: "stopped", label: "progress.step.stopped", done: ws.phase === "Stopped" },
  ];
}

function deleteDefs(ws: WorkspaceView): StepDef[] {
  const runtime = cond(ws, "RuntimeReady");
  const degraded = cond(ws, "Degraded");
  const teardown = runtime?.status === "False" ? runtime.reason : undefined;
  const tIdx = teardown !== undefined ? TEARDOWN_ORDER.indexOf(teardown as never) : -1;
  const degradedReason = degraded?.status === "True" ? degraded.reason : undefined;
  const gone = ws.conditions.length === 0;
  const defs: StepDef[] = [
    // The 202 that flipped the row to Terminating is the acceptance.
    { id: "accepted", label: "progress.step.accepted", done: true },
    {
      id: "sessions",
      label: "progress.step.sessions",
      done: gone || tIdx >= 3,
      ...(tIdx >= 0 && tIdx < 3 && teardown !== undefined
        ? { reason: teardown }
        : degradedReason === "StreamDraining" || degradedReason === "DrainTimedOut"
          ? { reason: degradedReason }
          : {}),
    },
    {
      id: "runtime",
      label: "progress.step.runtime",
      done: gone || tIdx >= 4,
      ...(teardown === "StoppingRuntime" ? { reason: teardown } : {}),
    },
  ];
  if (ws.dataPolicy === "Retain") {
    defs.push({
      id: "data",
      label: "progress.step.data",
      done: gone || tIdx >= 5,
      ...(teardown === "ApplyingRetention"
        ? { reason: teardown }
        : degradedReason === "RetentionPending"
          ? { reason: degradedReason }
          : {}),
    });
  }
  defs.push({
    id: "finishing",
    label: "progress.step.finishing",
    done: false,
    ...(teardown === "CleaningUp"
      ? { reason: teardown }
      : degradedReason === "CleanupRetry" || degradedReason === "MissingWorkspaceID"
        ? { reason: degradedReason }
        : {}),
  });
  return defs;
}

// The desktop step's label follows the active reason (image pull vs pod
// init vs crash loop); labels for the other steps are fixed per id.
function resolveLabel(def: StepDef, state: StepState): MessageKey {
  if (def.id === "desktop" && (state === "active" || state === "failed") && def.reason) {
    return DESKTOP_LABEL[def.reason] ?? def.label;
  }
  return def.label;
}

function noticeFor(step: ProgressStep, slow: boolean): MessageKey | undefined {
  if (step.reason) {
    const n = REASON_NOTICE[step.reason];
    if (n) return n;
  }
  if (slow) {
    if (step.id === "desktop" && step.reason === "PullingImage") return "progress.slow.imagePull";
    return "progress.slow.generic";
  }
  return undefined;
}

function failedNotice(step: ProgressStep, ws: WorkspaceView): MessageKey {
  if (step.reason) {
    const n = REASON_NOTICE[step.reason];
    if (n && n !== "progress.slow.generic") return n;
  }
  if (ws.failureReason === "BootDeadlineExceeded") return "progress.failed.deadline";
  return "progress.failed.generic";
}

/**
 * The replica-safe latch + timing pass over a raw derivation. `rawIndex` is
 * the first not-done step in the freshly derived list; the returned index is
 * what the UI shows after the lag rules. Mutates `tracker`.
 */
export function applyTracker(
  tracker: ProgressTracker,
  ws: WorkspaceView,
  rawIndex: number,
  stepCount: number,
  delayed: boolean,
  now: number,
): number {
  const key = `${ws.id}${ws.updatedAt}`;
  if (tracker.key !== key) {
    tracker.key = key;
    tracker.phase = ws.phase;
    tracker.seen = ws;
    tracker.stepIndex = rawIndex;
    tracker.lag = 0;
    tracker.stepSince = now;
    return rawIndex;
  }
  // The same view object is a re-derive (the elapsed ticker), not a poll:
  // lag counts observations, not renders.
  if (tracker.seen !== ws) {
    tracker.seen = ws;
    if (ws.phase !== tracker.phase) {
      // A phase change is authoritative: accept whatever it carries.
      tracker.phase = ws.phase;
      tracker.stepIndex = rawIndex;
      tracker.lag = 0;
      tracker.stepSince = now;
    } else if (rawIndex > tracker.stepIndex && !delayed) {
      tracker.stepIndex = rawIndex;
      tracker.lag = 0;
      tracker.stepSince = now;
    } else if (rawIndex < tracker.stepIndex) {
      tracker.lag += 1;
      if (tracker.lag > LAG_MAX) {
        tracker.stepIndex = rawIndex;
        tracker.stepSince = now;
        tracker.lag = 0;
      }
    }
  }
  return Math.min(Math.max(tracker.stepIndex, 0), stepCount - 1);
}

export function deriveProgress(
  ws: WorkspaceView,
  tracker?: ProgressTracker,
  now: number = serverNow(),
): ProgressModel | null {
  const op = opOf(ws);
  if (op === null) return null;
  const terminal: ProgressModel["terminal"] = ws.phase === "Failed" ? "failed" : null;
  const delayed = isDelayed(ws);
  const defs = op === "delete" ? deleteDefs(ws) : op === "stop" ? stopDefs(ws) : startDefs(ws);

  let index = defs.findIndex((d) => !d.done);
  if (index === -1) index = defs.length;

  // At Failed the stalled step is located by the pod reason the operator
  // kept on ConnectionReady, not the generic first-not-done position.
  if (terminal && (op === "start" || op === "create")) {
    const reason = cond(ws, "ConnectionReady")?.reason;
    const at = defs.findIndex((d) => d.id === stepOfReason(reason));
    if (at >= 0) {
      index = at;
      defs[index] = { ...defs[index], reason: reason! };
    } else if (index < defs.length && defs[index].reason === undefined && ws.failureReason) {
      defs[index] = { ...defs[index], reason: ws.failureReason };
    }
  }

  let stepElapsedMs: number | undefined;
  if (tracker) {
    index = terminal ? index : applyTracker(tracker, ws, index, defs.length, delayed, now);
    stepElapsedMs = Math.max(0, now - tracker.stepSince);
  }

  const parsed = Date.parse(ws.updatedAt);
  const startedAtMs = Number.isNaN(parsed) ? now : parsed;
  const elapsedMs = Math.max(0, now - startedAtMs);
  const slow =
    index < defs.length && stepElapsedMs !== undefined && stepElapsedMs > SLOW_MS[defs[index].id];
  const stalled = (op === "start" || op === "create") && !terminal && elapsedMs > STALL_MS;

  const steps: ProgressStep[] = defs.map((d, i) => {
    const state: StepState = terminal
      ? i < index
        ? "done"
        : i === index
          ? "failed"
          : "skipped"
      : i < index
        ? "done"
        : i === index
          ? "active"
          : "waiting";
    const step: ProgressStep = { id: d.id, label: resolveLabel(d, state), state };
    if (i === index && (state === "active" || state === "failed") && d.reason !== undefined) {
      step.reason = d.reason;
      if (!KNOWN_REASONS.has(d.reason)) step.unknownReason = true;
    }
    return step;
  });

  // A Degraded holdup reason (drain timeout, retention wait, cleanup retry)
  // outranks the coarse teardown mark for the active delete step's notice.
  if (op === "delete") {
    const dr = cond(ws, "Degraded");
    const reason = dr?.status === "True" ? dr.reason : undefined;
    const cur = steps[index];
    if (cur && reason !== undefined && reason !== "StatusStale") {
      cur.reason = reason;
      if (!KNOWN_REASONS.has(reason)) cur.unknownReason = true;
    }
  }

  const current = steps[index];
  if (current && (current.state === "active" || current.state === "failed")) {
    const notice = terminal === "failed" ? failedNotice(current, ws) : noticeFor(current, slow);
    if (notice !== undefined) current.notice = notice;
  }

  return {
    op,
    title: OP_TITLE[op],
    steps,
    current: index,
    terminal,
    startedAtMs,
    elapsedMs,
    ...(stepElapsedMs !== undefined ? { stepElapsedMs } : {}),
    slow,
    stalled,
    delayed,
  };
}

// --- polling cadence -----------------------------------------------------------

// One request per tick: fast while the operation is young, backing off as it
// ages; after 10 min a 10 s "stalled" cadence. ±20 % jitter keeps replicas
// and tabs from lock-stepping.
export function opPollMs(startedAtMs: number, now: number = serverNow()): number {
  const age = Math.max(0, now - startedAtMs);
  if (age < 20_000) return 1_500;
  if (age < 120_000) return 3_000;
  if (age < 600_000) return 5_000;
  return 10_000;
}

export function withJitter(ms: number, rand: number = Math.random()): number {
  return Math.round(ms * (0.8 + rand * 0.4));
}

/** Poll interval for a workspace: operation cadence while in flight, else idle. */
export function workspacePollMs(
  ws: WorkspaceView | undefined,
  idleMs: number,
  now: number = serverNow(),
): number {
  if (!ws) return idleMs;
  const m = deriveProgress(ws, undefined, now);
  return m !== null && m.terminal === null ? opPollMs(m.startedAtMs, now) : idleMs;
}

/** A start/create intent the operator has not finished (or given up on) yet. */
export function startInFlight(ws: WorkspaceView): boolean {
  const m = deriveProgress(ws);
  return m !== null && m.terminal === null && (m.op === "start" || m.op === "create");
}

// --- display helpers ------------------------------------------------------------

/** "0:35" under an hour, "1:02:03" past it — for the elapsed counters. */
export function formatElapsed(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  const pad = (n: number) => String(n).padStart(2, "0");
  return h > 0 ? `${h}:${pad(m)}:${pad(sec)}` : `${m}:${pad(sec)}`;
}
