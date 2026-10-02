import { useEffect } from "react";
import type { AppRoute } from "../app/route-types";
import { navigate } from "../lib/router";
import { t } from "../i18n";
import { OverviewPage } from "./OverviewPage";
import { QuotaPage } from "./QuotaPage";
import { TemplatesPage } from "./TemplatesPage";
import { WorkspacesAdminPage } from "./WorkspacesAdminPage";
import "./admin.css";

function Redirect({ to }: { to: string }) {
  useEffect(() => {
    navigate(to, { replace: true });
  }, [to]);
  return null;
}

// Tenant-admin area. Every route carries `requires: "tenant-admin"` so the
// app shell hides the nav entry and gates the path for regular users; the
// AdminLayout additionally re-checks /v1/me before rendering tenant data.
export const routes: AppRoute[] = [
  {
    path: "/admin",
    title: t("admin.route.overview"),
    requires: "tenant-admin",
    render: () => <OverviewPage />,
  },
  {
    path: "/admin/workspaces",
    title: t("admin.route.workspaces"),
    requires: "tenant-admin",
    render: () => <WorkspacesAdminPage />,
  },
  {
    path: "/admin/quota",
    title: t("admin.route.quota"),
    requires: "tenant-admin",
    render: () => <QuotaPage />,
  },
  {
    path: "/admin/templates",
    title: t("admin.route.templates"),
    requires: "tenant-admin",
    render: () => <TemplatesPage />,
  },
  {
    path: "/admin/*",
    title: t("admin.route.overview"),
    requires: "tenant-admin",
    render: () => <Redirect to="/admin" />,
  },
];
