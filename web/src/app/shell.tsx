import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import {
  Alert,
  cx,
  EmptyState,
  IconCheck,
  IconLaptop,
  IconMoon,
  IconSun,
  IconButton,
  Menu,
  Spinner,
} from "../design";
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

const THEME_OPTIONS: { pref: ThemePreference; label: string; icon: ReactNode }[] = [
  { pref: "light", label: "Light", icon: <IconSun /> },
  { pref: "dark", label: "Dark", icon: <IconMoon /> },
  { pref: "system", label: "System", icon: <IconLaptop /> },
];

function ThemeMenu() {
  const { preference, resolved, setPreference } = useTheme();
  return (
    <Menu
      label="Theme"
      align="end"
      trigger={(props) => (
        <IconButton
          {...props}
          size="sm"
          label="Theme"
          icon={resolved === "dark" ? <IconMoon /> : <IconSun />}
        />
      )}
      items={THEME_OPTIONS.map((o) => ({
        id: o.pref,
        label: o.label,
        icon: preference === o.pref ? <IconCheck /> : o.icon,
        onSelect: () => setPreference(o.pref),
      }))}
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
      <Alert tone="danger" title="This page could not be loaded">
        Reload the page to try again.
      </Alert>
    );
  } else if (current === null) {
    content = (
      <EmptyState
        title="Page not found"
        description="The address does not match any page in this portal."
        action={<Link to="/workspaces">Go to workspaces</Link>}
      />
    );
  } else {
    content = current.route.render(current.params);
  }

  return (
    <div className={cx("tc-shell", bleed && "tc-shell--bleed")}>
      <header className="tc-topbar">
        <Link to="/" className="tc-topbar__brand">
          {logoSrc ? <img className="tc-topbar__mark" src={logoSrc} alt="" /> : null}
          <span className="tc-topbar__name">{productName}</span>
          {showByline ? <span className="tc-topbar__byline">by TinyOrbit</span> : null}
        </Link>
        <div className="tc-topbar__actions">
          <ThemeMenu />
          {me ? <span className="tc-topbar__user">{me.displayName}</span> : null}
        </div>
      </header>
      <div className="tc-shell__body">
        {bleed ? null : (
          <nav className="tc-sidebar" aria-label="Sections">
            <ul className="tc-sidebar__list">
              <li>
                <NavLink to="/workspaces" activePrefixes={["/"]}>
                  Workspaces
                </NavLink>
              </li>
              <li>
                <NavLink to="/templates">Templates</NavLink>
              </li>
              <li>
                <NavLink to="/data">Data</NavLink>
              </li>
              {isTenantAdmin(me) ? (
                <li>
                  <NavLink to="/admin">Admin</NavLink>
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
