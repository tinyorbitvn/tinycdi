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
  getAdminUser,
  listAdminUserPage,
  scanScopedData,
  scanScopedWorkspaces,
  type AdminTenantUser,
  type AdminUserLimitEntry,
  type ScopedRetainedData,
  type ScopedWorkspace,
} from "./api";
import { formatAge, formatAgo, formatDateTime } from "./format";
import { isPortalApiError } from "../api/errors";
import { loadMe, useLoader, useMeLoaded } from "../app/me";
import { phaseLabelKey } from "../workspaces/helpers";
import { DataStateBadge } from "../data/DataListPage";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { AdminLayout } from "./AdminLayout";
import { LimitDialog } from "./UserLimitsSection";
import { UserSessionsSection } from "./UserSessionsSection";

export interface AdminUserRow {
  subject: string;
  ownerRef: string;
  displayName: string;
  email: string;
  lastSeenAt: string;
  workspaces: number;
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

export function userRowOf(user: AdminTenantUser, disks: ReadonlyMap<string, number>): AdminUserRow {
  return {
    subject: user.subject,
    ownerRef: user.ownerRef,
    displayName: user.displayName || user.subject,
    email: user.email,
    lastSeenAt: user.lastSeenAt,
    workspaces: user.workspaces,
    running: user.limits.running,
    override: user.limits.limit,
    effective: user.limits.effective,
    retainedDisks: disks.get(user.subject) ?? 0,
    limitsEntry: user.limits,
  };
}

export function matchesUserFilter(row: AdminUserRow, text: string): boolean {
  const q = text.trim().toLowerCase();
  if (!q) return true;
  return [row.displayName, row.subject].some((v) => v.toLowerCase().includes(q));
}

export function userDetailPath(ownerRef: string): string {
  const bin = Array.from(new TextEncoder().encode(ownerRef), (b) => String.fromCharCode(b)).join("");
  const seg = btoa(bin).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
  return "/admin/users/" + seg;
}

// Inverse of the path segment userDetailPath builds; undefined for a segment that is not valid base64url UTF-8.
export function ownerRefFromParam(param: string): string | undefined {
  try {
    const bin = atob(param.replaceAll("-", "+").replaceAll("_", "/"));
    return new TextDecoder("utf-8", { fatal: true }).decode(Uint8Array.from(bin, (c) => c.charCodeAt(0)));
  } catch {
    return undefined;
  }
}

/** The issuer half of an `issuer|subject` owner reference ("—" when absent). */
function issuerOf(ownerRef: string): string {
  const cut = ownerRef.lastIndexOf("|");
  return cut > 0 ? ownerRef.slice(0, cut) : "—";
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

const USER_SORT_FIELDS = { user: "displayName", lastSeen: "lastSeen" } as const;

export function UsersAdminPage() {
  const api = useApi();
  const me = useMeLoaded();
  const tenant = me.data?.tenant;
  const disks = useLoader(loadDiskCounts, "admin-user-disks");
  const [filter, setFilter] = useState("");
  const load = useCallback(
    (cursor?: string, sort?: string) =>
      tenant === undefined
        ? Promise.resolve({ items: [] as AdminTenantUser[] })
        : listAdminUserPage(api, tenant, cursor, sort),
    [api, tenant],
  );
  const list = useServerTable(load, {
    fields: USER_SORT_FIELDS,
    initial: { key: "lastSeen", direction: "descending" },
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
          <Link to={userDetailPath(r.ownerRef)}>{r.displayName}</Link>
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
    {
      key: "lastSeen",
      header: t("admin.users.column.lastSeen"),
      sortable: true,
      hideOnMobile: true,
      render: (r) => (
        <time dateTime={r.lastSeenAt} title={formatDateTime(r.lastSeenAt)}>
          {formatAgo(r.lastSeenAt)}
        </time>
      ),
    },
    {
      key: "workspaces",
      header: t("admin.users.column.workspaces"),
      align: "end",
      hideOnMobile: true,
      render: (r) => r.workspaces,
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
        fieldClassName="tc-admin-toolbar tc-admin-filter"
        type="search"
        placeholder={t("admin.users.filter.placeholder")}
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
        icon={<IconSearch />}
        onClear={() => setFilter("")}
      />
      <ApiErrorAlert error={me.error ?? list.error} onRetry={me.error ? me.reload : list.refresh} />
      <ApiErrorAlert error={disks.error} onRetry={disks.reload} />
      <Table
        caption={t("admin.users.caption")}
        columns={columns}
        rows={rows}
        rowKey={(r) => r.ownerRef}
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
  user: AdminTenantUser | undefined;
  disks: ScopedRetainedData[];
}

// The directory answers 404 for a principal who never signed in; that is the
// not-found state, not an error alert.
async function loadUserDetail(api: ApiClient, ownerRef: string): Promise<UserDetailData> {
  if (ownerRef === "") return { tenant: "", user: undefined, disks: [] };
  const me = await loadMe(api);
  const [user, disks] = await Promise.all([
    getAdminUser(api, me.tenant, ownerRef).catch((e: unknown) => {
      if (isPortalApiError(e) && e.httpStatus === 404) return undefined;
      throw e;
    }),
    scanScopedData(api, "tenant"),
  ]);
  return { tenant: me.tenant, user, disks: user ? disks.filter((d) => d.owner?.subject === user.subject) : [] };
}

export function UserDetailPage({ ownerRef, now }: { ownerRef: string; now?: number }) {
  const u = useLoader((a) => loadUserDetail(a, ownerRef), `admin-user:${ownerRef}`);
  const ws = useLoader((a) => scanScopedWorkspaces(a, "tenant"), "admin-user-ws");
  const [editing, setEditing] = useState(false);
  const ts = now ?? Date.now();

  const row = u.data?.user ? userRowOf(u.data.user, new Map([[u.data.user.subject, u.data.disks.length]])) : undefined;
  const workspaces = useMemo(
    () => (ws.data ?? []).filter((w) => w.owner?.subject === u.data?.user?.subject),
    [ws.data, u.data],
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
        <Section title={t("admin.userDetail.identity.title")}>
          <DescriptionList
            items={[
              { term: t("admin.userDetail.field.issuer"), detail: <code>{issuerOf(row.limitsEntry.ownerRef)}</code> },
              { term: t("admin.userDetail.field.subject"), detail: <code>{row.subject}</code> },
              {
                term: t("admin.userDetail.field.ownerRef"),
                detail: <code>{row.limitsEntry.ownerRef}</code>,
              },
              { term: t("admin.userDetail.field.email"), detail: row.email || "—" },
              {
                term: t("admin.userDetail.field.lastSeen"),
                detail: <time dateTime={row.lastSeenAt}>{formatDateTime(row.lastSeenAt)}</time>,
              },
            ]}
          />
        </Section>
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
        <UserSessionsSection tenant={u.data.tenant} ownerRef={row.ownerRef} displayName={row.displayName} />
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
      back={{ to: "/admin/users", label: t("admin.userDetail.notFound.back") }}
    >
      <ApiErrorAlert error={u.error} onRetry={u.reload} />
      <ApiErrorAlert error={ws.error} onRetry={ws.reload} />
      {body}
    </AdminLayout>
  );
}
