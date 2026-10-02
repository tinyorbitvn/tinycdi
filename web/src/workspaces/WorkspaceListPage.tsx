import { useCallback } from "react";
import { buttonClass, EmptyState, Page, Spinner, StatusPill, Table } from "../design";
import type { Column } from "../design/Table";
import { IconGrid, IconPlus } from "../design/icons";
import { t } from "../i18n";
import { useApi } from "../api/context";
import { Link } from "../lib/router";
import { listWorkspaces } from "./api";
import type { WorkspaceView } from "./helpers";
import { isConnectable, phaseLabelKey } from "./helpers";
import { useResource } from "./resource";
import { ErrorBanner } from "./ErrorBanner";
import { dataPolicyLabel } from "../templates/format";

const BUSY_MS = 1500;
const IDLE_MS = 10_000;

/** Poll fast while any workspace is transitional, slow when all are settled. */
function pollDelay(items: WorkspaceView[] | undefined): number {
  const busy = (items ?? []).some(
    (w) => w.phase === "Pending" || w.phase === "Provisioning" || w.phase === "Stopping" || w.phase === "Terminating",
  );
  return busy ? BUSY_MS : IDLE_MS;
}

const COLUMNS: Column<WorkspaceView>[] = [
  {
    key: "name",
    header: t("workspaces.list.col.name"),
    rowHeader: true,
    render: (w) => <Link to={`/workspaces/${w.id}`}>{w.name}</Link>,
  },
  {
    key: "template",
    header: t("workspaces.list.col.template"),
    hideOnMobile: true,
    render: (w) => `${w.template.name}@${w.template.revision}`,
  },
  {
    key: "phase",
    header: t("workspaces.list.col.phase"),
    render: (w) => (
      <>
        <StatusPill phase={w.phase} label={t(phaseLabelKey(w.phase))} />{" "}
        {!isConnectable(w) && w.phase === "Ready" ? (
          <small>{t("workspaces.list.waitingConnection")}</small>
        ) : null}
      </>
    ),
  },
  {
    key: "desiredState",
    header: t("workspaces.list.col.desired"),
    hideOnMobile: true,
    render: (w) => w.desiredState,
  },
  {
    key: "dataPolicy",
    header: t("workspaces.list.col.dataPolicy"),
    hideOnMobile: true,
    render: (w) => dataPolicyLabel(w.dataPolicy),
  },
  {
    key: "manage",
    header: "",
    align: "end",
    render: (w) => <Link to={`/workspaces/${w.id}`}>{t("workspaces.list.manage")}</Link>,
  },
];

export function WorkspaceListPage({ pollIntervalMs }: { pollIntervalMs?: number }) {
  const api = useApi();
  const load = useCallback(() => listWorkspaces(api), [api]);
  const list = useResource(load, pollIntervalMs ?? pollDelay);

  const items = list.data ?? [];
  return (
    <Page
      title={t("workspaces.list.title")}
      actions={
        <>
          <Link to="/templates" className={buttonClass("secondary")}>
            <IconGrid size={16} /> {t("nav.templates")}
          </Link>
          <Link to="/workspaces/new" className={buttonClass("primary")}>
            <IconPlus size={16} /> {t("nav.newWorkspace")}
          </Link>
        </>
      }
    >
      <ErrorBanner error={list.error} onRetry={() => void list.refresh()} onDismiss={list.clearError} />
      {list.loading && !list.data ? (
        <Spinner label={t("workspaces.list.loading")} />
      ) : items.length === 0 ? (
        <EmptyState
          icon={<IconGrid size={24} />}
          title={t("workspaces.list.empty")}
          description={t("workspaces.list.emptyBody")}
          action={
            <Link to="/templates" className={buttonClass("primary")}>
              {t("workspaces.list.emptyCta")}
            </Link>
          }
        />
      ) : (
        <>
          <p className="tc-field__hint">{t("workspaces.list.count", { n: items.length })}</p>
          <Table
            columns={COLUMNS}
            rows={items}
            rowKey={(w) => w.id}
            caption={t("workspaces.list.label")}
            empty={t("workspaces.list.empty")}
          />
        </>
      )}
    </Page>
  );
}
