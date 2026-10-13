import { useCallback, useMemo, useState } from "react";
import {
  Badge,
  Button,
  Card,
  DescriptionList,
  EmptyState,
  Input,
  Pagination,
  Section,
  Spinner,
  StatusPill,
  Table,
  useServerTable,
} from "../design";
import type { Column } from "../design/Table";
import { IconRefresh, IconSearch, IconUsers } from "../design/icons";
import { Link } from "../lib/router";
import { t } from "../i18n";
import type { ApiClient } from "../api/client";
import { useApi } from "../api/context";
import {
  listAdminUserLimitPage,
  scanAdminUserLimits,
  scanScopedData,
  scanScopedWorkspaces,
  type AdminUserLimitEntry,
  type ScopedRetainedData,
  type ScopedWorkspace,
} from "./api";
import { formatAge, formatDateTime } from "./format";
import { loadMe, useLoader, useMeLoaded } from "../app/me";
import { phaseLabelKey } from "../workspaces/helpers";
import { DataStateBadge } from "../data/DataListPage";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { AdminLayout } from "./AdminLayout";
import { LimitDialog } from "./UserLimitsSection";

// One tenant member's row on /admin/users: a per-user limits item (one per
// principal who holds workspaces or a limit override — it is not a tenant
// directory) plus that principal's retained-disk count.
export interface AdminUserRow {
  subject: string;
  displayName: string;
  running: number;
  // Stored override (null = inherits the tenant default).
  override: number | null;
  // Resolved limit; null renders as unlimited.
  effective: number | null;
  retainedDisks: number;
  // The user-limits item — also the edit target: its ownerRef
  // ("issuer|sub") addresses the override and its etag guards the write.
  limitsEntry: AdminUserLimitEntry;
}

export function diskCounts(disks: readonly ScopedRetainedData[]): Map<string, number> {
  const counts = new Map<string, number>();
  for (const d of disks) {
    const sub = d.owner?.subject;
    if (sub) counts.set(sub, (counts.get(sub) ?? 0) + 1);
  }
  return counts;
}

export function userRowOf(entry: AdminUserLimitEntry, disks: ReadonlyMap<string, number>): AdminUserRow {
  return {
    subject: entry.subject,
    displayName: entry.displayName || entry.subject,
    running: entry.running,
    override: entry.limit,
    effective: entry.effective,
    retainedDisks: disks.get(entry.subject) ?? 0,
    limitsEntry: entry,
  };
}

export function matchesUserFilter(row: AdminUserRow, text: string): boolean {
  const q = text.trim().toLowerCase();
  if (!q) return true;
  return [row.displayName, row.subject].some((v) => v.toLowerCase().includes(q));
}

export function userDetailPath(subject: string): string {
  return `/admin/users/${encodeURIComponent(subject)}`;
}

// Per-user disk counts need the whole tenant's retained-data list; the
// user rows themselves page on the server.
async function loadDiskCounts(api: ApiClient): Promise<Map<string, number>> {
  return diskCounts(await scanScopedData(api, "tenant"));
}

function OverrideCell({ row }: { row: AdminUserRow }) {
  if (row.override == null) return <Badge tone="neutral">{t("admin.userLimits.inherit")}</Badge>;
  return <>{row.override}</>;
}

function EffectiveCell({ row }: { row: AdminUserRow }) {
  return <>{row.effective == null ? t("admin.userLimits.unlimited") : row.effective}</>;
}

// The collection sorts on ownerRef only (the `issuer|sub` reference).
const USER_SORT_FIELDS = { user: "ownerRef" } as const;

export function UsersAdminPage() {
  const api = useApi();
  const me = useMeLoaded();
  const tenant = me.data?.tenant;
  const disks = useLoader(loadDiskCounts, "admin-user-disks");
  const [filter, setFilter] = useState("");
  const load = useCallback(
    (cursor?: string, sort?: string) =>
      tenant === undefined
        ? Promise.resolve({ items: [] as AdminUserLimitEntry[] })
        : listAdminUserLimitPage(api, tenant, cursor, sort),
    [api, tenant],
  );
  const list = useServerTable(load, {
    fields: USER_SORT_FIELDS,
    initial: { key: "user", direction: "ascending" },
    key: tenant,
  });

  // The search narrows the rows loaded so far; "Load more" brings in the
  // next server page.
  const rows = useMemo(() => {
    const counts = disks.data ?? new Map<string, number>();
    return list.items.map((e) => userRowOf(e, counts)).filter((r) => matchesUserFilter(r, filter));
  }, [list.items, disks.data, filter]);

  const columns: Column<AdminUserRow>[] = [
    {
      key: "user",
      header: t("admin.quota.column.user"),
      rowHeader: true,
      sortable: true,
      render: (r) => (
        <span className="tc-admin-cell-stack">
          <Link to={userDetailPath(r.subject)}>{r.displayName}</Link>
          <code className="tc-admin-muted">{r.subject}</code>
        </span>
      ),
    },
    {
      key: "running",
      header: t("admin.userLimits.column.running"),
      align: "end",
      render: (r) => r.running,
    },
    {
      key: "override",
      header: t("admin.userLimits.column.limit"),
      align: "end",
      hideOnMobile: true,
      render: (r) => <OverrideCell row={r} />,
    },
    {
      key: "effective",
      header: t("admin.userLimits.column.effective"),
      align: "end",
      render: (r) => <EffectiveCell row={r} />,
    },
    {
      key: "disks",
      header: t("admin.users.column.disks"),
      align: "end",
      hideOnMobile: true,
      render: (r) => (disks.data ? r.retainedDisks : "—"),
    },
  ];

  const reload = () => {
    list.refresh();
    disks.reload();
  };

  return (
    <AdminLayout
      title={t("admin.users.title")}
      description={t("admin.users.description")}
      actions={
        <Button icon={<IconRefresh />} onClick={reload} loading={list.loading && list.loaded}>
          {t("admin.action.refresh")}
        </Button>
      }
    >
      <Input
        label={t("admin.users.filter.label")}
        fieldClassName="tc-admin-toolbar"
        type="search"
        placeholder={t("admin.users.filter.placeholder")}
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
        icon={<IconSearch />}
        onClear={() => setFilter("")}
      />
      <p className="tc-admin-muted">{t("admin.users.note")}</p>
      <ApiErrorAlert error={me.error ?? list.error} onRetry={me.error ? me.reload : list.refresh} />
      <ApiErrorAlert error={disks.error} onRetry={disks.reload} />
      <Table
        caption={t("admin.users.caption")}
        columns={columns}
        rows={rows}
        rowKey={(r) => r.subject}
        sort={list.tableSort}
        onSortChange={list.onSortChange}
        loading={(tenant === undefined || !list.loaded) && !list.error && !me.error}
        density="compact"
        empty={filter ? t("admin.users.empty.filtered") : t("admin.users.empty.all")}
      />
      {filter && list.hasNext ? (
        <p className="tc-admin-muted" role="status">
          {t("common.filter.partial", { n: list.items.length })}
        </p>
      ) : null}
      <Pagination
        loaded={list.items.length}
        hasNext={list.hasNext}
        loading={list.loading}
        onLoadMore={list.next}
        label={t("common.pagination.label", { name: t("admin.users.caption") })}
      />
    </AdminLayout>
  );
}

// Per-user drill-down. A dedicated route (not a Drawer) because every other
// detail view in the portal is a page — /workspaces/:id, /data/:id — which
// keeps the user detail deep-linkable and inside the admin tab context.
interface UserDetailData {
  tenant: string;
  entry: AdminUserLimitEntry | undefined;
  disks: ScopedRetainedData[];
}

// The detail reads the whole per-user limits collection to find the
// principal (there is no single-item GET) and the tenant's retained disks
// for its own; the workspaces load separately.
async function loadUserDetail(api: ApiClient, subject: string): Promise<UserDetailData> {
  const me = await loadMe(api);
  const [entries, disks] = await Promise.all([
    scanAdminUserLimits(api, me.tenant),
    scanScopedData(api, "tenant"),
  ]);
  return {
    tenant: me.tenant,
    entry: entries.find((e) => e.subject === subject),
    disks: disks.filter((d) => d.owner?.subject === subject),
  };
}

export function UserDetailPage({ subject, now }: { subject: string; now?: number }) {
  const u = useLoader((a) => loadUserDetail(a, subject), `admin-user:${subject}`);
  const ws = useLoader((a) => scanScopedWorkspaces(a, "tenant"), "admin-user-ws");
  const [editing, setEditing] = useState(false);
  const ts = now ?? Date.now();

  const row = u.data?.entry ? userRowOf(u.data.entry, new Map([[subject, u.data.disks.length]])) : undefined;
  const workspaces = useMemo(
    () => (ws.data ?? []).filter((w) => w.owner?.subject === subject),
    [ws.data, subject],
  );
  const disks = u.data?.disks ?? [];

  const wsColumns: Column<ScopedWorkspace>[] = [
    {
      key: "name",
      header: t("admin.workspaces.column.workspace"),
      rowHeader: true,
      render: (w) => (
        <span className="tc-admin-cell-stack">
          <Link to={`/workspaces/${w.id}`}>{w.name}</Link>
          <code className="tc-admin-muted">{w.id}</code>
        </span>
      ),
    },
    {
      key: "template",
      header: t("admin.workspaces.column.template"),
      hideOnMobile: true,
      render: (w) => `${w.template.name} ${t("admin.templates.revision", { n: w.template.revision })}`,
    },
    {
      key: "phase",
      header: t("admin.workspaces.column.phase"),
      render: (w) => (
        <span className="tc-admin-cell-stack">
          <StatusPill phase={w.phase} label={t(phaseLabelKey(w.phase))} />
          {w.phase === "Failed" && w.failureReason ? (
            <span className="tc-admin-muted">{w.failureReason}</span>
          ) : null}
        </span>
      ),
    },
    {
      key: "age",
      header: t("admin.workspaces.column.age"),
      align: "end",
      hideOnMobile: true,
      render: (w) => (
        <time dateTime={w.createdAt} title={formatDateTime(w.createdAt)}>
          {formatAge(w.createdAt, ts)}
        </time>
      ),
    },
  ];

  const diskColumns: Column<ScopedRetainedData>[] = [
    {
      key: "id",
      header: t("data.list.col.id"),
      rowHeader: true,
      render: (d) => <Link to={`/data/${d.id}`}>{d.id}</Link>,
    },
    {
      key: "state",
      header: t("data.list.col.state"),
      render: (d) => <DataStateBadge state={d.state} />,
    },
    {
      key: "size",
      header: t("data.list.col.size"),
      align: "end",
      render: (d) => t("data.list.sizeGib", { n: d.sizeGib }),
    },
    {
      key: "source",
      header: t("data.list.col.source"),
      hideOnMobile: true,
      render: (d) => d.sourceWorkspaceName,
    },
    {
      key: "age",
      header: t("admin.data.column.age"),
      align: "end",
      hideOnMobile: true,
      render: (d) => (
        <time dateTime={d.retainedAt} title={formatDateTime(d.retainedAt)}>
          {formatAge(d.retainedAt, ts)}
        </time>
      ),
    },
  ];

  let body;
  if (u.data && !row) {
    body = (
      <EmptyState
        icon={<IconUsers size={32} />}
        title={t("admin.userDetail.notFound.title")}
        description={t("admin.userDetail.notFound.body")}
        action={<Link to="/admin/users">{t("admin.userDetail.notFound.back")}</Link>}
      />
    );
  } else if (!u.data) {
    body = u.loading ? (
      <Card>
        <Spinner label={t("admin.users.loading")} />
      </Card>
    ) : null;
  } else if (row) {
    body = (
      <>
        <Section
          title={t("admin.userDetail.limits.title")}
          actions={
            <Button size="sm" variant="secondary" onClick={() => setEditing(true)}>
              {t("admin.userLimits.edit.action")}
            </Button>
          }
        >
          <DescriptionList
            items={[
              { term: t("admin.userDetail.field.subject"), detail: <code>{row.subject}</code> },
              {
                term: t("admin.userDetail.field.ownerRef"),
                detail: <code>{row.limitsEntry.ownerRef}</code>,
              },
              { term: t("admin.userLimits.column.running"), detail: String(row.running) },
              { term: t("admin.userLimits.column.limit"), detail: <OverrideCell row={row} /> },
              {
                term: t("admin.userLimits.column.effective"),
                detail: <EffectiveCell row={row} />,
              },
            ]}
          />
        </Section>
        <Section title={t("admin.userDetail.workspaces.title")}>
          <Table
            caption={t("admin.userDetail.workspaces.caption")}
            columns={wsColumns}
            rows={workspaces}
            rowKey={(w) => w.id}
            loading={ws.loading && !ws.data}
            density="compact"
            empty={t("admin.userDetail.workspaces.empty")}
          />
        </Section>
        <Section
          title={t("admin.userDetail.disks.title")}
          actions={<Link to="/admin/data">{t("admin.userDetail.disks.all")}</Link>}
        >
          <Table
            caption={t("admin.userDetail.disks.caption")}
            columns={diskColumns}
            rows={disks}
            rowKey={(d) => d.id}
            density="compact"
            empty={t("admin.userDetail.disks.empty")}
          />
        </Section>
        {editing && u.data ? (
          <LimitDialog
            tenant={u.data.tenant}
            target={row.limitsEntry}
            onClose={() => setEditing(false)}
            onSaved={u.reload}
          />
        ) : null}
      </>
    );
  }

  return (
    <AdminLayout
      title={row?.displayName ?? t("admin.userDetail.titleFallback")}
      description={<code className="tc-admin-muted">{subject}</code>}
    >
      <ApiErrorAlert error={u.error} onRetry={u.reload} />
      <ApiErrorAlert error={ws.error} onRetry={ws.reload} />
      {body}
    </AdminLayout>
  );
}
