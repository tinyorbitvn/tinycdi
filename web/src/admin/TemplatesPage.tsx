import { useCallback, useMemo } from "react";
import { Badge, Button, Pagination, Table, useServerTable } from "../design";
import type { Column } from "../design/Table";
import { IconRefresh } from "../design/icons";
import { useApi } from "../api/context";
import { t } from "../i18n";
import {
  clipboardPolicyLabel,
  dataPolicyLabel,
  experienceLabel,
  networkProfileLabel,
  runtimeLabel,
} from "../templates/format";
import {
  listAllScopedWorkspaces,
  listTemplatePageAdmin,
  type AdminTemplateView,
  type ScopedWorkspace,
  templateFamily,
} from "./api";
import { formatDateTime, formatQuota, formatSeconds } from "./format";
import { useLoader } from "../app/me";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { AdminLayout } from "./AdminLayout";

export interface CatalogRow {
  template: AdminTemplateView;
  inUse: number;
  running: number;
}

// engineLabel names a browser engine for the stale-image view; unknown
// engines keep their annotation key.
function engineLabel(name: string): string {
  switch (name) {
    case "chromium":
      return t("admin.templates.engine.chromium");
    case "firefox":
      return t("admin.templates.engine.firefox");
    default:
      return name;
  }
}

// formatEngines renders the image's engine versions — e.g.
// "Chromium 154.0.8037.92 · Firefox ESR 153.4.0esr" — or "" when absent.
function formatEngines(engines: Record<string, string> | undefined): string {
  if (!engines) return "";
  return Object.entries(engines)
    .map(([name, version]) => `${engineLabel(name)} ${version}`)
    .join(" · ");
}

// Joins the catalog with the tenant's workspaces so admins can see which
// templates are actually used before retiring one. Workspaces pin a template
// revision; the join is on the family, which survives a revision bump.
// Rows keep the server's template order.
export function catalogRows(
  templates: readonly AdminTemplateView[],
  workspaces: readonly ScopedWorkspace[],
): CatalogRow[] {
  return templates.map((template) => {
    const users = workspaces.filter(
      (w) => templateFamily(w.template) === templateFamily(template) && w.phase !== "Terminating",
    );
    return {
      template,
      inUse: users.length,
      running: users.filter((w) => w.desiredState === "Running").length,
    };
  });
}

// GET /v1/templates sorts on `name` only.
const TEMPLATE_SORT_FIELDS = { name: "name" } as const;

const columns: Column<CatalogRow>[] = [
  {
    key: "name",
    header: t("admin.templates.column.template"),
    rowHeader: true,
    sortable: true,
    render: ({ template: tpl }) => (
      <span className="tc-admin-cell-stack">
        <span>
          {tpl.name} <span className="tc-admin-muted">{t("admin.templates.revision", { n: tpl.revision })}</span>
        </span>
        {tpl.description ? <span className="tc-admin-muted">{tpl.description}</span> : null}
      </span>
    ),
  },
  {
    key: "kind",
    header: t("admin.templates.column.kind"),
    render: ({ template: tpl }) => (
      <span className="tc-admin-cell-stack">
        <span>{experienceLabel(tpl.experience)}</span>
        <span className="tc-admin-muted">{runtimeLabel(tpl.runtime)}</span>
      </span>
    ),
  },
  {
    key: "resources",
    header: t("admin.templates.column.resources"),
    hideOnMobile: true,
    render: ({ template: tpl }) =>
      `${formatQuota("cpuMillicores", tpl.resources.cpuMillicores)} · ${formatQuota("memoryMib", tpl.resources.memoryMib)} · ${formatQuota("storageGib", tpl.resources.storageGib)}`,
  },
  {
    key: "image",
    header: t("admin.templates.column.image"),
    hideOnMobile: true,
    render: ({ template: tpl }) => {
      const engines = formatEngines(tpl.imageEngines);
      if (!tpl.imageBuiltAt && engines === "") {
        return <span className="tc-admin-muted">{t("admin.templates.imageUnknown")}</span>;
      }
      return (
        <span className="tc-admin-cell-stack">
          {tpl.imageBuiltAt ? (
            <time dateTime={tpl.imageBuiltAt}>{formatDateTime(tpl.imageBuiltAt)}</time>
          ) : null}
          {engines !== "" ? <span className="tc-admin-muted">{engines}</span> : null}
          {tpl.imageBlocked === true ? (
            <span title={t("admin.templates.imageBlockedHint")}>
              <Badge tone="danger">{t("admin.templates.imageBlocked")}</Badge>
            </span>
          ) : tpl.imageStale === true ? (
            <span title={t("admin.templates.imageStaleHint")}>
              <Badge tone="warning">{t("admin.templates.imageStale")}</Badge>
            </span>
          ) : null}
        </span>
      );
    },
  },
  {
    key: "policy",
    header: t("admin.templates.column.policy"),
    hideOnMobile: true,
    render: ({ template: tpl }) => (
      <span className="tc-admin-badges">
        <Badge tone={tpl.dataPolicyDefault === "Retain" ? "info" : "neutral"}>
          {t("admin.templates.badge.data", { value: dataPolicyLabel(tpl.dataPolicyDefault) })}
        </Badge>
        <Badge tone={tpl.clipboardPolicy === "Disabled" ? "neutral" : "warning"}>
          {t("admin.templates.badge.clipboard", { value: clipboardPolicyLabel(tpl.clipboardPolicy) })}
        </Badge>
        {tpl.networkProfile ? (
          <Badge>{t("admin.templates.badge.network", { value: networkProfileLabel(tpl.networkProfile) })}</Badge>
        ) : null}
      </span>
    ),
  },
  {
    key: "lifecycle",
    header: t("admin.templates.column.lifecycle"),
    hideOnMobile: true,
    render: ({ template: tpl }) =>
      `${formatSeconds(tpl.lifecycleDefaults.idleTimeoutSeconds)} / ${formatSeconds(tpl.lifecycleDefaults.disconnectGraceSeconds)} / ${formatSeconds(tpl.lifecycleDefaults.maxRunningSeconds)}`,
  },
  {
    key: "usage",
    header: t("admin.templates.column.usage"),
    align: "end",
    render: (r) => (
      <span aria-label={t("admin.templates.usageLabel", { inUse: r.inUse, running: r.running })}>
        {r.inUse} <span className="tc-admin-muted">{t("admin.templates.usage.running", { n: r.running })}</span>
      </span>
    ),
  },
  {
    key: "published",
    header: t("admin.templates.column.published"),
    hideOnMobile: true,
    render: ({ template: tpl }) => <time dateTime={tpl.publishedAt}>{formatDateTime(tpl.publishedAt)}</time>,
  },
];

export function TemplatesPage() {
  const api = useApi();
  // The in-use counts need every tenant workspace; the catalog rows page.
  const workspaces = useLoader((a) => listAllScopedWorkspaces(a, "tenant"), "catalog-workspaces");
  const load = useCallback(
    (cursor?: string, sort?: string) => listTemplatePageAdmin(api, cursor, sort),
    [api],
  );
  const list = useServerTable(load, {
    fields: TEMPLATE_SORT_FIELDS,
    initial: { key: "name", direction: "ascending" },
  });
  const rows = useMemo(() => catalogRows(list.items, workspaces.data ?? []), [list.items, workspaces.data]);
  const reload = () => {
    list.refresh();
    workspaces.reload();
  };
  return (
    <AdminLayout
      title={t("admin.templates.title")}
      description={t("admin.templates.description")}
      actions={
        <Button icon={<IconRefresh />} onClick={reload} loading={list.loading && list.loaded}>
          {t("admin.action.refresh")}
        </Button>
      }
    >
      <ApiErrorAlert error={list.error} onRetry={list.refresh} />
      <ApiErrorAlert error={workspaces.error} onRetry={workspaces.reload} />
      <Table
        caption={t("admin.templates.caption")}
        columns={columns}
        rows={rows}
        rowKey={(r) => r.template.id}
        sort={list.tableSort}
        onSortChange={list.onSortChange}
        loading={!list.loaded && !list.error}
        empty={t("admin.templates.empty")}
      />
      <Pagination
        loaded={list.items.length}
        hasNext={list.hasNext}
        loading={list.loading}
        onLoadMore={list.next}
        label={t("common.pagination.label", { name: t("admin.templates.caption") })}
      />
    </AdminLayout>
  );
}
