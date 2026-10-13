import { useEffect } from "react";
import type { AppRoute } from "../app/route-types";
import { navigate } from "../lib/router";
import { t } from "../i18n";
import { OverviewPage } from "./OverviewPage";
import { QuotaPage } from "./QuotaPage";
import { SessionPolicyPage } from "./SessionPolicyPage";
import { TemplatesPage } from "./TemplatesPage";
import { WorkspacesAdminPage } from "./WorkspacesAdminPage";
import { DataAdminPage } from "./DataAdminPage";
import { UserDetailPage, UsersAdminPage } from "./UsersAdminPage";
import { AuditPage } from "./AuditPage";
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
    path: "/admin/session-policy",
    title: t("admin.route.sessionPolicy"),
    requires: "tenant-admin",
    render: () => <SessionPolicyPage />,
  },
  {
    path: "/admin/templates",
    title: t("admin.route.templates"),
    requires: "tenant-admin",
    render: () => <TemplatesPage />,
  },
  {
    path: "/admin/data",
    title: t("admin.route.data"),
    requires: "tenant-admin",
    render: () => <DataAdminPage />,
  },
  {
    path: "/admin/users",
    title: t("admin.route.users"),
    requires: "tenant-admin",
    render: () => <UsersAdminPage />,
  },
  {
    path: "/admin/users/:subject",
    title: t("admin.route.user"),
    requires: "tenant-admin",
    render: (p) => <UserDetailPage subject={p.subject!} />,
  },
  {
    path: "/admin/audit",
    title: t("admin.route.audit"),
    requires: "tenant-admin",
    render: () => <AuditPage />,
  },
  {
    path: "/admin/*",
    title: t("admin.route.overview"),
    requires: "tenant-admin",
    render: () => <Redirect to="/admin" />,
  },
];
