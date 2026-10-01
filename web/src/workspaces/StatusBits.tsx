import { t } from "../i18n";
import type { WorkspaceView } from "./helpers";

export function PhaseBadge({ phase }: { phase: WorkspaceView["phase"] }) {
  return <span className={`phase phase-${phase.toLowerCase()}`}>{phase}</span>;
}

export function ConditionsTable({ workspace }: { workspace: WorkspaceView }) {
  if (workspace.conditions.length === 0) {
    return <p>{t("workspaces.conditions.empty")}</p>;
  }
  return (
    <table aria-label={t("workspaces.conditions.label")}>
      <thead>
        <tr>
          <th>{t("workspaces.conditions.col.type")}</th>
          <th>{t("workspaces.conditions.col.status")}</th>
          <th>{t("workspaces.conditions.col.reason")}</th>
          <th>{t("workspaces.conditions.col.message")}</th>
          <th>{t("workspaces.conditions.col.since")}</th>
        </tr>
      </thead>
      <tbody>
        {workspace.conditions.map((c) => (
          <tr key={c.type}>
            <td>{c.type}</td>
            <td>{c.status}</td>
            <td>{c.reason}</td>
            <td>{c.message ?? ""}</td>
            <td>{new Date(c.lastTransitionTime).toLocaleString()}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
