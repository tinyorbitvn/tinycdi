import { useCallback, useState } from "react";
import {
  Alert,
  Button,
  EmptyState,
  Pagination,
  Table,
  useServerTable,
  VisuallyHidden,
} from "../design";
import type { Column } from "../design/Table";
import { IconRefresh, IconTrash } from "../design/icons";
import { Link } from "../lib/router";
import { useApi } from "../api/context";
import { t } from "../i18n";
import { listScopedDataPage, type ScopedRetainedData } from "./api";
import { formatAge, formatDateTime, ownerLabel } from "./format";
import { canPurge, DataStateBadge, PURGING_POLL_MS } from "../data/DataListPage";
import { PurgeDialog } from "../data/PurgeDialog";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { AdminLayout } from "./AdminLayout";

// Columns the server sorts on (GET /v1/data `sort`); `name` is the source
// workspace name. The API already drops Purged records.
const ADMIN_DATA_SORT_FIELDS = { source: "name", age: "retainedAt" } as const;

export function DataAdminPage({ now, pollIntervalMs }: { now?: number; pollIntervalMs?: number }) {
  const api = useApi();
  const load = useCallback(
    (cursor?: string, sort?: string) => listScopedDataPage(api, "tenant", cursor, sort),
    [api],
  );
  // The API's newest-first order is the default sort (age descending).
  const list = useServerTable(load, {
    fields: ADMIN_DATA_SORT_FIELDS,
    initial: { key: "age", direction: "descending" },
    poll: (items) => (items.some((r) => r.state === "Purging") ? (pollIntervalMs ?? PURGING_POLL_MS) : null),
  });
  const [purging, setPurging] = useState<ScopedRetainedData | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const ts = now ?? Date.now();

  const columns: Column<ScopedRetainedData>[] = [
    {
      key: "id",
      header: t("data.list.col.id"),
      rowHeader: true,
      render: (r) => <Link to={`/data/${r.id}`}>{r.id}</Link>,
    },
    {
      key: "owner",
      header: t("data.list.col.owner"),
      render: (r) => ownerLabel(r.owner),
    },
    {
      key: "state",
      header: t("data.list.col.state"),
      render: (r) => <DataStateBadge state={r.state} />,
    },
    {
      key: "size",
      header: t("data.list.col.size"),
      align: "end",
      render: (r) => t("data.list.sizeGib", { n: r.sizeGib }),
    },
    {
      key: "source",
      header: t("data.list.col.source"),
      hideOnMobile: true,
      sortable: true,
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
      sortStart: "descending",
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

  function onPurged() {
    setPurging(null);
    setNotice(t("data.purge.scheduled"));
    // refresh() re-arms the stopped poll, so the now-Purging row is
    // followed until the sweep drops it from the list.
    list.refresh();
  }

  return (
    <AdminLayout
      title={t("admin.data.title")}
      description={t("admin.data.description")}
      actions={
        <Button icon={<IconRefresh />} onClick={list.refresh} loading={list.loading && list.loaded}>
          {t("admin.action.refresh")}
        </Button>
      }
    >
      {notice ? (
        <Alert tone="success" onDismiss={() => setNotice(null)}>
          {notice}
        </Alert>
      ) : null}
      <ApiErrorAlert error={list.error} onRetry={list.refresh} />
      <Table
        caption={t("admin.data.caption")}
        columns={columns}
        rows={list.items}
        rowKey={(r) => r.id}
        sort={list.tableSort}
        onSortChange={list.onSortChange}
        loading={!list.loaded}
        empty={
          <EmptyState
            title={t("data.list.empty.title")}
            description={t("data.list.empty.description")}
          />
        }
      />
      <Pagination
        loaded={list.items.length}
        hasNext={list.hasNext}
        loading={list.loading}
        onLoadMore={list.next}
        label={t("common.pagination.label", { name: t("admin.data.caption") })}
      />
      {purging ? (
        <PurgeDialog record={purging} onClose={() => setPurging(null)} onPurged={onPurged} />
      ) : null}
    </AdminLayout>
  );
}
