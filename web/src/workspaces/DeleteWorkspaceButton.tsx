import { useState } from "react";
import { Button, ConfirmDialog } from "../design";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { newIdempotencyKey, unwrap } from "../api/client";
import type { WorkspaceView } from "./helpers";
import { ErrorBanner } from "./ErrorBanner";

// Delete ≠ Purge: deleting a workspace frees the runtime and handles data per
// its dataPolicy (Retain moves the disk to the retained inventory, Ephemeral
// destroys it). The confirm dialog names that consequence explicitly.
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
    <>
      <Button variant="danger" onClick={() => setConfirming(true)}>
        {t("workspaces.delete.action")}
      </Button>
      <ConfirmDialog
        open={confirming}
        title={t("workspaces.delete.title", { name: workspace.name })}
        confirmLabel={busy ? t("workspaces.delete.confirming") : t("workspaces.delete.confirmButton")}
        cancelLabel={t("common.cancel")}
        busy={busy}
        onConfirm={() => void doDelete()}
        onCancel={() => setConfirming(false)}
      >
        <p>{t("workspaces.delete.confirm", { name: workspace.name })}</p>
        <p>
          {workspace.dataPolicy === "Retain"
            ? t("workspaces.delete.dataRetain")
            : t("workspaces.delete.dataEphemeral")}
        </p>
        <ErrorBanner error={error} onDismiss={() => setError(null)} />
      </ConfirmDialog>
    </>
  );
}
