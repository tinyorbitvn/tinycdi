import type { components } from "../api/generated/schema";
import type { MessageKey } from "../i18n";

type Schemas = components["schemas"];

export type Owner = Schemas["Owner"];
export type WorkspaceView = Schemas["WorkspaceView"];
export type WorkspacePhase = Schemas["WorkspacePhase"];
export type RetainedDataView = Schemas["RetainedDataView"];

/** GET /v1/workspaces/{id}/events item (newest first). */
// `id` (stable per condition type + reason) is a contract addition typed here
// until the generated schema carries it.
export type WorkspaceEvent = Schemas["WorkspaceEvent"] & { id?: string };

// A workspace is connectable only when the phase says Ready AND the
// ConnectionReady condition confirms the streaming endpoint is usable —
// phase alone is a summary and can overstate readiness (design §5).
export function isConnectable(ws: WorkspaceView): boolean {
  if (ws.phase !== "Ready" || ws.desiredState !== "Running") return false;
  const conn = ws.conditions.find((c) => c.type === "ConnectionReady");
  return conn?.status === "True";
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
