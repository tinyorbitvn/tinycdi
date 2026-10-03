import { useEffect, type ReactNode } from "react";
import { EmptyState, Page, Spinner } from "../design";
import { IconShield } from "../design/icons";
import { Link, navigate, usePathname } from "../lib/router";
import { cx } from "../design/cx";
import { t, type MessageKey } from "../i18n";
import { isTenantAdmin, useMeLoaded } from "../app/me";
import { ApiErrorAlert } from "./ApiErrorAlert";

export const ADMIN_SECTIONS = [
  { path: "/admin", labelKey: "admin.nav.overview" },
  { path: "/admin/workspaces", labelKey: "admin.nav.workspaces" },
  { path: "/admin/quota", labelKey: "admin.nav.quota" },
  { path: "/admin/templates", labelKey: "admin.nav.templates" },
] as const satisfies readonly { path: string; labelKey: MessageKey }[];

function AdminNav() {
  const path = usePathname().replace(/\/+$/, "") || "/";
  return (
    <nav aria-label={t("admin.nav.ariaLabel")} className="tc-admin-nav">
      <ul>
        {ADMIN_SECTIONS.map((s) => {
          const current = path === s.path;
          return (
            <li key={s.path}>
              <Link
                to={s.path}
                className={cx("tc-admin-nav__link", current && "tc-admin-nav__link--current")}
                {...(current ? { "aria-current": "page" as const } : {})}
              >
                {t(s.labelKey)}
              </Link>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}

// The shell hides admin routes from non-admins; this layout re-checks so a
// deep link never renders tenant-wide data before /v1/me confirms the role
// (the API enforces it regardless: scope=tenant is 403 for non-admins).
// A confirmed non-admin is redirected to the workspace list; the empty state
// below is the fallback while that navigation lands.
export function AdminLayout({
  title,
  description,
  actions,
  children,
}: {
  title: string;
  description?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
}) {
  const me = useMeLoaded();
  const denied = me.data !== undefined && !isTenantAdmin(me.data);
  useEffect(() => {
    if (denied) navigate("/workspaces", { replace: true });
  }, [denied]);
  let body: ReactNode;
  if (me.loading && !me.data) {
    body = <Spinner label={t("admin.gate.checking")} />;
  } else if (me.error) {
    body = <ApiErrorAlert error={me.error} onRetry={me.reload} />;
  } else if (!isTenantAdmin(me.data)) {
    body = (
      <EmptyState
        icon={<IconShield size={32} />}
        title={t("admin.gate.title")}
        description={t("admin.gate.body")}
        action={<Link to="/">{t("admin.gate.back")}</Link>}
      />
    );
  } else {
    body = children;
  }
  return (
    <Page
      eyebrow={me.data?.tenant ? t("admin.eyebrow.tenant", { tenant: me.data.tenant }) : t("admin.eyebrow")}
      title={title}
      width="full"
      {...(description ? { description } : {})}
      {...(actions && isTenantAdmin(me.data) ? { actions } : {})}
    >
      <AdminNav />
      {body}
    </Page>
  );
}
