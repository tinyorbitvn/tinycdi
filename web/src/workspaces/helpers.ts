import type { components } from "../api/generated/schema";
import { t, type MessageKey } from "../i18n";

type Schemas = components["schemas"];

export type Owner = Schemas["Owner"];
export type WorkspaceView = Schemas["WorkspaceView"];
export type WorkspacePhase = Schemas["WorkspacePhase"];
export type RetainedDataView = Schemas["RetainedDataView"];

/** GET /v1/workspaces/{id}/events item (newest first). */
export type WorkspaceEvent = Schemas["WorkspaceEvent"];

// A workspace is connectable only when the phase says Ready AND the
// ConnectionReady condition confirms the streaming endpoint is usable —
// phase alone is a summary and can overstate readiness (design §5).
export function isConnectable(ws: WorkspaceView): boolean {
  if (ws.phase !== "Ready" || ws.desiredState !== "Running") return false;
  const conn = ws.conditions.find((c) => c.type === "ConnectionReady");
  return conn?.status === "True";
}

/**
 * The "Desired" cell. A workspace being deleted has no desired state worth
 * showing (it will never run again), so it renders a dash instead of a stale
 * Running/Stopped value.
 */
const DESIRED_LABEL: Record<string, MessageKey> = {
  Running: "workspaces.desired.running",
  Stopped: "workspaces.desired.stopped",
};

export function desiredLabel(ws: WorkspaceView): string {
  if (ws.phase === "Terminating") return "—";
  const key = DESIRED_LABEL[ws.desiredState];
  return key ? t(key) : ws.desiredState;
}

export interface Blocker {
  key: MessageKey;
  params?: Record<string, string | number>;
}

/** Why a Ready-looking workspace cannot be connected yet (null = no block). */
export function blockingReason(ws: WorkspaceView): Blocker | null {
  if (ws.phase !== "Ready") return null;
  if (ws.desiredState !== "Running") return { key: "workspaces.detail.blocker.stopped" };
  const conn = ws.conditions.find((c) => c.type === "ConnectionReady");
  if (conn?.status !== "True") {
    return conn
      ? {
          key: "workspaces.detail.blocker.connectionReady",
          params: { status: conn.status, reason: conn.reason },
        }
      : { key: "workspaces.detail.blocker.noConnection" };
  }
  return null;
}

const PHASE_LABEL: Record<WorkspacePhase, MessageKey> = {
  Pending: "workspaces.phase.pending",
  Provisioning: "workspaces.phase.provisioning",
  Ready: "workspaces.phase.ready",
  Stopping: "workspaces.phase.stopping",
  Stopped: "workspaces.phase.stopped",
  Failed: "workspaces.phase.failed",
  Terminating: "workspaces.phase.terminating",
};

export function phaseLabelKey(phase: WorkspacePhase): MessageKey {
  return PHASE_LABEL[phase];
}

/** Portal route hosting the in-portal session view for a workspace. */
export function sessionPath(workspaceId: string): string {
  return `/workspaces/${encodeURIComponent(workspaceId)}/session`;
}

/**
 * "name@rev" for the template a workspace points at; revision 0 means "no
 * published revision yet" and is dropped rather than shown as "@0" (T5.4).
 */
export function templateRefLabel(template: WorkspaceView["template"]): string {
  return template.revision > 0 ? `${template.name}@${template.revision}` : template.name;
}
