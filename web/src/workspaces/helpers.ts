import { t } from "../i18n";
import type { components } from "../api/generated/schema";

export type WorkspaceView = components["schemas"]["WorkspaceView"];
export type TemplateView = components["schemas"]["TemplateView"];
export type RetainedDataView = components["schemas"]["RetainedDataView"];

// A workspace is connectable only when the phase says Ready AND the
// ConnectionReady condition confirms the streaming endpoint is usable —
// phase alone is a summary and can overstate readiness (design §5).
export function isConnectable(ws: WorkspaceView): boolean {
  if (ws.phase !== "Ready" || ws.desiredState !== "Running") return false;
  const conn = ws.conditions.find((c) => c.type === "ConnectionReady");
  return conn?.status === "True";
}

export function blockingCondition(ws: WorkspaceView): string | null {
  if (ws.phase !== "Ready") return null;
  if (ws.desiredState !== "Running") {
    return t("workspaces.detail.blocker.stopped");
  }
  const conn = ws.conditions.find((c) => c.type === "ConnectionReady");
  if (conn?.status !== "True") {
    return conn
      ? t("workspaces.detail.blocker.connectionReady", {
          status: conn.status,
          reason: conn.reason,
        })
      : t("workspaces.detail.blocker.noConnection");
  }
  return null;
}
