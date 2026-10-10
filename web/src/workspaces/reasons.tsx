// Workspace reason tokens → portal copy (V3.14b, params B3-PARAMS).
// Events and conditions arrive with the server's English message text and,
// since v0.4, an optional `params` string map carrying the values the
// message interpolated (revision, recorded cause, teardown step, drain
// budget...). A known token renders its catalog template with the params
// formatted for the active locale — token values (step, skipReason) map
// through their own catalog keys, numbers/durations/sizes through the
// shared formatters, and every value lands as text so React escapes it.
// A missing param, an unknown token or an unknown param value falls back
// to the server message (or the raw token), unchanged from v0.3.x.
//
// Token inventory: internal/api/events.go (intent + curated events),
// internal/operator/{status,workspace_controller,finalizer}.go (condition
// reasons), internal/runtime/linux/backend.go podReason (pod wait tokens),
// internal/api/statusview.go (StatusStale) — plus the demo/fixture tokens
// the mock API emits so dev-mode screens read the same way. Param keys:
// condition-params annotation (operator) → condition params; events.go →
// event params.

import {
  t,
  formatNumber,
  type MessageKey,
} from "../i18n";
import {
  formatCpu,
  formatDuration,
  formatMemory,
  formatStorage,
} from "../templates/format";

const REASON_TEXT: Readonly<Record<string, MessageKey>> = {
  // API-curated lifecycle events (internal/api/events.go).
  Created: "workspaces.reason.created",
  StartRequested: "workspaces.reason.startRequested",
  StopRequested: "workspaces.reason.stopRequested",
  MaxDurationReached: "workspaces.reason.maxDurationReached",
  DeleteRequested: "workspaces.reason.deleteRequested",
  TemplateUpdateSkipped: "workspaces.reason.templateUpdateSkipped",

  // Condition reasons (internal/operator).
  TemplateResolved: "workspaces.reason.templateResolved",
  TemplateNotFound: "workspaces.reason.templateNotFound",
  TemplateRejected: "workspaces.reason.templateRejected",
  TemplateSnapshotInvalid: "workspaces.reason.templateSnapshotInvalid",
  TemplateInvalid: "workspaces.reason.templateInvalid",
  TemplateRevisionGone: "workspaces.reason.templateRevisionGone",
  IntentApplied: "workspaces.reason.intentApplied",
  Provisioning: "workspaces.reason.provisioning",
  WaitingForDisk: "workspaces.reason.waitingForDisk",
  Ready: "workspaces.reason.ready",
  Stopped: "workspaces.reason.stopped",
  Terminating: "workspaces.reason.terminating",
  NameConflict: "workspaces.reason.nameConflict",
  BootDeadlineExceeded: "workspaces.reason.bootDeadlineExceeded",
  BackendError: "workspaces.reason.backendError",
  Nominal: "workspaces.reason.nominal",
  RetainedClaimMissing: "workspaces.reason.retainedClaimMissing",
  IdleTimeout: "workspaces.reason.idleTimeout",
  DisconnectTimeout: "workspaces.reason.disconnectTimeout",
  MaxDuration: "workspaces.reason.maxDuration",
  StreamDraining: "workspaces.reason.streamDraining",
  DrainTimedOut: "workspaces.reason.drainTimedOut",
  RetentionPending: "workspaces.reason.retentionPending",
  CleanupRetry: "workspaces.reason.cleanupRetry",
  FailedCleanup: "workspaces.reason.failedCleanup",
  MissingWorkspaceID: "workspaces.reason.missingWorkspaceID",
  IntentBehind: "workspaces.reason.intentBehind",

  // Finalizer step marks on RuntimeReady during teardown.
  BlockingConnects: "workspaces.reason.blockingConnects",
  RevokingLeases: "workspaces.reason.revokingLeases",
  DrainingStreams: "workspaces.reason.drainingStreams",
  StoppingRuntime: "workspaces.reason.stoppingRuntime",
  ApplyingRetention: "workspaces.reason.applyingRetention",
  CleaningUp: "workspaces.reason.cleaningUp",

  // Pod observation tokens (internal/runtime/linux/backend.go podReason).
  Unschedulable: "workspaces.reason.unschedulable",
  PreparingPod: "workspaces.reason.preparingPod",
  PullingImage: "workspaces.reason.pullingImage",
  ContainerCreating: "workspaces.reason.containerCreating",
  PodInitializing: "workspaces.reason.podInitializing",
  NotReady: "workspaces.reason.notReady",
  ImagePullBackOff: "workspaces.reason.imagePull",
  ErrImagePull: "workspaces.reason.imagePull",
  CrashLoopBackOff: "workspaces.reason.crashLoopBackOff",
  CreateContainerConfigError: "workspaces.reason.createContainerConfigError",
  PodFailed: "workspaces.reason.podFailed",
  PodExited: "workspaces.reason.podExited",

  // API-projected markers (internal/api/statusview.go, events.go).
  StatusStale: "workspaces.reason.statusStale",
  Unknown: "workspaces.reason.unknown",

  // Mock/fixture tokens (web/tests/mock-api) — the demo tenant and the
  // contract fixtures stand in for real surfaces; mapping them keeps the
  // dev portal readable in every locale.
  Admitted: "workspaces.reason.admitted",
  QuotaReserved: "workspaces.reason.quotaReserved",
  VolumeBound: "workspaces.reason.volumeBound",
  RuntimeUp: "workspaces.reason.runtimeUp",
  StreamEndpointUp: "workspaces.reason.streamEndpointUp",
  RuntimeStopped: "workspaces.reason.runtimeStopped",
  RuntimeReady: "workspaces.reason.runtimeUp",
  Pulling: "workspaces.reason.pullingImage",
  ImagePulling: "workspaces.reason.pullingImage",
  Starting: "workspaces.reason.startRequested",
  Stopping: "workspaces.reason.stopRequested",
  Deleting: "workspaces.reason.deleteRequested",
  SessionStarted: "workspaces.reason.sessionStarted",
  SessionTakenOver: "workspaces.reason.sessionTakenOver",
  StartTimeout: "workspaces.reason.bootDeadlineExceeded",
  RuntimeStartTimeout: "workspaces.reason.bootDeadlineExceeded",
  GuestAgentUnreachable: "workspaces.reason.guestAgentUnreachable",
};

// ---------------------------------------------------------------------------
// Reason params (B3-PARAMS)
// ---------------------------------------------------------------------------

/** The `params` string map events and conditions may carry. */
export type ReasonParams = Record<string, string> | undefined;

/**
 * How a param value is rendered for the active locale: `raw` passes
 * through, `int`/`durationSeconds`/`cpu`/`mib`/`gib` go through the shared
 * number and unit formatters, and `step`/`skipReason` map a machine token
 * to its own catalog text.
 */
export type ParamFormat =
  | "raw"
  | "int"
  | "durationSeconds"
  | "cpu"
  | "mib"
  | "gib"
  | "step"
  | "skipReason";

// Every condition-derived event params {condition, status} — the values
// that selected its curated message (internal/api/events.go
// conditionEvent). They sit beside each token's own params below.
const CONDITION_EVENT_PARAMS: Readonly<Record<string, ParamFormat>> = {
  condition: "raw",
  status: "raw",
};

// The token-specific params each emit site produces — the same values the
// server interpolated into the English message (or the event id). The
// catalog completeness test asserts every template placeholder resolves
// against this table plus CONDITION_EVENT_PARAMS.
const REASON_PARAM_TABLE: Readonly<Record<string, Readonly<Record<string, ParamFormat>>>> = {
  // API-curated lifecycle events: revision is the intent revision the
  // event id embeds; cause is the broker-recorded stop reason.
  Created: { revision: "int" },
  StartRequested: { revision: "int" },
  StopRequested: { revision: "int", cause: "raw" },
  IdleTimeout: { revision: "int", cause: "raw" },
  DisconnectTimeout: { revision: "int", cause: "raw" },
  MaxDurationReached: { revision: "int", cause: "raw" },
  DeleteRequested: { revision: "int" },
  TemplateUpdateSkipped: { revision: "int", skipReason: "skipReason" },

  // Teardown: markStep marks name their step already; the blocked-step
  // marks (CleanupRetry/RetentionPending/StreamDraining) carry it as a
  // param. The drain conditions also report the drain budget.
  BlockingConnects: { step: "step" },
  RevokingLeases: { step: "step" },
  DrainingStreams: { step: "step" },
  StoppingRuntime: { step: "step" },
  ApplyingRetention: { step: "step" },
  CleaningUp: { step: "step" },
  CleanupRetry: { step: "step" },
  RetentionPending: { step: "step" },
  StreamDraining: { step: "step", budgetSeconds: "durationSeconds" },
  DrainTimedOut: { step: "step", budgetSeconds: "durationSeconds" },

  // Intent-fence drift: the revisions the IntentBehind condition reports.
  IntentBehind: { crRevision: "int", rowRevision: "int" },

  // Mock/demo tokens whose fixture messages interpolate resource sizes.
  QuotaReserved: { cpuMillicores: "cpu", memoryMiB: "mib", storageGiB: "gib" },
  VolumeBound: { sizeGiB: "gib" },
};

/** The declared param keys (and formats) a reason token may carry. */
export function reasonParamFormats(
  token: string,
): Readonly<Record<string, ParamFormat>> {
  return { ...CONDITION_EVENT_PARAMS, ...REASON_PARAM_TABLE[token] };
}

// FinalizerStep tokens ("block-connects", ...) → catalog text, used when a
// template embeds {step}. Unknown tokens render verbatim — never dropped.
const STEP_TEXT: Readonly<Record<string, MessageKey>> = {
  "block-connects": "workspaces.reason.param.step.blockConnects",
  "revoke-leases": "workspaces.reason.param.step.revokeLeases",
  "drain-streams": "workspaces.reason.param.step.drainStreams",
  "stop-runtime": "workspaces.reason.param.step.stopRuntime",
  retention: "workspaces.reason.param.step.retention",
  cleanup: "workspaces.reason.param.step.cleanup",
};

// Recorded guard-skip tokens (provisioning.SkipReason*) → catalog text.
const SKIP_REASON_TEXT: Readonly<Record<string, MessageKey>> = {
  "runtime-changed": "workspaces.reason.param.skipReason.runtimeChanged",
  "experience-changed": "workspaces.reason.param.skipReason.experienceChanged",
  "data-policy-changed": "workspaces.reason.param.skipReason.dataPolicyChanged",
  "storage-smaller": "workspaces.reason.param.skipReason.storageSmaller",
};

// formatReasonParam renders one param value for the active locale. A
// number that does not parse and a token without a catalog entry render
// raw — the placeholder still fills with readable text.
export function formatReasonParam(format: ParamFormat, value: string): string {
  const num = Number(value);
  switch (format) {
    case "int":
      return Number.isFinite(num) ? formatNumber(num) : value;
    case "durationSeconds":
      if (!Number.isFinite(num) || num <= 0) return value;
      // Sub-minute budgets deserve seconds; formatDuration floors to min.
      if (num < 60) return t("templates.format.duration.s", { n: num });
      return formatDuration(num);
    case "cpu":
      return Number.isFinite(num) ? formatCpu(num) : value;
    case "mib":
      return Number.isFinite(num) ? formatMemory(num) : value;
    case "gib":
      return Number.isFinite(num) ? formatStorage(num) : value;
    case "step": {
      const key = STEP_TEXT[value];
      return key ? t(key) : value;
    }
    case "skipReason": {
      const key = SKIP_REASON_TEXT[value];
      return key ? t(key) : value;
    }
    default:
      return value;
  }
}

/**
 * The message for a reason token: its catalog template with `params`
 * formatted per their declared kind. Values are substituted as text —
 * React escapes them on render, so a hostile param can never inject
 * markup. When a template placeholder has no matching param (an older
 * server never sent it) or the token is unknown, returns the server's own
 * message — or the raw token when there is none — exactly as v0.3.x did.
 */
export function formatReasonMessage(
  reason: string,
  params: ReasonParams,
  detail?: string,
): string {
  const key = REASON_TEXT[reason];
  if (key === undefined) return detail || reason;
  let formatted: Record<string, string> | undefined;
  if (params) {
    const decl = reasonParamFormats(reason);
    formatted = {};
    for (const [name, value] of Object.entries(params)) {
      formatted[name] = formatReasonParam(decl[name] ?? "raw", value);
    }
  }
  try {
    return t(key, formatted);
  } catch {
    // A placeholder without a param: the server message is the
    // complete-if-English rendering of the same fact.
    return detail || reason;
  }
}

/** Every reason token with a catalog template (completeness test seam). */
export const REASON_TOKENS: readonly string[] = Object.keys(REASON_TEXT);

/** Catalog key for a reason token; undefined for a token we do not know. */
export function reasonMessageKey(reason: string): MessageKey | undefined {
  return REASON_TEXT[reason];
}

/** Localized text for a reason token; the raw token when it is unknown. */
export function reasonText(reason: string, params?: ReasonParams): string {
  return formatReasonMessage(reason, params);
}

/**
 * The message cell for an event or condition row: localized text for a
 * known reason, keeping the server message as secondary detail only when
 * it adds information the reason text does not already carry; the server
 * message (or the bare token) unchanged for unknown reasons and missing
 * params.
 */
export function ReasonText({
  reason,
  detail,
  params,
}: {
  reason: string;
  detail?: string | undefined;
  params?: ReasonParams;
}) {
  const key = REASON_TEXT[reason];
  if (key === undefined) return <>{detail || reason}</>;
  const text = formatReasonMessage(reason, params, detail);
  if (!detail || detail === text) return <>{text}</>;
  return (
    <>
      {text}{" "}
      <span className="tc-field__hint" title={detail}>
        {detail}
      </span>
    </>
  );
}
