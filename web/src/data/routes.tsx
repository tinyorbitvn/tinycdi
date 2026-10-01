import type { AppRoute } from "../app/route-types";
import { DataPage } from "../workspaces/DataPage";

// Stub created by the app shell (T3.2); owned by the data area — replace
// freely, keeping the `routes` export. Until T3.7 builds the data module
// this delegates /data to the retained-data page living in workspaces/.
export const routes: AppRoute[] = [
  { path: "/data", title: "Retained data", render: () => <DataPage /> },
];
