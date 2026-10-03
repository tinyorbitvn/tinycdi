import { Alert, Card, Grid, Section, Spinner, StatusPill } from "../design";
import type { ApiClient } from "../api/client";
import { Link } from "../lib/router";
import { t } from "../i18n";
import {
  fetchQuota,
  listScopedWorkspaces,
  type QuotaView,
  type ScopedWorkspace,
  type WorkspacePhase,
} from "./api";
import { formatAgo, formatDateTime, ownerLabel } from "./format";
import { useLoader } from "../app/me";
import { ApiErrorAlert } from "./ApiErrorAlert";
import { AdminLayout } from "./AdminLayout";
import { QuotaMeters } from "./QuotaPage";

const PHASE_ORDER: WorkspacePhase[] = [
  "Ready",
  "Provisioning",
  "Pending",
  "Stopping",
  "Stopped",
  "Failed",
  "Terminating",
];

export function phaseCounts(items: ScopedWorkspace[]): { phase: WorkspacePhase; count: number }[] {
  return PHASE_ORDER.map((phase) => ({
    phase,
    count: items.filter((w) => w.phase === phase).length,
  })).filter((p) => p.count > 0);
}

interface Overview {
  quota: QuotaView;
  workspaces: ScopedWorkspace[];
}

async function loadOverview(api: ApiClient): Promise<Overview> {
  const [quota, ws] = await Promise.all([fetchQuota(api), listScopedWorkspaces(api, "tenant")]);
  return { quota, workspaces: ws.items };
}

export function OverviewPage({ now }: { now?: number }) {
  const o = useLoader(loadOverview, "overview");
  const ts = now ?? Date.now();
  const failed = o.data?.workspaces.filter((w) => w.phase === "Failed") ?? [];
  return (
    <AdminLayout title={t("admin.overview.title")} description={t("admin.overview.description")}>
      <ApiErrorAlert error={o.error} onRetry={o.reload} />
      {!o.data ? (
        o.loading ? <Spinner label={t("admin.overview.loading")} /> : null
      ) : (
        <>
          <Section
            title={t("admin.overview.capacity")}
            actions={<Link to="/admin/quota">{t("admin.overview.usageByUser")}</Link>}
          >
            <QuotaMeters
              configured={o.data.quota.configured}
              limits={o.data.quota.limits}
              usage={o.data.quota.usage}
            />
          </Section>
          <Section
            title={t("admin.overview.byPhase")}
            actions={<Link to="/admin/workspaces">{t("admin.overview.allWorkspaces")}</Link>}
          >
            {o.data.workspaces.length === 0 ? (
              <p className="tc-admin-muted">{t("admin.overview.empty")}</p>
            ) : (
              <Grid gap={3} min="xs">
                {phaseCounts(o.data.workspaces).map(({ phase, count }) => (
                  <Card key={phase} as="div" className="tc-admin-stat">
                    <span className="tc-admin-stat__value">{count}</span>
                    <StatusPill phase={phase} />
                  </Card>
                ))}
              </Grid>
            )}
          </Section>
          {failed.length > 0 ? (
            <Alert
              tone="warning"
              title={t(failed.length === 1 ? "admin.overview.failed.one" : "admin.overview.failed.other", {
                n: failed.length,
              })}
            >
              <ul className="tc-admin-list">
                {failed.map((w) => (
                  <li key={w.id}>
                    <Link to={`/workspaces/${w.id}`}>{w.name}</Link> ({ownerLabel(w.owner)}){" "}
                    {w.failureReason ? <code>{w.failureReason}</code> : null}{" "}
                    <time
                      className="tc-admin-muted"
                      dateTime={w.updatedAt}
                      title={formatDateTime(w.updatedAt)}
                    >
                      {t("admin.overview.failedAgo", { age: formatAgo(w.updatedAt, ts) })}
                    </time>
                  </li>
                ))}
              </ul>
            </Alert>
          ) : null}
        </>
      )}
    </AdminLayout>
  );
}
