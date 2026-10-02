import type { AppRoute } from "../app/route-types";
import { t } from "../i18n";
import { DataDetailPage } from "./DataDetailPage";
import { DataListPage } from "./DataListPage";

// Data area routes (T3.7): the retained-disk inventory and the per-record
// detail view. The app shell mounts this module for paths under /data.
export const routes: AppRoute[] = [
  { path: "/data", title: t("data.list.title"), render: () => <DataListPage /> },
  {
    path: "/data/:id",
    title: (p) => t("data.detail.title", { id: p.id ?? "" }),
    render: (p) => <DataDetailPage dataId={p.id!} />,
  },
];
