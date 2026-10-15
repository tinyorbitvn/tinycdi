import { Alert, Badge, Card, Grid, Section, Spinner, StatusPill } from "../design";
import type { ApiClient } from "../api/client";
import { Link } from "../lib/router";
import { t } from "../i18n";
import { phaseLabelKey } from "../workspaces/helpers";
import {
  fetchAdminOverview,
  fetchQuota,
  scanScopedData,
  scanScopedWorkspaces,
  type AdminOverview,
  type QuotaView,
  type ScopedRetainedData,
  type ScopedWorkspace,
  type WorkspacePhase,
} from "./api";
import { formatAgo, formatDateTime, ownerLabel } from "./format";
import { loadMe, useLoader } from "../app/me";
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
  retained: ScopedRetainedData[];
}

async function loadOverview(api: ApiClient): Promise<Overview> {
  const [quota, ws, data] = await Promise.all([
    fetchQuota(api),
    scanScopedWorkspaces(api, "tenant"),
    scanScopedData(api, "tenant"),
  ]);
  return { quota, workspaces: ws, retained: data };
}

// The health aggregate loads on its own loader: a failed overview call must
// not blank the capacity and phase sections, and each loader surfaces its
// own ApiErrorAlert.
async function loadHealth(api: ApiClient): Promise<AdminOverview> {
  const me = await loadMe(api);
  return fetchAdminOverview(api, me.tenant);
}

export function OverviewPage({ now }: { now?: number }) {
  const o = useLoader(loadOverview, "overview");
  const h = useLoader(loadHealth, "overview-health");
  const ts = now ?? Date.now();
  const failed = o.data?.workspaces.filter((w) => w.phase === "Failed") ?? [];
  return (
    <AdminLayout title={t("admin.overview.title")} description={t("admin.overview.description")}>
      <ApiErrorAlert error={o.error} onRetry={o.reload} />
      <ApiErrorAlert error={h.error} onRetry={h.reload} />
      {h.data ? (
        <Section title={t("admin.overview.health.title")}>
          {h.data.fresh === false ? (
            <Alert tone="warning">{t("admin.overview.health.stale")}</Alert>
          ) : null}
          <Grid gap={3} min="xs">
            <Card as="div" className="tc-admin-stat">
              <span className="tc-admin-stat__value">{h.data.liveSessions}</span>
              <span className="tc-admin-muted">{t("admin.overview.health.liveSessions")}</span>
            </Card>
            <Card as="div" className="tc-admin-stat">
              <span className="tc-admin-stat__value">
                {h.data.provisioning.p95Seconds === undefined
                  ? t("admin.overview.health.noSamples")
                  : `${Math.round(h.data.provisioning.p95Seconds)} s`}
              </span>
              <span className="tc-admin-muted">{t("admin.overview.health.p95")}</span>
              <span className="tc-admin-muted">
                {t("admin.overview.health.samples", { n: h.data.provisioning.samples })}
              </span>
            </Card>
            <Card as="div" className="tc-admin-stat">
              <span className="tc-admin-stat__value">{h.data.stuckFinalizers}</span>
              {h.data.stuckFinalizers > 0 ? (
                <Link to="/admin/workspaces">
                  <Badge tone="warning">{t("admin.overview.health.stuck")}</Badge>
                </Link>
              ) : (
                <span className="tc-admin-muted">{t("admin.overview.health.stuck")}</span>
              )}
            </Card>
            <Card as="div" className="tc-admin-stat">
              <span className="tc-admin-muted">{t("admin.overview.health.running")}</span>
              {h.data.runningByTemplate.length === 0 ? (
                <span className="tc-admin-muted">{t("admin.overview.health.noneRunning")}</span>
              ) : (
                <ul className="tc-admin-list">
                  {h.data.runningByTemplate.map((r) => (
                    <li key={r.template}>{`${r.template} — ${r.count}`}</li>
                  ))}
                </ul>
              )}
            </Card>
          </Grid>
        </Section>
      ) : h.loading ? (
        <Spinner label={t("admin.overview.loading")} />
      ) : null}
      {!o.data ? (
        o.loading ? <Spinner label={t("admin.overview.loading")} /> : null
      ) : (
        <>
          <Section
            title={t("admin.overview.capacity")}
            actions={<Link to="/admin/users">{t("admin.overview.usageByUser")}</Link>}
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
                    <StatusPill phase={phase} label={t(phaseLabelKey(phase))} />
                  </Card>
                ))}
              </Grid>
            )}
          </Section>
          <Section
            title={t("admin.overview.data.title")}
            actions={<Link to="/admin/data">{t("admin.overview.data.all")}</Link>}
          >
            <Card as="div" className="tc-admin-stat">
              <span className="tc-admin-stat__value">{o.data.retained.length}</span>
              <span className="tc-admin-muted">{t("admin.overview.data.disks")}</span>
            </Card>
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
