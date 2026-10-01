import { useState } from "react";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { newIdempotencyKey, unwrap } from "../api/client";
import type { WorkspaceView } from "./helpers";
import { ErrorBanner } from "./ErrorBanner";

// Delete ≠ Purge: deleting a workspace frees the runtime and handles data per
// its dataPolicy (Retain moves the disk to the retained inventory, Ephemeral
// destroys it). Purging retained disks is a separate flow on /data.
export function DeleteWorkspaceButton({
  workspace,
  onDeleted,
}: {
  workspace: WorkspaceView;
  onDeleted: () => void;
}) {
  const api = useApi();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  async function doDelete() {
    setBusy(true);
    setError(null);
    try {
      unwrap(
        await api.DELETE("/v1/workspaces/{workspaceId}", {
          params: {
            path: { workspaceId: workspace.id },
            header: { "Idempotency-Key": newIdempotencyKey() },
          },
        }),
      );
      onDeleted();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  }

  return (
    <span>
      <button type="button" className="danger" onClick={() => setConfirming(true)}>
        {t("workspaces.delete.action")}
      </button>
      {confirming ? (
        <div
          role="dialog"
          aria-label={t("workspaces.delete.label")}
          className="dialog"
        >
          <p>
            {t("workspaces.delete.confirm", { name: workspace.name })}{" "}
            {workspace.dataPolicy === "Retain"
              ? t("workspaces.delete.dataRetain")
              : t("workspaces.delete.dataEphemeral")}
          </p>
          <ErrorBanner error={error} onDismiss={() => setError(null)} />
          <button
            type="button"
            className="danger"
            disabled={busy}
            onClick={() => void doDelete()}
          >
            {busy
              ? t("workspaces.delete.confirming")
              : t("workspaces.delete.confirmButton")}
          </button>
          <button type="button" disabled={busy} onClick={() => setConfirming(false)}>
            {t("common.cancel")}
          </button>
        </div>
      ) : null}
    </span>
  );
}
