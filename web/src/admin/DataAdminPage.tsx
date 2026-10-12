import { useCallback, useMemo, useState } from "react";
import {
  Alert,
  Button,
  EmptyState,
  Pagination,
  Table,
  usePagination,
  useTableSort,
  VisuallyHidden,
} from "../design";
import type { Column } from "../design/Table";
import { IconRefresh, IconTrash } from "../design/icons";
import { Link } from "../lib/router";
import { useApi } from "../api/context";
import { useResource } from "../workspaces/resource";
import { t } from "../i18n";
import { listScopedData, type ScopedRetainedData } from "./api";
import { formatAge, formatDateTime, ownerLabel } from "./format";
import { canPurge, DataStateBadge, PURGING_POLL_MS } from "../data/DataListPage";
import { PurgeDialog } from "../data/PurgeDialog";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { AdminLayout } from "./AdminLayout";

// Rows for the tenant-wide retained-disk inventory: the API already drops
// Purged records; the client filter keeps the view honest against an older
// backend, and the newest disk surfaces first (retainedAt desc).
export function adminDataRows(items: ScopedRetainedData[]): ScopedRetainedData[] {
  return items
    .filter((r) => r.state !== "Purged")
    .sort((a, b) => b.retainedAt.localeCompare(a.retainedAt));
}

export function DataAdminPage({ now, pollIntervalMs }: { now?: number; pollIntervalMs?: number }) {
  const api = useApi();
  const load = useCallback(() => listScopedData(api, "tenant"), [api]);
  const list = useResource(load, (data) =>
    data?.items.some((r) => r.state === "Purging") ? (pollIntervalMs ?? PURGING_POLL_MS) : null,
  );
  const [purging, setPurging] = useState<ScopedRetainedData | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const ts = now ?? Date.now();
  const rows = useMemo(() => adminDataRows(list.data?.items ?? []), [list.data]);

  const columns: Column<ScopedRetainedData>[] = [
    {
      key: "id",
      header: t("data.list.col.id"),
      rowHeader: true,
      sortable: true,
      compare: (a, b) => a.id.localeCompare(b.id),
      render: (r) => <Link to={`/data/${r.id}`}>{r.id}</Link>,
    },
    {
      key: "owner",
      header: t("data.list.col.owner"),
      sortable: true,
      compare: (a, b) => ownerLabel(a.owner).localeCompare(ownerLabel(b.owner)),
      tiebreak: (a, b) => a.id.localeCompare(b.id),
      render: (r) => ownerLabel(r.owner),
    },
    {
      key: "state",
      header: t("data.list.col.state"),
      sortable: true,
      compare: (a, b) => a.state.localeCompare(b.state),
      tiebreak: (a, b) => a.id.localeCompare(b.id),
      render: (r) => <DataStateBadge state={r.state} />,
    },
    {
      key: "size",
      header: t("data.list.col.size"),
      align: "end",
      sortable: true,
      compare: (a, b) => a.sizeGib - b.sizeGib,
      tiebreak: (a, b) => a.id.localeCompare(b.id),
      render: (r) => t("data.list.sizeGib", { n: r.sizeGib }),
    },
    {
      key: "source",
      header: t("data.list.col.source"),
      hideOnMobile: true,
      sortable: true,
      compare: (a, b) => a.sourceWorkspaceName.localeCompare(b.sourceWorkspaceName),
      tiebreak: (a, b) => a.id.localeCompare(b.id),
      render: (r) => r.sourceWorkspaceName,
    },
    {
      key: "attached",
      header: t("data.detail.field.consuming"),
      hideOnMobile: true,
      render: (r) =>
        r.consumingWorkspaceId ? (
          <Link to={`/workspaces/${r.consumingWorkspaceId}`}>
            <code>{r.consumingWorkspaceId}</code>
          </Link>
        ) : null,
    },
    {
      key: "age",
      header: t("admin.data.column.age"),
      align: "end",
      hideOnMobile: true,
      sortable: true,
      compare: (a, b) => a.retainedAt.localeCompare(b.retainedAt),
      render: (r) => (
        <time dateTime={r.retainedAt} title={formatDateTime(r.retainedAt)}>
          {formatAge(r.retainedAt, ts)}
        </time>
      ),
    },
    {
      key: "actions",
      header: <VisuallyHidden>{t("data.list.col.actions")}</VisuallyHidden>,
      align: "end",
      render: (r) =>
        canPurge(r) ? (
          <Button
            size="sm"
            variant="danger"
            icon={<IconTrash />}
            aria-label={t("admin.data.action.purgeLabel", { id: r.id })}
            onClick={() => setPurging(r)}
          >
            {t("data.action.purge")}
          </Button>
        ) : null,
    },
  ];

  // The API's newest-first order is the default sort (age descending); the
  // page clamps itself when a purged row drops out of the list.
  const sort = useTableSort(columns, rows, { key: "age", direction: "descending" });
  const paged = usePagination(sort.sortedRows);

  function onPurged() {
    setPurging(null);
    setNotice(t("data.purge.scheduled"));
    // refresh() re-arms the stopped poll, so the now-Purging row is
    // followed until the sweep drops it from the list.
    void list.refresh();
  }

  return (
    <AdminLayout
      title={t("admin.data.title")}
      description={t("admin.data.description")}
      actions={
        <Button icon={<IconRefresh />} onClick={() => void list.refresh()} loading={list.loading && !!list.data}>
          {t("admin.action.refresh")}
        </Button>
      }
    >
      {notice ? (
        <Alert tone="success" onDismiss={() => setNotice(null)}>
          {notice}
        </Alert>
      ) : null}
      <ApiErrorAlert error={list.error} onRetry={() => void list.refresh()} />
      {list.data?.truncated ? (
        <Alert tone="warning">{t("data.list.truncated", { n: rows.length })}</Alert>
      ) : null}
      <Table
        caption={t("admin.data.caption")}
        columns={columns}
        rows={paged.pageRows}
        rowKey={(r) => r.id}
        sort={sort.sort}
        onSortChange={sort.onSortChange}
        loading={list.loading && !list.data}
        empty={
          <EmptyState
            title={t("data.list.empty.title")}
            description={t("data.list.empty.description")}
          />
        }
      />
      <Pagination {...paged.props} label={t("common.pagination.label", { name: t("admin.data.caption") })} />
      {purging ? (
        <PurgeDialog record={purging} onClose={() => setPurging(null)} onPurged={onPurged} />
      ) : null}
    </AdminLayout>
  );
}
