import { useMemo, useState } from "react";
import {
  Badge,
  Button,
  Card,
  DescriptionList,
  EmptyState,
  Input,
  Section,
  Spinner,
  StatusPill,
  Table,
} from "../design";
import type { Column } from "../design/Table";
import { IconRefresh, IconSearch, IconUsers } from "../design/icons";
import { Link } from "../lib/router";
import { t } from "../i18n";
import type { ApiClient } from "../api/client";
import {
  fetchAdminQuota,
  fetchAdminUserLimits,
  listScopedData,
  listScopedWorkspaces,
  type AdminUserLimitEntry,
  type AdminUserLimitsView,
  type AdminQuotaView,
  type QuotaAmounts,
  type ScopedRetainedData,
  type ScopedWorkspace,
  type UserUsage,
} from "./api";
import { formatAge, formatDateTime, formatQuota, quotaLabel } from "./format";
import { loadMe, useLoader } from "../app/me";
import { phaseLabelKey } from "../workspaces/helpers";
import { DataStateBadge } from "../data/DataListPage";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { AdminLayout } from "./AdminLayout";
import { UsageCell } from "./QuotaPage";
import { LimitDialog } from "./UserLimitsSection";

// One tenant member's row on /admin/users. The list is the union of the
// principals the quota view reports usage for and the per-user limit rows —
// it is not a tenant directory: a user who has never held a workspace,
// retained disk or limit override does not appear (see docs/releases).
export interface AdminUserRow {
  subject: string;
  displayName: string;
  usage: QuotaAmounts;
  running: number;
  // Stored override (null = inherits the tenant default); undefined when the
  // user-limits view returned no row for the principal, so "unknown" and
  // "inherits" never render alike.
  override: number | null | undefined;
  // Resolved limit — the limits row's effective, else the quota user's
  // effective `limit`; null/undefined render as unlimited.
  effective: number | null | undefined;
  retainedDisks: number;
  // The user-limits row when one exists — also the edit target: the PUT
  // needs its ownerRef ("issuer|sub"), which quota alone never exposes.
  limitsEntry?: AdminUserLimitEntry;
}

const ZERO_USAGE: QuotaAmounts = {
  workspaces: 0,
  runningWorkspaces: 0,
  cpuMillicores: 0,
  memoryMib: 0,
  storageGib: 0,
};

export function deriveUserRows(
  quotaUsers: readonly UserUsage[],
  limitRows: readonly AdminUserLimitEntry[],
  disks: readonly ScopedRetainedData[],
): AdminUserRow[] {
  const diskCount = new Map<string, number>();
  for (const d of disks) {
    if (d.state === "Purged") continue;
    const sub = d.owner?.subject;
    if (sub) diskCount.set(sub, (diskCount.get(sub) ?? 0) + 1);
  }
  const rows = new Map<string, AdminUserRow>();
  for (const u of quotaUsers) {
    rows.set(u.subject, {
      subject: u.subject,
      displayName: u.displayName || u.subject,
      usage: u.usage,
      running: u.usage.runningWorkspaces,
      override: undefined,
      effective: u.limit,
      retainedDisks: diskCount.get(u.subject) ?? 0,
    });
  }
  for (const l of limitRows) {
    const row = rows.get(l.subject);
    if (row) {
      row.override = l.limit;
      row.effective = l.effective;
      row.limitsEntry = l;
      if (row.displayName === row.subject) row.displayName = l.displayName || l.subject;
    } else {
      rows.set(l.subject, {
        subject: l.subject,
        displayName: l.displayName || l.subject,
        usage: { ...ZERO_USAGE },
        running: l.running,
        override: l.limit,
        effective: l.effective,
        retainedDisks: diskCount.get(l.subject) ?? 0,
        limitsEntry: l,
      });
    }
  }
  // Directory order: display name, then subject for a stable tiebreak.
  return [...rows.values()].sort(
    (a, b) => a.displayName.localeCompare(b.displayName) || a.subject.localeCompare(b.subject),
  );
}

export function matchesUserFilter(row: AdminUserRow, text: string): boolean {
  const q = text.trim().toLowerCase();
  if (!q) return true;
  return [row.displayName, row.subject].some((v) => v.toLowerCase().includes(q));
}

export function userDetailPath(subject: string): string {
  return `/admin/users/${encodeURIComponent(subject)}`;
}

interface UsersData {
  tenant: string;
  quota: AdminQuotaView;
  limits: AdminUserLimitsView;
  disks: ScopedRetainedData[];
}

// The page's three reads — quota (usage rows), user-limits (limit rows) and
// the tenant-scope retained-data list (per-user disk counts) — are all
// tenant-admin only, matching the page's own gate.
async function loadUsersData(api: ApiClient): Promise<UsersData> {
  const me = await loadMe(api);
  const [quota, limits, data] = await Promise.all([
    fetchAdminQuota(api, me.tenant),
    fetchAdminUserLimits(api, me.tenant),
    listScopedData(api, "tenant"),
  ]);
  return { tenant: me.tenant, quota, limits, disks: data.items };
}

function OverrideCell({ row }: { row: AdminUserRow }) {
  if (row.limitsEntry === undefined) return <span className="tc-admin-muted">—</span>;
  if (row.override == null) return <Badge tone="neutral">{t("admin.userLimits.inherit")}</Badge>;
  return <>{row.override}</>;
}

function EffectiveCell({ row }: { row: AdminUserRow }) {
  return <>{row.effective == null ? t("admin.userLimits.unlimited") : row.effective}</>;
}

// The usage vector mirrors QuotaPage: used/limit against the tenant limits,
// with the same warn/at-limit badges.
const USAGE_VECTOR_KEYS = ["cpuMillicores", "memoryMib", "storageGib"] as const;

export function UsersAdminPage() {
  const u = useLoader(loadUsersData, "admin-users");
  const [filter, setFilter] = useState("");

  const allRows = useMemo(
    () => (u.data ? deriveUserRows(u.data.quota.users, u.data.limits.users, u.data.disks) : []),
    [u.data],
  );
  const rows = useMemo(
    () => allRows.filter((r) => matchesUserFilter(r, filter)),
    [allRows, filter],
  );

  const columns: Column<AdminUserRow>[] = [
    {
      key: "user",
      header: t("admin.quota.column.user"),
      rowHeader: true,
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
    ...USAGE_VECTOR_KEYS.map(
      (k): Column<AdminUserRow> => ({
        key: k,
        header: quotaLabel(k),
        align: "end",
        hideOnMobile: k !== "storageGib",
        render: (r) => (
          <UsageCell k={k} value={r.usage[k]} limit={u.data?.quota.limits?.[k]} />
        ),
      }),
    ),
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
      render: (r) => r.retainedDisks,
    },
  ];

  return (
    <AdminLayout
      title={t("admin.users.title")}
      description={t("admin.users.description")}
      actions={
        <Button icon={<IconRefresh />} onClick={u.reload} loading={u.loading && !!u.data}>
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
      <ApiErrorAlert error={u.error} onRetry={u.reload} />
      <Table
        caption={t("admin.users.caption")}
        columns={columns}
        rows={rows}
        rowKey={(r) => r.subject}
        loading={u.loading && !u.data}
        density="compact"
        empty={filter ? t("admin.users.empty.filtered") : t("admin.users.empty.all")}
      />
      <p className="tc-admin-muted" aria-live="polite">
        {u.data ? t("admin.users.shown", { shown: rows.length, total: allRows.length }) : ""}
      </p>
    </AdminLayout>
  );
}

// Per-user drill-down. A dedicated route (not a Drawer) because every other
// detail view in the portal is a page — /workspaces/:id, /data/:id — which
// keeps the user detail deep-linkable and inside the admin tab context.
export function UserDetailPage({ subject, now }: { subject: string; now?: number }) {
  const u = useLoader(loadUsersData, "admin-users");
  const ws = useLoader((a) => listScopedWorkspaces(a, "tenant"), "admin-user-ws");
  const [editing, setEditing] = useState(false);
  const ts = now ?? Date.now();

  const rows = useMemo(
    () => (u.data ? deriveUserRows(u.data.quota.users, u.data.limits.users, u.data.disks) : []),
    [u.data],
  );
  const row = rows.find((r) => r.subject === subject);
  const workspaces = useMemo(
    () => (ws.data?.items ?? []).filter((w) => w.owner?.subject === subject),
    [ws.data, subject],
  );
  const disks = useMemo(
    () => (u.data?.disks ?? []).filter((d) => d.state !== "Purged" && d.owner?.subject === subject),
    [u.data, subject],
  );

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
    const usageText = USAGE_VECTOR_KEYS.map(
      (k) => `${quotaLabel(k)} ${formatQuota(k, row.usage[k])}`,
    ).join(" · ");
    body = (
      <>
        <Section
          title={t("admin.userDetail.limits.title")}
          actions={
            row.limitsEntry ? (
              <Button size="sm" variant="secondary" onClick={() => setEditing(true)}>
                {t("admin.userLimits.edit.action")}
              </Button>
            ) : undefined
          }
        >
          <DescriptionList
            items={[
              { term: t("admin.userDetail.field.subject"), detail: <code>{row.subject}</code> },
              ...(row.limitsEntry
                ? [
                    {
                      term: t("admin.userDetail.field.ownerRef"),
                      detail: <code>{row.limitsEntry.ownerRef}</code>,
                    },
                  ]
                : []),
              { term: t("admin.userLimits.column.running"), detail: String(row.running) },
              { term: t("admin.userDetail.field.usage"), detail: usageText },
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
        {editing && row.limitsEntry && u.data ? (
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
