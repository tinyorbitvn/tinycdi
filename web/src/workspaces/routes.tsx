import type { AppRoute } from "../app/route-types";
import { CatalogPage } from "./CatalogPage";
import { CreateWorkspacePage } from "./CreateWorkspacePage";
import { DataPage } from "./DataPage";
import { WorkspaceDetailPage } from "./WorkspaceDetailPage";
import { WorkspaceListPage } from "./WorkspaceListPage";

// Stub created by the app shell (T3.2); owned by the workspaces area — replace
// freely, keeping the `routes` export.
export const routes: AppRoute[] = [
  { path: "/", title: "Workspaces", render: () => <WorkspaceListPage /> },
  { path: "/workspaces", title: "Workspaces", render: () => <WorkspaceListPage /> },
  { path: "/workspaces/new", title: "New workspace", render: () => <CreateWorkspacePage /> },
  {
    path: "/workspaces/:id",
    title: "Workspace",
    render: (p) => <WorkspaceDetailPage workspaceId={p.id!} />,
  },
  { path: "/templates", title: "Templates", render: () => <CatalogPage /> },
  { path: "/data", title: "Data", render: () => <DataPage /> },
];
