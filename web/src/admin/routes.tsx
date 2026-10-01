import type { AppRoute } from "../app/route-types";
import { EmptyState, Page } from "../design";

// Stub created by the app shell (c4); owned by the admin area — replace
// freely, keeping the `routes` export.
export const routes: AppRoute[] = [
  {
    path: "/admin/*",
    title: "Admin",
    requires: "tenant-admin",
    render: () => (
      <Page title="Admin">
        <EmptyState title="Admin views" description="Tenant administration is not built yet." />
      </Page>
    ),
  },
];
