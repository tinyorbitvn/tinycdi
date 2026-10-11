import { useEffect, useRef, type ReactNode } from "react";
import { EmptyState, Page, Spinner, Tabs } from "../design";
import { IconShieldCheck } from "../design/icons";
import { Link, navigate, usePathname } from "../lib/router";
import { t, type MessageKey } from "../i18n";
import { isTenantAdmin, useMeLoaded } from "../app/me";
import { ApiErrorAlert } from "./ApiErrorAlert";

export const ADMIN_SECTIONS = [
  { path: "/admin", labelKey: "admin.nav.overview" },
  { path: "/admin/workspaces", labelKey: "admin.nav.workspaces" },
  { path: "/admin/quota", labelKey: "admin.nav.quota" },
  { path: "/admin/templates", labelKey: "admin.nav.templates" },
] as const satisfies readonly { path: string; labelKey: MessageKey }[];

// Design-system Tabs as the section switcher: roving tabindex, arrow-key
// activation navigates. The route — not local state — owns the selection,
// so a deep link highlights the right tab (longest-prefix wins: "/admin"
// must not eat "/admin/workspaces"). Every admin page remounts this nav, so
// a tab-initiated navigation sets a flag that puts focus back on the newly
// selected tab — otherwise it would die on <body>.
let focusSelectedTab = false;

function AdminNav() {
  const rootRef = useRef<HTMLDivElement>(null);
  const path = usePathname().replace(/\/+$/, "") || "/";
  const selected =
    [...ADMIN_SECTIONS]
      .sort((a, b) => b.path.length - a.path.length)
      .find((s) => path === s.path || path.startsWith(`${s.path}/`))?.path ??
    ADMIN_SECTIONS[0].path;
  // Mount only: the fresh page's nav restores focus to its selected tab
  // when the last navigation was tab-initiated.
  useEffect(() => {
    if (!focusSelectedTab) return;
    focusSelectedTab = false;
    rootRef.current
      ?.querySelector<HTMLElement>('[role="tab"][aria-selected="true"]')
      ?.focus();
  }, []);
  return (
    <div ref={rootRef}>
      <Tabs
        className="tc-admin-tabs"
        label={t("admin.nav.ariaLabel")}
        value={selected}
        onChange={(id) => {
          focusSelectedTab = true;
          navigate(id);
        }}
        tabs={ADMIN_SECTIONS.map((s) => ({ id: s.path, label: t(s.labelKey) }))}
      />
    </div>
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
        icon={<IconShieldCheck size={32} />}
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
