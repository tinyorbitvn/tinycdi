import { useCallback, useEffect, useState } from "react";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { newIdempotencyKey, unwrap } from "../api/client";
import { navigate, Link } from "../lib/router";
import type { WorkspaceView } from "./helpers";
import { blockingCondition, isConnectable } from "./helpers";
import { ConditionsTable, PhaseBadge } from "./StatusBits";
import { ConnectButton } from "./ConnectButton";
import { ErrorBanner } from "./ErrorBanner";
import { DeleteWorkspaceButton } from "./DeleteWorkspaceButton";

const TERMINAL = new Set(["Stopped", "Failed"]);

export function WorkspaceDetailPage({
  workspaceId,
  pollIntervalMs = 2000,
}: {
  workspaceId: string;
  pollIntervalMs?: number;
}) {
  const api = useApi();
  const [workspace, setWorkspace] = useState<WorkspaceView | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const ws = unwrap(
        await api.GET("/v1/workspaces/{workspaceId}", {
          params: { path: { workspaceId } },
        }),
      );
      setWorkspace(ws);
      setError(null);
    } catch (e) {
      setError(e);
    }
  }, [api, workspaceId]);

  useEffect(() => {
    void refresh();
    const timer = setInterval(() => void refresh(), pollIntervalMs);
    return () => clearInterval(timer);
  }, [refresh, pollIntervalMs]);

  async function act(kind: "start" | "stop") {
    if (!workspace) return;
    setBusy(kind);
    setError(null);
    try {
      if (kind === "start") {
        unwrap(
          await api.POST("/v1/workspaces/{workspaceId}/start", {
            params: {
              path: { workspaceId: workspace.id },
              header: { "Idempotency-Key": newIdempotencyKey() },
            },
          }),
        );
      } else {
        unwrap(
          await api.POST("/v1/workspaces/{workspaceId}/stop", {
            params: { path: { workspaceId: workspace.id } },
          }),
        );
      }
      await refresh();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(null);
    }
  }

  if (!workspace) {
    return (
      <main aria-busy="true">
        <ErrorBanner error={error} onDismiss={() => setError(null)} />
        {error ? null : t("workspaces.detail.loading")}
      </main>
    );
  }

  const blocker = blockingCondition(workspace);
  const canStart = workspace.phase === "Stopped" || workspace.phase === "Failed";
  const canStop = workspace.desiredState === "Running" && !TERMINAL.has(workspace.phase);

  return (
    <main>
      <p>
        <Link to="/">{t("nav.allWorkspaces")}</Link>
      </p>
      <h1>
        {workspace.name} <PhaseBadge phase={workspace.phase} />
      </h1>
      <ErrorBanner error={error} onRetry={() => void refresh()} onDismiss={() => setError(null)} />
      <dl>
        <dt>{t("workspaces.detail.field.id")}</dt>
        <dd>{workspace.id}</dd>
        <dt>{t("workspaces.detail.field.template")}</dt>
        <dd>
          {t("workspaces.detail.template", {
            name: workspace.template.name,
            revision: workspace.template.revision,
            runtime: workspace.template.runtime,
            experience: workspace.template.experience,
          })}
        </dd>
        <dt>{t("workspaces.detail.field.desired")}</dt>
        <dd>{workspace.desiredState}</dd>
        <dt>{t("workspaces.detail.field.dataPolicy")}</dt>
        <dd>{workspace.dataPolicy}</dd>
        {workspace.failureReason ? (
          <>
            <dt>{t("workspaces.detail.field.failure")}</dt>
            <dd>{workspace.failureReason}</dd>
          </>
        ) : null}
        <dt>{t("workspaces.detail.field.created")}</dt>
        <dd>{new Date(workspace.createdAt).toLocaleString()}</dd>
        <dt>{t("workspaces.detail.field.updated")}</dt>
        <dd>{new Date(workspace.updatedAt).toLocaleString()}</dd>
      </dl>

      <h2>{t("workspaces.detail.conditions.title")}</h2>
      <ConditionsTable workspace={workspace} />

      <div className="actions">
        {canStart ? (
          <button
            type="button"
            disabled={busy !== null}
            onClick={() => void act("start")}
          >
            {busy === "start"
              ? t("workspaces.detail.action.starting")
              : workspace.phase === "Failed"
                ? t("workspaces.detail.action.retryStart")
                : t("workspaces.detail.action.start")}
          </button>
        ) : null}
        {canStop || workspace.phase === "Ready" || workspace.phase === "Provisioning" ? (
          <button
            type="button"
            disabled={busy !== null || workspace.desiredState === "Stopped"}
            onClick={() => void act("stop")}
          >
            {busy === "stop"
              ? t("workspaces.detail.action.stopping")
              : t("workspaces.detail.action.stop")}
          </button>
        ) : null}
        <ConnectButton workspace={workspace} disabled={!isConnectable(workspace)} />
        {blocker ? (
          <small aria-label={t("workspaces.detail.connectStatus")}>
            {t("workspaces.detail.connectUnavailable", { reason: blocker })}
          </small>
        ) : null}
        <DeleteWorkspaceButton
          workspace={workspace}
          onDeleted={() => navigate("/")}
        />
      </div>
    </main>
  );
}
