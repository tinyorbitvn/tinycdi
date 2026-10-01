import type { ReactNode } from "react";

// Contract between the app shell (web/src/app) and the feature areas
// (web/src/workspaces, web/src/session, web/src/admin). Each area's
// routes.tsx default-exports nothing and named-exports `routes: AppRoute[]`;
// the shell lazy-loads the module that owns the current path.

/** Path parameters captured from `:name` segments, already URI-decoded. */
export type RouteParams = Readonly<Record<string, string>>;

export interface AppRoute {
  /**
   * Path pattern: literal segments plus `:name` params, e.g.
   * `/workspaces/:id/session`. A trailing `/*` matches any remainder
   * (captured as the `*` param). Matching ignores a trailing slash.
   */
  path: string;
  /** Document title suffix ("<title> · TinyCDI"). */
  title?: string | ((params: RouteParams) => string);
  /** Renders the page. Wrap heavy pages in React.lazy inside the module. */
  render: (params: RouteParams) => ReactNode;
  /** Only shown to tenant admins (`/v1/me` roles contain `tenant-admin`). */
  requires?: "tenant-admin";
  /**
   * `page` (default): centered, max-width content column.
   * `bleed`: full-width, full-height content area under the top bar
   * (used by the in-portal session view).
   */
  layout?: "page" | "bleed";
}
