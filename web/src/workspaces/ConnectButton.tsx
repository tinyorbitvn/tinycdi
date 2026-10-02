import { buttonClass } from "../design";
import { t } from "../i18n";
import { Link } from "../lib/router";
import type { WorkspaceView } from "./helpers";
import { sessionPath } from "./helpers";

// Connect opens the in-portal session view at /workspaces/:id/session. The
// ticket request, takeover prompt and frame launch all live there (T2.5) —
// this button is pure navigation so it works without minting a ticket first.
export function ConnectButton({
  workspace,
  disabled,
}: {
  workspace: WorkspaceView;
  disabled?: boolean;
}) {
  const to = sessionPath(workspace.id);
  if (disabled) {
    // Links cannot be disabled; render an inert button so the disabled state
    // is announced and the control keeps one slot in the toolbar.
    return (
      <button type="button" className={buttonClass("primary")} disabled>
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
