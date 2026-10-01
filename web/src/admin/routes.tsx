import type { AppRoute } from "../app/route-types";
import { EmptyState, Page } from "../design";
import { t } from "../i18n";

// Stub created by the app shell (T3.2); owned by the admin area — replace
// freely, keeping the `routes` export.
export const routes: AppRoute[] = [
  {
    path: "/admin/*",
    title: "Admin",
    requires: "tenant-admin",
    render: () => (
      <Page title={t("nav.admin")}>
        <EmptyState title={t("nav.admin")} description={t("app.seed.admin.body")} />
      </Page>
    ),
  },
];
