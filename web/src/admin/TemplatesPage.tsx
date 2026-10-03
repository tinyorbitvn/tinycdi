import { Badge, Button, Table } from "../design";
import type { Column } from "../design/Table";
import { IconRefresh } from "../design/icons";
import type { ApiClient } from "../api/client";
import { t } from "../i18n";
import { clipboardPolicyLabel } from "../templates/format";
import {
  listScopedWorkspaces,
  listTemplates,
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

// Joins the catalog with the tenant's workspaces so admins can see which
// templates are actually used before retiring one. Workspaces pin a template
// revision; the join is on the family, which survives a revision bump.
export function catalogRows(
  templates: AdminTemplateView[],
  workspaces: ScopedWorkspace[],
): CatalogRow[] {
  return templates
    .map((template) => {
      const users = workspaces.filter((w) => templateFamily(w.template) === templateFamily(template) && w.phase !== "Terminating");
      return {
        template,
        inUse: users.length,
        running: users.filter((w) => w.desiredState === "Running").length,
      };
    })
    .sort((a, b) => b.inUse - a.inUse || a.template.name.localeCompare(b.template.name));
}

async function loadCatalog(api: ApiClient): Promise<CatalogRow[]> {
  const [templates, workspaces] = await Promise.all([
    listTemplates(api),
    listScopedWorkspaces(api, "tenant"),
  ]);
  return catalogRows(templates.items, workspaces.items);
}

const columns: Column<CatalogRow>[] = [
  {
    key: "name",
    header: t("admin.templates.column.template"),
    rowHeader: true,
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
        <span>{tpl.experience}</span>
        <span className="tc-admin-muted">{tpl.runtime}</span>
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
    render: ({ template: tpl }) =>
      tpl.imageBuiltAt ? (
        <span className="tc-admin-cell-stack">
          <time dateTime={tpl.imageBuiltAt}>{formatDateTime(tpl.imageBuiltAt)}</time>
          {tpl.imageStale === true ? (
            <span title={t("admin.templates.imageStaleHint")}>
              <Badge tone="warning">{t("admin.templates.imageStale")}</Badge>
            </span>
          ) : null}
        </span>
      ) : (
        <span className="tc-admin-muted">{t("admin.templates.imageUnknown")}</span>
      ),
  },
  {
    key: "policy",
    header: t("admin.templates.column.policy"),
    hideOnMobile: true,
    render: ({ template: tpl }) => (
      <span className="tc-admin-badges">
        <Badge tone={tpl.dataPolicyDefault === "Retain" ? "info" : "neutral"}>
          {t("admin.templates.badge.data", { value: tpl.dataPolicyDefault })}
        </Badge>
        <Badge tone={tpl.clipboardPolicy === "Disabled" ? "neutral" : "warning"}>
          {t("admin.templates.badge.clipboard", { value: clipboardPolicyLabel(tpl.clipboardPolicy) })}
        </Badge>
        {tpl.networkProfile ? (
          <Badge>{t("admin.templates.badge.network", { value: tpl.networkProfile })}</Badge>
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
  const catalog = useLoader(loadCatalog, "catalog");
  return (
    <AdminLayout
      title={t("admin.templates.title")}
      description={t("admin.templates.description")}
      actions={
        <Button icon={<IconRefresh />} onClick={catalog.reload} loading={catalog.loading && !!catalog.data}>
          {t("admin.action.refresh")}
        </Button>
      }
    >
      <ApiErrorAlert error={catalog.error} onRetry={catalog.reload} />
      <Table
        caption={t("admin.templates.caption")}
        columns={columns}
        rows={catalog.data ?? []}
        rowKey={(r) => r.template.id}
        loading={catalog.loading && !catalog.data}
        empty={t("admin.templates.empty")}
      />
    </AdminLayout>
  );
}
