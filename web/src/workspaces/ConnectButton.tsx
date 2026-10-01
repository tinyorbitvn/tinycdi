import { useState } from "react";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { unwrap } from "../api/client";
import { isPortalApiError } from "../api/errors";
import { useMe } from "../app/me";
import { launchInNewTab } from "../session/launch";
import type { components } from "../api/generated/schema";
import type { WorkspaceView } from "./helpers";
import { ErrorBanner } from "./ErrorBanner";

type LaunchTicket = components["schemas"]["LaunchTicket"];

// Opens the desktop session by POSTing the launch ticket to the
// workspace's own session host in a new tab (v0.2, D9). The ticket lives
// only in this form's POST body — it is never written to a URL, history
// entry, or web storage.
//
// SEC-26: the ticket is bearer-equivalent, so its destination is pinned to
// the session domain /v1/me published (see assertLaunchTarget). A launchUrl
// on any other host — or an absent configured domain — is refused rather
// than submitted.
export function launchSession(
  ticket: LaunchTicket,
  workspaceId: string,
  sessionDomain: string,
) {
  launchInNewTab(ticket, workspaceId, sessionDomain);
}

export function ConnectButton({
  workspace,
  disabled,
}: {
  workspace: WorkspaceView;
  disabled?: boolean;
}) {
  const api = useApi();
  const { me } = useMe();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [inUse, setInUse] = useState(false);

  async function connect(takeover: boolean) {
    setBusy(true);
    setError(null);
    try {
      const ticket = unwrap(
        await api.POST("/v1/workspaces/{workspaceId}/connections", {
          params: { path: { workspaceId: workspace.id } },
          body: { takeover },
        }),
      );
      setInUse(false);
      launchSession(ticket, workspace.id, me?.sessionDomain ?? "");
    } catch (e) {
      if (isPortalApiError(e) && e.code === "CONNECTION_IN_USE") {
        setInUse(true);
      } else {
        setError(e);
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <span className="connect-flow">
      <button
        type="button"
        disabled={disabled || busy || me === null}
        onClick={() => void connect(false)}
      >
        {busy
          ? t("workspaces.connect.connecting")
          : t("workspaces.connect.action")}
      </button>
      <ErrorBanner error={error} onDismiss={() => setError(null)} />
      {inUse ? (
        <div
          role="dialog"
          aria-label={t("workspaces.connect.inUse.label")}
          className="dialog"
        >
          <p>{t("workspaces.connect.inUse.body")}</p>
          <button
            type="button"
            disabled={busy}
            onClick={() => void connect(true)}
          >
            {t("workspaces.connect.inUse.takeover")}
          </button>
          <button type="button" disabled={busy} onClick={() => setInUse(false)}>
            {t("common.cancel")}
          </button>
        </div>
      ) : null}
    </span>
  );
}
