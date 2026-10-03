import { buttonClass } from "../design";
import { t } from "../i18n";
import { Link } from "../lib/router";
import type { WorkspaceView } from "./helpers";
import { isConnectable, sessionPath } from "./helpers";

// Connect opens the in-portal session view at /workspaces/:id/session. The
// ticket request, takeover prompt and frame launch all live there (T2.5) —
// this button is pure navigation so it works without minting a ticket first.
//
// A workspace that isn't Ready can't take a session: the control stays in
// the toolbar aria-disabled with a tooltip saying why ("Starting…" /
// "Failed") — never `disabled`, which would swallow the tooltip and hide
// the state from assistive tech. The click is blocked, so no launch.
export function ConnectButton({ workspace }: { workspace: WorkspaceView }) {
  const to = sessionPath(workspace.id);
  if (!isConnectable(workspace)) {
    const tooltip =
      workspace.phase === "Failed"
        ? t("workspaces.connect.disabledFailed")
        : workspace.phase !== "Ready"
          ? t("workspaces.connect.disabledStarting")
          : undefined;
    return (
      <button
        type="button"
        className={buttonClass("primary")}
        aria-disabled="true"
        title={tooltip}
        onClick={(e) => e.preventDefault()}
      >
        {t("workspaces.connect.action")}
      </button>
    );
  }
  return (
    <Link to={to} className={buttonClass("primary")}>
      {t("workspaces.connect.action")}
    </Link>
  );
}
