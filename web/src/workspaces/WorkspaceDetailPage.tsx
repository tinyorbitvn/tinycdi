import { useCallback, useEffect, useState } from "react";
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
    const t = setInterval(() => void refresh(), pollIntervalMs);
    return () => clearInterval(t);
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
        {error ? null : "Loading workspace…"}
      </main>
    );
  }

  const blocker = blockingCondition(workspace);
  const canStart = workspace.phase === "Stopped" || workspace.phase === "Failed";
  const canStop = workspace.desiredState === "Running" && !TERMINAL.has(workspace.phase);

  return (
    <main>
      <p>
        <Link to="/">← All workspaces</Link>
      </p>
      <h1>
        {workspace.name} <PhaseBadge phase={workspace.phase} />
      </h1>
      <ErrorBanner error={error} onRetry={() => void refresh()} onDismiss={() => setError(null)} />
      <dl>
        <dt>ID</dt>
        <dd>{workspace.id}</dd>
        <dt>Template</dt>
        <dd>
          {workspace.template.name}@{workspace.template.revision} (
          {workspace.template.runtime} / {workspace.template.experience})
        </dd>
        <dt>Desired state</dt>
        <dd>{workspace.desiredState}</dd>
        <dt>Data policy</dt>
        <dd>{workspace.dataPolicy}</dd>
        {workspace.failureReason ? (
          <>
            <dt>Failure</dt>
            <dd>{workspace.failureReason}</dd>
          </>
        ) : null}
        <dt>Created</dt>
        <dd>{new Date(workspace.createdAt).toLocaleString()}</dd>
        <dt>Updated</dt>
        <dd>{new Date(workspace.updatedAt).toLocaleString()}</dd>
      </dl>

      <h2>Conditions</h2>
      <ConditionsTable workspace={workspace} />

      <div className="actions">
        {canStart ? (
          <button
            type="button"
            disabled={busy !== null}
            onClick={() => void act("start")}
          >
            {busy === "start" ? "Starting…" : workspace.phase === "Failed" ? "Retry start" : "Start"}
          </button>
        ) : null}
        {canStop || workspace.phase === "Ready" || workspace.phase === "Provisioning" ? (
          <button
            type="button"
            disabled={busy !== null || workspace.desiredState === "Stopped"}
            onClick={() => void act("stop")}
          >
            {busy === "stop" ? "Stopping…" : "Stop"}
          </button>
        ) : null}
        <ConnectButton workspace={workspace} disabled={!isConnectable(workspace)} />
        {blocker ? <small aria-label="connect status">Connect unavailable: {blocker}</small> : null}
        <DeleteWorkspaceButton
          workspace={workspace}
          onDeleted={() => navigate("/")}
        />
      </div>
    </main>
  );
}
