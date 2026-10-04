import { StatusPill, Table } from "../design";
import type { Column } from "../design/Table";
import { t } from "../i18n";
import { formatDateTime } from "../templates/format";
import { phaseLabelKey } from "./helpers";
import { ReasonText } from "./reasons";
import type { WorkspaceView } from "./helpers";

type Condition = WorkspaceView["conditions"][number];

/** Phase pill with a catalog text label — status is never colour alone. */
export function PhasePill({ phase }: { phase: WorkspaceView["phase"] }) {
  return <StatusPill phase={phase} label={t(phaseLabelKey(phase))} />;
}

const COLUMNS: Column<Condition>[] = [
  { key: "type", header: t("workspaces.conditions.col.type"), rowHeader: true },
  { key: "status", header: t("workspaces.conditions.col.status") },
  { key: "reason", header: t("workspaces.conditions.col.reason") },
  {
    key: "message",
    header: t("workspaces.conditions.col.message"),
    render: (c) => <ReasonText reason={c.reason} detail={c.message} params={c.params} />,
  },
  {
    key: "lastTransitionTime",
    header: t("workspaces.conditions.col.since"),
    render: (c) => formatDateTime(c.lastTransitionTime),
  },
];

export function ConditionsTable({ workspace }: { workspace: WorkspaceView }) {
  return (
    <Table
      columns={COLUMNS}
      rows={workspace.conditions}
      rowKey={(c) => c.type}
      caption={t("workspaces.conditions.label")}
      empty={t("workspaces.conditions.empty")}
    />
  );
}
