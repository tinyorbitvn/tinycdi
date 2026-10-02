import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import {
  Alert,
  Button,
  cx,
  EmptyState,
  IconCheck,
  IconChevronDown,
  IconLaptop,
  IconLogOut,
  IconMoon,
  IconSun,
  IconUser,
  IconButton,
  Menu,
  Spinner,
  useToast,
} from "../design";
import { useApi } from "../api/context";
import { signOut } from "../auth/signOut";
import { Link, navigate, NavLink, usePathname } from "./router";
import {
  areaFor,
  loadArea,
  resolveRoute,
  ROUTE_AREAS,
  type ResolvedRoute,
  type RouteArea,
} from "./routes";
import { DEFAULT_BRANDING, loadBranding, type Branding } from "./branding";
import { isTenantAdmin, useMe, type Me } from "./me";
import { useTheme, type ThemePreference } from "./theme";
import { t, type MessageKey } from "../i18n";

// --- branding -------------------------------------------------------------

const BrandingContext = createContext<Branding | null>(null);

export function BrandingProvider({
  children,
  load = loadBranding,
}: {
  children: ReactNode;
  /** Injectable for tests. */
  load?: () => Promise<Branding>;
}) {
  const [branding, setBranding] = useState<Branding | null>(null);
  useEffect(() => {
    let cancelled = false;
    load().then((b) => !cancelled && setBranding(b));
    return () => {
      cancelled = true;
    };
  }, [load]);
  return <BrandingContext.Provider value={branding}>{children}</BrandingContext.Provider>;
}

/** Loaded branding, or null while /branding/branding.json is in flight. */
export function useBranding(): Branding | null {
  return useContext(BrandingContext);
}

// --- theme menu ------------------------------------------------------------

const THEME_OPTIONS: { pref: ThemePreference; label: MessageKey; icon: ReactNode }[] = [
  { pref: "light", label: "app.theme.light", icon: <IconSun /> },
  { pref: "dark", label: "app.theme.dark", icon: <IconMoon /> },
  { pref: "system", label: "app.theme.system", icon: <IconLaptop /> },
];

function ThemeMenu() {
  const { preference, resolved, setPreference } = useTheme();
  return (
    <Menu
      label={t("app.theme.label")}
      align="end"
      trigger={(props) => (
        <IconButton
          {...props}
          size="sm"
          label={t("app.theme.label")}
          icon={resolved === "dark" ? <IconMoon /> : <IconSun />}
        />
      )}
      items={THEME_OPTIONS.map((o) => ({
        id: o.pref,
        label: t(o.label),
        icon: preference === o.pref ? <IconCheck /> : o.icon,
        onSelect: () => setPreference(o.pref),
      }))}
    />
  );
}

// --- user menu --------------------------------------------------------------

function UserMenu({ me }: { me: Me }) {
  const api = useApi();
  const { toast } = useToast();
  const [signingOut, setSigningOut] = useState(false);
  const onSignOut = () => {
    if (signingOut) return;
    setSigningOut(true);
    signOut(api).catch(() => {
      setSigningOut(false);
      toast({
        tone: "danger",
        title: t("app.user.signOutFailed.title"),
        description: t("app.user.signOutFailed.body"),
      });
    });
  };
  return (
    <Menu
      label={t("app.user.menu", { name: me.displayName })}
      align="end"
      header={
        <div className="tc-topbar__identity">
          <span>{t("app.user.signedInAs", { name: me.displayName })}</span>
          {me.tenant ? <span className="tc-topbar__tenant">{t("app.user.tenant", { tenant: me.tenant })}</span> : null}
        </div>
      }
      trigger={(props) => (
        <Button
          {...props}
          variant="ghost"
          size="sm"
          icon={<IconUser />}
          iconEnd={<IconChevronDown />}
          loading={signingOut}
        >
          <span className="tc-topbar__user">{me.displayName}</span>
        </Button>
      )}
      items={[{ id: "sign-out", label: t("app.user.signOut"), icon: <IconLogOut />, onSelect: onSignOut }]}
    />
  );
}

// --- shell -----------------------------------------------------------------

type RouteResult = ResolvedRoute | "error" | null;

function routeTitle(result: RouteResult): string | undefined {
  if (!result || result === "error" || !result.route.title) return undefined;
  const t = result.route.title;
  return typeof t === "function" ? t(result.params) : t;
}

export function AppShell({ areas = ROUTE_AREAS }: { areas?: RouteArea[] }) {
  const pathname = usePathname();
  const meState = useMe();
  const branding = useBranding();
  const { resolved: resolvedTheme } = useTheme();
  const area = areaFor(pathname, areas);

  // `resolved.key === pathname` once the owning area module answered.
  const [resolved, setResolved] = useState<{ key: string; result: RouteResult }>({
    key: "",
    result: null,
  });
  useEffect(() => {
    let cancelled = false;
    loadArea(area).then(
      (mod) => !cancelled && setResolved({ key: pathname, result: resolveRoute(mod.routes, pathname) }),
      () => !cancelled && setResolved({ key: pathname, result: "error" }),
    );
    return () => {
      cancelled = true;
    };
  }, [area, pathname]);
  const current: RouteResult | undefined = resolved.key === pathname ? resolved.result : undefined;

  const route = current && current !== "error" ? current.route : null;
  const productName = branding?.productName ?? DEFAULT_BRANDING.productName;

  useEffect(() => {
    const t = routeTitle(current ?? null);
    document.title = t ? `${t} · ${productName}` : productName;
  }, [current, productName]);

  const me: Me | null = meState.status === "ready" ? meState.me : null;
  const needsAdmin = route?.requires === "tenant-admin";
  const denied = needsAdmin && meState.status !== "loading" && !isTenantAdmin(me);
  useEffect(() => {
    if (denied) navigate("/workspaces", { replace: true });
  }, [denied]);

  const bleed = route?.layout === "bleed";
  const logoSrc = branding
    ? resolvedTheme === "dark" && branding.logoDark
      ? branding.logoDark
      : branding.logo
    : null;
  const showByline = productName === DEFAULT_BRANDING.productName;

  let content: ReactNode;
  if (current === undefined || (needsAdmin && meState.status === "loading") || denied) {
    content = <Spinner className="tc-content__loading" />;
  } else if (current === "error") {
    content = (
      <Alert tone="danger" title={t("app.loadError.title")}>
        {t("app.loadError.body")}
      </Alert>
    );
  } else if (current === null) {
    content = (
      <EmptyState
        title={t("app.notFound.title")}
        description={t("app.notFound.body", { path: pathname })}
        action={<Link to="/workspaces">{t("app.notFound.action")}</Link>}
      />
    );
  } else {
    content = current.route.render(current.params);
  }

  return (
    <div className={cx("tc-shell", bleed && "tc-shell--bleed")}>
      <header className="tc-topbar">
        <Link to="/" className="tc-topbar__brand">
          {logoSrc ? <img className="tc-topbar__mark" src={logoSrc} alt={""} /> : null}
          <span className="tc-topbar__name">{productName}</span>
          {showByline ? <span className="tc-topbar__byline">{t("app.shell.byline")}</span> : null}
        </Link>
        <div className="tc-topbar__actions">
          <ThemeMenu />
          {me ? <UserMenu me={me} /> : null}
        </div>
      </header>
      <div className="tc-shell__body">
        {bleed ? null : (
          <nav className="tc-sidebar" aria-label={t("nav.sections")}>
            <ul className="tc-sidebar__list">
              <li>
                <NavLink to="/workspaces" activePrefixes={["/"]}>
                  {t("nav.workspaces")}
                </NavLink>
              </li>
              <li>
                <NavLink to="/templates">{t("nav.templates")}</NavLink>
              </li>
              <li>
                <NavLink to="/data">{t("nav.data")}</NavLink>
              </li>
              {isTenantAdmin(me) ? (
                <li>
                  <NavLink to="/admin">{t("nav.admin")}</NavLink>
                </li>
              ) : null}
            </ul>
          </nav>
        )}
        <main className={cx("tc-content", bleed && "tc-content--bleed")}>{content}</main>
      </div>
    </div>
  );
}
