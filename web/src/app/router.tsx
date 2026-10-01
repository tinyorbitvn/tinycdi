import type { AnchorHTMLAttributes, ReactNode } from "react";
import { Link, navigate, usePathname } from "../lib/router";
import { cx } from "../design";
import type { RouteParams } from "./route-types";

export { Link, navigate, usePathname };

function trimSlash(p: string): string {
  return p.length > 1 && p.endsWith("/") ? p.slice(0, -1) : p;
}

/**
 * Matches `pathname` against a pattern (`/a/:id`, `/a/*`). Returns decoded
 * params or null. `/*` also matches the bare prefix (`/admin/*` ~ `/admin`).
 */
export function matchPath(pattern: string, pathname: string): RouteParams | null {
  const pat = trimSlash(pattern).split("/");
  const path = trimSlash(pathname).split("/");
  const params: Record<string, string> = {};
  for (let i = 0; i < pat.length; i++) {
    const seg = pat[i]!;
    if (seg === "*" && i === pat.length - 1) {
      params["*"] = path.slice(i).join("/");
      return params;
    }
    const value = path[i];
    if (value === undefined) return null;
    if (seg.startsWith(":")) {
      if (value === "") return null;
      try {
        params[seg.slice(1)] = decodeURIComponent(value);
      } catch {
        return null;
      }
    } else if (seg !== value) {
      return null;
    }
  }
  return path.length === pat.length ? params : null;
}

export interface NavLinkProps extends Omit<AnchorHTMLAttributes<HTMLAnchorElement>, "style"> {
  to: string;
  /** Extra path prefixes that also mark this link active. */
  activePrefixes?: string[];
  /** Only exact matches are active (default: prefix match). */
  end?: boolean;
  children: ReactNode;
}

export function isActivePath(pathname: string, to: string, end = false, extra: string[] = []): boolean {
  const p = trimSlash(pathname);
  const candidates = [to, ...extra].map(trimSlash);
  return candidates.some((c) => (end || c === "/" ? p === c : p === c || p.startsWith(`${c}/`)));
}

/** Link with aria-current="page" when active. */
export function NavLink({ to, activePrefixes, end = false, className, children, ...rest }: NavLinkProps) {
  const pathname = usePathname();
  const active = isActivePath(pathname, to, end, activePrefixes);
  return (
    <Link
      to={to}
      className={cx(className, active && "is-active")}
      {...(active ? { "aria-current": "page" as const } : {})}
      {...rest}
    >
      {children}
    </Link>
  );
}
