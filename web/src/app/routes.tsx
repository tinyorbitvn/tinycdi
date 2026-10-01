import type { AppRoute, RouteParams } from "./route-types";
import { matchPath } from "./router";

// Route registry. Each feature area exports `routes: AppRoute[]` from its own
// routes.tsx; the shell lazy-loads only the area that owns the current path.
// Order matters: the first area whose `owns` matches is used.

export interface RouteModule {
  routes: AppRoute[];
}

export interface RouteArea {
  name: "session" | "admin" | "workspaces";
  owns: (pathname: string) => boolean;
  load: () => Promise<RouteModule>;
}

export const ROUTE_AREAS: RouteArea[] = [
  {
    name: "session",
    owns: (p) => /^\/workspaces\/[^/]+\/session\/?$/.test(p),
    load: () => import("../session/routes"),
  },
  {
    name: "admin",
    owns: (p) => p === "/admin" || p.startsWith("/admin/"),
    load: () => import("../admin/routes"),
  },
  {
    name: "workspaces",
    owns: () => true,
    load: () => import("../workspaces/routes"),
  },
];

export function areaFor(pathname: string, areas: RouteArea[] = ROUTE_AREAS): RouteArea {
  return areas.find((a) => a.owns(pathname)) ?? areas[areas.length - 1]!;
}

export interface ResolvedRoute {
  route: AppRoute;
  params: RouteParams;
}

/** First route in `routes` matching `pathname`. */
export function resolveRoute(routes: AppRoute[], pathname: string): ResolvedRoute | null {
  for (const route of routes) {
    const params = matchPath(route.path, pathname);
    if (params) return { route, params };
  }
  return null;
}

const cache = new Map<string, Promise<RouteModule>>();

/** Loads (once) the route module for an area. */
export function loadArea(area: RouteArea): Promise<RouteModule> {
  let p = cache.get(area.name);
  if (!p) {
    p = area.load();
    p.catch(() => cache.delete(area.name));
    cache.set(area.name, p);
  }
  return p;
}
