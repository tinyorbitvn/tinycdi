// Workspace reason tokens → portal copy (V3.14b). Events and conditions
// arrive with the server's English message text; the machine `reason` is
// the localizable part. A known token renders its catalog text and keeps
// the server message as secondary detail (the free text still carries the
// parameters — quota sizes, template refs — which are never parsed). An
// unknown token falls back to the server message, or the token itself.
//
// Token inventory: internal/api/events.go (intent + curated events),
// internal/operator/{status,workspace_controller,finalizer}.go (condition
// reasons), internal/runtime/linux/backend.go podReason (pod wait tokens),
// internal/api/statusview.go (StatusStale) — plus the demo/fixture tokens
// the mock API emits so dev-mode screens read the same way.

import { t, type MessageKey } from "../i18n";

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

/** Catalog key for a reason token; undefined for a token we do not know. */
export function reasonMessageKey(reason: string): MessageKey | undefined {
  return REASON_TEXT[reason];
}

/** Localized text for a reason token; the raw token when it is unknown. */
export function reasonText(reason: string): string {
  const key = REASON_TEXT[reason];
  return key !== undefined ? t(key) : reason;
}

/**
 * The message cell for an event or condition row: localized text for a
 * known reason, keeping the server message as secondary detail only when
 * it adds information the reason text does not already carry; the server
 * message (or the bare token) unchanged for unknown reasons.
 */
export function ReasonText({
  reason,
  detail,
}: {
  reason: string;
  detail?: string | undefined;
}) {
  const key = REASON_TEXT[reason];
  if (key === undefined) return <>{detail || reason}</>;
  const text = t(key);
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
