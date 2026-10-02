import type { AppRoute } from "../app/route-types";
import { t } from "../i18n";
import { CatalogPage } from "./CatalogPage";
import { CreateWorkspacePage } from "./CreateWorkspacePage";
import { WorkspaceDetailPage } from "./WorkspaceDetailPage";
import { WorkspaceListPage } from "./WorkspaceListPage";

// Workspace area routes (app-shell contract: web/src/app/route-types.ts).
// The /workspaces/:id/session path is claimed by the session area ahead of
// this catch-all area, so it must not appear here.
export const routes: AppRoute[] = [
  { path: "/", title: t("workspaces.list.title"), render: () => <WorkspaceListPage /> },
  { path: "/workspaces", title: t("workspaces.list.title"), render: () => <WorkspaceListPage /> },
  {
    path: "/workspaces/new",
    title: t("workspaces.create.title"),
    render: () => <CreateWorkspacePage />,
  },
  {
    path: "/workspaces/:id",
    title: t("workspaces.list.title"),
    render: (p) => <WorkspaceDetailPage workspaceId={p.id!} />,
  },
  { path: "/templates", title: t("templates.catalog.title"), render: () => <CatalogPage /> },
];
