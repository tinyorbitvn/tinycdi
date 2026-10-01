import { usePathname } from "./lib/router";
import { t } from "./i18n";
import { AuthGate } from "./auth/AuthGate";
import { CatalogPage } from "./workspaces/CatalogPage";
import { WorkspaceListPage } from "./workspaces/WorkspaceListPage";
import { WorkspaceDetailPage } from "./workspaces/WorkspaceDetailPage";
import { CreateWorkspacePage } from "./workspaces/CreateWorkspacePage";
import { DataPage } from "./workspaces/DataPage";

export function App() {
  const path = usePathname();

  let page: React.ReactNode;
  const wsMatch = path.match(/^\/workspaces\/(ws_[A-Za-z0-9]+)$/);
  if (path === "/" || path === "/workspaces") {
    page = <WorkspaceListPage />;
  } else if (path === "/templates") {
    page = <CatalogPage />;
  } else if (path === "/workspaces/new") {
    page = <CreateWorkspacePage />;
  } else if (path === "/data") {
    page = <DataPage />;
  } else if (wsMatch) {
    page = <WorkspaceDetailPage workspaceId={wsMatch[1]} />;
  } else {
    page = (
      <main>
        <h1>{t("app.notFound.title")}</h1>
        <p>{t("app.notFound.body", { path })}</p>
      </main>
    );
  }

  return <AuthGate>{page}</AuthGate>;
}
