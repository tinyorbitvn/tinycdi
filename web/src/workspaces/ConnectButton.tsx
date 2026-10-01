import { useState } from "react";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { unwrap, getSessionOrigin } from "../api/client";
import { isPortalApiError } from "../api/errors";
import type { components } from "../api/generated/schema";
import type { WorkspaceView } from "./helpers";
import { ErrorBanner } from "./ErrorBanner";

type LaunchTicket = components["schemas"]["LaunchTicket"];

// Form field name carrying the ticket in the POST body to the session origin.
// Never a query parameter: the ticket must not appear in URLs, history,
// referers or logs (ADR 0001/0002).
export const TICKET_FIELD = "ticket";

// Opens the desktop session by POSTing the launch ticket to the session
// origin in a new tab. The ticket lives only in this form's POST body — it is
// never written to a URL, history entry, or web storage.
//
// SEC-26: the ticket is bearer-equivalent, so its destination is pinned to
// the session origin the API published in the login cookie. A launchUrl on
// any other origin — or an absent configured origin — is refused rather
// than submitted.
export function launchSession(ticket: LaunchTicket) {
  let origin: string;
  try {
    origin = new URL(ticket.launchUrl).origin;
  } catch {
    throw new Error("malformed launchUrl in connection response");
  }
  const expected = getSessionOrigin();
  if (expected === undefined || origin !== expected) {
    throw new Error(
      `refusing to POST launch ticket to ${origin}: not the configured session origin`,
    );
  }
  const form = document.createElement("form");
  form.method = "POST";
  form.action = ticket.launchUrl;
  form.target = "_blank";
  form.rel = "noopener";
  const input = document.createElement("input");
  input.type = "hidden";
  input.name = TICKET_FIELD;
  input.value = ticket.ticket;
  form.append(input);
  document.body.append(form);
  form.submit();
  form.remove();
}

export function ConnectButton({
  workspace,
  disabled,
}: {
  workspace: WorkspaceView;
  disabled?: boolean;
}) {
  const api = useApi();
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
      launchSession(ticket);
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
        disabled={disabled || busy}
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
